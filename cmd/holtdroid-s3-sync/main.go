package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/sync/errgroup"

	"github.com/kriakiku/holtdroid-sync/internal/announce"
	"github.com/kriakiku/holtdroid-sync/internal/awssso"
	"github.com/kriakiku/holtdroid-sync/internal/broker"
	"github.com/kriakiku/holtdroid-sync/internal/config"
	"github.com/kriakiku/holtdroid-sync/internal/rules"
	"github.com/kriakiku/holtdroid-sync/internal/run"
	"github.com/kriakiku/holtdroid-sync/internal/s3sync"
	"github.com/kriakiku/holtdroid-sync/internal/secrets"
	"github.com/kriakiku/holtdroid-sync/internal/signin"
	"github.com/kriakiku/holtdroid-sync/internal/storage"
)

var version = "dev"

type SourceConfig struct {
	Bucket    string
	Prefix    string
	Region    string
	Endpoint  string
	PathStyle bool
	Include   []string
	Auth      AuthConfig
}

type AuthConfig struct {
	Type   string
	SSO    awssso.Config
	Static struct {
		AccessKey secrets.Ref
		SecretKey secrets.Ref
	}
}

type CopyConfig struct {
	Enabled  bool
	Interval time.Duration
	Prune    bool
	Target   storage.Config
}

type BrokerConfig struct {
	Enabled bool
	Token   secrets.Ref
}

type NotifyConfig struct {
	URLs   []secrets.Ref
	Remind time.Duration
	Builds announce.Config
}

type Config struct {
	Source  SourceConfig
	Copy    CopyConfig
	Broker  BrokerConfig
	Listen  string
	Notify  NotifyConfig
	Secrets struct {
		OpenBao secrets.OpenBaoConfig
	}
}

func defaults() Config {
	return Config{
		Source: SourceConfig{Auth: AuthConfig{Type: "sso"}},
		Copy:   CopyConfig{Enabled: true, Interval: 5 * time.Minute, Prune: true, Target: storage.Config{Type: "filesystem"}},
		Listen: ":8080",
		Notify: NotifyConfig{Remind: time.Hour, Builds: announce.Config{Source: "s3", MaxAge: 24 * time.Hour}},
	}
}

