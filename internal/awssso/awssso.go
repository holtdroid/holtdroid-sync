// Package awssso signs in to AWS IAM Identity Center with the OAuth device
// authorization flow, keeps the session alive with refresh tokens and turns
// it into role credentials. Nobody needs the AWS CLI: when the session ends,
// a person approves a fresh code in the browser.
package awssso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sso"
	ssotypes "github.com/aws/aws-sdk-go-v2/service/sso/types"
	"github.com/aws/aws-sdk-go-v2/service/ssooidc"
	oidctypes "github.com/aws/aws-sdk-go-v2/service/ssooidc/types"
)

var ErrLoginRequired = errors.New("aws sso: login required")

type Config struct {
	StartURL  string
	Region    string
	AccountID string
	RoleName  string
	CacheFile string
}

func (c Config) Validate() error {
	if c.StartURL == "" || c.Region == "" || c.AccountID == "" || c.RoleName == "" {
		return errors.New("sso: startURL, region, accountID and roleName are required")
	}
	return nil
}

type State string

const (
	StateReady   State = "ready"
	StateWaiting State = "waiting_for_approval"
	StateLogin   State = "login_required"
)

type Login struct {
	URL       string    `json:"url"`
	UserCode  string    `json:"userCode"`
	ExpiresAt time.Time `json:"expiresAt"`
}

type Status struct {
	State State  `json:"state"`
	Login *Login `json:"login,omitempty"`
}

type Notifier func(ctx context.Context, st Status)

type tokens struct {
	ClientID        string    `json:"clientId"`
	ClientSecret    string    `json:"clientSecret"`
	ClientExpiresAt time.Time `json:"clientExpiresAt"`
	AccessToken     string    `json:"accessToken"`
	AccessExpiresAt time.Time `json:"accessExpiresAt"`
	RefreshToken    string    `json:"refreshToken,omitempty"`
}

type Session struct {
	cfg    Config
	oidc   *ssooidc.Client
	portal *sso.Client
	notify Notifier
	pause  time.Duration
	log    *slog.Logger
	now    func() time.Time
	sleep  func(ctx context.Context, d time.Duration) error

	mu      sync.Mutex
	tok     tokens
	pending *Login
	wake    chan struct{}
}

// New creates a session. pause is the least time between two device codes
// while nobody approves, so reminders don't flood the notification channel.
func New(cfg Config, notify Notifier, pause time.Duration) *Session {
	if notify == nil {
		notify = func(context.Context, Status) {}
	}
	s := &Session{
		cfg:    cfg,
		oidc:   ssooidc.New(ssooidc.Options{Region: cfg.Region}),
		portal: sso.New(sso.Options{Region: cfg.Region}),
		notify: notify,
		pause:  pause,
		log:    slog.Default().With("component", "awssso"),
		now:    time.Now,
		sleep:  sleepCtx,
		wake:   make(chan struct{}, 1),
	}
	if err := s.load(); err != nil {
		s.log.Warn("ignoring the token cache", "file", cfg.CacheFile, "err", err)
	}
	return s
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (s *Session) Provider() aws.CredentialsProvider { return aws.NewCredentialsCache(s) }

func (s *Session) Retrieve(ctx context.Context) (aws.Credentials, error) {
	creds, err := s.retrieve(ctx)
	var unauthorized *ssotypes.UnauthorizedException
	if errors.As(err, &unauthorized) {
		s.mu.Lock()
		s.tok.AccessToken, s.tok.AccessExpiresAt = "", time.Time{}
		s.mu.Unlock()
		creds, err = s.retrieve(ctx)
	}
	if errors.As(err, &unauthorized) {
		s.mu.Lock()
		s.tok.AccessToken, s.tok.RefreshToken = "", ""
		s.save()
		s.mu.Unlock()
		s.signal()
		return aws.Credentials{}, ErrLoginRequired
	}
	return creds, err
}

func (s *Session) retrieve(ctx context.Context) (aws.Credentials, error) {
	token, err := s.accessToken(ctx)
	if err != nil {
		return aws.Credentials{}, err
	}
	out, err := s.portal.GetRoleCredentials(ctx, &sso.GetRoleCredentialsInput{
		AccessToken: aws.String(token),
		AccountId:   aws.String(s.cfg.AccountID),
		RoleName:    aws.String(s.cfg.RoleName),
	})
	if err != nil {
		return aws.Credentials{}, fmt.Errorf("aws sso: role credentials: %w", err)
	}
	rc := out.RoleCredentials
	return aws.Credentials{
		AccessKeyID:     aws.ToString(rc.AccessKeyId),
		SecretAccessKey: aws.ToString(rc.SecretAccessKey),
		SessionToken:    aws.ToString(rc.SessionToken),
		Source:          "holtdroid-s3-sync sso",
		CanExpire:       true,
		Expires:         time.UnixMilli(rc.Expiration),
	}, nil
}

func (s *Session) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.pending != nil:
		login := *s.pending
		return Status{State: StateWaiting, Login: &login}
	case s.usable():
		return Status{State: StateReady}
	default:
		return Status{State: StateLogin}
	}
}

