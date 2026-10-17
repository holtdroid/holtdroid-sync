package rules

import "testing"

func TestMatch(t *testing.T) {
	tests := []struct {
		name     string
		patterns []string
		key      string
		want     bool
	}{
		{"no rules take everything", nil, "a/b.apk", true},
		{"anchored dir glob", []string{"/releases/*"}, "releases/app.apk", true},
		{"anchored dir glob covers subdirectories", []string{"/releases/*"}, "releases/2026/app.apk", true},
		{"anchored dir glob skips other trees", []string{"/releases/*"}, "nightly/app.apk", false},
		{"anchored glob is anchored", []string{"/releases/*"}, "old/releases/app.apk", false},
		{"exclusion after inclusion", []string{"/releases/*", "!/releases/night/*"}, "releases/night/app.apk", false},
		{"exclusion covers deeper files", []string{"/releases/*", "!/releases/night/*"}, "releases/night/x/app.apk", false},
		{"exclusion leaves siblings", []string{"/releases/*", "!/releases/night/*"}, "releases/day/app.apk", true},
		{"re-inclusion wins when last", []string{"/releases/*", "!/releases/night/*", "/releases/night/keep.apk"}, "releases/night/keep.apk", true},
		{"leading exclusion starts from everything", []string{"!**/debug/**"}, "a/b.apk", true},
		{"leading exclusion excludes", []string{"!**/debug/**"}, "a/debug/b.apk", false},
		{"name pattern matches at any depth", []string{"*-release.apk"}, "x/y/app-release.apk", true},
		{"name pattern rejects other names", []string{"*-release.apk"}, "x/y/app-debug.apk", false},
		{"double star across directories", []string{"builds/**/prod-*.apk"}, "builds/a/b/prod-1.apk", true},
		{"dir-only pattern skips files", []string{"night/"}, "night", false},
		{"dir-only pattern matches directories", []string{"night/"}, "x/night/app.apk", true},
		{"comments and blanks are ignored", []string{"# comment", "", "/a/*"}, "a/b.apk", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := Parse(tt.patterns)
			if err != nil {
				t.Fatal(err)
			}
			if got := r.Match(tt.key); got != tt.want {
				t.Fatalf("Match(%q) with %q = %v, want %v", tt.key, tt.patterns, got, tt.want)
			}
		})
	}
}

func TestParseRejectsInvalidPatterns(t *testing.T) {
	for _, p := range []string{"[", "!", "/"} {
		if _, err := Parse([]string{p}); err == nil {
			t.Errorf("Parse(%q) accepted", p)
		}
	}
}
