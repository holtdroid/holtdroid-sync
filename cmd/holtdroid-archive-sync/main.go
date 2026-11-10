package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/holtdroid/holtdroid-sync/internal/archivesync"
	"github.com/holtdroid/holtdroid-sync/internal/config"
	"github.com/holtdroid/holtdroid-sync/internal/rules"
	"github.com/holtdroid/holtdroid-sync/internal/run"
	"github.com/holtdroid/holtdroid-sync/internal/secrets"
	"github.com/holtdroid/holtdroid-sync/internal/storage"
)

var version = "dev"

type ArchiveConfig struct {
	Name        string
	Title       string
	URL         string
	VersionDir  string
	FileVersion string
	Depth       int
	Include     []string
	Versions    int
}

type Config struct {
	Archives []ArchiveConfig
	Interval time.Duration
	Prune    bool
	Target   storage.Config
	Secrets  struct {
		OpenBao secrets.OpenBaoConfig
	}
}

func defaults() Config {
	return Config{Interval: 6 * time.Hour, Prune: true, Target: storage.Config{Type: "filesystem"}}
}

func (c Config) Validate() error {
	var errs []error
	if len(c.Archives) == 0 {
		errs = append(errs, errors.New("archives: list at least one"))
	}
	seen := map[string]bool{}
	for i, a := range c.Archives {
		if _, err := a.build(); err != nil {
			errs = append(errs, fmt.Errorf("archives[%d]: %w", i, err))
		}
		if seen[a.Name] {
			errs = append(errs, fmt.Errorf("archives[%d]: %s is listed twice", i, a.Name))
		}
		seen[a.Name] = true
	}
	if c.Interval <= 0 {
		errs = append(errs, errors.New("interval must be positive"))
	}
	return errors.Join(errs...)
}

func (a ArchiveConfig) build() (archivesync.Archive, error) {
	out := archivesync.Archive{Name: a.Name, Title: a.Title, Depth: a.Depth, Versions: a.Versions}
	if a.Name == "" {
		return out, errors.New("name is required")
	}
	u, err := url.Parse(a.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return out, fmt.Errorf("url must be http(s), got %q", a.URL)
	}
	if u.Path == "" || u.Path[len(u.Path)-1] != '/' {
		u.Path += "/"
	}
	out.URL = u
	if a.Versions <= 0 {
		out.Versions = 5
	}
	if a.Depth < 0 {
		return out, errors.New("depth must not be negative")
	}
	if a.VersionDir != "" {
		if out.VersionDir, err = regexp.Compile(a.VersionDir); err != nil {
			return out, fmt.Errorf("versionDir: %w", err)
		}
	}
	if a.FileVersion != "" {
		if out.FileVersion, err = regexp.Compile(a.FileVersion); err != nil {
			return out, fmt.Errorf("fileVersion: %w", err)
		}
		if out.FileVersion.NumSubexp() != 1 {
			return out, errors.New("fileVersion needs exactly one capture group")
		}
	}
	if out.Rules, err = rules.Parse(a.Include); err != nil {
		return out, fmt.Errorf("include: %w", err)
	}
	return out, nil
}

func loadConfig(path string) (Config, error) {
	cfg := defaults()
	if err := config.Load(envPrefix, path, cfg, &cfg); err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

const envPrefix = "HOLTDROID_ARCHIVE_SYNC_"

func main() {
	run.Main(run.Job{
		Name:      "holtdroid-archive-sync",
		Short:     "Copy APKs from public release archives into a Holtdroid source",
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
	archives := make([]archivesync.Archive, 0, len(cfg.Archives))
	for _, a := range cfg.Archives {
		built, _ := a.build()
		archives = append(archives, built)
	}
	sync := archivesync.New(&http.Client{Timeout: 30 * time.Minute}, "holtdroid-archive-sync/"+version, archives, store, cfg.Prune)
	slog.Info("starting", "version", version, "archives", len(archives))

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
