package slacksync

import (
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var entity = regexp.MustCompile(`<([^<>|]+)(?:\|([^<>]*))?>`)

func plainText(text string, userName func(id string) string) string {
	text = entity.ReplaceAllStringFunc(text, func(m string) string {
		parts := entity.FindStringSubmatch(m)
		target, label := parts[1], parts[2]
		switch {
		case strings.HasPrefix(target, "@"):
			if label != "" {
				return "@" + strings.TrimPrefix(label, "@")
			}
			return "@" + userName(target[1:])
		case strings.HasPrefix(target, "#"):
			if label != "" {
				return "#" + label
			}
			return target
		case strings.HasPrefix(target, "!"):
			if label != "" {
				return label
			}
			name, _, _ := strings.Cut(target[1:], "^")
			return "@" + name
		case label != "":
			return label
		default:
			return target
		}
	})
	return strings.TrimSpace(html.UnescapeString(text))
}

func parseTS(ts string) time.Time {
	sec, frac, _ := strings.Cut(ts, ".")
	s, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return time.Time{}
	}
	us, _ := strconv.ParseInt((frac + "000000")[:6], 10, 64)
	return time.Unix(s, us*1000).UTC()
}

func formatTS(t time.Time) string {
	return strconv.FormatInt(t.Unix(), 10) + ".000000"
}

func messageLink(team, channel, ts, threadTS string) string {
	u, err := url.Parse(team)
	if err != nil || u.Host == "" {
		return ""
	}
	link := url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/archives/" + channel + "/p" + strings.Replace(ts, ".", "", 1)}
	if threadTS != "" && threadTS != ts {
		link.RawQuery = url.Values{"thread_ts": {threadTS}, "cid": {channel}}.Encode()
	}
	return link.String()
}
