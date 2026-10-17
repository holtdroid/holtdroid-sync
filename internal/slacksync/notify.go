package slacksync

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/slack-go/slack"

	"github.com/kriakiku/holtdroid-sync/internal/layout"
)

func (s *Syncer) announce(ctx context.Context, ch string, m message, builds []build) {
	if !s.cfg.React.Enabled && !s.cfg.Reply.Enabled {
		return
	}
	if s.reacted(m) {
		return
	}
	if s.cfg.React.Enabled {
		err := s.api.AddReactionContext(ctx, s.cfg.React.Emoji, slack.NewRefToMessage(ch, m.Timestamp))
		if err != nil && err.Error() != "already_reacted" {
			s.log.Warn("cannot react; the token needs reactions:write and the emoji must exist", "channel", ch, "ts", m.Timestamp, "emoji", s.cfg.React.Emoji, "err", err)
		}
	}
	if !s.cfg.Reply.Enabled {
		return
	}
	thread := m.ThreadTimestamp
	if thread == "" {
		thread = m.Timestamp
	}
	for _, b := range builds {
		_, _, err := s.api.PostMessageContext(ctx, ch,
			slack.MsgOptionText(s.replyText(b), false),
			slack.MsgOptionBlocks(s.replyBlocks(b)...),
			slack.MsgOptionTS(thread),
			slack.MsgOptionDisableLinkUnfurl(),
		)
		if err != nil {
			s.log.Warn("cannot reply; the token needs chat:write", "channel", ch, "ts", m.Timestamp, "err", err)
		}
	}
}

func (s *Syncer) reacted(m message) bool {
	if !s.cfg.React.Enabled || s.botID == "" {
		return false
	}
	return slices.ContainsFunc(m.Reactions, func(r slack.ItemReaction) bool {
		return r.Name == s.cfg.React.Emoji && slices.Contains(r.Users, s.botID)
	})
}

func (s *Syncer) replyText(b build) string {
	if a := b.app; a != nil {
		return fmt.Sprintf("New build: %s %s · %s", cmp.Or(a.Label, b.name), a.VersionName, a.Package)
	}
	return "New build: " + b.name
}

func (s *Syncer) replyBlocks(b build) []slack.Block {
	mrkdwn := func(text string) *slack.TextBlockObject {
		return slack.NewTextBlockObject(slack.MarkdownType, text, false, false)
	}
	plain := func(text string) *slack.TextBlockObject {
		return slack.NewTextBlockObject(slack.PlainTextType, text, true, false)
	}

	title := "*" + escape(b.name) + "*"
	fields := []*slack.TextBlockObject{mrkdwn("*Size*\n" + layout.FormatSize(b.size))}
	if a := b.app; a != nil {
		title = "*" + escape(cmp.Or(a.Label, b.name)) + "*\n`" + a.Package + "`"
		version := escape(a.VersionName)
		if a.VersionCode != 0 {
			version = strings.TrimSpace(fmt.Sprintf("%s (%d)", version, a.VersionCode))
		}
		abis := "any"
		if len(a.ABIs) > 0 {
			abis = strings.Join(a.ABIs, ", ")
		}
		fields = []*slack.TextBlockObject{
			mrkdwn("*Version*\n" + cmp.Or(version, "—")),
			mrkdwn("*Size*\n" + layout.FormatSize(b.size)),
			mrkdwn("*Android*\n" + sdkRange(a.MinSDK, a.TargetSDK)),
			mrkdwn("*ABIs*\n" + abis),
		}
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(mrkdwn(":package: "+title), fields, nil),
		slack.NewContextBlock("", mrkdwn(escape(b.name))),
	}
	if launch := layout.LaunchURL(s.cfg.Reply.PortalURL, s.cfg.Reply.Source, b.key, false); launch != "" {
		blocks = append(blocks, slack.NewActionBlock("launch",
			slack.NewButtonBlockElement("quick-launch", "", plain(":zap: Quick launch")).WithURL(layout.LaunchURL(s.cfg.Reply.PortalURL, s.cfg.Reply.Source, b.key, true)).WithStyle(slack.StylePrimary),
			slack.NewButtonBlockElement("launch", "", plain("Launch…")).WithURL(launch),
		))
	}
	return blocks
}

func sdkRange(minSDK, targetSDK int32) string {
	switch {
	case minSDK == 0 && targetSDK == 0:
		return "—"
	case targetSDK == 0:
		return fmt.Sprintf("API %d+", minSDK)
	default:
		return fmt.Sprintf("API %d+, target %d", minSDK, targetSDK)
	}
}

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
