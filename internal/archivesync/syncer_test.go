package archivesync

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/holtdroid/holtdroid-sync/internal/layout"
	"github.com/holtdroid/holtdroid-sync/internal/rules"
	"github.com/holtdroid/holtdroid-sync/internal/storage"
)

type fakeArchive struct {
	srv       *httptest.Server
	apk       []byte
	mu        sync.Mutex
	pages     map[string][]string
	modified  time.Time
	downloads int
	fail      string
	agents    map[string]bool
}

func newFake(t *testing.T) *fakeArchive {
	raw, err := os.ReadFile("../apk/testdata/helloworld.apk")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeArchive{apk: raw, pages: map[string][]string{}, modified: time.Date(2026, 10, 23, 15, 35, 0, 0, time.UTC), agents: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeArchive) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agents[r.UserAgent()] = true
	if r.URL.Path == f.fail {
		http.Error(w, "boom", http.StatusBadGateway)
		return
	}
	if entries, ok := f.pages[r.URL.Path]; ok {
		for _, e := range append([]string{"../", "https://other.example.com/x.apk", "?C=N;O=D"}, entries...) {
			fmt.Fprintf(w, "<a href=%q>%s</a>\n", e, e)
		}
		return
	}
	if !strings.HasSuffix(r.URL.Path, ".apk") {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Last-Modified", f.modified.Format(http.TimeFormat))
	w.Header().Set("Content-Length", strconv.Itoa(len(f.apk)))
	if r.Method == http.MethodGet {
		f.downloads++
		_, _ = w.Write(f.apk)
	}
}

func (f *fakeArchive) archive(t *testing.T, a Archive, root string, patterns ...string) Archive {
	u, err := url.Parse(f.srv.URL + root)
	if err != nil {
		t.Fatal(err)
	}
	if a.Rules, err = rules.Parse(patterns); err != nil {
		t.Fatal(err)
	}
	a.URL = u
	return a
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
	out := []string{}
	for _, o := range objs {
		if layout.IsAPK(o.Key) {
			out = append(out, o.Key)
		}
	}
	slices.Sort(out)
	return out
}

func (f *fakeArchive) operaPages() {
	f.pages["/android/"] = []string{"10200/", "9900/", "10300/", "mini/"}
	f.pages["/android/10300/"] = []string{
		"Opera_103_1_5234_90883_Generic_Opera_arm8_LZMA.apk", "Opera_103_1_5234_90883_Generic_Opera_x86_LZMA.apk",
		"Opera_103_3_5234_91185_Generic_Opera_arm8_LZMA.apk", "Opera_103_3_5234_91185_Generic_Opera_arm8_LZMA.apk.sha256sum",
		"Opera_103_3_5234_91185_Generic_Opera_x86_LZMA.apk", "Opera_103_3_5234_91185_Generic_Opera_arm7_LZMA.apk",
	}
	f.pages["/android/10200/"] = []string{"Opera_102_9_1_2_Generic_Opera_arm8_LZMA.apk"}
}

var operaVersion = regexp.MustCompile(`Opera_([0-9_]+)_Generic`)

func TestNewestFilePerVersionDirectory(t *testing.T) {
	f := newFake(t)
	f.operaPages()
	store := newStore(t)
	a := f.archive(t, Archive{Name: "opera", Title: "Opera", VersionDir: regexp.MustCompile(`^\d+$`), FileVersion: operaVersion, Versions: 2}, "/android/", "*_arm8_*.apk", "*_x86_*.apk")
	s := New(f.srv.Client(), "holtdroid-archive-sync/test", []Archive{a}, store, true)
	st, err := s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"opera/10200/Opera_102_9_1_2_Generic_Opera_arm8_LZMA.apk",
		"opera/10300/Opera_103_3_5234_91185_Generic_Opera_arm8_LZMA.apk",
		"opera/10300/Opera_103_3_5234_91185_Generic_Opera_x86_LZMA.apk",
	}
	if got := apks(t, store); !slices.Equal(got, want) || st.Copied != 3 {
		t.Fatalf("copied %v (%d)", got, st.Copied)
	}
	if !f.agents["holtdroid-archive-sync/test"] || len(f.agents) != 1 {
		t.Fatalf("user agents %v", f.agents)
	}

	rc, _, err := store.Get(context.Background(), want[1]+layout.MetaSuffix)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	raw, _ := io.ReadAll(rc)
	var m layout.Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if !m.UploadedAt.Equal(f.modified) || m.Origin != "archive" || !strings.HasPrefix(m.Title, "Opera") || !strings.HasSuffix(m.Link, "/android/10300/") {
		t.Fatalf("meta %+v", m)
	}

	before := f.downloads
	if st, err := s.Sync(context.Background()); err != nil || st.Copied != 0 || f.downloads != before {
		t.Fatalf("second pass copied %d (%d downloads): %v", st.Copied, f.downloads-before, err)
	}
}

