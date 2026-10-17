package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/kriakiku/holtdroid-sync/internal/secrets"
)

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `
slack:
  token: openbao:secret/holtdroid#slack_bot_token
  channels: [C0123ABCD]
notify:
  urls: [env:NOTIFY_URL]
  portalURL: https://android.example.com
target:
  type: s3
  s3:
    endpoint: https://s3.example.com
    bucket: apks
    prefix: slack
    accessKey: env:S3_ACCESS_KEY
    secretKey: env:S3_SECRET_KEY
    pathStyle: true
secrets:
  openbao:
    address: https://bao.example.com
    kubernetes:
      role: holtdroid-slack-sync
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOLTDROID_SLACK_SYNC_SLACK__CHANNELS", "C1,C2")
	t.Setenv("HOLTDROID_SLACK_SYNC_INTERVAL", "30s")

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.Slack.Channels, []string{"C1", "C2"}) || cfg.Interval != 30*time.Second {
		t.Fatalf("env overrides lost: %+v", cfg)
	}
	if cfg.Slack.Token != secrets.Ref("openbao:secret/holtdroid#slack_bot_token") || !cfg.Slack.Threads || cfg.Slack.Lookback != 7*24*time.Hour {
		t.Fatalf("slack %+v", cfg.Slack)
	}
	if len(cfg.Notify.URLs) != 1 || cfg.Notify.PortalURL != "https://android.example.com" || cfg.Notify.Source != "slack" || cfg.Notify.MaxAge != 24*time.Hour {
		t.Fatalf("notify %+v", cfg.Notify)
	}
	if cfg.Target.S3.Bucket != "apks" || !cfg.Target.S3.PathStyle || cfg.Target.S3.AccessKey != "env:S3_ACCESS_KEY" {
		t.Fatalf("target %+v", cfg.Target)
	}
	if cfg.Secrets.OpenBao.Kubernetes.Role != "holtdroid-slack-sync" {
		t.Fatalf("secrets %+v", cfg.Secrets)
	}
}

func TestLoadConfigRequiresChannels(t *testing.T) {
	if _, err := loadConfig(""); err == nil {
		t.Fatal("config without channels accepted")
	}
}
