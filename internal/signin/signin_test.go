package signin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/holtdroid/holtdroid-sync/internal/awssso"
)

func TestNotify(t *testing.T) {
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, string(raw))
		mu.Unlock()
	}))
	defer srv.Close()

	n, err := New([]string{"generic://" + strings.TrimPrefix(srv.URL, "http://") + "/hook?disabletls=yes"}, "work bucket")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	n.Notify(ctx, awssso.Status{State: awssso.StateReady})
	n.Notify(ctx, awssso.Status{State: awssso.StateWaiting, Login: &awssso.Login{
		URL: "https://device.sso/?user_code=ABCD", UserCode: "ABCD", ExpiresAt: time.Date(2026, 10, 17, 12, 30, 0, 0, time.UTC),
	}})
	n.Notify(ctx, awssso.Status{State: awssso.StateReady})

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || !strings.Contains(got[0], "https://device.sso/?user_code=ABCD") || !strings.Contains(got[0], "12:30 UTC") || !strings.Contains(got[1], "signed in") {
		t.Fatalf("sent %q", got)
	}
}

func TestInvalidURL(t *testing.T) {
	if _, err := New([]string{"nope://secret-token@x"}, "x"); err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("err = %v", err)
	}
}
