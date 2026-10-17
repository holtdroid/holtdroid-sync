package slacksync

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/kriakiku/holtdroid-sync/internal/layout"
	"github.com/kriakiku/holtdroid-sync/internal/secrets"
	"github.com/kriakiku/holtdroid-sync/internal/storage"
)

type Config struct {
	Token     secrets.Ref
	Channels  []string
	Threads   bool
	Lookback  time.Duration
	Retention time.Duration
	APIURL    string
}

func DefaultConfig() Config {
	return Config{
		Token:    "env:SLACK_BOT_TOKEN",
		Threads:  true,
		Lookback: 7 * 24 * time.Hour,
		APIURL:   slack.APIURL,
	}
}

var channelID = regexp.MustCompile(`^[A-Z0-9]+$`)

func (c Config) Validate() error {
	if len(c.Channels) == 0 {
		return errors.New("slack.channels: at least one channel ID is required")
	}
	for _, ch := range c.Channels {
		if !channelID.MatchString(ch) {
			return fmt.Errorf("slack.channels: %q is not a channel ID like C0123ABCD", ch)
		}
	}
	if c.Lookback <= 0 {
		return errors.New("slack.lookback must be positive")
	}
	if c.Retention > 0 && c.Retention <= c.Lookback {
		return errors.New("slack.retention must be longer than slack.lookback, or pruned builds are downloaded again")
	}
	return nil
}

type Syncer struct {
	cfg   Config
	token string
	api   *slack.Client
	http  *http.Client
	store storage.Store
	log   *slog.Logger
	now   func() time.Time

	team    string
	users   map[string]string
	threads map[string]string
}

func New(cfg Config, token string, store storage.Store, client *http.Client) *Syncer {
	if client == nil {
		client = http.DefaultClient
	}
	cfg.APIURL = strings.TrimRight(cfg.APIURL, "/") + "/"
	return &Syncer{
		cfg:     cfg,
		token:   token,
		api:     slack.New(token, slack.OptionAPIURL(cfg.APIURL), slack.OptionHTTPClient(client)),
		http:    client,
		store:   store,
		log:     slog.Default().With("component", "syncer"),
		now:     time.Now,
		users:   map[string]string{},
		threads: map[string]string{},
	}
}

type stats struct {
	added, updated, pruned int
	builds                 []layout.Build
}

func (a *stats) add(b stats) {
	a.added += b.added
	a.updated += b.updated
	a.pruned += b.pruned
	a.builds = append(a.builds, b.builds...)
}

func (s *Syncer) Sync(ctx context.Context) (layout.Stats, error) {
	var total layout.Stats
	if s.team == "" {
		auth, err := retry(ctx, s.log, func() (*slack.AuthTestResponse, error) { return s.api.AuthTestContext(ctx) })
		if err != nil {
			return total, fmt.Errorf("auth.test: %w", err)
		}
		s.team = auth.URL
	}
	var errs []error
	for _, ch := range s.cfg.Channels {
		st, err := s.syncChannel(ctx, ch)
		if err != nil {
			errs = append(errs, fmt.Errorf("channel %s: %w", ch, err))
		}
		level := slog.LevelDebug
		if st.added+st.updated+st.pruned > 0 {
			level = slog.LevelInfo
		}
		s.log.Log(ctx, level, "channel synced", "channel", ch, "added", st.added, "updated", st.updated, "pruned", st.pruned)
		total.Copied += st.added
		total.Pruned += st.pruned
		total.Builds = append(total.Builds, st.builds...)
	}
	return total, errors.Join(errs...)
}

type channelRun struct {
	ch     string
	synced map[string]string
	seen   map[string]bool
}

