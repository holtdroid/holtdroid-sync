package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `
milestones:
  latest: 4
  extra: [130]
target:
  type: s3
  s3: { endpoint: https://s3.example.com, bucket: holtdroid, prefix: webview }
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOLTDROID_WEBVIEW_SYNC_INTERVAL", "1h")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Milestones.Latest != 4 || cfg.Milestones.Every != 5 || len(cfg.Milestones.Extra) != 1 || cfg.Milestones.Extra[0] != 130 ||
		cfg.Interval != time.Hour || !cfg.Prune || cfg.Target.S3.Prefix != "webview" {
		t.Fatalf("config %+v", cfg)
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"nothing picked", func(c *Config) { c.Milestones.Latest, c.Milestones.Every = 0, 0 }},
		{"negative", func(c *Config) { c.Milestones.Latest = -1 }},
		{"interval", func(c *Config) { c.Interval = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := defaults()
			tc.edit(&cfg)
			if cfg.Validate() == nil {
				t.Fatal("valid")
			}
		})
	}
	if err := defaults().Validate(); err != nil {
		t.Fatal(err)
	}
}
