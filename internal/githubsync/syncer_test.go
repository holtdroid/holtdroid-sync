package githubsync

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-github/v93/github"

	"github.com/holtdroid/holtdroid-sync/internal/layout"
	"github.com/holtdroid/holtdroid-sync/internal/rules"
	"github.com/holtdroid/holtdroid-sync/internal/storage"
)

var apkBytes = func() []byte {
	raw, err := os.ReadFile("../apk/testdata/helloworld.apk")
	if err != nil {
		panic(err)
	}
	return raw
}()

type fakeAsset struct {
	ID      int64     `json:"id"`
	Name    string    `json:"name"`
	Size    int       `json:"size"`
	Updated time.Time `json:"updated_at"`
	body    []byte
}

type fakeRelease struct {
	Tag        string      `json:"tag_name"`
	Name       string      `json:"name"`
	Body       string      `json:"body"`
	Draft      bool        `json:"draft"`
	Prerelease bool        `json:"prerelease"`
	HTMLURL    string      `json:"html_url"`
	Assets     []fakeAsset `json:"assets"`
}

type fakeGitHub struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	releases  map[string][]fakeRelease
	failRepo  string
	downloads int
	pageSize  int
	nextID    int64
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{t: t, releases: map[string][]fakeRelease{}, pageSize: 2}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) release(repo, tag string, prerelease bool, assets ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := fakeRelease{Tag: tag, Name: "Release " + tag, Body: "notes for " + tag + "\r\n* fix", Prerelease: prerelease, HTMLURL: "https://github.com/" + repo + "/releases/tag/" + tag}
	for _, name := range assets {
		f.nextID++
		body := apkBytes
		if !strings.HasSuffix(name, ".apk") {
			body = []byte("not an apk")
		}
		r.Assets = append(r.Assets, fakeAsset{ID: f.nextID, Name: name, Size: len(body), Updated: time.Now().Add(-time.Minute), body: body})
	}
	f.releases[repo] = append([]fakeRelease{r}, f.releases[repo]...)
}

func (f *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 4 && parts[0] == "repos" && parts[3] == "releases":
		repo := parts[1] + "/" + parts[2]
		if repo == f.failRepo {
			http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		all := f.releases[repo]
		start, end := min((page-1)*f.pageSize, len(all)), min(page*f.pageSize, len(all))
		if end < len(all) {
			next := url.URL{Path: r.URL.Path, RawQuery: url.Values{"page": {strconv.Itoa(page + 1)}, "per_page": {"100"}}.Encode()}
			w.Header().Set("Link", fmt.Sprintf(`<%s%s>; rel="next"`, f.srv.URL, next.String()))
		}
		_ = json.NewEncoder(w).Encode(all[start:end])
	case len(parts) == 6 && parts[3] == "releases" && parts[4] == "assets":
		if r.Header.Get("Accept") != "application/octet-stream" {
			f.t.Errorf("asset download with Accept %q", r.Header.Get("Accept"))
		}
		http.Redirect(w, r, f.srv.URL+"/blob/"+parts[5], http.StatusFound)
	case len(parts) == 2 && parts[0] == "blob":
		id, _ := strconv.ParseInt(parts[1], 10, 64)
		for _, rels := range f.releases {
			for _, rel := range rels {
				for _, a := range rel.Assets {
					if a.ID == id {
						f.downloads++
						_, _ = w.Write(a.body)
						return
					}
				}
			}
		}
		http.NotFound(w, r)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL)
		http.NotFound(w, r)
	}
}