func TestVersionsDirectlyUnderTheRoot(t *testing.T) {
	f := newFake(t)
	f.pages["/gx/"] = []string{"opera-gx-3.3.9.apk", "opera-gx-3.3.18.apk", "opera-gx-3.3.13.apk", "notes.txt"}
	store := newStore(t)
	a := f.archive(t, Archive{Name: "gx", FileVersion: regexp.MustCompile(`opera-gx-([0-9.]+)\.apk`), Versions: 2}, "/gx/")
	if _, err := New(f.srv.Client(), "ua", []Archive{a}, store, true).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := apks(t, store), []string{"gx/opera-gx-3.3.13.apk", "gx/opera-gx-3.3.18.apk"}; !slices.Equal(got, want) {
		t.Fatalf("copied %v", got)
	}
}

func TestSubdirectoriesAndAbsoluteLinks(t *testing.T) {
	f := newFake(t)
	f.pages["/fenix/"] = []string{"/fenix/160.0/", "/fenix/160.0b1/", "/fenix/159.0.2/"}
	f.pages["/fenix/160.0/"] = []string{"/fenix/160.0/android/"}
	f.pages["/fenix/160.0/android/"] = []string{"/fenix/160.0/android/fenix-160.0-android-x86_64/", "/fenix/160.0/android/fenix-160.0-android-armeabi-v7a/"}
	f.pages["/fenix/160.0/android/fenix-160.0-android-x86_64/"] = []string{"/fenix/160.0/android/fenix-160.0-android-x86_64/fenix-160.0.multi.android-x86_64.apk"}
	f.pages["/fenix/160.0/android/fenix-160.0-android-armeabi-v7a/"] = []string{"/fenix/160.0/android/fenix-160.0-android-armeabi-v7a/fenix-160.0.multi.android-armeabi-v7a.apk"}
	store := newStore(t)
	a := f.archive(t, Archive{Name: "firefox", VersionDir: regexp.MustCompile(`^\d+\.\d+(\.\d+)?$`), FileVersion: regexp.MustCompile(`fenix-([0-9.]+)\.multi`), Depth: 2, Versions: 1}, "/fenix/", "*x86_64.apk")
	if _, err := New(f.srv.Client(), "ua", []Archive{a}, store, true).Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"firefox/160.0/android/fenix-160.0-android-x86_64/fenix-160.0.multi.android-x86_64.apk"}
	if got := apks(t, store); !slices.Equal(got, want) {
		t.Fatalf("copied %v", got)
	}
}

func TestPrune(t *testing.T) {
	f := newFake(t)
	f.operaPages()
	store := newStore(t)
	a := f.archive(t, Archive{Name: "opera", VersionDir: regexp.MustCompile(`^\d+$`), FileVersion: operaVersion, Versions: 1}, "/android/", "*_arm8_*.apk")
	s := New(f.srv.Client(), "ua", []Archive{a}, store, true)
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.fail = "/android/10300/"
	f.pages["/android/"] = []string{"10400/", "10300/"}
	f.pages["/android/10400/"] = []string{"Opera_104_0_1_2_Generic_Opera_arm8_LZMA.apk"}
	a.Versions = 2
	s = New(f.srv.Client(), "ua", []Archive{a}, store, true)
	if _, err := s.Sync(context.Background()); err == nil {
		t.Fatal("no error from a failed listing")
	}
	if got := apks(t, store); len(got) != 1 {
		t.Fatalf("failed listing changed copies: %v", got)
	}

	f.fail = ""
	a.Versions = 1
	s = New(f.srv.Client(), "ua", []Archive{a}, store, true)
	st, err := s.Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := apks(t, store), []string{"opera/10400/Opera_104_0_1_2_Generic_Opera_arm8_LZMA.apk"}; !slices.Equal(got, want) || st.Pruned != 1 {
		t.Fatalf("after prune %v (%d)", got, st.Pruned)
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"10300", "9900", 1},
		{"3.3.18", "3.3.9", 1},
		{"159.0.2", "160.0", -1},
		{"160.0", "160.0.1", -1},
		{"103_3_5234_91185", "103_3_5234_91185", 0},
	}
	for _, tt := range tests {
		if got := compareVersions(tt.a, tt.b); got != tt.want {
			t.Errorf("compare(%s, %s) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}
