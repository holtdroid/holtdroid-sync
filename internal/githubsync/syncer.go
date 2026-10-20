package githubsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/go-github/v93/github"

	"github.com/holtdroid/holtdroid-sync/internal/layout"
	"github.com/holtdroid/holtdroid-sync/internal/rules"
	"github.com/holtdroid/holtdroid-sync/internal/storage"
)

const maxChangelog = 64 << 10

type Repo struct {
	Owner, Name string
	Rules       rules.Rules
	Prereleases bool
	Releases    int
}

func (r Repo) String() string { return r.Owner + "/" + r.Name }

type Syncer struct {
	client *github.Client
	repos  []Repo
	store  storage.Store
	prune  bool
	log    *slog.Logger
}

func New(client *github.Client, repos []Repo, store storage.Store, prune bool) *Syncer {
	return &Syncer{client: client, repos: repos, store: store, prune: prune, log: slog.Default().With("component", "syncer")}
}

type asset struct {
	repo    Repo
	release *github.RepositoryRelease
	asset   *github.ReleaseAsset
}

func (a asset) modified() time.Time { return a.asset.GetUpdatedAt().Time }

func (s *Syncer) Sync(ctx context.Context) (layout.Stats, error) {
	var st layout.Stats
	want := map[string]asset{}
	listed := true
	var errs []error
	for _, repo := range s.repos {
		found, err := s.list(ctx, repo)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", repo, err))
			listed = false
			continue
		}
		maps.Copy(want, found)
	}

	objs, err := s.store.List(ctx, "")
	if err != nil {
		return st, errors.Join(append(errs, fmt.Errorf("list target: %w", err))...)
	}
	have := map[string]storage.Object{}
	for _, o := range objs {
		have[o.Key] = o
	}

	for key, a := range want {
		if t, ok := have[key]; !layout.Outdated(t, ok, int64(a.asset.GetSize()), a.modified()) {
			st.Skipped++
			continue
		}
		b, err := s.copy(ctx, key, a)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			continue
		}
		st.Copied++
		st.Builds = append(st.Builds, b)
		s.log.Info("copied", "key", key, "size", b.Size)
	}

	if s.prune && listed {
		for key := range have {
			if _, keep := want[key]; keep || !layout.IsAPK(key) {
				continue
			}
			if err := layout.Remove(ctx, s.store, key); err != nil {
				errs = append(errs, fmt.Errorf("prune: %w", err))
			}
			st.Pruned++
			s.log.Info("pruned", "key", key)
		}
	}
	return st, errors.Join(errs...)
}

func (s *Syncer) list(ctx context.Context, repo Repo) (map[string]asset, error) {
	out := map[string]asset{}
	taken := 0
	opts := &github.ListOptions{PerPage: 100}
	for {
		releases, resp, err := s.client.Repositories.ListReleases(ctx, repo.Owner, repo.Name, opts)
		if err != nil {
			return nil, fmt.Errorf("list releases: %w", err)
		}
		for _, release := range releases {
			if release.GetDraft() || (release.GetPrerelease() && !repo.Prereleases) {
				continue
			}
			if repo.Releases > 0 && taken == repo.Releases {
				return out, nil
			}
			taken++
			for _, a := range release.Assets {
				rel := release.GetTagName() + "/" + a.GetName()
				if !layout.IsAPK(rel) || layout.Hidden(rel) || !repo.Rules.Match(rel) {
					continue
				}
				out[repo.String()+"/"+rel] = asset{repo: repo, release: release, asset: a}
			}
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

func (s *Syncer) copy(ctx context.Context, key string, a asset) (layout.Build, error) {
	body, _, err := s.client.Repositories.DownloadReleaseAsset(ctx, a.repo.Owner, a.repo.Name, a.asset.GetID(), http.DefaultClient)
	if err != nil {
		return layout.Build{}, fmt.Errorf("download: %w", err)
	}
	staged, err := layout.Stage(body, int64(a.asset.GetSize()))
	_ = body.Close()
	if err != nil {
		return layout.Build{}, err
	}
	defer staged.Close()
	if err := staged.Publish(ctx, s.store, key, metaFor(a)); err != nil {
		return layout.Build{}, err
	}
	return layout.Build{Key: key, Size: staged.Size, Modified: a.modified(), Link: a.release.GetHTMLURL(), App: staged.App}, nil
}

func metaFor(a asset) layout.Meta {
	r := a.release
	note := strings.TrimSpace(r.GetName())
	if note == "" {
		note = r.GetTagName()
	}
	tags := map[string]string{"repo": a.repo.String(), "release": r.GetTagName()}
	if r.GetPrerelease() {
		note += " (pre-release)"
		tags["prerelease"] = "true"
	}
	if c := r.GetTargetCommitish(); c != "" {
		tags["target"] = c
	}
	return layout.Meta{
		Note:       note,
		Changelog:  truncate(strings.TrimSpace(strings.ReplaceAll(r.GetBody(), "\r\n", "\n")), maxChangelog),
		UploadedBy: a.asset.GetUploader().GetLogin(),
		UploadedAt: a.modified().UTC(),
		Link:       r.GetHTMLURL(),
		Origin:     "github",
		Tags:       tags,
	}
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
