package snapshotsync

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/holtdroid/holtdroid-sync/internal/layout"
	"github.com/holtdroid/holtdroid-sync/internal/storage"
)

const (
	DashURL     = "https://chromiumdash.appspot.com"
	SnapshotURL = "https://storage.googleapis.com"
	Bucket      = "chromium-browser-snapshots"
	Platform    = "AndroidDesktop_x64"
	searchStep  = 5000
	searchLimit = 50000
)

var channels = []string{"Stable", "Beta", "Dev", "Canary"}

// App is one of the APKs in a snapshot's chrome-android-desktop.zip.
type App struct {
	Entry   string
	Package string
	Name    string
}

var (
	WebView = App{Entry: "/apks/SystemWebView.apk", Package: "com.android.webview", Name: "Chromium WebView"}
	Browser = App{Entry: "/apks/ChromePublic.apk", Package: "org.chromium.chrome", Name: "Chromium"}
)

// Selection picks milestones by their place among the available ones,
// newest first: the newest Latest, every milestone divisible by Every, and
// past the newest Older.After only those divisible by Older.Every.
type Selection struct {
	Latest     int
	Every      int
	Older      Thinning
	Milestones []int
}

type Thinning struct {
	After int
	Every int
}

func (sel Selection) picks(i, m int) bool {
	every := sel.Every
	if sel.Older.After > 0 && i >= sel.Older.After {
		every = sel.Older.Every
	}
	return i < sel.Latest || (every > 0 && m%every == 0) || slices.Contains(sel.Milestones, m)
}

// Requirement raises the Android API level builds from a milestone on need,
// for breakages their manifest doesn't declare.
type Requirement struct {
	From   int
	MinSDK int
}

type Syncer struct {
	hc        *http.Client
	dash      string
	snapshots string
	platform  string
	app       App
	sel       Selection
	reqs      []Requirement
	store     storage.Store
	prune     bool
	log       *slog.Logger
}

func New(hc *http.Client, app App, sel Selection, reqs []Requirement, store storage.Store, prune bool) *Syncer {
	return &Syncer{
		hc: hc, dash: DashURL, snapshots: SnapshotURL, platform: Platform, app: app,
		sel: sel, reqs: reqs, store: store, prune: prune, log: slog.Default().With("component", "syncer"),
	}
}

type release struct {
	Milestone int    `json:"milestone"`
	Version   string `json:"version"`
	Position  int    `json:"chromium_main_branch_position"`
	Time      int64  `json:"time"`
	Hashes    struct {
		Chromium string `json:"chromium"`
	} `json:"hashes"`
}

type milestone struct {
	Milestone int `json:"milestone"`
	Position  int `json:"chromium_main_branch_position"`
}

type build struct {
	milestone int
	release   release
	revision  int
	channels  []string
}

func Key(milestone int) string { return fmt.Sprintf("chromium-%d.apk", milestone) }

