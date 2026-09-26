package main

import "strings"

// govcsDefault is cmd/go's own built-in fallback rule, applied whenever no
// explicit GOVCS entry matches (see `go help vcs`: "the 'go get' command
// applies its default rule, which can now be summarized in GOVCS notation
// as 'public:git|hg,private:all'"). Confirmed live that `go env GOVCS`
// prints an empty string when the var is unset — cmd/go resolves this
// default internally rather than persisting it, so callers must supply it
// themselves rather than trusting an empty `go env GOVCS` to mean
// "anything goes."
const govcsDefault = "public:git|hg,private:all"

// govcsAllowsGit reports whether raw (the local `go` command's effective
// GOVCS setting, or "" for unset) permits a direct git fetch of module,
// given whether module is already known to be private (matches the local
// GOPRIVATE/GONOPROXY pattern list — see localModulePrivate).
//
// Mirrors cmd/go's own rule-matching exactly (internal/vcs/vcs.go's
// govcsRule, per `go help vcs`): GOVCS is a comma-separated list of
// pattern:vcslist rules tried in the order written — NOT public/private
// rules first — where the *earliest* rule whose pattern matches wins, even
// if a later, more specific rule would also match. "public" and "private"
// are special patterns matching the already-known private/public status
// (not a glob against module), everything else is a plain GOPRIVATE-style
// glob checked via matchesPrefixPattern. vcslist is "off" (block
// entirely), "all" (allow anything), or a "|"-separated list of VCS names.
//
// Confirmed live against a real toolchain across four cases: a specific
// pattern before a broader public/private one ("github.com:off,public:git|hg"
// blocks; the reverse order, "public:off,github.com:git", also blocks,
// proving the *first* rule wins regardless of specificity), a vcslist that
// simply omits git ("public:hg" blocks), and the default-equivalent
// ("public:git|hg" allows) — each verified with `GOPROXY=direct go mod
// download -x` against github.com/golang/protobuf@v1.5.0, comparing a
// real "GOVCS disallows using git for public github.com/golang/protobuf"
// failure against a successful `git ls-remote`/`git archive` trace.
func govcsAllowsGit(module string, private bool, raw string) bool {
	if strings.TrimSpace(raw) == "" {
		raw = govcsDefault
	}
	for _, rule := range strings.Split(raw, ",") {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		i := strings.LastIndex(rule, ":")
		if i < 0 {
			continue // malformed rule; real `go` errors out here, best-effort skip
		}
		pattern, vcslist := rule[:i], rule[i+1:]
		var matched bool
		switch pattern {
		case "public":
			matched = !private
		case "private":
			matched = private
		default:
			matched = matchesPrefixPattern(pattern, module)
		}
		if !matched {
			continue
		}
		if vcslist == "off" {
			return false
		}
		if vcslist == "all" {
			return true
		}
		for _, vcs := range strings.Split(vcslist, "|") {
			if vcs == "git" {
				return true
			}
		}
		return false
	}
	return true // no rule matched at all; shouldn't happen given govcsDefault always matches, but fail open rather than block the real check
}
