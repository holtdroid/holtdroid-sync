// Package chromiumbuilds copies the releases of holtdroid/chromium-builds
// into a Holtdroid source (the browsers) and WebView catalog.
package chromiumbuilds

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v93/github"

	"github.com/holtdroid/holtdroid-sync/internal/apk"
	"github.com/holtdroid/holtdroid-sync/internal/layout"
	"github.com/holtdroid/holtdroid-sync/internal/snapshotsync"
	"github.com/holtdroid/holtdroid-sync/internal/storage"
)

const (
	browser = "browser"
	webview = "webview"
)

var (
	products = []struct{ dir, file string }{{browser, "chromium"}, {webview, "webview"}}
	arches   = []string{"x86_64", "arm64"}
	released = regexp.MustCompile(`(?m)^Released to Android (Stable|Beta):[ \t]*(\S+)[ \t\r]*$`)
)

// Requirements raise the Android API level of milestones that break on
// older Android although their manifest allows it.
type Requirements struct {
	Browser []snapshotsync.Requirement
	WebView []snapshotsync.Requirement
}

type Syncer struct {
	client      *github.Client
	hc          *http.Client
	owner, name string
	reqs        Requirements
	store       storage.Store
	prune       bool
	log         *slog.Logger
}

func New(client *github.Client, hc *http.Client, owner, name string, reqs Requirements, store storage.Store, prune bool) *Syncer {
	return &Syncer{client: client, hc: hc, owner: owner, name: name, reqs: reqs, store: store, prune: prune, log: slog.Default().With("component", "syncer")}
}

type file struct {
	key, dir, arch string
	asset          *github.ReleaseAsset
}

type release struct {
	tag      string
	version  []int
	stable   bool
	released time.Time
	link     string
	channel  string
	files    []file
}

func (r release) milestone() int { return r.version[0] }

