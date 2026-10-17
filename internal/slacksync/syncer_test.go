package slacksync

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kriakiku/holtdroid-sync/internal/layout"
	"github.com/kriakiku/holtdroid-sync/internal/storage"
)

const token = "xoxb-test"

var apkZip = func() string {
	raw, err := os.ReadFile("../apk/testdata/helloworld.apk")
	if err != nil {
		panic(err)
	}
	return string(raw)
}()

var now = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

type fakeSlack struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	history     map[string][]map[string]any
	replies     map[string][]map[string]any
	files       map[string]map[string]any
	users       map[string]string
	bodies      map[string]string
	downloads   int
	rateLimited int
	calls       []string
}

func newFakeSlack(t *testing.T) *fakeSlack {
	f := &fakeSlack{
		t:       t,
		history: map[string][]map[string]any{},
		replies: map[string][]map[string]any{},
		files:   map[string]map[string]any{},
		users:   map[string]string{"U1": "Alice"},
		bodies:  map[string]string{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/", f.api)
	mux.HandleFunc("/files/", f.download)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) api(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := r.ParseForm(); err != nil {
		f.t.Errorf("parse form: %v", err)
	}
	if r.Header.Get("Authorization") != "Bearer "+token && r.FormValue("token") != token {
		f.t.Errorf("%s without the token", r.URL.Path)
	}
	method := strings.TrimPrefix(r.URL.Path, "/api/")
	f.calls = append(f.calls, method)
	if f.rateLimited > 0 {
		f.rateLimited--
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	var resp map[string]any
	switch method {
	case "conversations.history":
		var msgs []map[string]any
		for _, m := range f.history[r.FormValue("channel")] {
			if m["ts"].(string) >= r.FormValue("oldest") {
				msgs = append(msgs, m)
			}
		}
		resp = map[string]any{"messages": msgs}
	case "conversations.replies":
		resp = map[string]any{"messages": f.replies[r.FormValue("ts")]}
	case "files.info":
		resp = map[string]any{"file": f.files[r.FormValue("file")]}
	case "users.info":
		name, ok := f.users[r.FormValue("user")]
		if !ok {
			resp = map[string]any{"ok": false, "error": "user_not_found"}
			break
		}
		resp = map[string]any{"user": map[string]any{"id": r.FormValue("user"), "name": strings.ToLower(name), "profile": map[string]any{"display_name": name}}}
	case "auth.test":
		resp = map[string]any{"url": "https://acme.slack.com/", "user_id": "UBOT"}
	default:
		f.t.Errorf("unexpected call %s", method)
	}
	if _, ok := resp["ok"]; !ok {
		resp["ok"] = true
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (f *fakeSlack) download(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+token {
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>sign in</html>")
		return
	}
	body, ok := f.bodies[r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	f.downloads++
	_, _ = io.WriteString(w, body)
}

func (f *fakeSlack) file(id, name string) map[string]any {
	p := "/files/" + id + "/" + name
	f.bodies[p] = apkZip
	file := map[string]any{
		"id": id, "name": name, "title": name, "size": len(apkZip),
		"url_private": f.srv.URL + p, "url_private_download": f.srv.URL + p + "?download=1",
	}
	f.files[id] = file
	return file
}

func ts(t time.Time) string { return formatTS(t) }

func newSyncer(t *testing.T, f *fakeSlack, mutate func(*Config)) (*Syncer, storage.Store) {
	t.Helper()
	store, err := storage.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Channels = []string{"C1"}
	cfg.APIURL = f.srv.URL + "/api/"
	if mutate != nil {
		mutate(&cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s := New(cfg, token, store, f.srv.Client())
	s.now = func() time.Time { return now }
	s.log = slog.New(slog.DiscardHandler)
	return s, store
}

func keys(t *testing.T, store storage.Store) []string {
	t.Helper()
	objs, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, o := range objs {
		out = append(out, o.Key)
	}
	slices.Sort(out)
	return out
}

func readMeta(t *testing.T, store storage.Store, key string) layout.Meta {
	t.Helper()
	rc, _, err := store.Get(context.Background(), key+layout.MetaSuffix)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	var m layout.Meta
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSyncCopiesAPKsWithMeta(t *testing.T) {
	f := newFakeSlack(t)
	posted := now.Add(-time.Hour)
	f.history["C1"] = []map[string]any{
		{"ts": ts(posted), "user": "U1", "text": "Build for <@U1> &amp; QA: <https://ci.example/1|pipeline>", "files": []any{f.file("F1", "app-release.apk")}},
		{"ts": ts(now.Add(-2 * time.Hour)), "user": "U1", "text": "logs", "files": []any{f.file("F2", "log.txt")}},
		{"ts": ts(now.Add(-30 * 24 * time.Hour)), "user": "U1", "text": "ancient", "files": []any{f.file("F3", "old.apk")}},
		{"ts": ts(now.Add(-3 * time.Hour)), "bot_profile": map[string]any{"name": "CI"}, "text": "nightly", "files": []any{map[string]any{"id": "F4", "file_access": "check_file_info"}}},
	}
	f.file("F4", "Nightly.APK")
	f.files["F4"]["title"] = "Nightly build"

	s, store := newSyncer(t, f, nil)
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"C1/F1/app-release.apk", "C1/F1/app-release.apk.icon.png", "C1/F1/app-release.apk.meta.json",
		"C1/F4/Nightly.APK", "C1/F4/Nightly.APK.icon.png", "C1/F4/Nightly.APK.meta.json",
	}
	if got := keys(t, store); !slices.Equal(got, want) {
		t.Fatalf("keys %v, want %v", got, want)
	}
	rc, _, _ := store.Get(context.Background(), "C1/F1/app-release.apk")
	body, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(body) != apkZip {
		t.Fatalf("apk body %q", body)
	}

	got := readMeta(t, store, "C1/F1/app-release.apk")
	link := "https://acme.slack.com/archives/C1/p" + strings.Replace(ts(posted), ".", "", 1)
	wantMeta := layout.Meta{
		Note:       link,
		Changelog:  "Build for @Alice & QA: pipeline",
		UploadedBy: "Alice",
		UploadedAt: posted,
		Link:       link,
		Origin:     "slack",
	}
	if !jsonEqual(got, wantMeta) {
		t.Fatalf("meta %+v\nwant %+v", got, wantMeta)
	}
	if m := readMeta(t, store, "C1/F4/Nightly.APK"); m.UploadedBy != "CI" || m.Title != "Nightly build" {
		t.Fatalf("bot meta %+v", m)
	}

	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.downloads != 2 {
		t.Fatalf("downloads = %d, want 2 (second sync must skip synced files)", f.downloads)
	}
}

func TestSyncThreads(t *testing.T) {
	for _, threads := range []bool{true, false} {
		t.Run(map[bool]string{true: "on", false: "off"}[threads], func(t *testing.T) {
			f := newFakeSlack(t)
			parent := ts(now.Add(-2 * time.Hour))
			f.history["C1"] = []map[string]any{{"ts": parent, "user": "U1", "text": "release thread", "reply_count": 1, "latest_reply": ts(now.Add(-time.Hour))}}
			f.replies[parent] = []map[string]any{
				f.history["C1"][0],
				{"ts": ts(now.Add(-time.Hour)), "thread_ts": parent, "user": "U1", "text": "rc1", "files": []any{f.file("F1", "rc1.apk")}},
			}
			s, store := newSyncer(t, f, func(c *Config) { c.Threads = threads })
			if _, err := s.Sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			got := keys(t, store)
			if !threads {
				if len(got) != 0 {
					t.Fatalf("threads off synced %v", got)
				}
				return
			}
			if !slices.Contains(got, "C1/F1/rc1.apk") {
				t.Fatalf("keys %v", got)
			}
			if link := readMeta(t, store, "C1/F1/rc1.apk").Link; !strings.Contains(link, "thread_ts="+parent) {
				t.Fatalf("link %q", link)
			}
		})
	}
}

func TestSyncEditedMessageRefreshesMeta(t *testing.T) {
	f := newFakeSlack(t)
	msg := map[string]any{"ts": ts(now.Add(-time.Hour)), "user": "U1", "text": "v1", "files": []any{f.file("F1", "app.apk")}}
	f.history["C1"] = []map[string]any{msg}
	s, store := newSyncer(t, f, nil)
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	msg["text"] = "v1 with changelog"
	msg["edited"] = map[string]any{"user": "U1", "ts": ts(now)}
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m := readMeta(t, store, "C1/F1/app.apk"); m.Changelog != "v1 with changelog" {
		t.Fatalf("changelog %q", m.Changelog)
	}
	if f.downloads != 1 {
		t.Fatalf("downloads = %d, an edit must not download again", f.downloads)
	}
}

func TestSyncRejectsNonAPKDownloads(t *testing.T) {
	f := newFakeSlack(t)
	file := f.file("F1", "app.apk")
	f.bodies["/files/F1/app.apk"] = "plain text, not a zip"
	f.history["C1"] = []map[string]any{{"ts": ts(now.Add(-time.Hour)), "user": "U1", "files": []any{file}}}

	s, store := newSyncer(t, f, nil)
	if _, err := s.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "not an APK") {
		t.Fatalf("err = %v", err)
	}
	if got := keys(t, store); slices.Contains(got, "C1/F1/app.apk") {
		t.Fatalf("broken download stored: %v", got)
	}
}

func TestSyncRefusesForeignFileHosts(t *testing.T) {
	f := newFakeSlack(t)
	file := f.file("F1", "app.apk")
	file["url_private_download"] = "https://evil.example/app.apk"
	f.history["C1"] = []map[string]any{{"ts": ts(now.Add(-time.Hour)), "user": "U1", "files": []any{file}}}

	s, _ := newSyncer(t, f, nil)
	if _, err := s.Sync(context.Background()); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v", err)
	}
}

func TestSyncRetriesRateLimits(t *testing.T) {
	f := newFakeSlack(t)
	f.history["C1"] = []map[string]any{{"ts": ts(now.Add(-time.Hour)), "user": "U1", "files": []any{f.file("F1", "app.apk")}}}
	f.rateLimited = 2
	s, store := newSyncer(t, f, nil)
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(keys(t, store), "C1/F1/app.apk") {
		t.Fatal("not synced after rate limits")
	}
}

func TestSyncPrunesOldBuilds(t *testing.T) {
	f := newFakeSlack(t)
	s, store := newSyncer(t, f, func(c *Config) { c.Retention = 30 * 24 * time.Hour })
	ctx := context.Background()
	for _, key := range []string{"C1/F9/old.apk", "C1/F9/old.apk.meta.json", "C2/F8/other.apk"} {
		if err := store.Put(ctx, key, strings.NewReader("x"), 1); err != nil {
			t.Fatal(err)
		}
	}
	s.now = func() time.Time { return time.Now().Add(31 * 24 * time.Hour) }
	if _, err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, store); !slices.Equal(got, []string{"C2/F8/other.apk"}) {
		t.Fatalf("keys %v", got)
	}
}

func TestSyncForwardedMessages(t *testing.T) {
	f := newFakeSlack(t)
	original := now.Add(-48 * time.Hour)
	forward := now.Add(-time.Hour)
	f.history["C1"] = []map[string]any{{
		"ts": ts(forward), "user": "U2", "text": "for QA",
		"attachments": []any{map[string]any{
			"ts": ts(original), "author_id": "U1", "author_name": "alice", "is_share": true,
			"text":  "release 1.2: fixes <https://jira.example/X-1|X-1>",
			"files": []any{f.file("F1", "app.apk")},
		}},
	}}
	f.users["U2"] = "Bob"

	s, store := newSyncer(t, f, nil)
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := readMeta(t, store, "C1/F1/app.apk")
	if m.Changelog != "for QA\n\nrelease 1.2: fixes X-1" || m.UploadedBy != "Alice" || !m.UploadedAt.Equal(original) {
		t.Fatalf("meta %+v", m)
	}
	if want := "https://acme.slack.com/archives/C1/p" + strings.Replace(ts(forward), ".", "", 1); m.Link != want || m.Note != want {
		t.Fatalf("link %q, note %q, want %q", m.Link, m.Note, want)
	}
}

func TestSyncSkipsUnchangedThreads(t *testing.T) {
	f := newFakeSlack(t)
	parent := ts(now.Add(-2 * time.Hour))
	reply := map[string]any{"ts": ts(now.Add(-time.Hour)), "thread_ts": parent, "user": "U1", "files": []any{f.file("F1", "rc1.apk")}}
	f.history["C1"] = []map[string]any{{"ts": parent, "user": "U1", "reply_count": 1, "latest_reply": reply["ts"]}}
	f.replies[parent] = []map[string]any{f.history["C1"][0], reply}
	s, store := newSyncer(t, f, nil)

	countReplies := func() int {
		n := 0
		for _, c := range f.calls {
			if c == "conversations.replies" {
				n++
			}
		}
		return n
	}
	for range 2 {
		if _, err := s.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := countReplies(); n != 1 {
		t.Fatalf("conversations.replies called %d times for an unchanged thread", n)
	}

	reply2 := map[string]any{"ts": ts(now.Add(-time.Minute)), "thread_ts": parent, "user": "U1", "files": []any{f.file("F2", "rc2.apk")}}
	f.replies[parent] = append(f.replies[parent], reply2)
	f.history["C1"][0]["reply_count"] = 2
	f.history["C1"][0]["latest_reply"] = reply2["ts"]
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := countReplies(); n != 2 || !slices.Contains(keys(t, store), "C1/F2/rc2.apk") {
		t.Fatalf("new reply not picked up: %d calls, keys %v", n, keys(t, store))
	}
}

func TestSyncReturnsNewBuilds(t *testing.T) {
	f := newFakeSlack(t)
	posted := now.Add(-time.Hour)
	f.history["C1"] = []map[string]any{{"ts": ts(posted), "user": "U1", "text": "new build", "files": []any{f.file("F1", "hello.apk")}}}
	s, _ := newSyncer(t, f, nil)

	st, err := s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Builds) != 1 || st.Copied != 1 {
		t.Fatalf("stats %+v", st)
	}
	b := st.Builds[0]
	if b.Key != "C1/F1/hello.apk" || !b.Modified.Equal(posted.Truncate(time.Microsecond)) || !strings.Contains(b.Link, "/archives/C1/p") ||
		b.App == nil || b.App.Package != "com.example.helloworld" || b.Size == 0 {
		t.Fatalf("build %+v", b)
	}

	if st, err := s.Sync(context.Background()); err != nil || len(st.Builds) != 0 {
		t.Fatalf("an already copied build must not be reported again: %+v, %v", st, err)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		ok     bool
	}{
		{"defaults with a channel", func(*Config) {}, true},
		{"no channels", func(c *Config) { c.Channels = nil }, false},
		{"channel name instead of ID", func(c *Config) { c.Channels = []string{"#builds"} }, false},
		{"retention shorter than lookback", func(c *Config) { c.Retention = time.Hour }, false},
		{"retention longer than lookback", func(c *Config) { c.Retention = 30 * 24 * time.Hour }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Channels = []string{"C0123ABCD"}
			tt.mutate(&cfg)
			if err := cfg.Validate(); (err == nil) != tt.ok {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
