package awssso

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sso"
	"github.com/aws/aws-sdk-go-v2/service/ssooidc"
)

type fakeIdentityCenter struct {
	t   *testing.T
	srv *httptest.Server

	mu            sync.Mutex
	pendingPolls  int
	approve       bool
	codes         int
	refreshOK     bool
	refreshCalls  int
	roleCalls     int
	expiresIn     int32
	validTokens   map[string]bool
	issuedRefresh bool
}

func newFake(t *testing.T) *fakeIdentityCenter {
	f := &fakeIdentityCenter{t: t, approve: true, refreshOK: true, expiresIn: 600, validTokens: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func writeErr(w http.ResponseWriter, status int, kind string) {
	w.Header().Set("X-Amzn-ErrorType", kind)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":"`+kind+`"}`)
}

func (f *fakeIdentityCenter) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var in map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&in)
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/client/register":
		_ = json.NewEncoder(w).Encode(map[string]any{"clientId": "cid", "clientSecret": "csecret", "clientSecretExpiresAt": time.Now().Add(90 * 24 * time.Hour).Unix()})
	case "/device_authorization":
		f.codes++
		code := "CODE-" + string(rune('A'+f.codes-1))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"deviceCode": "dev-" + code, "userCode": code, "expiresIn": f.expiresIn, "interval": 1,
			"verificationUri": "https://device.sso.example/", "verificationUriComplete": "https://device.sso.example/?user_code=" + code,
		})
	case "/token":
		switch in["grantType"] {
		case "refresh_token":
			f.refreshCalls++
			if !f.refreshOK {
				writeErr(w, http.StatusBadRequest, "InvalidGrantException")
				return
			}
			f.validTokens["refreshed"] = true
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "refreshed", "expiresIn": 3600, "refreshToken": "rt-2"})
		default:
			if f.pendingPolls > 0 || !f.approve {
				f.pendingPolls--
				writeErr(w, http.StatusBadRequest, "AuthorizationPendingException")
				return
			}
			f.validTokens["device"] = true
			f.issuedRefresh = true
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "device", "expiresIn": 3600, "refreshToken": "rt-1"})
		}
	case "/federation/credentials":
		f.roleCalls++
		if !f.validTokens[r.Header.Get("X-Amz-Sso_bearer_token")] {
			writeErr(w, http.StatusUnauthorized, "UnauthorizedException")
			return
		}
		if r.URL.Query().Get("account_id") != "123456789012" || r.URL.Query().Get("role_name") != "ApkReader" {
			f.t.Errorf("role request %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"roleCredentials": map[string]any{
			"accessKeyId": "ASIA", "secretAccessKey": "secret", "sessionToken": "session",
			"expiration": time.Now().Add(time.Hour).UnixMilli(),
		}})
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) sleep(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

type recorder struct {
	mu   sync.Mutex
	seen []Status
}

func (r *recorder) notify(_ context.Context, st Status) {
	r.mu.Lock()
	r.seen = append(r.seen, st)
	r.mu.Unlock()
}

func (r *recorder) states() []Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Status(nil), r.seen...)
}

