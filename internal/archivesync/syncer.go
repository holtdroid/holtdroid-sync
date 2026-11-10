// Package archivesync copies APKs from public HTTP directory listings, such
// as the release archives of Opera and Firefox, into a Holtdroid source.
package archivesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/holtdroid/holtdroid-sync/internal/layout"
	"github.com/holtdroid/holtdroid-sync/internal/rules"
	"github.com/holtdroid/holtdroid-sync/internal/storage"
)

const maxListing = 8 << 20

var (
	hrefs  = regexp.MustCompile(`(?i)href="([^"#?]+)"`)
	digits = regexp.MustCompile(`\d+`)
)

// Archive is one directory tree. With VersionDir, the newest Versions
// matching directories are read and each gives its newest FileVersion; without
// it, the newest Versions file versions directly under URL are taken.
type Archive struct {
	Name, Title string
	URL         *url.URL
	VersionDir  *regexp.Regexp
	FileVersion *regexp.Regexp
	Depth       int
	Rules       rules.Rules
	Versions    int
}

type Syncer struct {
	hc        *http.Client
	userAgent string
	archives  []Archive
	store     storage.Store
	prune     bool
	log       *slog.Logger
}

func New(hc *http.Client, userAgent string, archives []Archive, store storage.Store, prune bool) *Syncer {
	return &Syncer{hc: hc, userAgent: userAgent, archives: archives, store: store, prune: prune, log: slog.Default().With("component", "syncer")}
}

type file struct {
	archive  Archive
	url      *url.URL
	rel      string
	size     int64
	modified time.Time
}

func (s *Syncer) Sync(ctx context.Context) (layout.Stats, error) {
	var st layout.Stats
	var errs []error
	want := map[string]file{}
	complete := true
	for _, a := range s.archives {
		found, err := s.plan(ctx, a)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a.Name, err))
			complete = false
			continue
		}
		for _, f := range found {
			want[a.Name+"/"+f.rel] = f
		}
	}

	objs, err := s.store.List(ctx, "")
	if err != nil {
		return st, errors.Join(append(errs, fmt.Errorf("list target: %w", err))...)
	}
	have := map[string]storage.Object{}
	for _, o := range objs {
		have[o.Key] = o
	}

	for key, f := range want {
		if t, ok := have[key]; !layout.Outdated(t, ok, f.size, f.modified) {
			st.Skipped++
			if err := s.refresh(ctx, key, f); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", key, err))
			}
			continue
		}
		if err := s.copy(ctx, key, f); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			continue
		}
		st.Copied++
		s.log.Info("copied", "key", key, "size", f.size)
	}

	if s.prune && complete {
		for key := range have {
			if _, keep := want[key]; keep || !layout.IsAPK(key) {
				continue
			}
			if err := layout.Remove(ctx, s.store, key); err != nil {
				errs = append(errs, fmt.Errorf("prune: %w", err))
				continue
			}
			st.Pruned++
			s.log.Info("pruned", "key", key)
		}
	}
	return st, errors.Join(errs...)
}

func (s *Syncer) plan(ctx context.Context, a Archive) ([]file, error) {
	var out []file
	if a.VersionDir == nil {
		found, err := s.walk(ctx, a, a.URL, "", 0)
		if err != nil {
			return nil, err
		}
		return s.stat(ctx, a, newest(found, a.FileVersion, a.Versions))
	}
	dirs, _, err := s.list(ctx, a.URL)
	if err != nil {
		return nil, err
	}
	dirs = slices.DeleteFunc(dirs, func(d string) bool { return !a.VersionDir.MatchString(d) })
	slices.SortFunc(dirs, func(x, y string) int { return compareVersions(y, x) })
	for _, d := range dirs[:min(len(dirs), a.Versions)] {
		base := a.URL.JoinPath(d)
		base.Path += "/"
		found, err := s.walk(ctx, a, base, d+"/", 0)
		if err != nil {
			return nil, err
		}
		files, err := s.stat(ctx, a, newest(found, a.FileVersion, 1))
		if err != nil {
			return nil, err
		}
		out = append(out, files...)
	}
	return out, nil
}

func (s *Syncer) walk(ctx context.Context, a Archive, dir *url.URL, rel string, depth int) ([]file, error) {
	dirs, files, err := s.list(ctx, dir)
	if err != nil {
		return nil, err
	}
	var out []file
	for _, name := range files {
		r := rel + name
		if layout.IsAPK(name) && !layout.Hidden(r) && a.Rules.Match(r) {
			u := dir.JoinPath(name)
			out = append(out, file{archive: a, url: u, rel: r})
		}
	}
	if depth < a.Depth {
		for _, d := range dirs {
			next := dir.JoinPath(d)
			next.Path += "/"
			found, err := s.walk(ctx, a, next, rel+d+"/", depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, found...)
		}
	}
	return out, nil
}

