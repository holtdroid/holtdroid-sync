package s3sync

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"log/slog"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/holtdroid/holtdroid-sync/internal/layout"
	"github.com/holtdroid/holtdroid-sync/internal/rules"
	"github.com/holtdroid/holtdroid-sync/internal/storage"
)

var apkBytes = func() []byte {
	raw, err := os.ReadFile("../apk/testdata/helloworld.apk")
	if err != nil {
		panic(err)
	}
	return raw
}()

type harness struct {
	t      *testing.T
	client *s3.Client
	store  storage.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket("work"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	client := s3.New(s3.Options{
		Region: "eu-west-1", BaseEndpoint: aws.String(srv.URL), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""),
	})
	store, err := storage.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	slog.SetDefault(slog.New(slog.DiscardHandler))
	return &harness{t: t, client: client, store: store}
}

func (h *harness) put(key string, body []byte) {
	h.t.Helper()
	if _, err := h.client.PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String("work"), Key: aws.String(key), Body: bytes.NewReader(body)}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) syncer(patterns ...string) *Syncer {
	h.t.Helper()
	r, err := rules.Parse(patterns)
	if err != nil {
		h.t.Fatal(err)
	}
	return New(Source{Client: h.client, Bucket: "work", Prefix: "android/", Region: "eu-west-1", AWS: true, Rules: r}, h.store, true)
}

func (h *harness) keys() []string {
	h.t.Helper()
	objs, err := h.store.List(context.Background(), "")
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, o := range objs {
		out = append(out, o.Key)
	}
	slices.Sort(out)
	return out
}

func (h *harness) meta(key string) layout.Meta {
	h.t.Helper()
	rc, _, err := h.store.Get(context.Background(), key+layout.MetaSuffix)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rc.Close()
	var m layout.Meta
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		h.t.Fatal(err)
	}
	return m
}

func TestSyncCopiesMatchingAPKs(t *testing.T) {
	h := newHarness(t)
	h.put("android/releases/app-1.0.apk", apkBytes)
	h.put("android/releases/night/app-nightly.apk", apkBytes)
	h.put("android/releases/notes.txt", []byte("not an apk"))
	h.put("android/feature/app-x.apk", apkBytes)
	h.put("android/.trash/old.apk", apkBytes)
	h.put("ios/app.apk", apkBytes)

	st, err := h.syncer("/releases/*", "!/releases/night/*").Sync(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"releases/app-1.0.apk", "releases/app-1.0.apk.icon.png", "releases/app-1.0.apk.meta.json"}
	if got := h.keys(); !slices.Equal(got, want) || st.Copied != 1 {
		t.Fatalf("keys %v, stats %+v", got, st)
	}
	if len(st.Builds) != 1 || st.Builds[0].Key != "releases/app-1.0.apk" || st.Builds[0].App == nil || st.Builds[0].App.Package != "com.example.helloworld" || st.Builds[0].Modified.IsZero() {
		t.Fatalf("builds %+v", st.Builds)
	}
	m := h.meta("releases/app-1.0.apk")
	if m.Origin != "s3" || m.UploadedAt.IsZero() || !strings.HasPrefix(m.Link, "https://eu-west-1.console.aws.amazon.com/s3/object/work?") || !strings.Contains(m.Link, "android%2Freleases%2Fapp-1.0.apk") {
		t.Fatalf("meta %+v", m)
	}

	st, err = h.syncer("/releases/*", "!/releases/night/*").Sync(context.Background())
	if err != nil || st.Copied != 0 || st.Skipped != 1 {
		t.Fatalf("second pass %+v, %v", st, err)
	}
}

func TestSyncUsesPublishedSidecars(t *testing.T) {
	h := newHarness(t)
	h.put("android/app.apk", apkBytes)
	h.put("android/app.apk.meta.json", []byte(`{"changelog":"fixed login","tags":{"branch":"main"}}`))
	icon := image.NewNRGBA(image.Rect(0, 0, 16, 16))
	var buf bytes.Buffer
	_ = png.Encode(&buf, icon)
	h.put("android/app.apk.icon.png", buf.Bytes())

	if _, err := h.syncer().Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	m := h.meta("app.apk")
	if m.Changelog != "fixed login" || m.Tags["branch"] != "main" || m.Origin != "s3" || m.UploadedAt.IsZero() {
		t.Fatalf("meta %+v", m)
	}
	rc, _, err := h.store.Get(context.Background(), "app.apk.icon.png")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	img, err := png.Decode(rc)
	if err != nil || img.Bounds().Dx() != 256 {
		t.Fatalf("icon must be re-rendered: %v %v", img.Bounds(), err)
	}
}

func TestSyncReplacesAndPrunes(t *testing.T) {
	h := newHarness(t)
	h.put("android/a.apk", apkBytes)
	h.put("android/b.apk", apkBytes)
	s := h.syncer()
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	time.Sleep(1100 * time.Millisecond)
	h.put("android/a.apk", append(slices.Clone(apkBytes), 0))
	if _, err := h.client.DeleteObject(context.Background(), &s3.DeleteObjectInput{Bucket: aws.String("work"), Key: aws.String("android/b.apk")}); err != nil {
		t.Fatal(err)
	}
	st, err := s.Sync(context.Background())
	if err != nil || st.Copied != 1 || st.Pruned != 1 {
		t.Fatalf("stats %+v, %v", st, err)
	}
	if got := h.keys(); slices.ContainsFunc(got, func(k string) bool { return strings.HasPrefix(k, "b.apk") }) {
		t.Fatalf("b.apk not pruned: %v", got)
	}
	o, err := h.store.Stat(context.Background(), "a.apk")
	if err != nil || o.Size != int64(len(apkBytes)+1) {
		t.Fatalf("a.apk not replaced: %+v %v", o, err)
	}
}
