package main

import (
	"path"
	"strings"
)

// matchesPrefixPattern reports whether modulePath is covered by pattern,
// mirroring golang.org/x/mod/module.MatchPrefixPatterns exactly (the same
// algorithm the real `go` command applies to GOPRIVATE/GONOPROXY/
// GONOSUMDB, see `go help goproxy`): count the path separators in pattern
// to find how many leading segments of modulePath to keep as a prefix,
// then run a single path.Match of pattern against that whole prefix — not
// a per-segment path.Match, which silently breaks backslash-escaped
// separators and bracket expressions containing "/" (both valid
// path.Match glob syntax the real go command still honors correctly).
// Ported verbatim from github.com/experimental-gains/goprivaudit's
// pattern.go, where it was already verified against the real x/mod oracle
// via a fuzz pass — see that repo's fuzz_test.go for the divergence a
// naive per-segment implementation had.
func matchesPrefixPattern(pattern, modulePath string) bool {
	pattern = strings.TrimSuffix(pattern, "/")
	if pattern == "" {
		return false
	}
	n := strings.Count(pattern, "/")
	prefix := modulePath
	for i := 0; i < len(modulePath); i++ {
		if modulePath[i] == '/' {
			if n == 0 {
				prefix = modulePath[:i]
				break
			}
			n--
		}
	}
	if n > 0 {
		return false // modulePath has fewer segments than pattern requires
	}
	ok, err := path.Match(pattern, prefix)
	return err == nil && ok
}

// matchesAnyPattern reports whether modulePath is covered by any pattern in
// the comma-separated GOPRIVATE-style pattern list.
func matchesAnyPattern(modulePath string, patterns []string) bool {
	for _, p := range patterns {
		if matchesPrefixPattern(p, modulePath) {
			return true
		}
	}
	return false
}

// splitPatterns splits a GOPRIVATE/GONOPROXY/GONOSUMDB-style comma
// separated env value into its individual patterns, dropping empty
// entries — but, matching golang.org/x/mod/module.MatchPrefixPatterns
// exactly (the real algorithm `go` itself applies here, confirmed against
// that source across x/mod v0.19.0 through v0.41.0 and the go1.24/go1.27
// vendored copies: none of them ever call strings.TrimSpace on an
// individual glob, only strings.TrimSuffix(glob, "/")), deliberately NOT
// trimming surrounding whitespace from each pattern.
//
// This used to TrimSpace each entry, which silently accepted a config
// style real `go` does not: GOPRIVATE="nomatch/*, golang.org/x/text" (a
// space after the comma — a natural way to write a comma list by hand)
// makes the second glob " golang.org/x/text", leading space included: an
// actual module path never starts with a space, so real `go` never
// matches it and fetches that module through the normal public proxy —
// confirmed live with `go mod download -x golang.org/x/text@v0.14.0`
// under that exact GOPRIVATE value, which shows ordinary
// proxy.golang.org GETs, not a git ls-remote/fetch direct-VCS trace (the
// same command with no leading space goes direct, as expected). This
// tool's own trimming made it treat golang.org/x/text as private anyway,
// reporting a false private-module-locally and skipping the real
// proxy/sumdb check `go install` will actually perform.
func splitPatterns(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
