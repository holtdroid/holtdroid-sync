package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestRegistry(t *testing.T) {
	t.Setenv("HOLTDROID_SYNC_TEST_TOKEN", "from-env")
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		ref     Ref
		want    string
		wantErr error
	}{
		{ref: "env:HOLTDROID_SYNC_TEST_TOKEN", want: "from-env"},
		{ref: Ref("file:" + file), want: "from-file"},
		{ref: "xoxb-plain-token", wantErr: ErrUnknownScheme},
		{ref: "vault:secret/x#y", wantErr: ErrUnknownScheme},
		{ref: "env:", wantErr: ErrUnknownScheme},
	}
	r := NewRegistry()
	for _, tt := range tests {
		t.Run(string(tt.ref), func(t *testing.T) {
			got, err := r.Resolve(context.Background(), tt.ref)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("got %q, %v; want %q", got, err, tt.want)
			}
		})
	}

	if _, err := r.Resolve(context.Background(), "env:HOLTDROID_SYNC_TEST_MISSING"); err == nil {
		t.Fatal("expected error for missing env")
	}
}

func TestOpenBaoToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "root" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.URL.Path != "/v1/secret/data/holtdroid" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"slack": "xoxb-1"}}})
	}))
	defer srv.Close()

	t.Setenv("BAO_TOKEN", "root")
	base := NewRegistry()
	base.Register("openbao", NewOpenBao(OpenBaoConfig{Address: srv.URL, Token: "env:BAO_TOKEN"}, base, srv.Client()))

	got, err := base.Resolve(context.Background(), "openbao:secret/holtdroid#slack")
	if err != nil || got != "xoxb-1" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := base.Resolve(context.Background(), "openbao:secret/holtdroid#missing"); err == nil {
		t.Fatal("expected missing field error")
	}
	if _, err := base.Resolve(context.Background(), "openbao:secret/other#slack"); err == nil {
		t.Fatal("expected not found error")
	}
}

func TestOpenBaoKubernetesAuth(t *testing.T) {
	jwt := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(jwt, []byte("sa-jwt"), 0o600); err != nil {
		t.Fatal(err)
	}
	logins := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/kubernetes/login":
			logins++
			var in map[string]string
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["role"] != "holtdroid" || in["jwt"] != "sa-jwt" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "k8s-token"}})
		case "/v1/kv/data/app":
			if r.Header.Get("X-Vault-Token") != "k8s-token" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"k": "v"}}})
		}
	}))
	defer srv.Close()

	base := NewRegistry()
	bao := NewOpenBao(OpenBaoConfig{Address: srv.URL, Kubernetes: OpenBaoKubernetesAuth{Role: "holtdroid", TokenPath: jwt}}, base, srv.Client())
	for range 2 {
		got, err := bao.Resolve(context.Background(), "kv/app#k")
		if err != nil || got != "v" {
			t.Fatalf("got %q, %v", got, err)
		}
	}
	if logins != 1 {
		t.Fatalf("logins = %d, want 1", logins)
	}
}
