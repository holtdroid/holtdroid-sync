// Package broker serves temporary AWS credentials in the ECS container
// credentials format, so Holtdroid (or any AWS SDK via
// AWS_CONTAINER_CREDENTIALS_FULL_URI) can read the bucket directly.
package broker

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/holtdroid/holtdroid-sync/internal/awssso"
)

type Server struct {
	Credentials aws.CredentialsProvider
	Token       string
	Status      func() any
	Broker      bool
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.Handle("GET /status", s.auth(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, s.Status()) }))
	if s.Broker {
		mux.Handle("GET /credentials", s.auth(s.credentials))
	}
	return mux
}

func (s *Server) auth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(s.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	})
}

func (s *Server) credentials(w http.ResponseWriter, r *http.Request) {
	creds, err := s.Credentials.Retrieve(r.Context())
	if errors.Is(err, awssso.ErrLoginRequired) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "aws login required"})
		return
	}
	if err != nil {
		slog.Warn("cannot get aws credentials", "component", "broker", "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "cannot get aws credentials"})
		return
	}
	body := map[string]any{
		"AccessKeyId":     creds.AccessKeyID,
		"SecretAccessKey": creds.SecretAccessKey,
		"Token":           creds.SessionToken,
	}
	if creds.CanExpire {
		body["Expiration"] = creds.Expires.UTC().Format(time.RFC3339)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
