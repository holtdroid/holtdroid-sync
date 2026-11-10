package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `
github:
  token: env:GITHUB_TOKEN
target:
  type: s3
  s3: { endpoint: https://s3.example.com, bucket: holtdroid, prefix: chromium-builds }
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOLTDROID_CHROMIUM_BUILDS_SYNC_INTERVAL", "1h")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Repo != "holtdroid/chromium-builds" || cfg.Interval != time.Hour || !cfg.Prune || cfg.Target.S3.Bucket != "holtdroid" ||
		cfg.GitHub.Token != "env:GITHUB_TOKEN" || len(cfg.Requirements.Browser) != 2 || len(cfg.Requirements.WebView) != 1 {
		t.Fatalf("config %+v", cfg)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"defaults", func(*Config) {}, ""},
		{"bad repo", func(c *Config) { c.Repo = "nope" }, "owner/name"},
		{"bad interval", func(c *Config) { c.Interval = 0 }, "interval"},
		{"bad requirement", func(c *Config) { c.Requirements.WebView[0].MinSDK = 0 }, "requirements.webview[0]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaults()
			tt.edit(&cfg)
			err := cfg.Validate()
			if tt.want == "" && err != nil {
				t.Fatal(err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
		})
	}
}