func (s *Syncer) syncChannel(ctx context.Context, ch string) (stats, error) {
	var st stats
	objs, err := s.store.List(ctx, ch+"/")
	if err != nil {
		return st, fmt.Errorf("list target: %w", err)
	}
	run := channelRun{ch: ch, synced: map[string]string{}, seen: map[string]bool{}}
	for _, o := range objs {
		if layout.IsAPK(o.Key) {
			run.synced[fileIDOf(o.Key)] = o.Key
		}
	}

	oldest := formatTS(s.now().Add(-s.cfg.Lookback))
	msgs, err := s.pages(ctx, "conversations.history", url.Values{"channel": {ch}, "oldest": {oldest}})
	if err != nil {
		return st, err
	}
	var errs []error
	ms, err := s.syncMessages(ctx, &run, msgs)
	st.add(ms)
	if err != nil {
		errs = append(errs, err)
	}

	if s.cfg.Threads {
		ts, err := s.syncThreads(ctx, &run, msgs, oldest)
		st.add(ts)
		if err != nil {
			errs = append(errs, err)
		}
	}

	if s.cfg.Retention > 0 {
		n, err := s.prune(ctx, objs)
		st.pruned = n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return st, errors.Join(errs...)
}

func (s *Syncer) syncThreads(ctx context.Context, run *channelRun, parents []message, oldest string) (stats, error) {
	var st stats
	var errs []error
	for key := range s.threads {
		if strings.HasPrefix(key, run.ch+"/") && !slices.ContainsFunc(parents, func(m message) bool { return key == run.ch+"/"+m.Timestamp }) {
			delete(s.threads, key)
		}
	}
	for _, parent := range parents {
		key := run.ch + "/" + parent.Timestamp
		if parent.ReplyCount == 0 || parent.LatestReply < oldest || s.threads[key] == parent.LatestReply {
			continue
		}
		replies, err := s.pages(ctx, "conversations.replies", url.Values{"channel": {run.ch}, "ts": {parent.Timestamp}, "oldest": {oldest}})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		replies = slices.DeleteFunc(replies, func(m message) bool { return m.Timestamp == parent.Timestamp })
		rs, err := s.syncMessages(ctx, run, replies)
		st.add(rs)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.threads[key] = parent.LatestReply
	}
	return st, errors.Join(errs...)
}

type post struct {
	msg      message
	text     string
	authorID string
	author   string
	ts       string
	files    []slack.File
}

func posts(m message) []post {
	var out []post
	if len(m.Files) > 0 {
		bot := ""
		if m.BotProfile != nil {
			bot = m.BotProfile.Name
		}
		out = append(out, post{msg: m, text: m.Text, authorID: m.User, author: cmp.Or(bot, m.Username), ts: m.Timestamp, files: m.Files})
	}
	for _, a := range m.Attachments {
		if len(a.Files) == 0 {
			continue
		}
		text := strings.TrimSpace(strings.Join(slices.DeleteFunc([]string{m.Text, a.Text}, func(s string) bool { return strings.TrimSpace(s) == "" }), "\n\n"))
		out = append(out, post{msg: m, text: text, authorID: a.AuthorID, author: a.AuthorName, ts: cmp.Or(a.Ts.String(), m.Timestamp), files: a.Files})
	}
	return out
}

func (s *Syncer) syncMessages(ctx context.Context, run *channelRun, msgs []message) (stats, error) {
	var st stats
	var errs []error
	for _, m := range msgs {
		for _, p := range posts(m) {
			for _, f := range p.files {
				if run.seen[f.ID] {
					continue
				}
				run.seen[f.ID] = true
				changed, b, err := s.syncFile(ctx, run, p, f)
				if err != nil {
					errs = append(errs, fmt.Errorf("file %s: %w", f.ID, err))
					continue
				}
				switch changed {
				case added:
					st.added++
					st.builds = append(st.builds, b)
				case updated:
					st.updated++
				}
			}
		}
	}
	return st, errors.Join(errs...)
}

type change int

const (
	unchanged change = iota
	added
	updated
)

func (s *Syncer) syncFile(ctx context.Context, run *channelRun, p post, f slack.File) (change, layout.Build, error) {
	if f.Mode == "tombstone" || f.Mode == "hidden_by_limit" || f.IsExternal {
		return unchanged, layout.Build{}, nil
	}
	if key, ok := run.synced[f.ID]; ok {
		if p.msg.Edited == nil {
			return unchanged, layout.Build{}, nil
		}
		changed, err := s.refreshMeta(ctx, run.ch, p, f, key)
		return changed, layout.Build{}, err
	}

	if f.Name == "" || f.URLPrivateDownload == "" {
		full, _, _, err := s.api.GetFileInfoContext(ctx, f.ID, 0, 0)
		if err != nil {
			return unchanged, layout.Build{}, fmt.Errorf("files.info: %w", err)
		}
		f = *full
	}
	if !layout.IsAPK(f.Name) {
		return unchanged, layout.Build{}, nil
	}

	key := run.ch + "/" + f.ID + "/" + safeName(f.Name)
	b, err := s.copyFile(ctx, run.ch, p, f, key)
	if err != nil {
		return unchanged, layout.Build{}, err
	}
	run.synced[f.ID] = key
	s.log.Info("synced", "channel", run.ch, "file", f.ID, "key", key, "size", b.Size)
	return added, b, nil
}

func (s *Syncer) copyFile(ctx context.Context, ch string, p post, f slack.File, key string) (layout.Build, error) {
	body, err := s.open(ctx, f)
	if err != nil {
		return layout.Build{}, err
	}
	staged, err := layout.Stage(body, -1)
	_ = body.Close()
	if err != nil {
		return layout.Build{}, err
	}
	defer staged.Close()
	meta := s.meta(ctx, ch, p, f)
	if err := staged.Publish(ctx, s.store, key, meta); err != nil {
		return layout.Build{}, err
	}
	return layout.Build{Key: key, Size: staged.Size, Modified: meta.UploadedAt, Link: meta.Link, App: staged.App}, nil
}

func (s *Syncer) refreshMeta(ctx context.Context, ch string, p post, f slack.File, key string) (change, error) {
	if f.Name == "" {
		f.Name = path.Base(key)
	}
	want, err := s.meta(ctx, ch, p, f).Encode()
	if err != nil {
		return unchanged, err
	}
	rc, _, err := s.store.Get(ctx, key+layout.MetaSuffix)
	if err == nil {
		have, readErr := io.ReadAll(rc)
		_ = rc.Close()
		if readErr == nil && bytes.Equal(have, want) {
			return unchanged, nil
		}
	} else if !errors.Is(err, storage.ErrNotExist) {
		return unchanged, err
	}
	if err := layout.PutMeta(ctx, s.store, key, want); err != nil {
		return unchanged, err
	}
	return updated, nil
}

func (s *Syncer) meta(ctx context.Context, ch string, p post, f slack.File) layout.Meta {
	link := messageLink(s.team, ch, p.msg.Timestamp, p.msg.ThreadTimestamp)
	author := p.author
	if p.authorID != "" {
		author = s.userName(ctx, p.authorID)
	}
	meta := layout.Meta{
		Note:       link,
		Changelog:  plainText(p.text, func(id string) string { return s.userName(ctx, id) }),
		UploadedBy: author,
		UploadedAt: parseTS(p.ts),
		Link:       link,
		Origin:     "slack",
	}
	if title := strings.TrimSpace(f.Title); title != "" && title != f.Name && title != strings.TrimSuffix(f.Name, path.Ext(f.Name)) {
		meta.Title = title
	}
	return meta
}

func (s *Syncer) userName(ctx context.Context, id string) string {
	if name, ok := s.users[id]; ok {
		return name
	}
	name := id
	u, err := retry(ctx, s.log, func() (*slack.User, error) { return s.api.GetUserInfoContext(ctx, id) })
	if err != nil {
		s.log.Warn("users.info failed, showing the user ID; the token needs users:read", "user", id, "err", err)
	} else {
		name = cmp.Or(u.Profile.DisplayName, u.Profile.RealName, u.RealName, u.Name, id)
	}
	s.users[id] = name
	return name
}

func (s *Syncer) open(ctx context.Context, f slack.File) (io.ReadCloser, error) {
	src := cmp.Or(f.URLPrivateDownload, f.URLPrivate)
	if err := s.checkFileURL(src); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	fail := func(err error) (io.ReadCloser, error) {
		_ = resp.Body.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("download: %s", resp.Status))
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		return fail(errors.New("download: got an HTML page instead of the file; the token needs files:read"))
	}
	body := bufio.NewReader(resp.Body)
	if magic, err := body.Peek(4); err != nil || string(magic) != "PK\x03\x04" {
		return fail(errors.New("download: not an APK (no zip header)"))
	}
	return struct {
		io.Reader
		io.Closer
	}{body, resp.Body}, nil
}

