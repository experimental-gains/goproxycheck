package main

import (
	"fmt"
	"regexp"
	"strings"
)

// bitbucketRepoPattern is githubRepoPattern's (proxy.go) sibling for
// bitbucket.org — also a static, two-segment-root entry in real cmd/go's
// own vcsPaths table (go1.24.4 source,
// cmd/go/internal/vcs/vcs.go:1538-1544:
// `^(?P<root>bitbucket\.org/[\w.\-]+/[\w.\-]+)(/[\w.\-]+)*$`, vcs: "git"),
// truncating any path segment past "owner/repo" exactly the way github.com's
// entry truncates past "owner/repo". Live-verified (2026-10-04, go1.24.4,
// GOPROXY=direct GOSUMDB=off, a fresh, isolated GOMODCACHE): with
// GOVCS="bitbucket.org/owner/fake-repo/sub:off" (naming a bitbucket.org
// import path's own subdirectory, not its two-segment repo root), `go mod
// download -x bitbucket.org/owner/fake-repo/sub@v1.0.0` does NOT hit a
// "GOVCS disallows" Fatal at all — it proceeds straight into a real `git
// ls-remote` attempt against bitbucket.org (reaching the network and 404ing
// on the repo's own nonexistence, exactly the same "got past the local
// config check, hit a real remote failure next" shape a genuine leak would
// show reaching sum.golang.org) — while GOVCS="bitbucket.org/owner/fake-repo:off"
// (naming exactly the truncated root) Fatals immediately and offline with
// "GOVCS disallows using git for public bitbucket.org/owner/fake-repo; see
// 'go help vcs'". Before this fix, this tool's GOVCS-disallowed check (in
// run(), both the GOPRIVATE/GONOPROXY-direct-fetch branch and the
// GOPROXY=direct branch) was gated on githubRepoPattern.MatchString(module)
// specifically — meaning for a bitbucket.org module it never even attempted
// localGovcsAllowsGit at all, not just with the wrong (untruncated) root:
// any GOVCS rule naming a bitbucket.org module, even one matching the full
// import path *exactly* with no subdirectory at all (no truncation question
// involved whatsoever), was silently ignored and the tool reported "will
// fetch it directly, no problem" for a module a real `go install` Fatals on
// outright. Confirmed this exact total-miss shape live too: with
// GOVCS="bitbucket.org/owner/fake-repo:off" matching a *non-nested*
// bitbucket.org/owner/fake-repo@v1.0.0 exactly, real go still Fatals
// identically ("GOVCS disallows using git for public
// bitbucket.org/owner/fake-repo"), but the pre-fix tool reported
// statusPrivateModuleLocally regardless, since the call site never reached
// localGovcsAllowsGit for any non-github.com host in the first place.
var bitbucketRepoPattern = regexp.MustCompile(`^bitbucket\.org/([^/]+)/([^/]+)`)

// hubJazzNetRepoPattern is githubRepoPattern's sibling for IBM DevOps
// Services' old JazzHub — a third hardcoded, static-root vcsPaths entry
// (cmd/go/internal/vcs/vcs.go:1547-1553:
// `^(?P<root>hub\.jazz\.net/git/[a-z0-9]+/[\w.\-]+)(/[\w.\-]+)*$`, vcs:
// "git"), truncating any path segment past "git/<user>/<project>" the same
// way github.com's and bitbucket.org's entries truncate past "owner/repo".
var hubJazzNetRepoPattern = regexp.MustCompile(`^hub\.jazz\.net/git/[a-z0-9]+/([^/]+)`)

// openstackRepoPattern is githubRepoPattern's sibling for the old
// git.openstack.org — a fourth hardcoded, static-root vcsPaths entry
// (cmd/go/internal/vcs/vcs.go:1564-1569:
// `^(?P<root>git\.openstack\.org/[\w.\-]+/[\w.\-]+)(\.git)?(/[\w.\-]+)*$`,
// vcs: "git"), truncating any path segment past "<project>/<repo>" the same
// way bitbucket.org's entry truncates past "owner/repo". Unlike
// git.apache.org's sibling table entry (whose repo name must always
// literally end in ".git", so it's already resolvable via
// generalVCSGitSuffixPattern below) and chiselapp.com's (anchored with a
// trailing "$", so it allows no subdirectory past its root at all — its
// "root" and "full import path" are always identical, nothing to
// truncate), git.openstack.org's ".git" suffix is optional, so a path with
// no literal suffix segment at all (the common case) needs this dedicated
// entry.
var openstackRepoPattern = regexp.MustCompile(`^git\.openstack\.org/([^/]+)/([^/]+)`)

