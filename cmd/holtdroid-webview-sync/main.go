package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/holtdroid/holtdroid-sync/internal/config"
	"github.com/holtdroid/holtdroid-sync/internal/run"
	"github.com/holtdroid/holtdroid-sync/internal/secrets"
	"github.com/holtdroid/holtdroid-sync/internal/snapshotsync"
	"github.com/holtdroid/holtdroid-sync/internal/storage"
)

var version = "dev"

type Config struct {
	Milestones struct {
		Latest int
		Every  int
		Older  snapshotsync.Thinning
		Extra  []int
	}
	// Requirements raise the Android API level of milestones that break on
	// older Android although their manifest allows it.
	Requirements []snapshotsync.Requirement
	Interval     time.Duration
	Prune        bool
	Target       storage.Config
	Secrets      struct {
		OpenBao secrets.OpenBaoConfig
	}
}

func defaults() Config {
	cfg := Config{
		Requirements: []snapshotsync.Requirement{{From: 149, MinSDK: 34}},
		Interval:     6 * time.Hour,
		Prune:        true,
		Target:       storage.Config{Type: "filesystem"},
	}
	cfg.Milestones.Latest = 6
	cfg.Milestones.Every = 5
	return cfg
}

func (c Config) Validate() error {
	var errs []error
	if c.Milestones.Latest < 0 || c.Milestones.Every < 0 || c.Milestones.Older.After < 0 || c.Milestones.Older.Every < 0 {
		errs = append(errs, errors.New("milestones: latest, every and older must not be negative"))
	}
	if c.Milestones.Latest == 0 && c.Milestones.Every == 0 && (c.Milestones.Older.After == 0 || c.Milestones.Older.Every == 0) && len(c.Milestones.Extra) == 0 {
		errs = append(errs, errors.New("milestones: pick at least one with latest, every, older or extra"))
	}
	for i, r := range c.Requirements {
		if r.From <= 0 || r.MinSDK <= 0 {
			errs = append(errs, fmt.Errorf("requirements[%d]: from and minSdk must be positive", i))
		}
	}
	if c.Interval <= 0 {
		errs = append(errs, errors.New("interval must be positive"))
	}
	return errors.Join(errs...)
}

func loadConfig(path string) (Config, error) {
	cfg := defaults()
	if err := config.Load(envPrefix, path, cfg, &cfg); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

const envPrefix = "HOLTDROID_WEBVIEW_SYNC_"

func main() {
	run.Main(run.Job{
		Name:      "holtdroid-webview-sync",
		Short:     "Copy Chromium WebView snapshot builds into a Holtdroid WebView catalog",
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
	store, err := storage.New(ctx, cfg.Target, reg)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	sel := snapshotsync.Selection{Latest: cfg.Milestones.Latest, Every: cfg.Milestones.Every, Older: cfg.Milestones.Older, Milestones: cfg.Milestones.Extra}
	sync := snapshotsync.New(&http.Client{}, snapshotsync.WebView, sel, cfg.Requirements, store, cfg.Prune)
	slog.Info("starting", "version", version, "latest", sel.Latest, "every", sel.Every, "older", sel.Older, "extra", sel.Milestones)

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
