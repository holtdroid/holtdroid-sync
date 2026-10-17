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
repos:
  - repo: WebView-CG/CanIAndroidWebView
    releases: 3
  - repo: acme/app
    prereleases: true
    include: ["!*-debug.apk"]
target:
  type: s3
  s3: { endpoint: https://s3.example.com, bucket: holtdroid, prefix: github }
notify:
  urls: [env:NOTIFY_URL]
  portalURL: https://android.example.com
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOLTDROID_GITHUB_SYNC_INTERVAL", "1h")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Repos) != 2 || cfg.Repos[0].Releases != 3 || !cfg.Repos[1].Prereleases || len(cfg.Repos[1].Include) != 1 ||
		cfg.Interval != time.Hour || !cfg.Prune || cfg.Target.S3.Bucket != "holtdroid" || cfg.GitHub.Token != "env:GITHUB_TOKEN" ||
		cfg.Notify.Source != "github" || cfg.Notify.MaxAge != 24*time.Hour {
		t.Fatalf("config %+v", cfg)
	}
	got := repos(cfg)
	if got[0].Owner != "WebView-CG" || got[0].Name != "CanIAndroidWebView" || got[1].Rules.Match("v1/app-debug.apk") {
		t.Fatalf("repos %+v", got)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name  string
		edit  func(*Config)
		error string
	}{
		{"ok", func(*Config) {}, ""},
		{"no repos", func(c *Config) { c.Repos = nil }, "at least one"},
		{"bad repo", func(c *Config) { c.Repos[0].Repo = "acme" }, "owner/name"},
		{"nested repo", func(c *Config) { c.Repos[0].Repo = "acme/app/x" }, "owner/name"},
		{"twice", func(c *Config) { c.Repos = append(c.Repos, RepoConfig{Repo: "ACME/app"}) }, "listed twice"},
		{"bad rule", func(c *Config) { c.Repos[0].Include = []string{"["} }, "include"},
		{"negative releases", func(c *Config) { c.Repos[0].Releases = -1 }, "releases"},
		{"interval", func(c *Config) { c.Interval = 0 }, "interval"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaults()
			cfg.Repos = []RepoConfig{{Repo: "acme/app"}}
			tt.edit(&cfg)
			err := cfg.Validate()
			if tt.error == "" && err != nil || tt.error != "" && (err == nil || !strings.Contains(err.Error(), tt.error)) {
				t.Fatalf("Validate() = %v, want %q", err, tt.error)
			}
		})
	}
}
