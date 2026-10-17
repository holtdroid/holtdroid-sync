package slacksync

import (
	"testing"
	"time"
)

func TestPlainText(t *testing.T) {
	users := func(id string) string { return map[string]string{"U1": "alice"}[id] }
	tests := []struct{ in, want string }{
		{"plain", "plain"},
		{"hi <@U1>", "hi @alice"},
		{"<@U2|bob> ok", "@bob ok"},
		{"in <#C1|builds>", "in #builds"},
		{"<!here> new build", "@here new build"},
		{"<!subteam^S1|@qa> ping", "@qa ping"},
		{"see <https://ci.example/1|pipeline 1>", "see pipeline 1"},
		{"<https://ci.example/1>", "https://ci.example/1"},
		{"a &lt;b&gt; &amp; c\n", "a <b> & c"},
	}
	for _, tt := range tests {
		if got := plainText(tt.in, users); got != tt.want {
			t.Errorf("plainText(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseTS(t *testing.T) {
	if got, want := parseTS("1760702400.123456"), time.Unix(1760702400, 123456000).UTC(); !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if !parseTS("garbage").IsZero() {
		t.Fatal("garbage parsed")
	}
}

func TestMessageLink(t *testing.T) {
	tests := []struct{ name, ts, thread, want string }{
		{"top level", "1760702400.123456", "", "https://acme.slack.com/archives/C1/p1760702400123456"},
		{"thread parent", "1760702400.123456", "1760702400.123456", "https://acme.slack.com/archives/C1/p1760702400123456"},
		{"reply", "1760702500.000100", "1760702400.123456", "https://acme.slack.com/archives/C1/p1760702500000100?cid=C1&thread_ts=1760702400.123456"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := messageLink("https://acme.slack.com/", "C1", tt.ts, tt.thread); got != tt.want {
				t.Fatalf("got %s", got)
			}
		})
	}
}
