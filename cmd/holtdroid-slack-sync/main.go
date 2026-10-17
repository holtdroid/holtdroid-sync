package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/kriakiku/holtdroid-sync/internal/config"
	"github.com/kriakiku/holtdroid-sync/internal/run"
	"github.com/kriakiku/holtdroid-sync/internal/secrets"
	"github.com/kriakiku/holtdroid-sync/internal/slacksync"
	"github.com/kriakiku/holtdroid-sync/internal/storage"
)

var version = "dev"

type Config struct {
	Slack    slacksync.Config
	Target   storage.Config
	Interval time.Duration
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
	cfg := Config{Slack: slacksync.DefaultConfig(), Target: storage.Config{Type: "filesystem"}, Interval: time.Minute}
	if err := config.Load(envPrefix, path, cfg, &cfg); err != nil {
		return cfg, err
	}
	if err := cfg.Slack.Validate(); err != nil {
		return cfg, err
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

	s := slacksync.New(cfg.Slack, token, store, nil)
	slog.Info("starting", "version", version, "channels", cfg.Slack.Channels, "target", cfg.Target.Type, "once", once)
	if once {
		return s.Sync(ctx)
	}
	run.Every(ctx, cfg.Interval, func(ctx context.Context) {
		if err := s.Sync(ctx); err != nil && ctx.Err() == nil {
			slog.Error("sync failed", "err", err)
		}
	})
	return nil
}
