package announce

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kriakiku/holtdroid-sync/internal/apk"
	"github.com/kriakiku/holtdroid-sync/internal/layout"
)

func webhook(t *testing.T) (string, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, string(raw))
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return "generic://" + strings.TrimPrefix(srv.URL, "http://") + "/hook?disabletls=yes", func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

func TestAnnounceBuilds(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		builds []layout.Build
		portal string
		want   []string
		absent []string
		sent   bool
	}{
		{
			name: "one fresh build with launch links",
			builds: []layout.Build{{
				Key: "acme/app/v1.2/app.apk", Size: 2 << 20, Modified: now.Add(-time.Hour), Link: "https://github.com/acme/app/releases/tag/v1.2",
				App: &apk.Metadata{Label: "Hello", Package: "com.example.hello", VersionName: "1.2", VersionCode: 12},
			}},
			portal: "https://android.example.com/",
			want: []string{
				"New build in GitHub releases", "Hello 1.2 (12) · com.example.hello", "acme/app/v1.2/app.apk · 2.0 MiB",
				"https://android.example.com/?launch=YWNtZS9hcHAvdjEuMi9hcHAuYXBr&quick=1&source=github",
				"Launch: https://android.example.com/?launch=YWNtZS9hcHAvdjEuMi9hcHAuYXBr&source=github",
				"🔗 https://github.com/acme/app/releases/tag/v1.2",
			},
			sent: true,
		},
		{
			name:   "old builds from a first copy stay quiet",
			builds: []layout.Build{{Key: "old.apk", Modified: now.Add(-48 * time.Hour)}},
			sent:   false,
		},
		{
			name: "many builds are summed up",
			builds: func() []layout.Build {
				var out []layout.Build
				for i := range 12 {
					out = append(out, layout.Build{Key: fmt.Sprintf("b%02d.apk", i), Modified: now.Add(-time.Duration(i) * time.Minute)})
				}
				return out
			}(),
			want:   []string{"12 new builds", "b00.apk", "…and 2 more"},
			absent: []string{"b11.apk", "Quick launch"},
			sent:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook, sent := webhook(t)
			n, err := New([]string{hook}, tt.portal, "github", "GitHub releases", 24*time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			n.now = func() time.Time { return now }
			n.Announce(context.Background(), tt.builds)
			got := sent()
			if (len(got) == 1) != tt.sent || len(got) > 1 {
				t.Fatalf("sent %q", got)
			}
			if !tt.sent {
				return
			}
			msg := strings.ReplaceAll(got[0], `\u0026`, "&")
			for _, w := range tt.want {
				if !strings.Contains(msg, w) {
					t.Errorf("message lacks %q:\n%s", w, msg)
				}
			}
			for _, a := range tt.absent {
				if strings.Contains(msg, a) {
					t.Errorf("message has %q:\n%s", a, msg)
				}
			}
		})
	}
}
