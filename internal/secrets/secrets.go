package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Ref is a secret reference such as "env:SLACK_BOT_TOKEN". Its String form
// never reveals the resolved value.
type Ref string

func (r Ref) Scheme() string {
	scheme, _, _ := strings.Cut(string(r), ":")
	return scheme
}

func (r Ref) IsZero() bool { return r == "" }

type Resolver interface {
	Resolve(ctx context.Context, ref string) (string, error)
}

type ResolverFunc func(ctx context.Context, ref string) (string, error)

func (f ResolverFunc) Resolve(ctx context.Context, ref string) (string, error) { return f(ctx, ref) }

var ErrUnknownScheme = errors.New("unknown secret scheme")

type Registry struct {
	mu        sync.RWMutex
	resolvers map[string]Resolver
}

func NewRegistry() *Registry {
	r := &Registry{resolvers: map[string]Resolver{}}
	r.Register("env", ResolverFunc(resolveEnv))
	r.Register("file", ResolverFunc(resolveFile))
	return r
}

func (r *Registry) Register(scheme string, resolver Resolver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolvers[scheme] = resolver
}

func (r *Registry) Resolve(ctx context.Context, ref Ref) (string, error) {
	scheme, rest, ok := strings.Cut(string(ref), ":")
	if !ok || rest == "" {
		return "", fmt.Errorf("secret reference must look like scheme:value: %w", ErrUnknownScheme)
	}
	r.mu.RLock()
	resolver, found := r.resolvers[scheme]
	r.mu.RUnlock()
	if !found {
		return "", fmt.Errorf("%q: %w", scheme, ErrUnknownScheme)
	}
	value, err := resolver.Resolve(ctx, rest)
	if err != nil {
		return "", fmt.Errorf("resolve %s secret %q: %w", scheme, rest, err)
	}
	return value, nil
}

func resolveEnv(_ context.Context, name string) (string, error) {
	value, ok := os.LookupEnv(name)
	if !ok {
		return "", errors.New("environment variable not set")
	}
	return value, nil
}

func resolveFile(_ context.Context, path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(raw), "\r\n"), nil
}
