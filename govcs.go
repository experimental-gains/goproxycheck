package main

import (
	"fmt"
	"strings"
)

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

// govcsConfigError reports the parse error real cmd/go's own GOVCS validation
// (internal/vcs's parseGOVCS) would raise for raw, or nil if raw is
// well-formed. Confirmed live against five distinct cases (2026-09-27,
// GOPROXY=direct against a fresh GOMODCACHE so the direct-VCS path is
// actually exercised): a comma-separated empty entry
// ("public:git|hg,,private:all" → "empty entry in GOVCS"), an entry missing
// its colon ("badrule,public:git|hg" → `malformed entry in GOVCS (missing
// colon): "badrule"`), an empty pattern (":git" → "empty pattern in GOVCS"),
// an empty VCS list ("public:" → "empty VCS list in GOVCS"), a relative
// pattern ("./foo:git" → "relative pattern not allowed in GOVCS"), and a
// pattern repeated later in the list ("public:git,public:hg" → "unreachable
// pattern in GOVCS").
//
// Crucially, real `go` validates the *entire* GOVCS string up front
// (parseGOVCS, cached process-wide via sync.Once) before it ever walks the
// rules looking for a match — a single malformed entry anywhere in the list
// makes every direct-VCS fetch of every module fail with this same parse
// error, even a module whose own applicable rule (matched by pattern) would
// otherwise clearly have allowed it. Confirmed live: GOVCS=
// "badrule,public:git|hg" (an unrelated malformed first entry ahead of a
// normal, permissive "public:git|hg" rule) still fails outright — on a
// fresh module cache, `go mod download -x` for an ordinary public module
// gets `malformed entry in GOVCS (missing colon): "badrule"`, never reaching
// the second, perfectly valid rule that would have allowed it.
//
// govcsAllowsGit's own rule-matching loop deliberately tolerates a missing
// colon and an empty entry with a "best-effort skip" (see its comment) —
// evaluating the *reachable* rule for one module was that function's only
// concern — but that skip means govcsAllowsGit returns a confident
// true/false as if the config were fine, when real `go` would refuse to run
// at all. Called from localGovcsConfigError ahead of localGovcsAllowsGit so
// callers can report the real failure mode instead of a false "GOVCS
// disallows"/"will fetch directly" verdict — and, unlike govcsAllowsGit,
// this doesn't need to know the module's VCS type at all, since the error is
// a property of the GOVCS string itself, not of any one pattern match.
func govcsConfigError(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	seen := map[string]string{}
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return fmt.Errorf("empty entry in GOVCS")
		}
		pattern, list, found := strings.Cut(item, ":")
		if !found {
			return fmt.Errorf("malformed entry in GOVCS (missing colon): %q", item)
		}
		pattern, list = strings.TrimSpace(pattern), strings.TrimSpace(list)
		if pattern == "" {
			return fmt.Errorf("empty pattern in GOVCS: %q", item)
		}
		if list == "" {
			return fmt.Errorf("empty VCS list in GOVCS: %q", item)
		}
		if pattern == "." || pattern == ".." || strings.HasPrefix(pattern, "./") || strings.HasPrefix(pattern, "../") {
			return fmt.Errorf("relative pattern not allowed in GOVCS: %q", pattern)
		}
		if old, dup := seen[pattern]; dup {
			return fmt.Errorf("unreachable pattern in GOVCS: %q after %q", item, old)
		}
		seen[pattern] = item
		for _, vcs := range strings.Split(list, "|") {
			if strings.TrimSpace(vcs) == "" {
				return fmt.Errorf("empty VCS name in GOVCS: %q", item)
			}
		}
	}
	return nil
}
