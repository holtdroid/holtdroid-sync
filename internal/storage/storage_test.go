package storage

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"

	"github.com/kriakiku/holtdroid-sync/internal/secrets"
)

func stores(t *testing.T) map[string]Store {
	t.Helper()
	fsStore, err := NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	backend := s3mem.New()
	if err := backend.CreateBucket("apks"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)
	t.Setenv("TEST_S3_KEY", "key")
	t.Setenv("TEST_S3_SECRET", "secret")
	s3Store, err := New(context.Background(), Config{Type: "s3", S3: S3Config{
		Endpoint: srv.URL, Bucket: "apks", Prefix: "/cache/", Region: "us-east-1",
		AccessKey: "env:TEST_S3_KEY", SecretKey: "env:TEST_S3_SECRET", PathStyle: true,
	}}, secrets.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	return map[string]Store{"filesystem": fsStore, "s3": s3Store}
}

func TestStores(t *testing.T) {
	ctx := context.Background()
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			if _, _, err := s.Get(ctx, "a/missing"); !errors.Is(err, ErrNotExist) {
				t.Fatalf("missing get: %v", err)
			}
			for key, body := range map[string]string{"a/app.apk": "apk-bytes", "a/meta.json": "{}", "b/app.apk": "x"} {
				if err := s.Put(ctx, key, strings.NewReader(body), int64(len(body))); err != nil {
					t.Fatalf("put %s: %v", key, err)
				}
			}

			rc, size, err := s.Get(ctx, "a/app.apk")
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(rc)
			_ = rc.Close()
			if string(raw) != "apk-bytes" || size != 9 {
				t.Fatalf("got %q (%d)", raw, size)
			}

			objs, err := s.List(ctx, "a/")
			if err != nil {
				t.Fatal(err)
			}
			var keys []string
			for _, o := range objs {
				keys = append(keys, o.Key)
			}
			slices.Sort(keys)
			if !slices.Equal(keys, []string{"a/app.apk", "a/meta.json"}) {
				t.Fatalf("keys %v", keys)
			}

			if o, err := s.Stat(ctx, "a/app.apk"); err != nil || o.Size != 9 || o.Modified.IsZero() {
				t.Fatalf("stat %+v, %v", o, err)
			}
			if _, err := s.Stat(ctx, "a/missing"); !errors.Is(err, ErrNotExist) {
				t.Fatalf("missing stat: %v", err)
			}

			if err := s.Delete(ctx, "a/app.apk"); err != nil {
				t.Fatal(err)
			}
			if err := s.Delete(ctx, "a/app.apk"); err != nil {
				t.Fatalf("second delete: %v", err)
			}
			if _, _, err := s.Get(ctx, "a/app.apk"); !errors.Is(err, ErrNotExist) {
				t.Fatalf("get after delete: %v", err)
			}
		})
	}
}

func TestFilesystemRejectsEscapes(t *testing.T) {
	s, _ := NewFilesystem(t.TempDir())
	ctx := context.Background()
	if err := s.Put(ctx, "../../etc/x", strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	objs, _ := s.List(ctx, "")
	if len(objs) != 1 || objs[0].Key != "etc/x" {
		t.Fatalf("escape not contained: %+v", objs)
	}
}
