// Package signin sends AWS login requests through shoutrrr, so any of its
// services (Slack, Telegram, Discord, Matrix, ntfy, email, webhooks, …) works.
package signin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/nicholas-fedor/shoutrrr"
	"github.com/nicholas-fedor/shoutrrr/pkg/router"
	"github.com/nicholas-fedor/shoutrrr/pkg/types"

	"github.com/kriakiku/holtdroid-sync/internal/awssso"
)

type Notifier struct {
	sender *router.ServiceRouter
	label  string
	log    *slog.Logger

	mu      sync.Mutex
	pending bool
}

func New(urls []string, label string) (*Notifier, error) {
	sender, err := shoutrrr.CreateSenderWithOptions(types.SenderOptions{}, urls...)
	if err != nil {
		return nil, errors.New("notify.urls: invalid shoutrrr URL")
	}
	return &Notifier{sender: sender, label: label, log: slog.Default().With("component", "notify")}, nil
}

func (n *Notifier) Notify(ctx context.Context, st awssso.Status) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var text string
	switch {
	case st.State == awssso.StateWaiting && st.Login != nil:
		text = fmt.Sprintf("🔑 %s needs an AWS login. Open %s and check that the code is %s. The link works until %s UTC.",
			n.label, st.Login.URL, st.Login.UserCode, st.Login.ExpiresAt.UTC().Format("15:04"))
		n.pending = true
	case st.State == awssso.StateReady && n.pending:
		text = fmt.Sprintf("✅ %s is signed in to AWS again.", n.label)
		n.pending = false
	default:
		return
	}
	for _, err := range n.sender.SendContext(ctx, text, nil) {
		if err != nil {
			n.log.Warn("cannot send a notification; the login link is in the log", "err", err)
		}
	}
}