func newSession(t *testing.T, f *fakeIdentityCenter, cache string, rec *recorder) (*Session, *clock) {
	t.Helper()
	cfg := Config{StartURL: "https://acme.awsapps.com/start", Region: "eu-west-1", AccountID: "123456789012", RoleName: "ApkReader", CacheFile: cache}
	s := New(cfg, rec.notify, 0)
	s.oidc = ssooidc.New(ssooidc.Options{Region: cfg.Region, BaseEndpoint: aws.String(f.srv.URL), HTTPClient: f.srv.Client()})
	s.portal = sso.New(sso.Options{Region: cfg.Region, BaseEndpoint: aws.String(f.srv.URL), HTTPClient: f.srv.Client()})
	s.log = slog.New(slog.DiscardHandler)
	c := &clock{t: time.Now()}
	s.now, s.sleep = c.now, c.sleep
	return s, c
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDeviceLoginThenRefresh(t *testing.T) {
	f := newFake(t)
	f.pendingPolls = 2
	cache := filepath.Join(t.TempDir(), "sso", "tokens.json")
	rec := &recorder{}
	s, c := newSession(t, f, cache, rec)
	ctx := t.Context()

	if _, err := s.Retrieve(ctx); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("fresh session: %v", err)
	}
	go s.Run(ctx)
	waitFor(t, func() bool { return s.Status().State == StateReady })

	states := rec.states()
	if len(states) != 2 || states[0].State != StateWaiting || states[0].Login.UserCode != "CODE-A" ||
		!strings.Contains(states[0].Login.URL, "user_code=CODE-A") || states[1].State != StateReady {
		t.Fatalf("notifications %+v", states)
	}
	creds, err := s.Retrieve(ctx)
	if err != nil || creds.AccessKeyID != "ASIA" || creds.SessionToken != "session" || !creds.CanExpire {
		t.Fatalf("creds %+v, %v", creds, err)
	}

	raw, err := os.ReadFile(cache)
	if err != nil || !strings.Contains(string(raw), "rt-1") {
		t.Fatalf("token cache %s, %v", raw, err)
	}
	if info, _ := os.Stat(cache); info.Mode().Perm() != 0o600 {
		t.Fatalf("token cache mode %v", info.Mode())
	}

	_ = c.sleep(ctx, 2*time.Hour)
	if _, err := s.Retrieve(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if f.refreshCalls != 1 || f.codes != 1 {
		t.Fatalf("refresh calls %d, device codes %d", f.refreshCalls, f.codes)
	}

	restarted, _ := newSession(t, f, cache, &recorder{})
	restarted.now = c.now
	if restarted.Status().State != StateReady {
		t.Fatal("a restart must reuse the cached session")
	}
}

func TestExpiredRefreshNeedsLogin(t *testing.T) {
	f := newFake(t)
	f.refreshOK = false
	cache := filepath.Join(t.TempDir(), "tokens.json")
	s, c := newSession(t, f, cache, &recorder{})
	s.tok = tokens{ClientID: "cid", ClientSecret: "csecret", ClientExpiresAt: c.now().Add(time.Hour * 24), AccessToken: "old", AccessExpiresAt: c.now().Add(-time.Minute), RefreshToken: "rt-old"}

	if _, err := s.Retrieve(context.Background()); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("err = %v", err)
	}
	if s.tok.RefreshToken != "" || s.Status().State != StateLogin {
		t.Fatalf("a rejected refresh token must be dropped: %+v", s.Status())
	}
}

func TestRevokedSessionNeedsLogin(t *testing.T) {
	f := newFake(t)
	s, c := newSession(t, f, "", &recorder{})
	s.tok = tokens{ClientID: "cid", ClientSecret: "csecret", ClientExpiresAt: c.now().Add(24 * time.Hour), AccessToken: "revoked", AccessExpiresAt: c.now().Add(time.Hour)}
	if _, err := s.Retrieve(context.Background()); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("err = %v", err)
	}
	if f.roleCalls != 1 {
		t.Fatalf("role calls %d", f.roleCalls)
	}
}

func TestExpiredCodeStartsANewOne(t *testing.T) {
	f := newFake(t)
	f.approve = false
	f.expiresIn = 3
	rec := &recorder{}
	s, _ := newSession(t, f, "", rec)
	ctx := t.Context()
	go s.Run(ctx)

	waitFor(t, func() bool { return len(rec.states()) >= 2 })
	f.mu.Lock()
	f.approve = true
	f.mu.Unlock()
	waitFor(t, func() bool { return s.Status().State == StateReady })
	states := rec.states()
	if states[0].Login.UserCode == states[1].Login.UserCode {
		t.Fatalf("an expired code must be replaced: %+v", states)
	}
}

func TestRemindersArePaced(t *testing.T) {
	f := newFake(t)
	f.approve = false
	f.expiresIn = 3
	rec := &recorder{}
	s, c := newSession(t, f, "", rec)
	s.pause = time.Hour
	start := c.now()
	go s.Run(t.Context())

	waitFor(t, func() bool { return len(rec.states()) >= 2 })
	second := rec.states()[1].Login
	if gap := second.ExpiresAt.Add(-3 * time.Second).Sub(start); gap < time.Hour {
		t.Fatalf("second code after %v, want at least an hour", gap)
	}
}
