package apk

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParse(t *testing.T) {
	meta, _, err := Parse("testdata/helloworld.apk")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Package != "com.example.helloworld" {
		t.Errorf("package = %q", meta.Package)
	}
	if meta.VersionName != "1.0" || meta.VersionCode != 1 {
		t.Errorf("version = %q (%d)", meta.VersionName, meta.VersionCode)
	}
	if meta.Label == "" {
		t.Error("label is empty")
	}
	if meta.ABIs == nil {
		t.Error("abis must be non-nil")
	}
}

func TestParseRejectsNonAPK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.apk")
	if err := os.WriteFile(path, []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Parse(path); err == nil {
		t.Fatal("expected error")
	}
}

func TestDecodeManifestMatchesAndroidbinary(t *testing.T) {
	want, _, err := Parse("testdata/helloworld.apk")
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseManifest("testdata/helloworld.apk")
	if err != nil {
		t.Fatal(err)
	}
	if got.Package != want.Package || got.VersionName != want.VersionName || got.VersionCode != want.VersionCode ||
		got.MinSDK != want.MinSDK || got.TargetSDK != want.TargetSDK {
		t.Fatalf("fallback %+v, androidbinary %+v", got, want)
	}
	for _, raw := range [][]byte{nil, {3, 0, 8, 0}, {3, 0, 8, 0, 100, 0, 0, 0, 1, 0}} {
		if _, err := decodeManifest(raw); err == nil {
			t.Fatalf("decoded %v", raw)
		}
	}
}
