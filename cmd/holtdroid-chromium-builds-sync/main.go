package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/go-github/v93/github"

	"github.com/holtdroid/holtdroid-sync/internal/chromiumbuilds"
	"github.com/holtdroid/holtdroid-sync/internal/config"
	"github.com/holtdroid/holtdroid-sync/internal/run"
	"github.com/holtdroid/holtdroid-sync/internal/secrets"
	"github.com/holtdroid/holtdroid-sync/internal/snapshotsync"
	"github.com/holtdroid/holtdroid-sync/internal/storage"
)

var version = "dev"

type Config struct {
	GitHub struct {
		Token secrets.Ref
	}
	Repo         string
	Requirements chromiumbuilds.Requirements
	Interval     time.Duration
	Prune        bool
	Target       storage.Config
	Secrets      struct {
		OpenBao secrets.OpenBaoConfig
	}
}

func defaults() Config {
	return Config{
		Repo: "holtdroid/chromium-builds",
		Requirements: chromiumbuilds.Requirements{
			Browser: []snapshotsync.Requirement{{From: 149, MinSDK: 34}, {From: 157, MinSDK: 35}},
			WebView: []snapshotsync.Requirement{{From: 149, MinSDK: 34}},
		},
		Interval: 15 * time.Minute,
		Prune:    true,
		Target:   storage.Config{Type: "filesystem"},
	}
}

func (c Config) Validate() error {
	var errs []error
	if _, _, ok := splitRepo(c.Repo); !ok {
		errs = append(errs, fmt.Errorf("repo must be owner/name, got %q", c.Repo))
	}
	for name, reqs := range map[string][]snapshotsync.Requirement{"browser": c.Requirements.Browser, "webview": c.Requirements.WebView} {
		for i, r := range reqs {
			if r.From <= 0 || r.MinSDK <= 0 {
				errs = append(errs, fmt.Errorf("requirements.%s[%d]: from and minSdk must be positive", name, i))
			}
		}
	}
	if c.Interval <= 0 {
		errs = append(errs, errors.New("interval must be positive"))
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

const envPrefix = "HOLTDROID_CHROMIUM_BUILDS_SYNC_"

func main() {
	run.Main(run.Job{
		Name:      "holtdroid-chromium-builds-sync",
		Short:     "Copy Chromium and WebView builds from chromium-builds releases into Holtdroid",
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
	opts := []github.ClientOptionsFunc{github.WithTimeout(time.Minute), github.WithUserAgent("holtdroid-chromium-builds-sync/" + version)}
	if !cfg.GitHub.Token.IsZero() {
		token, err := reg.Resolve(ctx, cfg.GitHub.Token)
		if err != nil {
			return fmt.Errorf("github.token: %w", err)
		}
		opts = append(opts, github.WithAuthToken(token))
	}
	client, err := github.NewClient(opts...)
	if err != nil {
		return err
	}
	store, err := storage.New(ctx, cfg.Target, reg)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	owner, name, _ := splitRepo(cfg.Repo)
	sync := chromiumbuilds.New(client, &http.Client{Timeout: 30 * time.Minute}, owner, name, cfg.Requirements, store, cfg.Prune)
	slog.Info("starting", "version", version, "repo", cfg.Repo, "authenticated", !cfg.GitHub.Token.IsZero())

	pass := func(ctx context.Context) error {
		st, err := sync.Sync(ctx)
		st.Log(ctx)
		return err
	}
	if once {
		return pass(ctx)
	}
	run.Every(ctx, cfg.Interval, func(ctx context.Context) {
		if err := pass(ctx); err != nil && ctx.Err() == nil {
			slog.Error("sync failed", "err", err)
		}
	})
	return nil
}