func (s *Syncer) Sync(ctx context.Context) (layout.Stats, error) {
	var st layout.Stats
	ready, known, err := s.list(ctx)
	if err != nil {
		return st, fmt.Errorf("list releases: %w", err)
	}
	markChannels(ready)

	objs, err := s.store.List(ctx, "")
	if err != nil {
		return st, fmt.Errorf("list target: %w", err)
	}
	have := map[string]storage.Object{}
	for _, o := range objs {
		have[o.Key] = o
	}

	var errs []error
	for _, r := range ready {
		for _, f := range r.files {
			if err := ctx.Err(); err != nil {
				return st, errors.Join(append(errs, err)...)
			}
			t, ok := have[f.key]
			if layout.Outdated(t, ok, int64(f.asset.GetSize()), f.asset.GetUpdatedAt().Time) {
				size, err := s.copy(ctx, r, f)
				if err != nil {
					errs = append(errs, fmt.Errorf("%s: %w", f.key, err))
					continue
				}
				st.Copied++
				s.log.Info("copied", "key", f.key, "size", size)
				continue
			}
			st.Skipped++
			if err := s.refresh(ctx, r, f); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", f.key, err))
			}
		}
	}

	if s.prune {
		for key := range have {
			if known[key] || !layout.IsAPK(key) {
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

// list returns the releases that are complete, newest first, and every key
// a published release can have, so a release that is being re-uploaded
// keeps its copies.
func (s *Syncer) list(ctx context.Context) ([]release, map[string]bool, error) {
	var ready []release
	known := map[string]bool{}
	opts := &github.ListOptions{PerPage: 100}
	for {
		page, resp, err := s.client.Repositories.ListReleases(ctx, s.owner, s.name, opts)
		if err != nil {
			return nil, nil, err
		}
		for _, rel := range page {
			if rel.GetDraft() {
				continue
			}
			for _, f := range filesOf(rel.GetTagName(), nil) {
				known[f.key] = true
			}
			r, why := parse(rel)
			if why != "" {
				s.log.Debug("not ready", "tag", rel.GetTagName(), "why", why)
				continue
			}
			ready = append(ready, r)
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	slices.SortFunc(ready, func(a, b release) int { return slices.Compare(b.version, a.version) })
	return ready, known, nil
}

func filesOf(tag string, assets map[string]*github.ReleaseAsset) []file {
	var out []file
	for _, p := range products {
		for _, arch := range arches {
			name := fmt.Sprintf("%s-%s-%s.apk", p.file, tag, arch)
			out = append(out, file{key: p.dir + "/" + tag + "/" + name, dir: p.dir, arch: arch, asset: assets[name]})
		}
	}
	return out
}

func parse(rel *github.RepositoryRelease) (release, string) {
	tag := rel.GetTagName()
	version, err := parseVersion(tag)
	if err != nil {
		return release{}, err.Error()
	}
	m := released.FindStringSubmatch(rel.GetBody())
	if m == nil {
		return release{}, "no release date in the notes"
	}
	date, err := time.Parse(time.RFC3339, m[2])
	if err != nil {
		return release{}, "unreadable release date"
	}
	assets := map[string]*github.ReleaseAsset{}
	for _, a := range rel.Assets {
		if a.GetState() == "uploaded" {
			assets[a.GetName()] = a
		}
	}
	files := filesOf(tag, assets)
	for _, f := range files {
		if f.asset == nil {
			return release{}, "waiting for " + f.key
		}
	}
	return release{tag: tag, version: version, stable: m[1] == "Stable", released: date.UTC(), link: rel.GetHTMLURL(), files: files}, ""
}

func parseVersion(tag string) ([]int, error) {
	parts := strings.Split(tag, ".")
	if len(parts) != 4 {
		return nil, fmt.Errorf("tag %q is not a Chromium version", tag)
	}
	out := make([]int, 4)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("tag %q is not a Chromium version", tag)
		}
		out[i] = n
	}
	return out, nil
}

// markChannels tags the newest Stable and the newest Beta, the builds that
// are on those channels today.
func markChannels(newestFirst []release) {
	seen := map[bool]bool{}
	for i := range newestFirst {
		r := &newestFirst[i]
		if seen[r.stable] {
			continue
		}
		seen[r.stable] = true
		r.channel = "beta"
		if r.stable {
			r.channel = "stable"
		}
	}
}

func (s *Syncer) copy(ctx context.Context, r release, f file) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.asset.GetBrowserDownloadURL(), nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return 0, fmt.Errorf("download: %s", resp.Status)
	}
	staged, err := layout.Stage(resp.Body, int64(f.asset.GetSize()))
	if err != nil {
		return 0, err
	}
	defer staged.Close()
	meta := layout.Meta{Tags: appTags(staged.App)}
	s.apply(&meta, r, f)
	return staged.Size, staged.Publish(ctx, s.store, f.key, meta)
}

func appTags(app *apk.Metadata) map[string]string {
	tags := map[string]string{}
	if app == nil {
		return tags
	}
	tags["minSdk"] = strconv.Itoa(int(app.MinSDK))
	tags["targetSdk"] = strconv.Itoa(int(app.TargetSDK))
	if len(app.ABIs) > 0 {
		tags["abis"] = strings.Join(app.ABIs, ",")
	}
	return tags
}

func (s *Syncer) apply(m *layout.Meta, r release, f file) {
	m.Title = fmt.Sprintf("Chromium %d", r.milestone())
	m.Note = r.tag + " " + f.arch
	m.UploadedAt = r.released
	m.Link = r.link
	m.Origin = "chromium-builds"
	if m.Tags == nil {
		m.Tags = map[string]string{}
	}
	m.Tags["release"] = r.tag
	m.Tags["version"] = r.tag
	m.Tags["milestone"] = strconv.Itoa(r.milestone())
	setOrDelete(m.Tags, "channel", r.channel)
	reqs := s.reqs.Browser
	if f.dir == webview {
		reqs = s.reqs.WebView
	}
	sdk := 0
	for _, q := range reqs {
		if r.milestone() >= q.From {
			sdk = max(sdk, q.MinSDK)
		}
	}
	setOrDelete(m.Tags, "requiresSdk", "")
	if sdk > 0 {
		m.Tags["requiresSdk"] = strconv.Itoa(sdk)
	}
}

func setOrDelete(tags map[string]string, key, value string) {
	if value == "" {
		delete(tags, key)
		return
	}
	tags[key] = value
}

// refresh rewrites a sidecar that no longer matches the release, as when a
// newer build takes over the channel.
func (s *Syncer) refresh(ctx context.Context, r release, f file) error {
	rc, _, err := s.store.Get(ctx, f.key+layout.MetaSuffix)
	if err != nil {
		return fmt.Errorf("read meta: %w", err)
	}
	raw, err := io.ReadAll(io.LimitReader(rc, 1<<20))
	_ = rc.Close()
	if err != nil {
		return fmt.Errorf("read meta: %w", err)
	}
	var m layout.Meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("read meta: %w", err)
	}
	s.apply(&m, r, f)
	next, err := m.Encode()
	if err != nil {
		return err
	}
	if bytes.Equal(next, raw) {
		return nil
	}
	return layout.PutMeta(ctx, s.store, f.key, next)
}
