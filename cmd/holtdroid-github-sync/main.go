package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/go-github/v93/github"

	"github.com/kriakiku/holtdroid-sync/internal/announce"
	"github.com/kriakiku/holtdroid-sync/internal/config"
	"github.com/kriakiku/holtdroid-sync/internal/githubsync"
	"github.com/kriakiku/holtdroid-sync/internal/rules"
	"github.com/kriakiku/holtdroid-sync/internal/run"
	"github.com/kriakiku/holtdroid-sync/internal/secrets"
	"github.com/kriakiku/holtdroid-sync/internal/storage"
)

var version = "dev"

type RepoConfig struct {
	Repo        string
	Include     []string
	Prereleases bool
	Releases    int
}

type Config struct {
	GitHub struct {
		Token secrets.Ref
	}
	Repos    []RepoConfig
	Interval time.Duration
	Prune    bool
	Target   storage.Config
	Notify   announce.Config
	Secrets  struct {
		OpenBao secrets.OpenBaoConfig
	}
}

func defaults() Config {
	return Config{
		Interval: 15 * time.Minute,
		Prune:    true,
		Target:   storage.Config{Type: "filesystem"},
		Notify:   announce.Config{Source: "github", MaxAge: 24 * time.Hour},
	}
}

func (c Config) Validate() error {
	var errs []error
	if len(c.Repos) == 0 {
		errs = append(errs, errors.New("repos: list at least one owner/name"))
	}
	seen := map[string]bool{}
	for i, r := range c.Repos {
		if _, _, ok := splitRepo(r.Repo); !ok {
			errs = append(errs, fmt.Errorf("repos[%d].repo must be owner/name, got %q", i, r.Repo))
		} else if seen[strings.ToLower(r.Repo)] {
			errs = append(errs, fmt.Errorf("repos[%d]: %s is listed twice", i, r.Repo))
		}
		seen[strings.ToLower(r.Repo)] = true
		if _, err := rules.Parse(r.Include); err != nil {
			errs = append(errs, fmt.Errorf("repos[%d].include: %w", i, err))
		}
		if r.Releases < 0 {
			errs = append(errs, fmt.Errorf("repos[%d].releases must not be negative", i))
		}
	}
	if c.Interval <= 0 {
		errs = append(errs, errors.New("interval must be positive"))
	}
	if err := c.Notify.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("notify.%w", err))
	}
	return errors.Join(errs...)
}

func splitRepo(s string) (owner, name string, ok bool) {
	owner, name, ok = strings.Cut(s, "/")
	return owner, name, ok && owner != "" && name != "" && !strings.Contains(name, "/")
}

func loadConfig(path string) (Config, error) {
	cfg := defaults()
	if err := config.Load(envPrefix, path, cfg, &cfg); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

const envPrefix = "HOLTDROID_GITHUB_SYNC_"

func main() {
	run.Main(run.Job{
		Name:      "holtdroid-github-sync",
		Short:     "Copy APKs from GitHub releases into a Holtdroid source",
		EnvPrefix: envPrefix,
		Version:   version,
		Run:       runJob,
	})
}

func runJob(ctx context.Context, configPath string, once bool) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	reg := secrets.NewRegistry()
	if cfg.Secrets.OpenBao.Address != "" {
		reg.Register("openbao", secrets.NewOpenBao(cfg.Secrets.OpenBao, reg, nil))
	}

	client, err := githubClient(ctx, cfg, reg)
	if err != nil {
		return err
	}
	store, err := storage.New(ctx, cfg.Target, reg)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	sync := githubsync.New(client, repos(cfg), store, cfg.Prune)
	slog.Info("starting", "version", version, "repos", len(cfg.Repos), "authenticated", !cfg.GitHub.Token.IsZero())

	if once {
		st, err := sync.Sync(ctx)
		st.Log(ctx)
		return err
	}
	notify, err := announce.FromConfig(ctx, reg, cfg.Notify, "GitHub releases")
	if err != nil {
		return err
	}
	run.Every(ctx, cfg.Interval, func(ctx context.Context) {
		st, err := sync.Sync(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("sync failed", "err", err)
		}
		notify(ctx, st.Builds)
		st.Log(ctx)
	})
	return nil
}

func githubClient(ctx context.Context, cfg Config, reg *secrets.Registry) (*github.Client, error) {
	opts := []github.ClientOptionsFunc{github.WithTimeout(10 * time.Minute), github.WithUserAgent("holtdroid-github-sync/" + version)}
	if !cfg.GitHub.Token.IsZero() {
		token, err := reg.Resolve(ctx, cfg.GitHub.Token)
		if err != nil {
			return nil, fmt.Errorf("github.token: %w", err)
		}
		opts = append(opts, github.WithAuthToken(token))
	}
	return github.NewClient(opts...)
}

func repos(cfg Config) []githubsync.Repo {
	out := make([]githubsync.Repo, 0, len(cfg.Repos))
	for _, r := range cfg.Repos {
		owner, name, _ := splitRepo(r.Repo)
		include, _ := rules.Parse(r.Include)
		out = append(out, githubsync.Repo{Owner: owner, Name: name, Rules: include, Prereleases: r.Prereleases, Releases: r.Releases})
	}
	return out
}
