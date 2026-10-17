package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/kriakiku/holtdroid-sync/internal/awssso"
	"github.com/kriakiku/holtdroid-sync/internal/s3sync"
	"github.com/kriakiku/holtdroid-sync/internal/secrets"
	"github.com/kriakiku/holtdroid-sync/internal/storage"
)

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `
source:
  bucket: work-apks
  prefix: android/
  region: eu-west-1
  include: ["/releases/*", "!/releases/night/*"]
  auth:
    type: sso
    sso:
      startURL: https://acme.awsapps.com/start
      region: eu-west-1
      accountID: "123456789012"
      roleName: ApkReader
      cacheFile: /data/sso/tokens.json
copy:
  target:
    type: s3
    s3: { endpoint: https://s3.example.com, bucket: holtdroid, prefix: work }
broker:
  enabled: true
  token: env:BROKER_TOKEN
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOLTDROID_S3_SYNC_COPY__INTERVAL", "10m")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Source.Auth.SSO.AccountID != "123456789012" || len(cfg.Source.Include) != 2 || !cfg.Copy.Enabled || !cfg.Copy.Prune ||
		cfg.Copy.Interval.Minutes() != 10 || cfg.Copy.Target.S3.Bucket != "holtdroid" || !cfg.Broker.Enabled || cfg.Listen != ":8080" {
		t.Fatalf("config %+v", cfg)
	}
}

func TestValidate(t *testing.T) {
	valid := func() Config {
		c := defaults()
		c.Source.Bucket, c.Source.Region = "b", "eu-west-1"
		c.Source.Auth = AuthConfig{Type: "default"}
		return c
	}
	tests := []struct {
		name   string
		mutate func(*Config)
		ok     bool
	}{
		{"default auth copy", func(*Config) {}, true},
		{"no bucket", func(c *Config) { c.Source.Bucket = "" }, false},
		{"bad rule", func(c *Config) { c.Source.Include = []string{"["} }, false},
		{"sso without role", func(c *Config) { c.Source.Auth.Type = "sso" }, false},
		{"unknown auth", func(c *Config) { c.Source.Auth.Type = "saml" }, false},
		{"nothing enabled", func(c *Config) { c.Copy.Enabled = false }, false},
		{"broker without token", func(c *Config) { c.Broker.Enabled = true }, false},
		{"broker only", func(c *Config) { c.Copy.Enabled = false; c.Broker.Enabled = true; c.Broker.Token = "env:X" }, true},
		{"build notifications without copies", func(c *Config) {
			c.Copy.Enabled = false
			c.Broker.Enabled = true
			c.Broker.Token = "env:X"
			c.Notify.Builds.URLs = []secrets.Ref{"env:URL"}
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := valid()
			tt.mutate(&c)
			if err := c.Validate(); (err == nil) != tt.ok {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestLoginRequiredReachesTheCopyLoop(t *testing.T) {
	backend := s3mem.New()
	if err := backend.CreateBucket("work"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	defer srv.Close()

	session := awssso.New(awssso.Config{StartURL: "https://acme.awsapps.com/start", Region: "eu-west-1", AccountID: "1", RoleName: "r"}, nil, 0)
	client := s3.New(s3.Options{Region: "eu-west-1", Credentials: session.Provider(), BaseEndpoint: aws.String(srv.URL), UsePathStyle: true})
	store, err := storage.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, err = s3sync.New(s3sync.Source{Client: client, Bucket: "work"}, store, true).Sync(context.Background())
	if !errors.Is(err, awssso.ErrLoginRequired) {
		t.Fatalf("err = %v", err)
	}
}

func TestBrokerTokenOnlyWhenEnabled(t *testing.T) {
	cfg := defaults()
	cfg.Broker.Token = "env:S3_SYNC_TEST_MISSING_TOKEN"
	if tok, err := brokerToken(context.Background(), cfg, secrets.NewRegistry()); err != nil || tok != "" {
		t.Fatalf("disabled broker: %q, %v", tok, err)
	}
	cfg.Broker.Enabled = true
	if _, err := brokerToken(context.Background(), cfg, secrets.NewRegistry()); err == nil {
		t.Fatal("enabled broker with a missing token must fail")
	}
}
