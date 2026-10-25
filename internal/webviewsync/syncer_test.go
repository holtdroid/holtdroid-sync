package webviewsync

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/holtdroid/holtdroid-sync/internal/apk"
	"github.com/holtdroid/holtdroid-sync/internal/layout"
	"github.com/holtdroid/holtdroid-sync/internal/storage"
)

const testAPK = "../apk/testdata/helloworld.apk"

func snapshotZip(t *testing.T, method uint16) []byte {
	t.Helper()
	raw, err := os.ReadFile(testAPK)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range []struct {
		name string
		body []byte
	}{
		{"chrome-android-desktop/apks/ChromePublic.apk", bytes.Repeat([]byte("x"), 3<<20)},
		{"chrome-android-desktop/apks/SystemWebView.apk", raw},
	} {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: f.name, Method: method})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(f.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type fakeChromium struct {
	t   *testing.T
	srv *httptest.Server
	zip []byte

	mu         sync.Mutex
	channels   map[string]int
	canary     map[int]int
	snapshots  []int
	downloads  int
	dashFails  bool
	milestones []milestone
}

func newFakeChromium(t *testing.T) *fakeChromium {
	f := &fakeChromium{
		t:         t,
		zip:       snapshotZip(t, zip.Deflate),
		channels:  map[string]int{"Stable": 156, "Beta": 157, "Dev": 158, "Canary": 158},
		canary:    map[int]int{},
		snapshots: []int{1300000},
	}
	for m := 158; m >= 140; m-- {
		pos := 1300000 + (m-140)*10000
		f.canary[m] = pos + 100
		f.snapshots = append(f.snapshots, pos+50, pos+90)
		if m < 158 {
			f.milestones = append(f.milestones, milestone{Milestone: m, Position: pos})
		}
	}
	slices.Sort(f.snapshots)
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeChromium) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	switch {
	case r.URL.Path == "/fetch_releases":
		if f.dashFails {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		m := f.channels[q.Get("channel")]
		if s := q.Get("milestone"); s != "" {
			m, _ = strconv.Atoi(s)
		}
		pos, ok := f.canary[m]
		if !ok {
			_, _ = w.Write([]byte("[]"))
			return
		}
		writeJSON(w, []map[string]any{{
			"milestone": m, "version": fmt.Sprintf("%d.0.1.0", m), "chromium_main_branch_position": pos,
			"time": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).UnixMilli(), "hashes": map[string]string{"chromium": fmt.Sprintf("hash%d", m)},
		}})
	case r.URL.Path == "/fetch_milestones":
		writeJSON(w, f.milestones)
	case r.URL.Path == "/storage/v1/b/"+Bucket+"/o":
		if q.Get("prefix") != Platform+"/" || q.Get("delimiter") != "/" {
			f.t.Errorf("listing %s", r.URL.RawQuery)
		}
		var prefixes []string
		for _, rev := range f.snapshots {
			name := fmt.Sprintf("%s/%d/", Platform, rev)
			if s := q.Get("startOffset"); s != "" && name < s {
				continue
			}
			if e := q.Get("endOffset"); e != "" && name >= e {
				continue
			}
			prefixes = append(prefixes, name)
		}
		if q.Get("maxResults") == "1" && len(prefixes) > 1 {
			prefixes = prefixes[:1]
		}
		writeJSON(w, listing{Prefixes: prefixes})
	case strings.HasPrefix(r.URL.Path, "/"+Bucket+"/"+Platform+"/") && strings.HasSuffix(r.URL.Path, "/chrome-android-desktop.zip"):
		if r.Method == http.MethodGet && r.Header.Get("Range") != "" && !strings.HasSuffix(r.Header.Get("Range"), strconv.Itoa(len(f.zip)-1)) {
			f.downloads++
		}
		http.ServeContent(w, r, "chrome-android-desktop.zip", time.Time{}, bytes.NewReader(f.zip))
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeChromium) set(fn func(*fakeChromium)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeChromium) syncer(t *testing.T, sel Selection, store storage.Store) *Syncer {
	t.Helper()
	meta, _, err := apk.Parse(testAPK)
	if err != nil {
		t.Fatal(err)
	}
	s := New(f.srv.Client(), sel, []Requirement{{From: 150, MinSDK: 34}, {From: 157, MinSDK: 35}}, store, true)
	s.dash, s.snapshots, s.pkg = f.srv.URL, f.srv.URL, meta.Package
	return s
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

func apkKeys(t *testing.T, store storage.Store) []string {
	t.Helper()
	objs, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range objs {
		if layout.IsAPK(o.Key) {
			keys = append(keys, o.Key)
		}
	}
	slices.Sort(keys)
	return keys
}

func TestSync(t *testing.T) {
	ctx := context.Background()
	f := newFakeChromium(t)
	store, err := storage.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := f.syncer(t, Selection{Latest: 2, Every: 5, Milestones: []int{141, 120}}, store)

	st, err := s.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"chromium-140.apk", "chromium-141.apk", "chromium-145.apk", "chromium-150.apk", "chromium-155.apk", "chromium-156.apk", "chromium-157.apk", "chromium-158.apk"}
	if got := apkKeys(t, store); !slices.Equal(got, want) || st.Copied != len(want) || f.downloads != len(want) {
		t.Fatalf("keys %v, stats %+v, downloads %d", got, st, f.downloads)
	}
	m := readMeta(t, store, "chromium-158.apk")
	if m.Title != "Chromium 158" || m.Origin != "chromium-snapshots" || m.Tags["milestone"] != "158" || m.Tags["revision"] != "1480090" ||
		m.Tags["channel"] != "dev,canary" || m.Tags["frontend"] != "hash158" || m.Tags["version"] == "" || m.Tags["minSdk"] == "" {
		t.Fatalf("meta %+v", m)
	}
	if tags := readMeta(t, store, "chromium-150.apk").Tags; tags["channel"] != "" || tags["requiresSdk"] != "34" {
		t.Fatalf("150 tags %v", tags)
	}
	if tags := readMeta(t, store, "chromium-145.apk").Tags; tags["requiresSdk"] != "" {
		t.Fatalf("145 tags %v", tags)
	}
	if r := m.Tags["requiresSdk"]; r != "35" {
		t.Fatalf("158 requiresSdk %q", r)
	}

	st, err = s.Sync(ctx)
	if err != nil || st.Copied != 0 || st.Skipped != len(want) || f.downloads != len(want) {
		t.Fatalf("second pass %+v %v, downloads %d", st, err, f.downloads)
	}

	s.reqs = []Requirement{{From: 140, MinSDK: 31}}
	if _, err := s.Sync(ctx); err != nil || f.downloads != len(want) {
		t.Fatalf("new requirements: %v, downloads %d", err, f.downloads)
	}
	if r := readMeta(t, store, "chromium-158.apk").Tags["requiresSdk"]; r != "31" {
		t.Fatalf("158 requiresSdk after a rule change %q", r)
	}

	f.set(func(f *fakeChromium) {
		f.channels["Stable"], f.channels["Beta"] = 157, 158
		f.canary[158] += 30
		f.snapshots = append(f.snapshots, f.canary[158]-5)
	})
	st, err = s.Sync(ctx)
	if err != nil || st.Copied != 1 || st.Pruned != 1 || f.downloads != len(want)+1 {
		t.Fatalf("third pass %+v %v, downloads %d", st, err, f.downloads)
	}
	if m := readMeta(t, store, "chromium-157.apk"); m.Tags["channel"] != "stable" {
		t.Fatalf("157 channel %q", m.Tags["channel"])
	}
	if m := readMeta(t, store, "chromium-158.apk"); m.Tags["channel"] != "beta,dev,canary" || m.Tags["revision"] != "1480125" {
		t.Fatalf("158 meta %+v", m.Tags)
	}
	if got := apkKeys(t, store); slices.Contains(got, "chromium-156.apk") || len(got) != len(want)-1 {
		t.Fatalf("156 left the stable channel and no other rule picks it: %v", got)
	}

	f.set(func(f *fakeChromium) { f.dashFails = true })
	s.sel = Selection{Latest: 1}
	if _, err := s.Sync(ctx); err == nil {
		t.Fatal("sync with Chromium Dash down succeeded")
	}
	if got := apkKeys(t, store); len(got) != len(want)-1 {
		t.Fatalf("pruned after a failed listing: %v", got)
	}

	f.set(func(f *fakeChromium) { f.dashFails = false })
	st, err = s.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := apkKeys(t, store); !slices.Equal(got, []string{"chromium-157.apk", "chromium-158.apk"}) || st.Pruned != len(want)-3 {
		t.Fatalf("after prune %v, %+v", got, st)
	}
	if _, err := store.Stat(ctx, "chromium-150.apk"+layout.MetaSuffix); err == nil {
		t.Fatal("sidecar of a pruned build left behind")
	}
}

func TestOpenEntryStored(t *testing.T) {
	body := snapshotZip(t, zip.Store)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "x.zip", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()
	rc, size, err := openEntry(context.Background(), srv.Client(), srv.URL, apkEntry)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	got := new(bytes.Buffer)
	if _, err := got.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(testAPK)
	if size != int64(len(want)) || !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("got %d bytes (size %d), want %d", got.Len(), size, len(want))
	}
	if _, _, err := openEntry(context.Background(), srv.Client(), srv.URL, "/missing.apk"); err == nil {
		t.Fatal("missing entry opened")
	}
}
