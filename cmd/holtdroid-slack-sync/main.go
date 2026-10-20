package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/holtdroid/holtdroid-sync/internal/announce"
	"github.com/holtdroid/holtdroid-sync/internal/config"
	"github.com/holtdroid/holtdroid-sync/internal/run"
	"github.com/holtdroid/holtdroid-sync/internal/secrets"
	"github.com/holtdroid/holtdroid-sync/internal/slacksync"
	"github.com/holtdroid/holtdroid-sync/internal/storage"
)

var version = "dev"

type Config struct {
	Slack    slacksync.Config
	Target   storage.Config
	Interval time.Duration
	Notify   announce.Config
	Secrets  struct {
		OpenBao secrets.OpenBaoConfig
	}
}

const envPrefix = "HOLTDROID_SLACK_SYNC_"

func main() {
	run.Main(run.Job{
		Name:      "holtdroid-slack-sync",
		Short:     "Copy APKs posted to Slack channels into a Holtdroid artifact source",
		EnvPrefix: envPrefix,
		Version:   version,
		Run:       runJob,
	})
}

func loadConfig(path string) (Config, error) {
	cfg := Config{
		Slack: slacksync.DefaultConfig(), Target: storage.Config{Type: "filesystem"}, Interval: time.Minute,
		Notify: announce.Config{Source: "slack", MaxAge: 24 * time.Hour},
	}
	if err := config.Load(envPrefix, path, cfg, &cfg); err != nil {
		return cfg, err
	}
	if err := cfg.Slack.Validate(); err != nil {
		return cfg, err
	}
	if err := cfg.Notify.Validate(); err != nil {
		return cfg, fmt.Errorf("notify.%w", err)
	}
	if cfg.Interval <= 0 {
		return cfg, fmt.Errorf("interval must be positive, got %s", cfg.Interval)
	}
	return cfg, nil
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
	token, err := reg.Resolve(ctx, cfg.Slack.Token)
	if err != nil {
		return fmt.Errorf("slack.token: %w", err)
	}
	store, err := storage.New(ctx, cfg.Target, reg)
	if err != nil {
		return err
	}

	notify, err := announce.FromConfig(ctx, reg, cfg.Notify, "Slack")
	if err != nil {
		return fmt.Errorf("notify.urls: %w", err)
	}

	s := slacksync.New(cfg.Slack, token, store, nil)
	slog.Info("starting", "version", version, "channels", cfg.Slack.Channels, "target", cfg.Target.Type, "once", once)
	if once {
		st, err := s.Sync(ctx)
		notify(ctx, st.Builds)
		return err
	}
	run.Every(ctx, cfg.Interval, func(ctx context.Context) {
		st, err := s.Sync(ctx)
		if err != nil && ctx.Err() == nil {
			slog.Error("sync failed", "err", err)
		}
		notify(ctx, st.Builds)
	})
	return nil
}