func (f *fakeGitHub) client() *github.Client {
	base := f.srv.URL + "/"
	c, err := github.NewClient(github.WithHTTPClient(f.srv.Client()), github.WithURLs(&base, &base))
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func newStore(t *testing.T) storage.Store {
	t.Helper()
	store, err := storage.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	slog.SetDefault(slog.New(slog.DiscardHandler))
	return store
}

func repo(t *testing.T, full string, releases int, prereleases bool, patterns ...string) Repo {
	t.Helper()
	r, err := rules.Parse(patterns)
	if err != nil {
		t.Fatal(err)
	}
	owner, name, _ := strings.Cut(full, "/")
	return Repo{Owner: owner, Name: name, Rules: r, Releases: releases, Prereleases: prereleases}
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

func TestSync(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *fakeGitHub)
		repos func(t *testing.T) []Repo
		want  []string
	}{
		{
			name: "releases newest first, limited, without drafts and pre-releases",
			setup: func(f *fakeGitHub) {
				f.release("acme/app", "v1", false, "app.apk")
				f.release("acme/app", "v2", false, "app.apk", "notes.txt")
				f.release("acme/app", "v3-rc", true, "app.apk")
				f.release("acme/app", "v3", false, "app.apk")
				f.releases["acme/app"] = append([]fakeRelease{{Tag: "v4", Draft: true, Assets: []fakeAsset{{ID: 99, Name: "app.apk"}}}}, f.releases["acme/app"]...)
			},
			repos: func(t *testing.T) []Repo { return []Repo{repo(t, "acme/app", 2, false)} },
			want:  []string{"acme/app/v2/app.apk", "acme/app/v3/app.apk"},
		},
		{
			name: "pre-releases on request, include rules on tag/asset",
			setup: func(f *fakeGitHub) {
				f.release("acme/app", "v1", false, "app-release.apk", "app-debug.apk")
				f.release("acme/app", "v2-rc", true, "app-release.apk")
			},
			repos: func(t *testing.T) []Repo { return []Repo{repo(t, "acme/app", 0, true, "!*-debug.apk")} },
			want:  []string{"acme/app/v1/app-release.apk", "acme/app/v2-rc/app-release.apk"},
		},
		{
			name: "several repos",
			setup: func(f *fakeGitHub) {
				f.release("acme/app", "v1", false, "app.apk")
				f.release("other/tool", "2026.1", false, "tool.apk")
			},
			repos: func(t *testing.T) []Repo {
				return []Repo{repo(t, "acme/app", 0, false), repo(t, "other/tool", 0, false)}
			},
			want: []string{"acme/app/v1/app.apk", "other/tool/2026.1/tool.apk"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			tt.setup(f)
			store := newStore(t)
			st, err := New(f.client(), tt.repos(t), store, true).Sync(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, k := range keys(t, store) {
				if strings.HasSuffix(k, ".apk") {
					got = append(got, k)
				}
			}
			if !slices.Equal(got, tt.want) || st.Copied != len(tt.want) || len(st.Builds) != len(tt.want) {
				t.Fatalf("copied %v (stats %+v), want %v", got, st, tt.want)
			}
		})
	}
}

func TestSidecarsAndUpdates(t *testing.T) {
	f := newFakeGitHub(t)
	f.release("acme/app", "v1", false, "app.apk")
	store := newStore(t)
	s := New(f.client(), []Repo{repo(t, "acme/app", 1, false)}, store, true)
	ctx := context.Background()

	st, err := s.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if b := st.Builds[0]; b.App == nil || b.App.Package == "" || b.Link != "https://github.com/acme/app/releases/tag/v1" {
		t.Fatalf("build %+v", b)
	}
	if want := []string{"acme/app/v1/app.apk", "acme/app/v1/app.apk.icon.png", "acme/app/v1/app.apk.meta.json"}; !slices.Equal(keys(t, store), want) {
		t.Fatalf("keys %v", keys(t, store))
	}
	body, _, err := store.Get(ctx, "acme/app/v1/app.apk.meta.json")
	if err != nil {
		t.Fatal(err)
	}
	var m layout.Meta
	_ = json.NewDecoder(body).Decode(&m)
	_ = body.Close()
	if m.Origin != "github" || m.Note != "Release v1" || m.Changelog != "notes for v1\n* fix" || m.Link == "" ||
		m.Tags["repo"] != "acme/app" || m.Tags["release"] != "v1" || m.UploadedAt.IsZero() {
		t.Fatalf("meta %+v", m)
	}

	if st, err := s.Sync(ctx); err != nil || st.Copied != 0 || st.Skipped != 1 || f.downloads != 1 {
		t.Fatalf("second pass %+v, %v, downloads %d", st, err, f.downloads)
	}

	f.mu.Lock()
	f.releases["acme/app"][0].Assets[0].Updated = time.Now().Add(time.Hour)
	f.mu.Unlock()
	if st, err := s.Sync(ctx); err != nil || st.Copied != 1 {
		t.Fatalf("a re-uploaded asset must be copied again: %+v, %v", st, err)
	}

	f.release("acme/app", "v2", false, "app.apk")
	if st, err := s.Sync(ctx); err != nil || st.Copied != 1 || st.Pruned != 1 {
		t.Fatalf("new release %+v, %v", st, err)
	}
	if want := []string{"acme/app/v2/app.apk", "acme/app/v2/app.apk.icon.png", "acme/app/v2/app.apk.meta.json"}; !slices.Equal(keys(t, store), want) {
		t.Fatalf("an old release must be pruned with its sidecars: %v", keys(t, store))
	}
}

func TestFailedListingNeverPrunes(t *testing.T) {
	f := newFakeGitHub(t)
	f.release("acme/app", "v1", false, "app.apk")
	f.release("other/tool", "v1", false, "tool.apk")
	store := newStore(t)
	s := New(f.client(), []Repo{repo(t, "acme/app", 0, false), repo(t, "other/tool", 0, false)}, store, true)
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	f.mu.Lock()
	f.failRepo = "other/tool"
	f.releases["acme/app"] = nil
	f.mu.Unlock()
	st, err := s.Sync(context.Background())
	if err == nil || st.Pruned != 0 || len(keys(t, store)) != 6 {
		t.Fatalf("pass with a failed listing pruned: %+v, %v, %v", st, err, keys(t, store))
	}
}

func TestTruncate(t *testing.T) {
	for _, tt := range []struct {
		in    string
		limit int
		want  string
	}{
		{"short", 10, "short"},
		{"абвгд", 5, "аб…"},
		{"abcdef", 3, "abc…"},
	} {
		if got := truncate(tt.in, tt.limit); got != tt.want {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.limit, got, tt.want)
		}
	}
}