// generalVCSGitSuffixPattern mirrors cmd/go/internal/vcs's vcsPaths table's
// last entry — "General syntax for any server", explicitly comment-marked
// "Must be last." in the real go1.24.4 source
// (cmd/go/internal/vcs/vcs.go:1579-1584) — which resolves an import path to
// a fixed VCS repo root for literally ANY host, not just the four above,
// whenever the path spells out a literal VCS-suffix segment. The real
// table's regexp recognizes five suffixes (bzr/fossil/git/hg/svn) and reads
// the matched one back out to pick which VCS tool to invoke — but this
// tool's whole GOVCS-disallow diagnosis is worded specifically around git
// ("GOVCS disallows using git for ..."), so it's only sound to treat a
// match here as "VCS is certainly git" for the literal ".git" suffix;
// narrowed to just that one suffix, deliberately not the full alternation,
// unlike goprivaudit's own copy of this pattern (which only ever asks "is
// git specifically disallowed," a question that stays valid regardless of
// which VCS the path would really resolve to).
//
// Live-verified (2026-10-04, go1.24.4, GOPROXY=direct GOSUMDB=off): a
// require for "example.com/foo/bar.git/sub@v1.0.0". GOVCS=
// "example.com/foo/bar.git/sub:off" (the module's own full, uncollapsed
// path) does NOT block it — `go mod download -x` proceeds straight to an
// ordinary direct git-fetch attempt against example.com (reaching the
// network: the command actually dials out). GOVCS="example.com/foo/bar.git:off"
// (the regex-truncated root, dropping "/sub") DOES block it: `go:
// example.com/foo/bar.git/sub@v1.0.0: GOVCS disallows using git for public
// example.com/foo/bar.git; see 'go help vcs'`, Fatal, zero network access.
var generalVCSGitSuffixPattern = regexp.MustCompile(`^(([a-z0-9.\-]+\.)+[a-z0-9.\-]+(:[0-9]+)?(/~?[\w.\-]+)+?\.git)(/~?[\w.\-]+)*$`)

// bitbucketRepoRoot returns modulePath's "bitbucket.org/owner/repo" prefix,
// or "" if modulePath isn't bitbucket.org-hosted. See bitbucketRepoPattern.
func bitbucketRepoRoot(modulePath string) string {
	return bitbucketRepoPattern.FindString(modulePath)
}

// hubJazzNetRepoRoot returns modulePath's "hub.jazz.net/git/user/project"
// prefix, or "" if modulePath isn't hub.jazz.net/git-hosted. See
// hubJazzNetRepoPattern.
func hubJazzNetRepoRoot(modulePath string) string {
	return hubJazzNetRepoPattern.FindString(modulePath)
}

// openstackRepoRoot returns modulePath's "git.openstack.org/project/repo"
// prefix, or "" if modulePath isn't git.openstack.org-hosted. See
// openstackRepoPattern.
func openstackRepoRoot(modulePath string) string {
	return openstackRepoPattern.FindString(modulePath)
}

// generalVCSGitSuffixRoot returns modulePath's VCS repo root per
// generalVCSGitSuffixPattern — the segment up to and including its literal
// ".git" suffix — or "" if modulePath contains no such suffix.
func generalVCSGitSuffixRoot(modulePath string) string {
	m := generalVCSGitSuffixPattern.FindStringSubmatch(modulePath)
	if m == nil {
		return ""
	}
	return m[1]
}