func (s *Syncer) Sync(ctx context.Context) (layout.Stats, error) {
	var st layout.Stats
	want, err := s.plan(ctx)
	if err != nil {
		return st, err
	}
	objs, err := s.store.List(ctx, "")
	if err != nil {
		return st, fmt.Errorf("list target: %w", err)
	}
	have := map[string]storage.Object{}
	for _, o := range objs {
		have[o.Key] = o
	}

	var errs []error
	for _, b := range want {
		key := Key(b.milestone)
		meta, ok := s.current(ctx, key, have)
		if ok && meta.Tags["revision"] == strconv.Itoa(b.revision) {
			before := maps.Clone(meta.Tags)
			setChannels(&meta, b.channels)
			s.setRequires(&meta, b.milestone)
			if maps.Equal(before, meta.Tags) {
				st.Skipped++
				continue
			}
			raw, err := meta.Encode()
			if err == nil {
				err = layout.PutMeta(ctx, s.store, key, raw)
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", key, err))
			}
			st.Skipped++
			continue
		}
		size, err := s.copy(ctx, key, b)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			continue
		}
		st.Copied++
		s.log.Info("copied", "key", key, "revision", b.revision, "size", size)
	}

	if s.prune && len(errs) == 0 {
		keep := map[string]bool{}
		for _, b := range want {
			keep[Key(b.milestone)] = true
		}
		for key := range have {
			if keep[key] || !layout.IsAPK(key) {
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

// plan picks the milestones to keep, newest first, each with the snapshot
// closest below its last Canary release.
func (s *Syncer) plan(ctx context.Context) ([]build, error) {
	onChannel := map[int][]string{}
	canary := 0
	for _, ch := range channels {
		rs, err := s.releases(ctx, ch, 0)
		if err != nil {
			return nil, err
		}
		if len(rs) == 0 {
			continue
		}
		onChannel[rs[0].Milestone] = append(onChannel[rs[0].Milestone], strings.ToLower(ch))
		canary = max(canary, rs[0].Milestone)
	}
	var ms []milestone
	if err := s.getJSON(ctx, s.dash+"/fetch_milestones?only_branched=true", &ms); err != nil {
		return nil, fmt.Errorf("milestones: %w", err)
	}
	oldest, err := s.oldestSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	available := []int{canary}
	for _, m := range ms {
		if m.Milestone < canary && m.Position >= oldest {
			available = append(available, m.Milestone)
		}
	}
	slices.Sort(available)
	slices.Reverse(available)
	available = slices.Compact(available)

	var picked []int
	for i, m := range available {
		if s.sel.picks(i, m) || onChannel[m] != nil {
			picked = append(picked, m)
		}
	}

	out := make([]build, 0, len(picked))
	for _, m := range picked {
		rs, err := s.releases(ctx, "Canary", m)
		if err != nil {
			return nil, err
		}
		if len(rs) == 0 || rs[0].Position == 0 {
			s.log.Warn("no Canary release", "milestone", m)
			continue
		}
		rev, err := s.snapshotBelow(ctx, rs[0].Position, oldest)
		if err != nil {
			return nil, fmt.Errorf("milestone %d: %w", m, err)
		}
		if rev == 0 {
			continue
		}
		out = append(out, build{milestone: m, release: rs[0], revision: rev, channels: orderChannels(onChannel[m])})
	}
	return out, nil
}

func (s *Syncer) releases(ctx context.Context, channel string, milestone int) ([]release, error) {
	q := url.Values{"channel": {channel}, "platform": {"Android"}, "num": {"1"}}
	if milestone > 0 {
		q.Set("milestone", strconv.Itoa(milestone))
	}
	var rs []release
	if err := s.getJSON(ctx, s.dash+"/fetch_releases?"+q.Encode(), &rs); err != nil {
		return nil, fmt.Errorf("%s releases: %w", channel, err)
	}
	return rs, nil
}

type listing struct {
	Prefixes []string `json:"prefixes"`
}

func (s *Syncer) list(ctx context.Context, q url.Values) ([]int, error) {
	q.Set("prefix", s.platform+"/")
	q.Set("delimiter", "/")
	q.Set("fields", "prefixes")
	var l listing
	if err := s.getJSON(ctx, s.snapshots+"/storage/v1/b/"+Bucket+"/o?"+q.Encode(), &l); err != nil {
		return nil, fmt.Errorf("list snapshots: %w", err)
	}
	revs := make([]int, 0, len(l.Prefixes))
	for _, p := range l.Prefixes {
		if n, err := strconv.Atoi(strings.Trim(strings.TrimPrefix(p, s.platform+"/"), "/")); err == nil {
			revs = append(revs, n)
		}
	}
	slices.Sort(revs)
	return revs, nil
}

func (s *Syncer) oldestSnapshot(ctx context.Context) (int, error) {
	revs, err := s.list(ctx, url.Values{"maxResults": {"1"}})
	if err != nil {
		return 0, err
	}
	if len(revs) == 0 {
		return 0, errors.New("no snapshots")
	}
	return revs[0], nil
}

// snapshotBelow finds the newest snapshot at or before position. Snapshot
// folders are 7-digit revisions, so name order is numeric order.
func (s *Syncer) snapshotBelow(ctx context.Context, position, oldest int) (int, error) {
	for from := position - searchStep; from+searchStep > oldest && position-from <= searchLimit; from -= searchStep {
		revs, err := s.list(ctx, url.Values{
			"startOffset": {fmt.Sprintf("%s/%d", s.platform, max(from, oldest))},
			"endOffset":   {fmt.Sprintf("%s/%d", s.platform, position+1)},
		})
		if err != nil {
			return 0, err
		}
		if len(revs) > 0 {
			return revs[len(revs)-1], nil
		}
	}
	return 0, nil
}

func (s *Syncer) zipURL(rev int) string {
	return fmt.Sprintf("%s/%s/%s/%d/chrome-android-desktop.zip", s.snapshots, Bucket, s.platform, rev)
}

func (s *Syncer) copy(ctx context.Context, key string, b build) (int64, error) {
	body, size, err := openEntry(ctx, s.hc, s.zipURL(b.revision), s.app.Entry)
	if err != nil {
		return 0, err
	}
	staged, err := layout.Stage(body, size)
	_ = body.Close()
	if err != nil {
		return 0, err
	}
	defer staged.Close()
	if staged.App == nil || staged.App.Package != s.app.Package {
		return 0, fmt.Errorf("snapshot %d: not a %s APK", b.revision, s.app.Package)
	}
	meta := layout.Meta{
		Title:      fmt.Sprintf("Chromium %d", b.milestone),
		Note:       fmt.Sprintf("%s %s, snapshot r%d", s.app.Name, staged.App.VersionName, b.revision),
		UploadedAt: time.UnixMilli(b.release.Time).UTC(),
		Link:       fmt.Sprintf("https://commondatastorage.googleapis.com/%s/index.html?prefix=%s/%d/", Bucket, s.platform, b.revision),
		Origin:     "chromium-snapshots",
		Tags: map[string]string{
			"milestone": strconv.Itoa(b.milestone),
			"revision":  strconv.Itoa(b.revision),
			"version":   staged.App.VersionName,
			"minSdk":    strconv.Itoa(int(staged.App.MinSDK)),
			"targetSdk": strconv.Itoa(int(staged.App.TargetSDK)),
			"frontend":  b.release.Hashes.Chromium,
		},
	}
	setChannels(&meta, b.channels)
	s.setRequires(&meta, b.milestone)
	return staged.Size, staged.Publish(ctx, s.store, key, meta)
}

func (s *Syncer) current(ctx context.Context, key string, have map[string]storage.Object) (layout.Meta, bool) {
	if _, ok := have[key]; !ok {
		return layout.Meta{}, false
	}
	if _, ok := have[key+layout.MetaSuffix]; !ok {
		return layout.Meta{}, false
	}
	rc, _, err := s.store.Get(ctx, key+layout.MetaSuffix)
	if err != nil {
		return layout.Meta{}, false
	}
	defer rc.Close()
	var m layout.Meta
	if err := json.NewDecoder(io.LimitReader(rc, 64<<10)).Decode(&m); err != nil {
		return layout.Meta{}, false
	}
	return m, true
}

func setChannels(m *layout.Meta, chs []string) {
	if m.Tags == nil {
		m.Tags = map[string]string{}
	}
	if len(chs) == 0 {
		delete(m.Tags, "channel")
		return
	}
	m.Tags["channel"] = strings.Join(chs, ",")
}

func (s *Syncer) setRequires(m *layout.Meta, milestone int) {
	sdk := 0
	for _, r := range s.reqs {
		if milestone >= r.From {
			sdk = max(sdk, r.MinSDK)
		}
	}
	if sdk == 0 {
		delete(m.Tags, "requiresSdk")
		return
	}
	m.Tags["requiresSdk"] = strconv.Itoa(sdk)
}

func orderChannels(chs []string) []string {
	slices.SortFunc(chs, func(a, b string) int {
		return cmp.Compare(slices.IndexFunc(channels, func(c string) bool { return strings.EqualFold(c, a) }),
			slices.IndexFunc(channels, func(c string) bool { return strings.EqualFold(c, b) }))
	})
	return chs
}

func (s *Syncer) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", req.URL.Path, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(v)
}