func (c Config) Validate() error {
	var errs []error
	if c.Source.Bucket == "" || c.Source.Region == "" {
		errs = append(errs, errors.New("source.bucket and source.region are required"))
	}
	if _, err := rules.Parse(c.Source.Include); err != nil {
		errs = append(errs, fmt.Errorf("source.include: %w", err))
	}
	switch c.Source.Auth.Type {
	case "sso":
		if err := c.Source.Auth.SSO.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("source.auth.%w", err))
		}
	case "static":
		if c.Source.Auth.Static.AccessKey.IsZero() || c.Source.Auth.Static.SecretKey.IsZero() {
			errs = append(errs, errors.New("source.auth.static needs accessKey and secretKey"))
		}
	case "default":
	default:
		errs = append(errs, fmt.Errorf("source.auth.type must be sso, static or default, got %q", c.Source.Auth.Type))
	}
	if !c.Copy.Enabled && !c.Broker.Enabled {
		errs = append(errs, errors.New("enable copy, broker or both"))
	}
	if c.Copy.Enabled && c.Copy.Interval <= 0 {
		errs = append(errs, errors.New("copy.interval must be positive"))
	}
	if c.Notify.Remind < 0 {
		errs = append(errs, errors.New("notify.remind must not be negative"))
	}
	if err := c.Notify.Builds.Validate(); err != nil {
		errs = append(errs, fmt.Errorf("notify.builds.%w", err))
	}
	if len(c.Notify.Builds.URLs) > 0 && !c.Copy.Enabled {
		errs = append(errs, errors.New("notify.builds needs copy.enabled: only copies are announced"))
	}
	if c.Broker.Enabled && c.Broker.Token.IsZero() {
		errs = append(errs, errors.New("broker.token is required when the broker is enabled"))
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

const envPrefix = "HOLTDROID_S3_SYNC_"

func main() {
	run.Main(run.Job{
		Name:      "holtdroid-s3-sync",
		Short:     "Copy APKs from an S3 bucket into a Holtdroid source, or broker AWS credentials for reading it directly",
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

	notifier, pause, err := notifierFor(ctx, cfg, reg)
	if err != nil {
		return err
	}

	g, ctx := errgroup.WithContext(ctx)
	creds, status, err := credentialsFor(ctx, cfg, reg, notifier, pause, g)
	if err != nil {
		return err
	}
	client := s3.New(s3.Options{
		Region:       cfg.Source.Region,
		Credentials:  creds,
		BaseEndpoint: optional(cfg.Source.Endpoint),
		UsePathStyle: cfg.Source.PathStyle,
	})
	include, _ := rules.Parse(cfg.Source.Include)

	var sync *s3sync.Syncer
	if cfg.Copy.Enabled {
		store, err := storage.New(ctx, cfg.Copy.Target, reg)
		if err != nil {
			return fmt.Errorf("copy.target: %w", err)
		}
		sync = s3sync.New(s3sync.Source{
			Client: client, Bucket: cfg.Source.Bucket, Prefix: cfg.Source.Prefix,
			Region: cfg.Source.Region, AWS: cfg.Source.Endpoint == "", Rules: include,
		}, store, cfg.Copy.Prune)
	}
	slog.Info("starting", "version", version, "bucket", cfg.Source.Bucket, "prefix", cfg.Source.Prefix, "auth", cfg.Source.Auth.Type, "copy", cfg.Copy.Enabled, "broker", cfg.Broker.Enabled)

	if once {
		if sync == nil {
			return errors.New("--once needs copy.enabled")
		}
		st, err := sync.Sync(ctx)
		st.Log(ctx)
		return err
	}

	srv := &broker.Server{Credentials: creds, Status: status, Broker: cfg.Broker.Enabled}
	if srv.Token, err = brokerToken(ctx, cfg, reg); err != nil {
		return err
	}
	httpSrv := &http.Server{Addr: cfg.Listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }}
	g.Go(func() error {
		slog.Info("listening", "addr", cfg.Listen)
		if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdown)
	})
	if sync != nil {
		notify, err := announce.FromConfig(ctx, reg, cfg.Notify.Builds, "s3://"+cfg.Source.Bucket+"/"+cfg.Source.Prefix)
		if err != nil {
			return err
		}
		g.Go(func() error { run.Every(ctx, cfg.Copy.Interval, copyPass(sync, notify)); return nil })
	}
	return g.Wait()
}

func brokerToken(ctx context.Context, cfg Config, reg *secrets.Registry) (string, error) {
	if !cfg.Broker.Enabled {
		return "", nil
	}
	token, err := reg.Resolve(ctx, cfg.Broker.Token)
	if err != nil {
		return "", fmt.Errorf("broker.token: %w", err)
	}
	return token, nil
}

func notifierFor(ctx context.Context, cfg Config, reg *secrets.Registry) (awssso.Notifier, time.Duration, error) {
	if len(cfg.Notify.URLs) == 0 {
		return nil, 0, nil
	}
	urls, err := resolveAll(ctx, reg, cfg.Notify.URLs)
	if err != nil {
		return nil, 0, fmt.Errorf("notify.urls: %w", err)
	}
	n, err := signin.New(urls, "holtdroid-s3-sync (s3://"+cfg.Source.Bucket+"/"+cfg.Source.Prefix+")")
	if err != nil {
		return nil, 0, err
	}
	return n.Notify, cfg.Notify.Remind, nil
}

func credentialsFor(ctx context.Context, cfg Config, reg *secrets.Registry, notifier awssso.Notifier, pause time.Duration, g *errgroup.Group) (aws.CredentialsProvider, func() any, error) {
	switch cfg.Source.Auth.Type {
	case "sso":
		session := awssso.New(cfg.Source.Auth.SSO, notifier, pause)
		g.Go(func() error { session.Run(ctx); return nil })
		return session.Provider(), func() any { return session.Status() }, nil
	case "static":
		ak, err := reg.Resolve(ctx, cfg.Source.Auth.Static.AccessKey)
		if err != nil {
			return nil, nil, err
		}
		sk, err := reg.Resolve(ctx, cfg.Source.Auth.Static.SecretKey)
		if err != nil {
			return nil, nil, err
		}
		return awscreds.NewStaticCredentialsProvider(ak, sk, ""), staticStatus, nil
	default:
		awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Source.Region))
		if err != nil {
			return nil, nil, err
		}
		return awsCfg.Credentials, staticStatus, nil
	}
}

func staticStatus() any { return map[string]string{"state": string(awssso.StateReady)} }

func resolveAll(ctx context.Context, reg *secrets.Registry, refs []secrets.Ref) ([]string, error) {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		v, err := reg.Resolve(ctx, ref)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func copyPass(s *s3sync.Syncer, notify announce.Func) func(context.Context) {
	waiting := false
	return func(ctx context.Context) {
		st, err := s.Sync(ctx)
		switch {
		case errors.Is(err, awssso.ErrLoginRequired):
			if !waiting {
				slog.Warn("waiting for an AWS login; already copied builds stay available")
			}
			waiting = true
		case err != nil && ctx.Err() == nil:
			slog.Error("sync failed", "err", err)
		default:
			waiting = false
		}
		notify(ctx, st.Builds)
		st.Log(ctx)
	}
}
