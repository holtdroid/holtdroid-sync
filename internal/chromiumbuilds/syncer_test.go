package chromiumbuilds

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v93/github"

	"github.com/holtdroid/holtdroid-sync/internal/layout"
	"github.com/holtdroid/holtdroid-sync/internal/snapshotsync"
	"github.com/holtdroid/holtdroid-sync/internal/storage"
)

type fakeAsset struct {
	Name    string    `json:"name"`
	State   string    `json:"state"`
	Size    int       `json:"size"`
	Updated time.Time `json:"updated_at"`
	URL     string    `json:"browser_download_url"`
}

type fakeRelease struct {
	Tag        string      `json:"tag_name"`
	Body       string      `json:"body"`
	Draft      bool        `json:"draft"`
	Prerelease bool        `json:"prerelease"`
	HTMLURL    string      `json:"html_url"`
	Assets     []fakeAsset `json:"assets"`
}

type fakeGitHub struct {
	t         *testing.T
	srv       *httptest.Server
	apk       []byte
	mu        sync.Mutex
	releases  []*fakeRelease
	downloads int
	fail      bool
}

func newFake(t *testing.T) *fakeGitHub {
	raw, err := os.ReadFile("../apk/testdata/helloworld.apk")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeGitHub{t: t, apk: raw}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasSuffix(r.URL.Path, "/releases"):
		if f.fail {
			http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(f.releases)
	case strings.HasPrefix(r.URL.Path, "/blob/"):
		f.downloads++
		_, _ = w.Write(f.apk)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) add(tag, line string, prerelease bool, arches ...string) *fakeRelease {
	f.mu.Lock()
	defer f.mu.Unlock()
	rel := &fakeRelease{Tag: tag, Body: line + "\n\n\nChromium " + tag, Prerelease: prerelease, HTMLURL: "https://example.com/" + tag}
	for _, product := range []string{"chromium", "webview"} {
		for _, arch := range arches {
			name := fmt.Sprintf("%s-%s-%s.apk", product, tag, arch)
			rel.Assets = append(rel.Assets, fakeAsset{Name: name, State: "uploaded", Size: len(f.apk), Updated: time.Now().Add(-time.Hour), URL: f.srv.URL + "/blob/" + name})
		}
	}
	f.releases = append(f.releases, rel)
	return rel
}

func (f *fakeGitHub) syncer(t *testing.T, store storage.Store) *Syncer {
	base := f.srv.URL + "/"
	c, err := github.NewClient(github.WithHTTPClient(f.srv.Client()), github.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	reqs := Requirements{Browser: []snapshotsync.Requirement{{From: 149, MinSDK: 34}, {From: 157, MinSDK: 35}}, WebView: []snapshotsync.Requirement{{From: 149, MinSDK: 34}}}
	return New(c, f.srv.Client(), "holtdroid", "chromium-builds", reqs, store, true)
}

func newStore(t *testing.T) storage.Store {
	store, err := storage.New(context.Background(), storage.Config{Type: "filesystem", Filesystem: storage.FilesystemConfig{Dir: t.TempDir()}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func apks(t *testing.T, store storage.Store) []string {
	objs, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, o := range objs {
		if layout.IsAPK(o.Key) {
			out = append(out, o.Key)
		}
	}
	slices.Sort(out)
	return out
}

func meta(t *testing.T, store storage.Store, key string) layout.Meta {
	rc, _, err := store.Get(context.Background(), key+layout.MetaSuffix)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	raw, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	var m layout.Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func keysOf(tag string) []string {
	var out []string
	for _, p := range products {
		for _, arch := range arches {
			out = append(out, fmt.Sprintf("%s/%s/%s-%s-%s.apk", p.dir, tag, p.file, tag, arch))
		}
	}
	slices.Sort(out)
	return out
}

const stable = "Released to Android Stable: 2023-10-30T22:04:30Z"

func TestWaitsForAllFourAPKs(t *testing.T) {
	f := newFake(t)
	store := newStore(t)
	s := f.syncer(t, store)
	partial := f.add("150.0.1.2", stable, false, "x86_64")
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := apks(t, store); len(got) != 0 || f.downloads != 0 {
		t.Fatalf("copied a partial release: %v, %d downloads", got, f.downloads)
	}

	for _, product := range []string{"chromium", "webview"} {
		name := product + "-150.0.1.2-arm64.apk"
		partial.Assets = append(partial.Assets, fakeAsset{Name: name, State: "starter", Size: len(f.apk), Updated: time.Now().Add(-time.Hour), URL: f.srv.URL + "/blob/" + name})
	}
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := apks(t, store); len(got) != 0 {
		t.Fatalf("copied before every upload finished: %v", got)
	}

	for i := range partial.Assets {
		partial.Assets[i].State = "uploaded"
	}
	st, err := s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := apks(t, store), keysOf("150.0.1.2"); !slices.Equal(got, want) || st.Copied != 4 {
		t.Fatalf("copied %v (%d), want %v", got, st.Copied, want)
	}
}

func TestDateAndSidecar(t *testing.T) {
	f := newFake(t)
	store := newStore(t)
	s := f.syncer(t, store)
	f.add("150.0.1.2", stable, false, "x86_64", "arm64")
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := meta(t, store, "webview/150.0.1.2/webview-150.0.1.2-x86_64.apk")
	want := time.Date(2023, 10, 30, 22, 4, 30, 0, time.UTC)
	if !m.UploadedAt.Equal(want) || m.Title != "Chromium 150" || m.Link != "https://example.com/150.0.1.2" {
		t.Fatalf("meta %+v", m)
	}
	for k, v := range map[string]string{"version": "150.0.1.2", "milestone": "150", "channel": "stable", "requiresSdk": "34"} {
		if m.Tags[k] != v {
			t.Errorf("tag %s = %q, want %q", k, m.Tags[k], v)
		}
	}
	if m.Tags["minSdk"] == "" || m.Tags["targetSdk"] == "" {
		t.Errorf("no sdk tags: %v", m.Tags)
	}
}

func TestReleasesWithoutADateAreSkipped(t *testing.T) {
	f := newFake(t)
	store := newStore(t)
	f.add("150.0.1.2", "Some notes", false, "x86_64", "arm64")
	f.add("151.0.1.2", "Released to Android Stable: soon", false, "x86_64", "arm64")
	if _, err := f.syncer(t, store).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := apks(t, store); len(got) != 0 {
		t.Fatalf("copied %v", got)
	}
}

func TestRequirementsPerProduct(t *testing.T) {
	f := newFake(t)
	store := newStore(t)
	f.add("158.0.1.2", stable, false, "x86_64", "arm64")
	f.add("120.0.1.2", stable, false, "x86_64", "arm64")
	if _, err := f.syncer(t, store).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := meta(t, store, "browser/158.0.1.2/chromium-158.0.1.2-x86_64.apk").Tags["requiresSdk"]; got != "35" {
		t.Errorf("browser requiresSdk %q", got)
	}
	if got := meta(t, store, "webview/158.0.1.2/webview-158.0.1.2-x86_64.apk").Tags["requiresSdk"]; got != "34" {
		t.Errorf("webview requiresSdk %q", got)
	}
	if got, ok := meta(t, store, "webview/120.0.1.2/webview-120.0.1.2-x86_64.apk").Tags["requiresSdk"]; ok {
		t.Errorf("old build requiresSdk %q", got)
	}
}

func TestChannelMovesToTheNewestBuild(t *testing.T) {
	f := newFake(t)
	store := newStore(t)
	s := f.syncer(t, store)
	f.add("157.0.1.2", stable, false, "x86_64", "arm64")
	f.add("158.0.1.2", "Released to Android Beta: 2026-10-28T13:27:08Z", true, "x86_64", "arm64")
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := "browser/157.0.1.2/chromium-157.0.1.2-x86_64.apk"
	if got := meta(t, store, old).Tags["channel"]; got != "stable" {
		t.Fatalf("157 channel %q", got)
	}
	if got := meta(t, store, "browser/158.0.1.2/chromium-158.0.1.2-x86_64.apk").Tags["channel"]; got != "beta" {
		t.Fatalf("158 channel %q", got)
	}

	before := f.downloads
	f.add("157.0.3.4", stable, false, "x86_64", "arm64")
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, ok := meta(t, store, old).Tags["channel"]; ok {
		t.Fatalf("old stable keeps channel %q", got)
	}
	if got := meta(t, store, "browser/157.0.3.4/chromium-157.0.3.4-x86_64.apk").Tags["channel"]; got != "stable" {
		t.Fatalf("new stable channel %q", got)
	}
	if f.downloads != before+4 {
		t.Fatalf("%d downloads, want %d", f.downloads, before+4)
	}
}

func TestPrune(t *testing.T) {
	f := newFake(t)
	store := newStore(t)
	s := f.syncer(t, store)
	f.add("150.0.1.2", stable, false, "x86_64", "arm64")
	f.add("151.0.1.2", stable, false, "x86_64", "arm64")
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	f.fail = true
	if _, err := s.Sync(context.Background()); err == nil {
		t.Fatal("no error from a failed listing")
	}
	if got := apks(t, store); len(got) != 8 {
		t.Fatalf("failed listing pruned: %v", got)
	}
	f.fail = false

	f.releases[1].Assets = f.releases[1].Assets[:1]
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := apks(t, store); len(got) != 8 {
		t.Fatalf("a release being re-uploaded lost its copies: %v", got)
	}

	f.releases = f.releases[:1]
	st, err := s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := apks(t, store), keysOf("150.0.1.2"); !slices.Equal(got, want) || st.Pruned != 4 {
		t.Fatalf("after prune %v (%d)", got, st.Pruned)
	}
}
