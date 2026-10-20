package announce

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/nicholas-fedor/shoutrrr"
	"github.com/nicholas-fedor/shoutrrr/pkg/router"
	"github.com/nicholas-fedor/shoutrrr/pkg/types"

	"github.com/holtdroid/holtdroid-sync/internal/layout"
	"github.com/holtdroid/holtdroid-sync/internal/secrets"
)

const maxListed = 10

type Config struct {
	URLs      []secrets.Ref
	PortalURL string
	Source    string
	MaxAge    time.Duration
}

func (c Config) Validate() error {
	if c.MaxAge < 0 {
		return errors.New("maxAge must not be negative")
	}
	if c.PortalURL != "" {
		if u, err := url.Parse(c.PortalURL); err != nil || u.Host == "" {
			return fmt.Errorf("portalURL must be a URL like https://holtdroid.example.com, got %q", c.PortalURL)
		}
	}
	return nil
}

type Func func(context.Context, []layout.Build)

func FromConfig(ctx context.Context, reg *secrets.Registry, cfg Config, where string) (Func, error) {
	if len(cfg.URLs) == 0 {
		return func(context.Context, []layout.Build) {}, nil
	}
	urls := make([]string, 0, len(cfg.URLs))
	for _, ref := range cfg.URLs {
		v, err := reg.Resolve(ctx, ref)
		if err != nil {
			return nil, err
		}
		urls = append(urls, v)
	}
	n, err := New(urls, cfg.PortalURL, cfg.Source, where, cfg.MaxAge)
	if err != nil {
		return nil, err
	}
	return n.Announce, nil
}

type Announcer struct {
	sender *router.ServiceRouter
	portal string
	source string
	where  string
	maxAge time.Duration
	now    func() time.Time
	log    *slog.Logger
}

func New(urls []string, portalURL, source, where string, maxAge time.Duration) (*Announcer, error) {
	sender, err := shoutrrr.CreateSenderWithOptions(types.SenderOptions{}, urls...)
	if err != nil {
		return nil, errors.New("invalid shoutrrr URL")
	}
	return &Announcer{
		sender: sender, portal: strings.TrimRight(portalURL, "/"), source: source, where: where, maxAge: maxAge,
		now: time.Now, log: slog.Default().With("component", "notify"),
	}, nil
}

func (n *Announcer) Announce(ctx context.Context, builds []layout.Build) {
	fresh := slices.DeleteFunc(slices.Clone(builds), func(b layout.Build) bool {
		return n.maxAge > 0 && n.now().Sub(b.Modified) > n.maxAge
	})
	if len(fresh) == 0 {
		return
	}
	slices.SortFunc(fresh, func(a, b layout.Build) int { return cmp.Or(b.Modified.Compare(a.Modified), cmp.Compare(a.Key, b.Key)) })

	head := "📦 New build in " + n.where
	if len(fresh) > 1 {
		head = fmt.Sprintf("📦 %d new builds in %s", len(fresh), n.where)
	}
	lines := []string{head}
	for _, b := range fresh[:min(len(fresh), maxListed)] {
		lines = append(lines, n.describe(b)...)
	}
	if len(fresh) > maxListed {
		lines = append(lines, fmt.Sprintf("…and %d more", len(fresh)-maxListed))
	}
	for _, err := range n.sender.SendContext(ctx, strings.Join(lines, "\n"), nil) {
		if err != nil {
			n.log.Warn("cannot announce new builds", "err", err)
		}
	}
}

func (n *Announcer) describe(b layout.Build) []string {
	title := b.Key[strings.LastIndex(b.Key, "/")+1:]
	if a := b.App; a != nil {
		title = strings.TrimSpace(fmt.Sprintf("%s %s", cmp.Or(a.Label, title), a.VersionName))
		if a.VersionCode != 0 {
			title += fmt.Sprintf(" (%d)", a.VersionCode)
		}
		title += " · " + a.Package
	}
	out := []string{"• " + title, "  " + b.Key + " · " + layout.FormatSize(b.Size)}
	if n.portal != "" {
		out = append(out, "  ⚡ Quick launch: "+layout.LaunchURL(n.portal, n.source, b.Key, true), "  Launch: "+layout.LaunchURL(n.portal, n.source, b.Key, false))
	}
	if b.Link != "" {
		out = append(out, "  🔗 "+b.Link)
	}
	return out
}