// vcsStaticRepoRoot returns modulePath's statically-known, certainly-git
// VCS repo root — see githubRepoPattern (proxy.go), bitbucketRepoPattern,
// hubJazzNetRepoPattern, openstackRepoPattern, and
// generalVCSGitSuffixPattern above — or "" if modulePath doesn't match any
// of the shapes this tool can resolve offline with certainty. Used by
// localGovcsPrivate/localGovcsAllowsGit (main.go) in place of modulePath
// itself, and by run() to decide whether its GOVCS-disallowed check applies
// to a module at all: see those callers' doc comments for why a GOVCS/
// GOPRIVATE pattern more specific than the real VCS-resolved root (a
// "/v2"-suffixed or monorepo-nested import path, say) can match modulePath
// directly without ever matching the root real go actually classifies and
// matches against — and why, before this function existed, every host
// besides github.com was entirely out of scope for this tool's
// GOVCS-disallowed diagnosis, not just mishandled for the truncation case.
//
// git.apache.org and chiselapp.com — cmd/go/internal/vcs's remaining two
// pathPrefix-gated vcsPaths entries — are deliberately not added as their
// own cases here, mirroring goprivaudit's own vcsStaticRepoRoot: every
// valid git.apache.org path already ends in ".git" literally, so
// generalVCSGitSuffixRoot already resolves it; chiselapp.com's repo
// (fossil, not git, and anchored with a trailing "$" besides) is out of
// scope for a git-specific check either way.
func vcsStaticRepoRoot(modulePath string) string {
	if root := githubRepoRoot(modulePath); root != "" {
		return root
	}
	if root := bitbucketRepoRoot(modulePath); root != "" {
		return root
	}
	if root := hubJazzNetRepoRoot(modulePath); root != "" {
		return root
	}
	if root := openstackRepoRoot(modulePath); root != "" {
		return root
	}
	return generalVCSGitSuffixRoot(modulePath)
}

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
// entirely) or a "|"-separated list of VCS names, where "all" is itself
// just one more name in that list — matching real go's own govcsConfig.allow,
// "all" isn't only meaningful as the *entire* vcslist by itself (e.g.
// "public:all"); it also permits every VCS, including git, when it appears
// as one alternative alongside others (e.g. "public:hg|all") — see the
// live-verified case below.
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
//
// A fifth case, confirmed live the same way (2026-09-27, `GOPROXY=direct go
// mod tidy -x` against rsc.io/quote@v1.5.2 with a fresh GOMODCACHE): a rule
// whose vcslist itself contains a colon, e.g. "rsc.io:hg:git" (a plausible
// typo for the pipe-separated "rsc.io:hg|git"). Real cmd/go's parseGOVCS
// splits each rule on its *first* colon only (strings.Cut, mirrored by
// govcsConfigError above) — so "rsc.io:hg:git" is pattern "rsc.io", vcslist
// ["hg:git"] (a single, never-matching VCS name), meaning real `go` fails
// every git AND hg fetch of anything under rsc.io with "GOVCS disallows
// using git for public rsc.io/quote; see 'go help vcs'". This function used
// to split each rule on its *last* colon instead, parsing that same rule as
// pattern "rsc.io:hg" (which never matches any real module path, since
// paths don't contain colons) with vcslist "git" — so the rule matched
// nothing, execution fell through to the next rule, and this function
// reported git as allowed for a fetch real `go` actually refuses outright.
//
// A sixth case, confirmed live the same way (2026-09-27, `GOPROXY=direct go
// get -x` against github.com/golang/protobuf@v1.5.0 with a fresh
// GOMODCACHE): whitespace around the colon or the "|"-separated VCS names,
// e.g. "public : off" or "public: git | hg" — a natural way to hand-format a
// multi-rule GOVCS value. Real cmd/go's parseGOVCS trims each piece
// (pattern, vcslist, and every individual VCS name) after splitting; this
// function used to compare the untrimmed pieces directly, so "public : off"
// had pattern `"public "` (trailing space) which never equals the literal
// "public", the rule silently never matched, and execution fell through to
// the next rule (or all the way to the fail-open default) instead of
// blocking the fetch real `go` actually refuses with "GOVCS disallows using
// git for public github.com/golang/protobuf; see 'go help vcs'".
func govcsAllowsGit(module string, private bool, raw string) bool {
	if strings.TrimSpace(raw) == "" {
		raw = govcsDefault
	}
	for _, rule := range strings.Split(raw, ",") {
		rule = strings.TrimSpace(rule)
		if rule == "" {
			continue
		}
		pattern, vcslist, found := strings.Cut(rule, ":")
		if !found {
			continue // malformed rule; real `go` errors out here, best-effort skip
		}
		pattern, vcslist = strings.TrimSpace(pattern), strings.TrimSpace(vcslist)
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
		// Real cmd/go's own govcsConfig.allow (internal/vcs/vcs.go) doesn't
		// treat "all" as a distinct whole-vcslist keyword the way this used
		// to: it splits vcslist the same way regardless, then returns true
		// the moment *any* individual item equals the VCS name or "all" —
		// so "all" also permits git when it appears alongside other VCS
		// names in a "|"-separated list, not just when it's the entire
		// vcslist string by itself.
		//
		// Confirmed live (2026-09-28): with GOVCS="public:hg|all" and
		// GOPROXY=direct against a fresh GOMODCACHE, `go mod download -x
		// github.com/golang/protobuf@v1.5.0` succeeds — a full git
		// ls-remote/fetch/archive trace, no "GOVCS disallows" error — because
		// the "all" item in the list permits every VCS, including git, even
		// though "hg" (the other, git-excluding item) precedes it. Before
		// this fix, vcslist == "all" was only checked as an exact match
		// against the *whole* vcslist string, so "hg|all" fell through to
		// the per-item loop below, which only ever looked for a literal
		// "git" entry and never recognized "all" as one of the alternatives
		// — so this function reported the fetch as disallowed
		// (statusGovcsDisallowedLocally, "GOVCS disallows using git for
		// public github.com/golang/protobuf") for a fetch the real `go`
		// command actually performs successfully.
		for _, vcs := range strings.Split(vcslist, "|") {
			vcs = strings.TrimSpace(vcs)
			if vcs == "git" || vcs == "all" {
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