func (s *Session) usable() bool {
	return s.tok.AccessToken != "" && s.now().Before(s.tok.AccessExpiresAt.Add(-time.Minute)) ||
		s.tok.RefreshToken != "" && s.now().Before(s.tok.ClientExpiresAt)
}

func (s *Session) accessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tok.AccessToken != "" && s.now().Before(s.tok.AccessExpiresAt.Add(-time.Minute)) {
		return s.tok.AccessToken, nil
	}
	if s.tok.RefreshToken != "" && s.now().Before(s.tok.ClientExpiresAt) {
		out, err := s.oidc.CreateToken(ctx, &ssooidc.CreateTokenInput{
			ClientId:     aws.String(s.tok.ClientID),
			ClientSecret: aws.String(s.tok.ClientSecret),
			GrantType:    aws.String("refresh_token"),
			RefreshToken: aws.String(s.tok.RefreshToken),
		})
		if err == nil {
			s.store(out)
			s.log.Info("session refreshed", "until", s.tok.AccessExpiresAt)
			return s.tok.AccessToken, nil
		}
		s.log.Warn("cannot refresh the session, a new login is needed", "err", err)
		s.tok.RefreshToken = ""
		s.save()
	}
	s.signal()
	return "", ErrLoginRequired
}

func (s *Session) store(out *ssooidc.CreateTokenOutput) {
	s.tok.AccessToken = aws.ToString(out.AccessToken)
	s.tok.AccessExpiresAt = s.now().Add(time.Duration(out.ExpiresIn) * time.Second)
	if rt := aws.ToString(out.RefreshToken); rt != "" {
		s.tok.RefreshToken = rt
	}
	s.save()
}

func (s *Session) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run performs device logins whenever Retrieve finds no usable session,
// until ctx ends.
func (s *Session) Run(ctx context.Context) {
	s.mu.Lock()
	need := !s.usable()
	s.mu.Unlock()
	if need {
		s.signal()
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		}
		s.mu.Lock()
		need := !s.usable()
		s.mu.Unlock()
		if need {
			s.login(ctx)
		}
	}
}