func (s *Syncer) checkFileURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("download: invalid file URL %q", raw)
	}
	if api, err := url.Parse(s.cfg.APIURL); err == nil && u.Host == api.Host {
		return nil
	}
	host := u.Hostname()
	for _, domain := range []string{"slack.com", "slack-gov.com"} {
		if u.Scheme == "https" && (host == domain || strings.HasSuffix(host, "."+domain)) {
			return nil
		}
	}
	return fmt.Errorf("download: refusing to send the token to %s", u.Host)
}

func (s *Syncer) prune(ctx context.Context, objs []storage.Object) (int, error) {
	cutoff := s.now().Add(-s.cfg.Retention)
	var old []storage.Object
	for _, o := range objs {
		if o.Modified.Before(cutoff) {
			old = append(old, o)
		}
	}
	slices.SortStableFunc(old, func(a, b storage.Object) int {
		switch {
		case layout.IsAPK(a.Key) == layout.IsAPK(b.Key):
			return 0
		case layout.IsAPK(a.Key):
			return -1
		default:
			return 1
		}
	})
	var errs []error
	n := 0
	for _, o := range old {
		if err := s.store.Delete(ctx, o.Key); err != nil {
			errs = append(errs, fmt.Errorf("prune %s: %w", o.Key, err))
			continue
		}
		if layout.IsAPK(o.Key) {
			n++
			s.log.Info("pruned", "key", o.Key)
		}
	}
	return n, errors.Join(errs...)
}

func retry[T any](ctx context.Context, log *slog.Logger, call func() (T, error)) (T, error) {
	for {
		v, err := call()
		var limited *slack.RateLimitedError
		if !errors.As(err, &limited) {
			return v, err
		}
		log.Warn("slack rate limit", "retryAfter", limited.RetryAfter)
		select {
		case <-ctx.Done():
			return v, ctx.Err()
		case <-time.After(limited.RetryAfter):
		}
	}
}

func fileIDOf(key string) string {
	parts := strings.Split(key, "/")
	if len(parts) < 3 {
		return ""
	}
	return parts[len(parts)-2]
}

func safeName(name string) string {
	name = strings.TrimLeft(strings.NewReplacer("/", "_", "\\", "_", "\x00", "").Replace(name), ".")
	if name == "" || strings.EqualFold(name, "apk") {
		return "app.apk"
	}
	return name
}
