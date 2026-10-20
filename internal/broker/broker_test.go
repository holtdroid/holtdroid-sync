package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/holtdroid/holtdroid-sync/internal/awssso"
)

func TestCredentials(t *testing.T) {
	expires := time.Date(2026, 10, 17, 13, 0, 0, 0, time.UTC)
	loggedIn := true
	srv := &Server{
		Token:  "s3cret",
		Broker: true,
		Status: func() any { return map[string]string{"state": "ready"} },
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			if !loggedIn {
				return aws.Credentials{}, awssso.ErrLoginRequired
			}
			return aws.Credentials{AccessKeyID: "ASIA", SecretAccessKey: "sk", SessionToken: "st", CanExpire: true, Expires: expires}, nil
		}),
	}
	h := srv.Handler()
	get := func(path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			r.Header.Set("Authorization", token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	tests := []struct {
		name, path, token string
		want              int
	}{
		{"health is open", "/healthz", "", http.StatusOK},
		{"credentials need the token", "/credentials", "", http.StatusUnauthorized},
		{"wrong token", "/credentials", "nope", http.StatusUnauthorized},
		{"status needs the token", "/status", "", http.StatusUnauthorized},
		{"status", "/status", "s3cret", http.StatusOK},
	}
	for _, tt := range tests {
		if got := get(tt.path, tt.token).Code; got != tt.want {
			t.Errorf("%s: %d, want %d", tt.name, got, tt.want)
		}
	}

	w := get("/credentials", "s3cret")
	var body map[string]string
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %v", w.Code, err)
	}
	if body["AccessKeyId"] != "ASIA" || body["Token"] != "st" || body["Expiration"] != "2026-10-17T13:00:00Z" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("body %v", body)
	}

	loggedIn = false
	if got := get("/credentials", "s3cret").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("without a session: %d", got)
	}

	srv.Broker = false
	r := httptest.NewRequest(http.MethodGet, "/credentials", nil)
	r.Header.Set("Authorization", "s3cret")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("broker disabled: %d", rec.Code)
	}

	srv.Token = ""
	r = httptest.NewRequest(http.MethodGet, "/status", nil)
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status without a configured token must stay closed: %d", rec.Code)
	}
}
