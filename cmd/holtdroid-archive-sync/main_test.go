package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := `
archives:
  - name: opera
    title: Opera
    url: https://get.opera.com/pub/opera/android
    versionDir: '^\d+$'
    fileVersion: 'Opera_([0-9_]+)_Generic'
    include: ["*_arm8_*.apk"]
target:
  type: s3
  s3: { endpoint: https://s3.example.com, bucket: holtdroid, prefix: archive }
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := cfg.Archives[0].build()
	if err != nil {
		t.Fatal(err)
	}
	if a.URL.Path != "/pub/opera/android/" || a.Versions != 5 || a.VersionDir == nil || a.FileVersion == nil || !a.Rules.Match("10300/x_arm8_y.apk") || a.Rules.Match("10300/x_arm7_y.apk") {
		t.Fatalf("archive %+v", a)
	}
}

func TestValidate(t *testing.T) {
	good := ArchiveConfig{Name: "a", URL: "https://example.com/x/"}
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"fine", Config{Archives: []ArchiveConfig{good}, Interval: 1}, ""},
		{"none", Config{Interval: 1}, "at least one"},
		{"no name", Config{Archives: []ArchiveConfig{{URL: good.URL}}, Interval: 1}, "name"},
		{"bad url", Config{Archives: []ArchiveConfig{{Name: "a", URL: "ftp://x"}}, Interval: 1}, "http(s)"},
		{"twice", Config{Archives: []ArchiveConfig{good, good}, Interval: 1}, "twice"},
		{"bad regexp", Config{Archives: []ArchiveConfig{{Name: "a", URL: good.URL, VersionDir: "("}}, Interval: 1}, "versionDir"},
		{"no capture", Config{Archives: []ArchiveConfig{{Name: "a", URL: good.URL, FileVersion: "x"}}, Interval: 1}, "capture group"},
		{"interval", Config{Archives: []ArchiveConfig{good}}, "interval"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.want == "" && err != nil {
				t.Fatal(err)
			}
			if tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
		})
	}
}