func (s *Session) login(ctx context.Context) {
	for ctx.Err() == nil {
		started := s.now()
		err := s.deviceLogin(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		wait := s.pause - s.now().Sub(started)
		if errors.Is(err, errRetryLater) {
			wait = max(wait, time.Minute)
		}
		s.log.Warn("device login did not complete, will ask again", "err", err, "in", max(wait, 0).Round(time.Second))
		if wait > 0 {
			_ = s.sleep(ctx, wait)
		}
	}
}

var errRetryLater = errors.New("retry later")

func (s *Session) deviceLogin(ctx context.Context) error {
	if err := s.registerClient(ctx); err != nil {
		return fmt.Errorf("%w: register client: %w", errRetryLater, err)
	}
	s.mu.Lock()
	clientID, secret := s.tok.ClientID, s.tok.ClientSecret
	s.mu.Unlock()

	start, err := s.oidc.StartDeviceAuthorization(ctx, &ssooidc.StartDeviceAuthorizationInput{
		ClientId: aws.String(clientID), ClientSecret: aws.String(secret), StartUrl: aws.String(s.cfg.StartURL),
	})
	if err != nil {
		return fmt.Errorf("%w: start device authorization: %w", errRetryLater, err)
	}
	login := &Login{
		URL:       aws.ToString(start.VerificationUriComplete),
		UserCode:  aws.ToString(start.UserCode),
		ExpiresAt: s.now().Add(time.Duration(start.ExpiresIn) * time.Second),
	}
	s.mu.Lock()
	s.pending = login
	s.mu.Unlock()
	s.log.Info("AWS login required: open the link and approve the code", "url", login.URL, "code", login.UserCode, "expiresAt", login.ExpiresAt)
	s.notify(ctx, Status{State: StateWaiting, Login: login})

	interval := time.Duration(max(start.Interval, 1)) * time.Second
	for s.now().Before(login.ExpiresAt) {
		if err := s.sleep(ctx, interval); err != nil {
			return err
		}
		out, err := s.oidc.CreateToken(ctx, &ssooidc.CreateTokenInput{
			ClientId:     aws.String(clientID),
			ClientSecret: aws.String(secret),
			GrantType:    aws.String("urn:ietf:params:oauth:grant-type:device_code"),
			DeviceCode:   start.DeviceCode,
		})
		var pending *oidctypes.AuthorizationPendingException
		var slow *oidctypes.SlowDownException
		switch {
		case err == nil:
			s.mu.Lock()
			s.store(out)
			s.pending = nil
			s.mu.Unlock()
			s.log.Info("AWS login approved", "refreshable", out.RefreshToken != nil)
			s.notify(ctx, Status{State: StateReady})
			return nil
		case errors.As(err, &pending):
		case errors.As(err, &slow):
			interval += 5 * time.Second
		default:
			s.clearPending()
			return err
		}
	}
	s.clearPending()
	return errors.New("the code expired before anyone approved it")
}

func (s *Session) clearPending() {
	s.mu.Lock()
	s.pending = nil
	s.mu.Unlock()
}

func (s *Session) registerClient(ctx context.Context) error {
	s.mu.Lock()
	valid := s.tok.ClientID != "" && s.now().Before(s.tok.ClientExpiresAt.Add(-time.Hour))
	s.mu.Unlock()
	if valid {
		return nil
	}
	out, err := s.oidc.RegisterClient(ctx, &ssooidc.RegisterClientInput{
		ClientName: aws.String("holtdroid-s3-sync"),
		ClientType: aws.String("public"),
		Scopes:     []string{"sso:account:access"},
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.tok = tokens{
		ClientID:        aws.ToString(out.ClientId),
		ClientSecret:    aws.ToString(out.ClientSecret),
		ClientExpiresAt: time.Unix(out.ClientSecretExpiresAt, 0),
	}
	s.save()
	s.mu.Unlock()
	return nil
}

func (s *Session) load() error {
	if s.cfg.CacheFile == "" {
		return nil
	}
	raw, err := os.ReadFile(s.cfg.CacheFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, &s.tok)
}

func (s *Session) save() {
	if s.cfg.CacheFile == "" {
		return
	}
	raw, err := json.Marshal(s.tok)
	if err == nil {
		err = writeFileAtomic(s.cfg.CacheFile, raw)
	}
	if err != nil {
		s.log.Warn("cannot write the token cache; a restart will need a new login", "file", s.cfg.CacheFile, "err", err)
	}
}

func writeFileAtomic(name string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(name), ".tokens-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}
