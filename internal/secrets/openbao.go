package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type OpenBaoConfig struct {
	Address    string
	Token      Ref
	Kubernetes OpenBaoKubernetesAuth
}

type OpenBaoKubernetesAuth struct {
	Mount     string
	Role      string
	TokenPath string
}

// OpenBao resolves "mount/path#field" against a KV v2 engine. It also works
// with HashiCorp Vault, which shares the API.
type OpenBao struct {
	cfg    OpenBaoConfig
	base   *Registry
	client *http.Client

	mu    sync.Mutex
	token string
}

func NewOpenBao(cfg OpenBaoConfig, base *Registry, client *http.Client) *OpenBao {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Kubernetes.Mount == "" {
		cfg.Kubernetes.Mount = "kubernetes"
	}
	if cfg.Kubernetes.TokenPath == "" {
		cfg.Kubernetes.TokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	}
	return &OpenBao{cfg: cfg, base: base, client: client}
}

func (o *OpenBao) Resolve(ctx context.Context, ref string) (string, error) {
	path, field, ok := strings.Cut(ref, "#")
	if !ok || field == "" {
		return "", errors.New("expected mount/path#field")
	}
	mount, rest, ok := strings.Cut(path, "/")
	if !ok || rest == "" {
		return "", errors.New("expected mount/path#field")
	}

	token, err := o.login(ctx)
	if err != nil {
		return "", err
	}

	var body struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := o.do(ctx, http.MethodGet, "/v1/"+mount+"/data/"+rest, token, nil, &body); err != nil {
		return "", err
	}
	value, ok := body.Data.Data[field]
	if !ok {
		return "", fmt.Errorf("field %q not found", field)
	}
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("field %q is not a string", field)
	}
	return s, nil
}

func (o *OpenBao) login(ctx context.Context) (string, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.token != "" {
		return o.token, nil
	}

	if !o.cfg.Token.IsZero() {
		token, err := o.base.Resolve(ctx, o.cfg.Token)
		if err != nil {
			return "", err
		}
		o.token = token
		return token, nil
	}

	if o.cfg.Kubernetes.Role == "" {
		return "", errors.New("openbao: set either token or kubernetes.role")
	}
	jwt, err := os.ReadFile(o.cfg.Kubernetes.TokenPath)
	if err != nil {
		return "", fmt.Errorf("read service account token: %w", err)
	}
	var body struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	payload := map[string]string{"role": o.cfg.Kubernetes.Role, "jwt": strings.TrimSpace(string(jwt))}
	if err := o.do(ctx, http.MethodPost, "/v1/auth/"+o.cfg.Kubernetes.Mount+"/login", "", payload, &body); err != nil {
		return "", err
	}
	o.token = body.Auth.ClientToken
	return o.token, nil
}

func (o *OpenBao) do(ctx context.Context, method, path, token string, in, out any) error {
	var reader *bytes.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(o.cfg.Address, "/")+path, reader)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("openbao %s %s: %s", method, path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
