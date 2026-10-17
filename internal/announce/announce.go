package announce

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/nicholas-fedor/shoutrrr"
	"github.com/nicholas-fedor/shoutrrr/pkg/router"
	"github.com/nicholas-fedor/shoutrrr/pkg/types"

	"github.com/kriakiku/holtdroid-sync/internal/layout"
)

const maxListed = 10

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
		return nil, errors.New("notify.builds.urls: invalid shoutrrr URL")
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