// list reads the entries directly under dir from its HTML listing.
func (s *Syncer) list(ctx context.Context, dir *url.URL) (dirs, files []string, err error) {
	resp, err := s.do(ctx, http.MethodGet, dir)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxListing))
	if err != nil {
		return nil, nil, err
	}
	seen := map[string]bool{}
	for _, m := range hrefs.FindAllStringSubmatch(string(raw), -1) {
		ref, err := url.Parse(m[1])
		if err != nil {
			continue
		}
		target := dir.ResolveReference(ref)
		if target.Host != dir.Host || !strings.HasPrefix(target.Path, dir.Path) || target.Path == dir.Path {
			continue
		}
		rest := strings.TrimPrefix(target.Path, dir.Path)
		name := strings.TrimSuffix(rest, "/")
		if name == "" || strings.Contains(name, "/") || seen[name] {
			continue
		}
		seen[name] = true
		if strings.HasSuffix(rest, "/") {
			dirs = append(dirs, name)
		} else {
			files = append(files, name)
		}
	}
	return dirs, files, nil
}

func (s *Syncer) stat(ctx context.Context, a Archive, found []file) ([]file, error) {
	for i := range found {
		resp, err := s.do(ctx, http.MethodHead, found[i].url)
		if err != nil {
			return nil, err
		}
		_ = resp.Body.Close()
		if resp.ContentLength < 0 {
			return nil, fmt.Errorf("%s: no Content-Length", found[i].rel)
		}
		found[i].size = resp.ContentLength
		if found[i].modified, err = http.ParseTime(resp.Header.Get("Last-Modified")); err != nil {
			return nil, fmt.Errorf("%s: no Last-Modified", found[i].rel)
		}
	}
	return found, nil
}

func (s *Syncer) do(ctx context.Context, method string, u *url.URL) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.userAgent)
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("%s %s: %s", method, u, resp.Status)
	}
	return resp, nil
}

func (s *Syncer) copy(ctx context.Context, key string, f file) error {
	resp, err := s.do(ctx, http.MethodGet, f.url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	staged, err := layout.Stage(resp.Body, f.size)
	if err != nil {
		return err
	}
	defer staged.Close()
	meta := layout.Meta{
		Title:      f.archive.Title,
		Note:       path.Base(f.rel),
		UploadedAt: f.modified.UTC(),
		Link:       parent(f.url),
		Origin:     "archive",
		Tags:       map[string]string{"archive": f.archive.Name},
	}
	if staged.App != nil && staged.App.VersionName != "" {
		meta.Tags["version"] = staged.App.VersionName
	}
	return staged.Publish(ctx, s.store, key, meta)
}

// refresh rewrites the title of a copy when the config's title changed.
func (s *Syncer) refresh(ctx context.Context, key string, f file) error {
	rc, _, err := s.store.Get(ctx, key+layout.MetaSuffix)
	if err != nil {
		return fmt.Errorf("read meta: %w", err)
	}
	var m layout.Meta
	err = json.NewDecoder(io.LimitReader(rc, 1<<20)).Decode(&m)
	_ = rc.Close()
	if err != nil {
		return fmt.Errorf("read meta: %w", err)
	}
	if m.Title == f.archive.Title {
		return nil
	}
	m.Title = f.archive.Title
	raw, err := m.Encode()
	if err != nil {
		return err
	}
	return layout.PutMeta(ctx, s.store, key, raw)
}

func parent(u *url.URL) string {
	p := *u
	p.Path = path.Dir(u.Path) + "/"
	return p.String()
}

// newest keeps the files of the n newest versions.
func newest(found []file, version *regexp.Regexp, n int) []file {
	of := func(f file) string {
		base := path.Base(f.rel)
		if version == nil {
			return base
		}
		if m := version.FindStringSubmatch(base); len(m) > 1 {
			return m[1]
		}
		return ""
	}
	var versions []string
	for _, f := range found {
		if v := of(f); v != "" && !slices.Contains(versions, v) {
			versions = append(versions, v)
		}
	}
	slices.SortFunc(versions, func(x, y string) int { return compareVersions(y, x) })
	versions = versions[:min(len(versions), n)]
	return slices.DeleteFunc(slices.Clone(found), func(f file) bool { return !slices.Contains(versions, of(f)) })
}

func compareVersions(a, b string) int {
	return slices.Compare(numbers(a), numbers(b))
}

func numbers(s string) []int {
	var out []int
	for _, d := range digits.FindAllString(s, -1) {
		n, _ := strconv.Atoi(d)
		out = append(out, n)
	}
	return out
}
