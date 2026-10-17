// Package rules selects object keys with gitignore-style patterns: "/a/*"
// is anchored, "*.apk" matches a name at any depth, "**" spans directories,
// a pattern that matches a directory matches everything below it, "!"
// excludes, and the last matching pattern wins. A list that starts with an
// exclusion starts from "everything".
package rules

import (
	"fmt"
	"path"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

type rule struct {
	pattern  string
	negate   bool
	anchored bool
	dirOnly  bool
}

type Rules struct {
	rules []rule
}

func Parse(patterns []string) (Rules, error) {
	var out Rules
	for _, raw := range patterns {
		p := strings.TrimSpace(raw)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		var r rule
		if r.negate = strings.HasPrefix(p, "!"); r.negate {
			p = p[1:]
		}
		if r.dirOnly = strings.HasSuffix(p, "/"); r.dirOnly {
			p = strings.TrimRight(p, "/")
		}
		r.anchored = strings.Contains(p, "/")
		r.pattern = strings.TrimLeft(p, "/")
		if r.pattern == "" || !doublestar.ValidatePattern(r.pattern) {
			return Rules{}, fmt.Errorf("invalid pattern %q", raw)
		}
		out.rules = append(out.rules, r)
	}
	return out, nil
}

func (r Rules) Empty() bool { return len(r.rules) == 0 }

func (r Rules) Match(key string) bool {
	if len(r.rules) == 0 {
		return true
	}
	key = strings.Trim(key, "/")
	included := r.rules[0].negate
	for _, ru := range r.rules {
		if ru.matches(key) {
			included = !ru.negate
		}
	}
	return included
}

func (ru rule) matches(key string) bool {
	for cand, dir := key, false; cand != "." && cand != ""; cand, dir = path.Dir(cand), true {
		if ru.dirOnly && !dir {
			continue
		}
		target := cand
		if !ru.anchored {
			target = path.Base(cand)
		}
		if ok, _ := doublestar.Match(ru.pattern, target); ok {
			return true
		}
	}
	return false
}
