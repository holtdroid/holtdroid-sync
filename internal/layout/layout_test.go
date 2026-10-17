package layout

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kriakiku/holtdroid-sync/internal/storage"
)

func newStore(t *testing.T) storage.Store {
	t.Helper()
	store, err := storage.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func keys(t *testing.T, store storage.Store) []string {
	t.Helper()
	objs, err := store.List(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, o := range objs {
		out = append(out, o.Key)
	}
	slices.Sort(out)
	return out
}

func TestStagePublishRemove(t *testing.T) {
	raw, err := os.ReadFile("../apk/testdata/helloworld.apk")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store := newStore(t)
	if err := store.Put(ctx, "a/app.apk.icon.png", strings.NewReader("stale"), 5); err != nil {
		t.Fatal(err)
	}

	if _, err := Stage(bytes.NewReader(raw), int64(len(raw))+1); err == nil || !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("a short download must fail: %v", err)
	}
	staged, err := Stage(bytes.NewReader(raw), -1)
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	if staged.Size != int64(len(raw)) || staged.App == nil || staged.App.Package != "com.example.helloworld" || staged.Icon == nil {
		t.Fatalf("staged %+v", staged)
	}
	if err := staged.Publish(ctx, store, "a/app.apk", Meta{Origin: "test"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a/app.apk", "a/app.apk.icon.png", "a/app.apk.meta.json"}; !slices.Equal(keys(t, store), want) {
		t.Fatalf("keys %v", keys(t, store))
	}
	rc, _, err := store.Get(ctx, "a/app.apk.meta.json")
	if err != nil {
		t.Fatal(err)
	}
	var m Meta
	_ = json.NewDecoder(rc).Decode(&m)
	_ = rc.Close()
	if m.Origin != "test" {
		t.Fatalf("meta %+v", m)
	}
	if _, size, _ := store.Get(ctx, "a/app.apk.icon.png"); size == 5 {
		t.Fatal("the stale icon must be replaced")
	}

	staged.Icon = nil
	if err := staged.Publish(ctx, store, "a/app.apk", Meta{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"a/app.apk", "a/app.apk.meta.json"}; !slices.Equal(keys(t, store), want) {
		t.Fatalf("an apk without an icon must drop the old one: %v", keys(t, store))
	}

	if err := Remove(ctx, store, "a/app.apk"); err != nil {
		t.Fatal(err)
	}
	if got := keys(t, store); len(got) != 0 {
		t.Fatalf("left %v", got)
	}
}

func TestOutdated(t *testing.T) {
	now := time.Now()
	have := storage.Object{Size: 10, Modified: now}
	tests := []struct {
		name     string
		exists   bool
		size     int64
		modified time.Time
		want     bool
	}{
		{"missing", false, 10, now, true},
		{"same", true, 10, now.Add(-time.Hour), false},
		{"other size", true, 11, now.Add(-time.Hour), true},
		{"newer source", true, 10, now.Add(time.Minute), true},
	}
	for _, tt := range tests {
		if got := Outdated(have, tt.exists, tt.size, tt.modified); got != tt.want {
			t.Errorf("%s: Outdated = %v", tt.name, got)
		}
	}
}

func TestHelpers(t *testing.T) {
	for key, want := range map[string]bool{"a/b.apk": true, "a/B.APK": true, "a/b.apk.meta.json": false} {
		if IsAPK(key) != want {
			t.Errorf("IsAPK(%q) != %v", key, want)
		}
	}
	for rel, want := range map[string]bool{"a/b.apk": false, ".git/b.apk": true, "a/.b.apk": true, "a//b.apk": true} {
		if Hidden(rel) != want {
			t.Errorf("Hidden(%q) != %v", rel, want)
		}
	}
	for n, want := range map[int64]string{512: "512 B", 1536: "1.5 KiB", 52 << 20: "52.0 MiB"} {
		if got := FormatSize(n); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", n, got, want)
		}
	}
	if got := LaunchURL("https://hd.example/", "slack", "C1/F1/app.apk", true); got != "https://hd.example/?launch=QzEvRjEvYXBwLmFwaw&quick=1&source=slack" {
		t.Errorf("LaunchURL = %q", got)
	}
	if LaunchURL("", "slack", "x.apk", false) != "" {
		t.Error("no portal, no link")
	}
}
