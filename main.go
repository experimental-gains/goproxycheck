// Command goproxycheck checks whether a Go module version is actually
// fetchable via the public module proxy and checksum database — the same
// two systems a plain `go install module@version` depends on — and
// diagnoses *why* when it isn't, instead of leaving you to guess whether to
// wait, retry, or re-tag.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/mod/semver"
	"golang.org/x/mod/sumdb/note"

	// Aliased: resolveTarget's named return value is itself called "module"
	// (a string, the module path), which would otherwise shadow this package
	// for the whole function body.
	modulepkg "golang.org/x/mod/module"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, defaultEndpoints()))
}

func run(args []string, stdout, stderr io.Writer, ep endpoints) int {
	fs := flag.NewFlagSet("goproxycheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	wait := fs.Bool("wait", false, "poll until the version is ready (or --timeout elapses) instead of checking once")
	timeout := fs.Duration("timeout", 5*time.Minute, "max time to poll when --wait is set")
	interval := fs.Duration("interval", 15*time.Second, "how often to poll when --wait is set")
	jsonOut := fs.Bool("json", false, "print the diagnosis as JSON instead of text")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: goproxycheck [flags] [module@version]")
		_, _ = fmt.Fprintln(stderr, "  with no argument, reads the module path from ./go.mod and the version from the tag(s) at HEAD")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	module, version, err := resolveTarget(fs.Args())
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "goproxycheck:", err)
		return 2
	}

	var d diagnosis
	if priv, pattern := localModulePrivate(module); priv {
		// Verified live: with GOPRIVATE (or GONOPROXY directly) set to a
		// pattern matching this module, `go install`/`go get`/`go mod
		// download` never contact proxy.golang.org at all for it — they
		// fetch directly from the VCS host instead (confirmed with `go mod
		// download -x`: a matching module shows a `git ls-remote` trace and
		// no proxy.golang.org request whatsoever, where the same module
		// without the match shows only proxy.golang.org GETs). Probing the
		// public proxy for a module like this would report false
		// module-unknown/not-yet-indexed verdicts — the proxy genuinely has
		// never heard of it, by design, regardless of whether the real `go
		// install` would succeed fine via direct VCS fetch right now.
		// Mirrors the existing GOPROXY=off short-circuit below: local
		// config makes the whole proxy probe moot, so skip it instead of
		// reporting on a system `go` itself won't consult.
		//
		// But that "will fetch it directly" claim needs two more things to
		// hold, checked in order of how fundamental the failure is: first,
		// that the local GOVCS setting itself parses at all — a single
		// malformed entry anywhere in it (a missing colon, a stray empty
		// entry from a double comma, ...) makes real `go` refuse *every*
		// direct-VCS fetch outright with a parse error, even one a different,
		// well-formed rule in the same list would otherwise clearly have
		// allowed (confirmed live across five distinct malformed shapes, see
		// govcsConfigError's doc comment) — and only once that holds, that
		// GOVCS actually permits a direct git fetch of *this* module —
		// confirmed live it does not always: GOVCS="private:off" (or any
		// rule excluding git) makes `go mod download` fail outright with
		// "GOVCS disallows using git for private <module>", not fetch it,
		// while this tool used to unconditionally claim the fetch would
		// succeed. Only checkable when the VCS is known for certain
		// (github.com is always git; see githubRepoPattern's doc comment
		// for why other hosts are out of scope) — but the parse-error check
		// ahead of it needs no such certainty, since a malformed GOVCS string
		// breaks every direct fetch regardless of host or VCS type.
		govcsPrivate := localGovcsPrivate(module)
		switch govcsErr := localGovcsConfigError(); {
		case govcsErr != nil:
			d = diagnosis{statusGovcsMalformedLocally, fmt.Sprintf(
				"your local `GOPRIVATE`/`GONOPROXY` config matches %s via the pattern %q, so `go install`/`go get` would normally fetch it directly from its VCS host — but your local `GOVCS` setting is itself malformed (%v), which makes every direct-VCS-requiring `go` command fail outright with this exact parse error, not just for this module, and regardless of whether some other rule in the list would otherwise have allowed it. "+
					"That's your machine's own config, not a proxy-availability problem, and this tool's proxy/sumdb checks don't apply to it either way. Fix your `GOVCS` setting (see `go help vcs`) — real `go` validates the whole list up front, so one bad entry anywhere breaks every direct fetch.",
				module, pattern, govcsErr)}
		case githubRepoPattern.MatchString(module) && !localGovcsAllowsGit(module, govcsPrivate):
			d = diagnosis{statusGovcsDisallowedLocally, fmt.Sprintf(
				"your local `GOPRIVATE`/`GONOPROXY` config matches %s via the pattern %q, so `go install`/`go get` would normally fetch it directly from its VCS host — but your local `GOVCS` setting disallows git for this (%[3]s) module, so the real command fails outright with `GOVCS disallows using git for %[3]s %[1]s; see 'go help vcs'` instead of succeeding. "+
					"That's your machine's own config, not a proxy-availability problem, and this tool's proxy/sumdb checks don't apply to it either way. Adjust `GOVCS` (or `go env -w GOVCS=...`) if you meant to allow this.",
				module, pattern, govcsWhat(govcsPrivate))}
		default:
			d = diagnosis{statusPrivateModuleLocally, fmt.Sprintf(
				"your local `GOPRIVATE`/`GONOPROXY` config matches %s via the pattern %q, so `go install`/`go get` will fetch it directly from its VCS host here, never through proxy.golang.org — "+
					"that's your machine's own config, not a proxy-availability problem, and this tool's proxy/sumdb checks don't apply to it. If you meant to check a *public* module instead, verify the module path doesn't accidentally match your GOPRIVATE pattern.",
				module, pattern)}
		}
	} else if localGoproxyOff() {
		// Verified live: with GOPROXY=off (or its first comma/pipe-separated
		// entry), `go install`/`go mod download` refuse to fetch anything at
		// all — "module lookup disabled by GOPROXY=off" — regardless of what
		// proxy.golang.org itself has. Probing the public proxy in this case
		// would report a false "ready" (it did, before this check existed:
		// goproxycheck said `golang.org/x/mod@v0.30.0` was ready with
		// GOPROXY=off set, while the real `go install` in that exact
		// environment failed outright). Short-circuit instead of probing.
		d = diagnosis{statusGoproxyOffLocally, fmt.Sprintf(
			"your local `GOPROXY` is set to `off` (via env var or `go env -w`), so `go install`/`go get` will refuse to fetch %s@%s here at all — "+
				"that's your machine's own config, not a proxy-availability problem. Unset `GOPROXY` or point it at a real source "+
				"(e.g. `GOPROXY=https://proxy.golang.org,direct`) to install normally. This skips probing the proxy entirely: if %s@%s is "+
				"already sitting in your local module cache, `go install` can still succeed despite GOPROXY=off, since that bypasses the network fetch.",
			module, version, module, version)}
	} else if kind, custom := localGoproxyNonPublic(); kind == "direct" {
		// See localGoproxyNonPublic's doc comment. Same short-circuit shape
		// as the private-module/GOPROXY=off cases above: the public-proxy
		// probe below is moot when `go install` never talks to a proxy at
		// all for this fetch. Same two-stage GOVCS check as the
		// private-module branch above (parse error first, then the
		// pattern-specific disallow) — see that branch's comment for why the
		// parse-error case needs no VCS-type certainty and the disallow case
		// does.
		govcsPrivate := localGovcsPrivate(module)
		switch govcsErr := localGovcsConfigError(); {
		case govcsErr != nil:
			d = diagnosis{statusGovcsMalformedLocally, fmt.Sprintf(
				"your local `GOPROXY` resolves to `direct` (via env var or `go env -w`), so `go install`/`go get` would normally fetch %s@%s straight from its VCS host — but your local `GOVCS` setting is itself malformed (%v), which makes every direct-VCS-requiring `go` command fail outright with this exact parse error, regardless of whether some other rule in the list would otherwise have allowed this fetch. "+
					"That's your machine's own config, not a proxy-availability problem, and this tool's proxy/sumdb checks don't apply to it either way. Fix your `GOVCS` setting (see `go help vcs`) — real `go` validates the whole list up front, so one bad entry anywhere breaks every direct fetch.",
				module, version, govcsErr)}
		case githubRepoPattern.MatchString(module) && !localGovcsAllowsGit(module, govcsPrivate):
			d = diagnosis{statusGovcsDisallowedLocally, fmt.Sprintf(
				"your local `GOPROXY` resolves to `direct` (via env var or `go env -w`), so `go install`/`go get` would normally fetch %s@%s straight from its VCS host — but your local `GOVCS` setting disallows git for this (%[3]s) module, so the real command fails outright with `GOVCS disallows using git for %[3]s %[1]s; see 'go help vcs'` instead of succeeding. "+
					"That's your machine's own config, not a proxy-availability problem, and this tool's proxy/sumdb checks don't apply to it either way. Adjust `GOVCS` (or `go env -w GOVCS=...`) if you meant to allow this.",
				module, version, govcsWhat(govcsPrivate))}
		default:
			d = diagnosis{statusGoproxyDirectLocally, fmt.Sprintf(
				"your local `GOPROXY` resolves to `direct` (via env var or `go env -w`), so `go install`/`go get` will fetch %s@%s straight from its VCS host here, never through proxy.golang.org — "+
					"that's your machine's own config, not a proxy-availability problem, and this tool's proxy/sumdb checks don't apply to it. A real failure here (auth, an unreachable host, a GOVCS restriction) would show up as its own error straight from `go`, not from this tool.",
				module, version)}
		}
	} else if precedingCustom, anyErrorFallback, fallbackOK := publicProxyFallback(); kind == "custom" && !fallbackOK {
		d = diagnosis{statusGoproxyCustomLocally, fmt.Sprintf(
			"your local `GOPROXY` is set to %q (via env var or `go env -w`), not the public proxy.golang.org — so `go install`/`go get` will fetch %s@%s from that proxy here, not the one this tool checks. "+
				"This tool only knows how to probe the public proxy.golang.org/sum.golang.org anonymously over plain HTTP; it has no way to know your custom proxy's auth or protocol quirks, so it can't tell you whether %[2]s@%[3]s is actually ready there. "+
				"If you're chasing a real failure, check your proxy's own logs/status instead of trusting this tool's result — or temporarily set GOPROXY=https://proxy.golang.org,direct to check against the public proxy specifically.",
			custom, module, version)}
	} else {
		deadline := time.Now().Add(*timeout)
		for {
			r := ep.probe(module, version)
			d = diagnose(r)
			if d.status == statusSumdbLag {
				// Checked before localSumdbSkipped: a custom (non-"off",
				// non-public) $GOSUMDB only genuinely means "verification
				// goes elsewhere, the public sumdb's lag is irrelevant" when
				// it's actually a well-formed checksum-database verifier key
				// — see localGosumdbConfigError's doc comment for why a
				// malformed one is a real, unconditional failure instead, not
				// a safe skip.
				if err := localGosumdbConfigError(module); err != nil {
					d = diagnosis{statusGosumdbMalformedLocally, fmt.Sprintf(
						"%s is live on proxy.golang.org, but before that would even matter, your local `GOSUMDB` config is itself malformed (%v) — real `go install`/`go get` fails outright with `invalid GOSUMDB: %v` the moment it actually needs to verify this (or any) module against the checksum database, regardless of what sum.golang.org has. "+
							"That's your machine's own config, not a proxy-availability problem. Fix `GOSUMDB` (see `go help goproxy`), or set `GOSUMDB=off` if you intend to skip verification entirely.",
						displayTarget(r), err, err)}
				} else if skipped, reason := localSumdbSkipped(module); skipped {
					// See localSumdbSkipped's doc comment: a local GOSUMDB=off
					// or a matching GONOSUMDB pattern means `go install` never
					// consults sum.golang.org for this module at all, so a
					// sumdb-lag verdict from the public sumdb doesn't reflect
					// what will actually happen here — the proxy already has
					// it, so it's ready right now.
					d = diagnosis{statusReady, fmt.Sprintf(
						"%s is live on proxy.golang.org. sum.golang.org doesn't have it yet, but your local %s means "+
							"`go install`/`go get` won't consult the checksum database for this module here at all, so that lag doesn't block you — a plain `go install` will work right now.",
						displayTarget(r), reason)}
				}
			}
			// statusZipBuildError belongs in this early-break list for the same
			// reason as the other four: diagnose's own message for it says
			// this outright ("This is a permanent property of the tagged
			// tree... cutting a new tag won't help unless it also fixes the
			// underlying file problem") — waiting cannot change a
			// case-insensitive filename collision or oversized file in the
			// tagged tree. Before this, `--wait` polled a doomed zip-build
			// error at the full --interval cadence for the entire --timeout
			// (5 minutes by default), hammering proxy.golang.org for an
			// answer that was already final on the first probe — reproduced
			// live with a fake proxy serving a "create zip" 404 under
			// --wait --timeout=300ms: it polled the full 300ms instead of
			// returning immediately, the same shape of waste this list
			// already exists to prevent for the other permanent statuses.
			// statusDeprecated joins this list for the same reason as
			// statusRetracted right next to it: it's the maintainer's own
			// go.mod saying "don't use this," not a proxy/sumdb timing issue
			// — no amount of waiting changes a deprecation notice.
			// statusMajorVersionMismatch joins this list for the same reason
			// as statusZipBuildError right above: a go.mod missing its
			// required /vN path suffix at this tag is a permanent property
			// of that tag, confirmed live against github.com/osrg/gobgp@
			// v2.16.0 and others (see majorVersionMismatchMarker's doc
			// comment) — no amount of polling makes an existing tag's go.mod
			// grow the suffix it's missing.
			// statusUnknownRevision joins this list for the same reason as
			// statusZipBuildError and statusMajorVersionMismatch: a version
			// query naming a revision that doesn't exist in the repo at all
			// (see unknownRevisionMarker's doc comment) can never resolve no
			// matter how long this polls — there's no tag/branch/commit for
			// the proxy to eventually pick up.
			// statusInvalidPseudoVersion joins this list for the same reason
			// right next to it: a pseudo-version whose encoded timestamp or
			// base-tag segment doesn't match reality (see
			// invalidPseudoVersionMarker's doc comment) can never become
			// correct no matter how long this polls — the real commit's
			// timestamp is fixed forever, and a tag that was never cut can't
			// start existing at this exact version string by waiting.
			// statusGosumdbMalformedLocally joins this list for the same
			// reason as statusGovcsMalformedLocally isn't even reached by
			// this polling loop at all (it's diagnosed before the probe):
			// a malformed local $GOSUMDB is this machine's own config, and
			// no amount of proxy.golang.org/sum.golang.org catching up
			// changes it — it needs a config fix, not a wait.
			if !*wait || d.status == statusReady || d.status == statusModuleUnknown || d.status == statusBlocklistedMalicious || d.status == statusWrongImportPath || d.status == statusRetracted || d.status == statusDeprecated || d.status == statusZipBuildError || d.status == statusMajorVersionMismatch || d.status == statusUnknownRevision || d.status == statusInvalidPseudoVersion || d.status == statusGosumdbMalformedLocally || time.Now().After(deadline) {
				break
			}
			time.Sleep(*interval)
		}
		if kind == "custom" && fallbackOK {
			// See publicProxyFallback's doc comment: the chain reaches the
			// public proxy after one or more custom entries, so the probe
			// above is real and relevant, but only conditionally — prefix
			// the diagnosis with that caveat instead of either silently
			// omitting it (misleadingly presenting the result as
			// unconditional) or bailing out entirely (the old behavior,
			// which threw away a real, checkable answer).
			d.message = fallbackCaveat(precedingCustom, anyErrorFallback) + " " + d.message
		}
	}

	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]string{
			"module":  module,
			"version": version,
			"status":  string(d.status),
			"message": d.message,
		}); err != nil {
			_, _ = fmt.Fprintln(stderr, "goproxycheck:", err)
			return 2
		}
	} else if _, err := fmt.Fprintf(stdout, "%s@%s: %s\n%s\n", module, version, d.status, d.message); err != nil {
		_, _ = fmt.Fprintln(stderr, "goproxycheck:", err)
		return 2
	}

	if d.status == statusReady {
		return 0
	}
	return 1
}

// isComparisonVersionQuery reports whether v is one of the four version-range
// queries cmd/go's own modload/query.go special-cases (newQueryMatcher, cases
// "<=", "<", ">=", ">"; go.dev/ref/mod#version-queries) ahead of treating the
// string as a literal version or revision name. Both '<' and '>' are
// themselves disallowed characters in an ordinary version/revision string
// (see fileNameOK in golang.org/x/mod/module) — so a version beginning with
// either one can only ever be this kind of range query, never a literal name
// — which is why checking just the prefix here is enough to tell them apart.
//
// This exists so the disallowed-version-character check in resolveTarget
// below doesn't misfire on a query real `go` validates in a completely
// different way: confirmed live (2026-09-27) that `go get
// golang.org/x/mod@<v0.1:9` fails with `invalid semantic version "v0.1:9" in
// range "<v0.1:9"`, not the `disallowed version string` error the same
// embedded ':' produces in a literal version (`go get
// golang.org/x/mod@v0.1:9`). Without this exclusion, a legitimate range
// query like `@<v1.2.3` — valid, documented syntax real `go` accepts fine —
// would have been wrongly rejected by this tool as an invalid version string
// before ever being probed, when it isn't one.
func isComparisonVersionQuery(v string) bool {
	return strings.HasPrefix(v, "<") || strings.HasPrefix(v, ">")
}

// resolveTarget parses "module@version" from args, or falls back to reading
// the module path from ./go.mod and the version from the most recent git
// tag — the shape of "just tagged a release, is it live yet?" this tool is
// meant for.
func resolveTarget(args []string) (module, version string, err error) {
	if len(args) > 1 {
		return "", "", fmt.Errorf("expected at most one argument (module@version), got %d", len(args))
	}
	if len(args) == 1 {
		parts := strings.SplitN(args[0], "@", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("argument must be in module@version form, got %q", args[0])
		}
		if strings.ContainsFunc(parts[1], unicode.IsSpace) {
			// Confirmed live: a version query with a leading/trailing/embedded
			// space (e.g. "v0.19.0 ", picked up from a copy-paste, a CI
			// variable, or a file read with the newline only partly
			// stripped) is not rejected up front by cmd/go the way a
			// newline/colon/question-mark is ("disallowed version string") —
			// go percent-encodes the space and sends the request anyway,
			// gets a 404, and ultimately fails since no real tag or branch
			// can ever contain whitespace (git's own ref-name rules forbid
			// it: see git-check-ref-format). Before this check, goproxycheck
			// probed the same malformed query and reported
			// statusNotYetIndexed ("retry in a minute, or use --wait") —
			// under --wait, this polled the full --timeout for a version
			// string that will never be indexed because it was never a real
			// version to begin with, the same shape of waste the
			// statusZipBuildError/statusDeprecated early-break cases already
			// exist to prevent for other permanent, non-timing failures.
			return "", "", fmt.Errorf("version %q contains whitespace, which can never be part of a real module version, tag, or revision — check for a stray space or newline (e.g. from copy-paste, a shell variable, or a file read with a trailing newline)", parts[1])
		}
		if parts[1] == "patch" {
			// Confirmed live against cmd/go's own modload/query.go (Query,
			// case query == "patch": `if current == "" || current == "none"
			// { return nil, &NoPatchBaseError{path} }`): unlike "latest" and
			// "upgrade" (see probe()'s "latest"/"upgrade" handling), the
			// "patch" version query (go.dev/ref/mod#version-queries) is only
			// ever defined *relative to* a version already required by some
			// go.mod — it means "the latest available version with the same
			// major.minor as the version already required." With no existing
			// requirement to be relative to, which is always goproxycheck's
			// situation for a bare `module@version` CLI argument, a real `go
			// get module@patch` fails immediately and unconditionally with
			// `can't query version "patch" of module <path>: no existing
			// version is required` — verified live it does this even with
			// GOPROXY=off and even for a module that doesn't exist at all, so
			// it never even reaches the network, let alone the proxy this
			// tool checks. Before this check, goproxycheck sent "patch" as a
			// literal version string to @v/patch.info, which live testing
			// against proxy.golang.org shows 404s with a bare "not found:
			// invalid version" body (no "unknown revision" marker, so it
			// doesn't match isUnknownRevision either) for every module,
			// healthy or not, and fell through to statusNotYetIndexed —
			// "retry in a minute, or use --wait" for a query that can never
			// succeed no matter how long it's probed, the same shape of
			// doomed-poll bug already fixed for statusZipBuildError,
			// statusMajorVersionMismatch, and statusUnknownRevision.
			return "", "", fmt.Errorf(`version "patch" can only be resolved relative to a version %s already requires in some go.mod — goproxycheck has no such existing-requirement context for a bare module@version argument, and neither does a real 'go get %s@patch' run the same way: it fails immediately with `+"`can't query version \"patch\" of module %s: no existing version is required`"+`, without ever contacting the proxy. Check a concrete version, %s@latest, or %s@upgrade instead (upgrade IS well-defined with no existing requirement: it's equivalent to latest)`, parts[0], parts[0], parts[0], parts[0], parts[0])
		}
		if isComparisonVersionQuery(parts[1]) {
			// A comparison query is only actually well-formed if its operand is
			// itself a valid semantic version — confirmed live (2026-09-28) that
			// `go get golang.org/x/mod@<1.2.3` (missing the required "v" prefix),
			// `@>=badversion`, `@<v1.2.3-` (trailing hyphen with no pre-release
			// identifier), `@<=v1.2.3.4` (four components), and even the bare
			// operator `@<` all fail immediately and unconditionally with
			// `invalid semantic version %q in range %q`, entirely offline, before
			// cmd/go ever contacts a proxy — the same shape of offline rejection
			// isComparisonVersionQuery's own doc comment already documents for a
			// malformed operand containing a disallowed character (`<v0.1:9`).
			//
			// Before this check, resolveTarget let any string starting with '<'/'>'
			// through unconditionally, so a comparison query with an invalid
			// operand reached probe()'s comparisonQuery/resolveComparisonQuery,
			// which correctly declined to resolve it locally (operand isn't valid
			// semver) but then fell back to probing the literal query string
			// against the real proxy — a request that can never succeed, since no
			// real version is ever spelled with a leading '<' or '>' — and fell
			// through to the generic statusNotYetIndexed fallback ("ordinary
			// indexing lag ... retry in a minute, or use --wait"), the same
			// doomed-poll shape already fixed for "patch", whitespace, and
			// disallowed-character versions nearby in this function.
			if _, operand, ok := comparisonQuery(parts[1]); ok && !semver.IsValid(operand) {
				return "", "", fmt.Errorf("version %q is not a valid comparison version query (go.dev/ref/mod#version-queries) — %q is not a valid semantic version, so a real `go get`/`go install` rejects this exact string immediately with `invalid semantic version %q in range %q`, entirely offline, before ever contacting the proxy, so this could never resolve no matter how long you --wait or retry", parts[1], operand, operand, parts[1])
			}
		} else {
			// Confirmed live against cmd/go's own modfetch/proxy.go
			// (proxyRepo.Stat: `encRev, err := module.EscapeVersion(rev); if
			// err != nil { return nil, p.versionError(rev, err) }`, called
			// before any network request for every version/revision query
			// that isn't "latest"/"upgrade"/"patch" or a comparison range —
			// see isComparisonVersionQuery): a version or revision string
			// containing a character golang.org/x/mod/module's fileNameOK
			// disallows (the shell-special set double-quote, single-quote,
			// *, <, >, ?, backtick, and |; the path separators /, :, and
			// backslash; a bare ;; a literal !; a trailing .; or a
			// Windows-reserved element name like NUL/COM1) fails
			// immediately and unconditionally with `invalid version: version
			// %q invalid: disallowed version string`, entirely offline,
			// before cmd/go ever contacts a proxy — verified live
			// (2026-09-27) across all of those shapes: `go get
			// golang.org/x/mod@v0.1:9`, `@v0.1?9`, `@v0.1;9`, `@v0.1*9`,
			// `@v0.1!9`, `@v0.1.0.`, and `@NUL` every one fails this exact
			// way, even against a module path that doesn't exist at all.
			//
			// Before this check, goproxycheck sent a version like this
			// straight to e.get(fmt.Sprintf(".../@v/%s.info",
			// escapePath(checkVersion))) instead. For most of these
			// characters the real proxy 404s with a distinct "bad request:
			// invalid escaped version %q: invalid char %q" body (confirmed
			// live) that matches none of diagnose's specific markers
			// (isZipBuildError, majorVersionMismatchMarker,
			// isUnknownRevision all miss it), so it fell through to the
			// generic statusNotYetIndexed fallback — "ordinary indexing lag
			// ... retry in a minute, or use --wait" — and under --wait
			// polled the full --timeout for a version real `go` rejects
			// outright, offline, on every single invocation, the same
			// shape of waste already fixed for "patch" and whitespace
			// above. For an un-percent-encoded "?" specifically it's worse
			// than a wasted poll: net/url parses the raw "?" in the
			// constructed URL as the start of a query string, so the
			// request actually sent doesn't even reach the intended path —
			// confirmed live that building the URL with version "v0.1?9"
			// and issuing it sends path ".../@v/v0.1" with query "9.info"
			// attached, getting back "bad request: query parameters not
			// allowed" instead of any answer about the version that was
			// actually asked about.
			if _, err := modulepkg.EscapeVersion(parts[1]); err != nil {
				return "", "", fmt.Errorf("version %q is not a valid module version/revision string (%v) — a real `go get`/`go install` rejects this exact string immediately with `invalid version: version %q invalid: disallowed version string`, entirely offline, before ever contacting the proxy, so this could never resolve no matter how long you --wait or retry. Check for a stray character from copy-paste, URL-encoding, or shell quoting (e.g. a colon, question mark, backslash, asterisk, pipe, or quote)", parts[1], err, parts[1])
			}
		}
		return parts[0], parts[1], nil
	}

	module, err = moduleFromGoMod("go.mod")
	if err != nil {
		return "", "", fmt.Errorf("no argument given and couldn't read module path from ./go.mod: %w", err)
	}
	version, err = gitDescribeTag()
	if err != nil {
		return "", "", fmt.Errorf("no argument given and couldn't determine a git tag for HEAD: %w", err)
	}
	return module, version, nil
}

func moduleFromGoMod(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	mod, err := moduleDirective(string(data))
	if err != nil {
		return "", fmt.Errorf("%s %w", path, err)
	}
	return mod, nil
}

// moduleDirective extracts the module path from a go.mod-format body's
// `module` directive. Shared by moduleFromGoMod (reading the local
// ./go.mod) and canonicalModuleNote in diagnose.go (reading the go.mod the
// proxy serves for a resolved version) — same file format, same parsing
// rules, so one implementation covers both instead of drifting apart.
//
// Handles both the single-line form ("module example.com/foo") and the
// parenthesized block form ("module (\n\texample.com/foo\n)"). The latter
// isn't shown in go.dev/ref/mod#go-mod-file-module's prose, but real
// golang.org/x/mod/modfile's own lexer treats "module" as a valid block
// verb exactly like require/replace/tool/exclude (no per-verb exception),
// and modfile.Parse's semantic layer explicitly accepts it — confirmed
// live (2026-09-27): a go.mod written as
//
//	module (
//		example.com/foo/mymodule
//	)
//
//	go 1.24
//
// builds, `go list -m` reports "example.com/foo/mymodule", and `go mod
// tidy` rewrites it to the single-line form (accepted, if unusual, syntax,
// not just a lax-mode tolerance). Before this handled the block form, the
// first line ("module (") matched the single-line branch below with rest
// "(" — not a valid quoted string, so parseModulePath returned the literal
// "(" as the "module path" — and the block's real path line was left
// dangling, matched by nothing, silently ignored. goproxycheck's
// no-argument mode (reading ./go.mod) then probed the bogus module "(" and
// reported module-unknown instead of checking the real module; the same
// bug in canonicalModulePath's use of this function would have misfired a
// bogus wrong-import-path diagnosis had the proxy-served go.mod for a
// resolved version used this style.
//
// Also handles the block form written with no space before the opening
// paren ("module(\n\texample.com/foo\n)") — confirmed live (2026-09-28)
// against both golang.org/x/mod/modfile (Parse and ParseLax agree) and the
// real `go` toolchain (`go list -m`, `go mod verify` on a real go.mod
// written this way) that this parses identically to the spaced form: Go's
// lexer tokenizes "module" and "(" independently of whitespace, exactly
// like it does for every other verb/block-open pair. Before this, the
// no-space line matched neither the block-open check nor the single-line
// path check below (the CutPrefix guard required the very next byte to be
// a space or tab), so it fell through unmatched, the block's real path
// line was never reached, and the function reported "has no 'module'
// directive" for a go.mod the real toolchain reads fine — the same
// dropped-block failure mode as the space-before-paren bug fixed above,
// just one character earlier.
func moduleDirective(data string) (string, error) {
	inBlock := false
	blockMod := ""
	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		if inBlock {
			if stripLineComment(line) == ")" {
				if blockMod == "" {
					return "", fmt.Errorf("has a 'module' block with no path")
				}
				return blockMod, nil
			}
			if mod := parseModulePath(line); mod != "" {
				// A second non-empty line here is malformed (real go
				// errors with "repeated module statement"), so this can't
				// happen against a go.mod real `go` accepts — last one
				// wins is a harmless fallback, matching this function's
				// existing best-effort handling of malformed input.
				blockMod = mod
			}
			continue
		}
		// rest[0] == '(' (no space) is included alongside the ordinary
		// space/tab separator: "module(" is real, accepted go.mod syntax
		// for the block-open line — see this function's doc comment.
		if rest, ok := strings.CutPrefix(line, "module"); ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t' || rest[0] == '(') {
			rest = strings.TrimSpace(rest)
			if stripLineComment(rest) == "(" {
				inBlock = true
				continue
			}
			mod := parseModulePath(rest)
			if mod == "" {
				// e.g. a "module" line whose entire value is a "//"
				// comment (found via mutation testing, run #125: the
				// comment-stripping boundary at index 0 is exercised,
				// but nothing checked what it produces). Silently
				// probing an empty module path would send a malformed
				// request to the proxy instead of a clear error.
				return "", fmt.Errorf("has a 'module' directive with no path: %q", line)
			}
			return mod, nil
		}
	}
	if inBlock {
		return "", fmt.Errorf("has an unterminated 'module' block")
	}
	return "", fmt.Errorf("has no 'module' directive")
}

// stripLineComment strips a trailing "// ..." comment from a go.mod line
// fragment. go.mod's lexer only ever recognizes "//" comments — a "/* */"
// block comment is a parse error ("mod files must use // comments (not /*
// */ comments)", confirmed against golang.org/x/mod/modfile/read.go) — so
// this is the only comment form parseModulePath/moduleDirective ever need
// to account for.
func stripLineComment(s string) string {
	if i := strings.Index(s, "//"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	return s
}

// parseModulePath cleans up the raw text after "module " on a go.mod module
// line (or a line inside its parenthesized block form): strips a trailing
// "//" line comment (valid go.mod syntax — `go list -m` ignores it, but a
// naive TrimSpace would fold it straight into the module path and send
// goproxycheck probing a bogus URL) and unquotes the path if it's written
// as a quoted Go string literal (also valid go.mod syntax, just rarer).
func parseModulePath(s string) string {
	s = stripLineComment(s)
	if unquoted, err := strconv.Unquote(s); err == nil {
		return unquoted
	}
	return s
}

// firstGoproxyEntry returns the first comma/pipe-separated entry of the
// local `go` command's effective GOPROXY. Reads it via `go env GOPROXY`
// rather than os.Getenv("GOPROXY") directly, so a value persisted with `go
// env -w GOPROXY=...` is picked up too, not just an explicit env var — `go
// env` is the authoritative source either way.
//
// GOPROXY may be a comma- or pipe-separated list of sources tried in order,
// but only the *first* entry matters for both callers below: confirmed live
// that "off" is a definitive stop with no fallback to later entries
// (GOPROXY=off,direct and GOPROXY=off|direct both fail immediately with
// "module lookup disabled by GOPROXY=off", while GOPROXY=direct,off
// succeeds via direct and never reaches the off entry at all), and a
// comma-separated list only falls through to a later entry when the
// earlier one 404s/410s (a pipe-separated list falls through on any
// error) — either way, the first entry is what `go install` tries first
// and is what determines whether this tool's proxy.golang.org probe below
// even applies.
//
// Delegates to parseGoproxyChain (below) rather than re-splitting the raw
// string itself: a naive "split on the first ,/|" used to treat a
// leading/interior empty entry (e.g. `GOPROXY="$UNSET_VAR,off"`) as the
// first entry being "", never reaching "off" at all — verified live that
// real `go` skips blank entries instead, so `GOPROXY=",off"` disables
// lookups exactly like `GOPROXY=off` does, and this tool used to
// misreport such a config as fully working.
func firstGoproxyEntry() string {
	out, err := exec.Command("go", "env", "GOPROXY").Output()
	if err != nil {
		return "" // best-effort: don't block the real check on this
	}
	entries, _ := parseGoproxyChain(strings.TrimSpace(string(out)))
	if len(entries) == 0 {
		return ""
	}
	return entries[0]
}

// localGoproxyOff reports whether the local `go` command's effective
// GOPROXY disables module downloads outright (its first entry is "off").
func localGoproxyOff() bool {
	return firstGoproxyEntry() == "off"
}

// localGoproxyNonPublic reports whether the local `go` command's effective
// GOPROXY resolves to something other than the public proxy.golang.org this
// tool actually probes — either "direct" (skip the proxy protocol
// entirely and fetch straight from the VCS host) or a custom proxy URL (a
// private mirror: Athens, Artifactory, and regional mirrors like
// goproxy.cn are all in real, common use). Returns ("", "") when the first
// entry is the public proxy (the default, "https://proxy.golang.org,direct")
// or empty/unreadable, in which case the normal probe below applies as-is.
//
// Confirmed live: with GOPROXY pointed at a working custom proxy serving a
// module the public proxy has never heard of, `go mod download` (the same
// operation `go install`/`go get` depend on) succeeds in that environment,
// while probing proxy.golang.org unconditionally — what this tool did
// before this check existed — reported a false "module-unknown" telling
// the user to check for a typo, when nothing was wrong at all; it was just
// asking the wrong proxy. Mirrors the localGoproxyOff/localModulePrivate
// short-circuits above: local config makes the public-proxy probe not
// reflect what `go install` will actually do, so this reports that
// honestly instead of guessing at a private proxy's own auth/protocol
// (which this tool has no way to know).
func localGoproxyNonPublic() (kind, value string) {
	switch first := firstGoproxyEntry(); first {
	case "", "off":
		return "", "" // "" (unreadable): fall through to the normal probe; "off" is handled separately above
	case "direct":
		return "direct", ""
	default:
		switch normalizeGoproxyURL(first) {
		case defaultProxyBase, defaultProxyBase + "/":
			return "", ""
		default:
			return "custom", first
		}
	}
}

// normalizeGoproxyURL mirrors the implicit-scheme rule cmd/go's own
// proxyList (modfetch/proxy.go) applies to each GOPROXY entry before ever
// using it: "anything containing the string ':/' or matching an absolute
// file path must be a complete URL. For all other paths, implicitly add
// 'https://'." `go env GOPROXY` itself echoes back the raw, un-normalized
// config string (confirmed live: GOPROXY=proxy.golang.org makes `go env
// GOPROXY` print "proxy.golang.org", not "https://proxy.golang.org"), so
// firstGoproxyEntry/publicProxyFallback comparing that raw string directly
// against defaultProxyBase missed this real normalization step entirely.
//
// Confirmed live (2026-09-27): with GOPROXY=proxy.golang.org (no scheme —
// a natural way to write it by analogy to GOPRIVATE/GONOPROXY's bare-host
// patterns) or GOPROXY=proxy.golang.org/ (same, with a trailing slash),
// `go mod download -x golang.org/x/mod@v0.19.0` fetches from
// https://proxy.golang.org/... — byte-for-byte the same public proxy the
// default config uses, not some distinct scheme-less endpoint. Before
// this fix, localGoproxyNonPublic/publicProxyFallback classified either
// value as an opaque "custom" proxy this tool "has no way to know" how to
// check, when the probe below is actually exactly right.
func normalizeGoproxyURL(url string) string {
	if strings.ContainsAny(url, ".:/") && !strings.Contains(url, ":/") && !filepath.IsAbs(url) && !path.IsAbs(url) {
		return "https://" + url
	}
	return url
}

// parseGoproxyChain replicates cmd/go's own GOPROXY-list walk (proxyList in
// cmd/go/internal/modfetch/proxy.go, verified directly against that source)
// far enough to recover entry positions and separators: "," and "|" both
// separate entries ("|" meaning "fall back to the next entry on *any*
// error," not just 404/410), and both "off" and "direct" terminate the walk
// outright — real go's own comment says it ignores every entry after either
// "for forward-compatibility," so an entry placed after one is never
// actually reachable. seps[i] is the separator that precedes entries[i+1].
func parseGoproxyChain(raw string) (entries []string, seps []byte) {
	pending := raw
	for pending != "" {
		var url string
		var trailingSep byte
		if i := strings.IndexAny(pending, ",|"); i >= 0 {
			url, trailingSep, pending = pending[:i], pending[i], pending[i+1:]
		} else {
			url, pending = pending, ""
		}
		url = strings.TrimSpace(url)
		if url == "" {
			continue
		}
		entries = append(entries, url)
		if url == "off" || url == "direct" {
			break
		}
		if pending != "" {
			seps = append(seps, trailingSep)
		}
	}
	return entries, seps
}

// publicProxyFallback reports whether the local `go` command's effective
// GOPROXY reaches the public proxy.golang.org this tool actually probes as
// a *later* entry in the chain, rather than the first — the officially
// documented "https://corp.example.com,https://proxy.golang.org" pattern
// (go.dev/ref/mod#goproxy-protocol's own worked example): a company proxy
// for private modules, falling back to the public proxy for everything
// else. Before this existed, localGoproxyNonPublic only ever inspected the
// chain's first entry (see firstGoproxyEntry's doc comment), so this exact
// documented and common pattern made the tool claim total ignorance ("it
// has no way to know... can't tell you whether it's ready there") even
// though the public-proxy probe is still exactly the right thing to check
// — confirmed live: with a stand-in proxy that 404s on everything as the
// first entry and real proxy.golang.org as the second, `go mod download -x`
// falls straight through (404 on the first entry, per go's own documented
// rule) and succeeds via the public entry, for an ordinary public module.
//
// Returns ok=false when the public proxy isn't reachable via the chain at
// all (not present, or only present after an "off"/"direct" that already
// terminates the walk first) — that case keeps the existing "custom,
// can't check" behavior. precedingCustom lists the entries tried before
// reaching the public proxy (for the caveat message); anyErrorFallback is
// true when the separator immediately before the public entry is "|"
// (fallback on any error) rather than "," (fallback only on 404/410).
func publicProxyFallback() (precedingCustom []string, anyErrorFallback, ok bool) {
	out, err := exec.Command("go", "env", "GOPROXY").Output()
	if err != nil {
		return nil, false, false
	}
	entries, seps := parseGoproxyChain(strings.TrimSpace(string(out)))
	for i, url := range entries {
		normalized := normalizeGoproxyURL(url)
		if normalized != defaultProxyBase && normalized != defaultProxyBase+"/" {
			continue
		}
		if i == 0 {
			return nil, false, false // first entry is already public; not this case
		}
		return entries[:i], seps[i-1] == '|', true
	}
	return nil, false, false
}

// fallbackCaveat builds the explanatory prefix used when the public proxy
// is only reachable in the local GOPROXY chain after one or more custom
// entries (see publicProxyFallback) — it describes exactly what condition
// has to hold for the probe result that follows to actually apply.
func fallbackCaveat(precedingCustom []string, anyErrorFallback bool) string {
	trigger := "reports the module not found there (404/410)"
	if anyErrorFallback {
		trigger = "fails for any reason (a `|` separator precedes proxy.golang.org in your chain, so any error triggers fallback there, not just 404/410)"
	}
	return fmt.Sprintf(
		"Note: your local `GOPROXY` tries %s before proxy.golang.org, in that order — the documented go.dev/ref/mod#goproxy-protocol fallback-chain pattern (e.g. a company proxy for private modules, falling back to the public proxy for everything else). "+
			"This tool can only check proxy.golang.org directly, not your own %s, so the result below only applies once every entry ahead of it %s.",
		strings.Join(precedingCustom, ", "), strings.Join(precedingCustom, ", "), trigger)
}

// localModulePrivate reports whether module matches the local `go`
// command's effective GONOPROXY pattern list, returning the specific
// pattern that matched. GONOPROXY defaults to GOPRIVATE's value when not
// set explicitly (confirmed live: `GOPRIVATE=x go env GONOPROXY` prints
// x, with no GONOPROXY set at all) — `go env GONOPROXY` reports that
// already-resolved effective value either way, so reading it alone covers
// both GOPRIVATE and an explicit GONOPROXY override without needing to
// check both separately.
func localModulePrivate(module string) (bool, string) {
	out, err := exec.Command("go", "env", "GONOPROXY").Output()
	if err != nil {
		return false, "" // best-effort: don't block the real check on this
	}
	patterns := splitPatterns(strings.TrimSpace(string(out)))
	for _, p := range patterns {
		if matchesPrefixPattern(p, module) {
			return true, p
		}
	}
	return false, ""
}

// localGovcsPrivate reports whether module matches the local `go` command's
// effective GOPRIVATE pattern list — deliberately GOPRIVATE alone, NOT
// GONOPROXY, even though GONOPROXY defaults to GOPRIVATE's value when unset
// (see localModulePrivate). Real cmd/go's own GOVCS "public"/"private"
// pattern classification (internal/vcs/vcs.go's checkGOVCS: `private :=
// module.MatchPrefixPatterns(cfg.GOPRIVATE, root)`) always checks GOPRIVATE
// specifically, regardless of *why* a direct VCS fetch is happening —
// whether GONOPROXY matched (the localModulePrivate branch in run()) or
// GOPROXY resolved to "direct" outright (the other branch), using an
// unrelated GOPRIVATE/GONOPROXY setting either way.
//
// Confirmed live (2026-09-27) both directions, with GOPRIVATE and GONOPROXY
// deliberately set to different, non-overlapping patterns so the two checks
// can't coincidentally agree: with GOPRIVATE=nonmatching.example/* and
// GONOPROXY=github.com/golang/protobuf (so GONOPROXY forces a direct fetch
// but GOPRIVATE does NOT match it), GOVCS="public:off,private:git" — `go
// mod download -x github.com/golang/protobuf@v1.5.0` fails with "GOVCS
// disallows using git for *public* github.com/golang/protobuf", i.e. real
// go classified it public despite GONOPROXY's match. And with
// GOPROXY=direct, GOPRIVATE=github.com/golang/protobuf, GONOPROXY=
// nonmatching.example/*, GOVCS="private:off,public:git" — the same command
// fails with "GOVCS disallows using git for *private* github.com/golang/
// protobuf", i.e. real go classified it private purely off GOPRIVATE even
// though the fetch went direct for an unrelated reason (GOPROXY=direct, not
// a GONOPROXY match) and GONOPROXY itself didn't match.
//
// Before this existed, run() passed a hardcoded `true` (in the
// localModulePrivate branch) or `false` (in the GOPROXY=direct branch) to
// localGovcsAllowsGit instead of this — so a GOPRIVATE/GONOPROXY split
// config like either case above made this tool evaluate the wrong GOVCS
// rule (private:git instead of public:off, or public:git instead of
// private:off) and report the fetch as fine ("will fetch it directly, no
// problem") when a real `go mod download`/`go install` fails outright with
// "GOVCS disallows using git for ...".
func localGovcsPrivate(module string) bool {
	out, err := exec.Command("go", "env", "GOPRIVATE").Output()
	if err != nil {
		return false // best-effort: don't block the real check on this
	}
	return matchesAnyPattern(module, splitPatterns(strings.TrimSpace(string(out))))
}

// govcsWhat renders the "public"/"private" word real `go`'s own "GOVCS
// disallows using %s for %s %s" error message uses (internal/vcs/vcs.go's
// checkGOVCS), given the same GOPRIVATE-derived classification
// localGovcsPrivate computes.
func govcsWhat(private bool) string {
	if private {
		return "private"
	}
	return "public"
}

// localGovcsAllowsGit reports whether the local `go` command's effective
// GOVCS setting permits a direct git fetch of module, given whether module
// is already known to be private per real go's own GOPRIVATE-based
// classification (see localGovcsPrivate — NOT localModulePrivate, which
// mirrors GONOPROXY instead and answers a different question). Delegates
// the actual rule-matching to govcsAllowsGit (govcs.go); this wrapper only
// handles reading `go env GOVCS`, matching the localModulePrivate/
// localGoproxyOff shape above.
//
// Callers must only invoke this for a module whose direct-fetch VCS is
// known to be git — this tool only has that certainty for github.com-hosted
// modules (see githubRepoPattern's doc comment on why other hosts are out
// of scope), so it's gated on that at the call site in run(), not here.
func localGovcsAllowsGit(module string, private bool) bool {
	out, err := exec.Command("go", "env", "GOVCS").Output()
	if err != nil {
		return true // best-effort: don't block the real check on this
	}
	return govcsAllowsGit(module, private, strings.TrimSpace(string(out)))
}

// localGovcsConfigError reports the parse error the local `go` command's
// effective GOVCS setting would raise (see govcsConfigError), or nil if it's
// well-formed or unreadable (best-effort: don't block the real check on a
// failed `go env` invocation). Callers should check this before
// localGovcsAllowsGit: a malformed GOVCS breaks every direct-VCS fetch
// outright, a different and more fundamental failure than "GOVCS disallows
// using git for this module" (which presumes the config itself parsed).
func localGovcsConfigError() error {
	out, err := exec.Command("go", "env", "GOVCS").Output()
	if err != nil {
		return nil
	}
	return govcsConfigError(strings.TrimSpace(string(out)))
}

// sumdbName extracts the checksum-database name a raw GOSUMDB value
// resolves to, mirroring the parsing cmd/go itself does in
// modfetch/sumdb.go's dbDial before it ever opens a connection: $GOSUMDB is
// "off", a bare name, or "name[+key] [url]" (see `go help goproxy`) — the
// optional key/url fields say *how* to reach/verify the database, not
// *which* one it is, so only the first field (before the first space, then
// before the first "+") matters for identifying it. "sum.golang.google.cn"
// is a documented special case cmd/go rewrites internally to "sum.golang.org
// https://sum.golang.google.cn" (a China-reachable mirror of the *same*
// public tree, not a different database) before this same field-splitting
// — confirmed against modfetch/sumdb.go's dbDial source — so it resolves to
// "sum.golang.org" here too, matching real behavior instead of being
// mistaken for an unrelated custom database.
func sumdbName(gosumdb string) string {
	if gosumdb == "" || gosumdb == "sum.golang.google.cn" {
		return "sum.golang.org"
	}
	name, _, _ := strings.Cut(gosumdb, " ")
	name, _, _ = strings.Cut(name, "+")
	return name
}

// knownGOSUMDB mirrors cmd/go's own modfetch/key.go verbatim: the only bare
// name real go resolves to a known checksum-database verifier key without
// the caller spelling out the full "name+hash+base64key" form. Any other
// bare name that isn't itself a well-formed verifier key fails to parse
// below, exactly like it does in real go.
var knownGOSUMDB = map[string]string{
	"sum.golang.org": "sum.golang.org+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8",
}

// gosumdbConfigError reports the error real go's own dbDial (modfetch/
// sumdb.go, verified directly against that source) raises for a non-"off"
// $GOSUMDB value before it ever opens a connection to any checksum
// database, public or custom. sumdbName (above) only extracts *which*
// database a raw GOSUMDB value names — it doesn't check that the value
// actually parses as one, so a bare custom hostname (a very natural,
// plausible way to misconfigure this by analogy to GOPROXY's own bare-host
// GOPROXY=proxy.golang.org syntax — see normalizeGoproxyURL — but GOSUMDB's
// own format has no such implicit-scheme fallback) makes sumdbName return
// that hostname as if it named a real, working custom database.
//
// Confirmed live (2026-09-28): `GOSUMDB=sum.example.com go install
// golang.org/x/text@v0.14.0` (a module not already in any local go.sum, so
// verification is actually attempted) fails outright with "invalid GOSUMDB:
// malformed verifier id" — sumdbName("sum.example.com") returns
// "sum.example.com" unchanged, which localSumdbSkipped used to treat as "a
// real custom database is in use, so the public sumdb's lag is irrelevant,
// this is ready" (see its own doc comment, whose only confirmed-live case
// was a deliberately well-formed key+URL pair). That's backwards for this
// shape: the real command doesn't quietly use a different database, it
// refuses to install the module at all, for any module, until GOSUMDB is
// fixed. Also confirmed live: "invalid GOSUMDB: too many fields" for a
// three-field value, and "invalid GOSUMDB: invalid verifier hash" for a
// name+key pair whose embedded hash doesn't match its own key (a copy-paste
// truncation/corruption) — both go through the identical note.NewVerifier
// parse this mirrors.
func gosumdbConfigError(gosumdb string) error {
	if gosumdb == "sum.golang.google.cn" {
		gosumdb = "sum.golang.org https://sum.golang.google.cn"
	}
	if gosumdb == "off" {
		return nil
	}
	fields := strings.Fields(gosumdb)
	if len(fields) == 0 {
		return fmt.Errorf("missing GOSUMDB")
	}
	if len(fields) > 2 {
		return fmt.Errorf("invalid GOSUMDB: too many fields")
	}
	key := fields[0]
	if k, ok := knownGOSUMDB[key]; ok {
		key = k
	}
	verifier, err := note.NewVerifier(key)
	if err != nil {
		return fmt.Errorf("invalid GOSUMDB: %v", err)
	}
	if err := validSumdbName(verifier.Name()); err != nil {
		return err
	}
	if len(fields) == 2 {
		if _, err := url.Parse(fields[1]); err != nil {
			return fmt.Errorf("invalid GOSUMDB URL: %v", err)
		}
	}
	return nil
}

// validSumdbName mirrors the extra host-shape validation cmd/go's own
// dbDial (modfetch/sumdb.go) performs on a verifier key's embedded name
// after note.NewVerifier has already accepted the key as well-formed.
// note.NewVerifier's own name check (isValidName) only rejects whitespace,
// invalid UTF-8, and a literal "+" — it happily accepts a name that isn't a
// valid URL host at all, which dbDial separately, and unconditionally,
// rejects before ever dialing anything: `direct, err := url.Parse("https://"
// + name); if err != nil || strings.HasSuffix(name, "/") || *direct !=
// (url.URL{Scheme: "https", Host: direct.Host, Path: direct.Path, RawPath:
// direct.RawPath}) || direct.RawPath != "" || direct.Host == "" { return ...
// "invalid sumdb name (must be host[/path])" ... }`.
//
// Confirmed live (2026-09-28): a GOSUMDB key generated with name
// "example.com/" (a plausible copy-paste mistake — keeping a trailing slash
// from a URL when only a bare host[/path] is wanted) passes note.NewVerifier
// fine — isValidName has no opinion on a trailing slash — but `go get
// golang.org/x/text@v0.14.0` with GOSUMDB set to that key, against a fresh
// GOMODCACHE so verification is actually attempted, fails outright before
// ever making a network request:
//
//	go: golang.org/x/text@v0.14.0: verifying module: invalid sumdb name
//	(must be host[/path]): example.com/ {Scheme:https ... Path:/ ...}
//
// Before this check, gosumdbConfigError returned nil for that exact value —
// note.NewVerifier was the only validation performed — so goproxycheck
// reported the local GOSUMDB config as fine (and, via localSumdbSkipped/
// localGosumdbConfigError's callers in run(), could report a module ready)
// when a real `go install`/`go get` refuses to run at all until GOSUMDB is
// fixed, for any module needing verification.
func validSumdbName(name string) error {
	direct, err := url.Parse("https://" + name)
	if err != nil || strings.HasSuffix(name, "/") || direct.Host == "" || direct.RawPath != "" ||
		*direct != (url.URL{Scheme: "https", Host: direct.Host, Path: direct.Path, RawPath: direct.RawPath}) {
		return fmt.Errorf("invalid GOSUMDB: invalid sumdb name (must be host[/path]): %s", name)
	}
	return nil
}

// localGosumdbConfigError reports the error a real `go install`/`go get`
// would raise from a malformed local $GOSUMDB the moment it actually needs
// to verify module against the checksum database — nil when verification
// would be skipped anyway (GOSUMDB=off, or module matches GONOSUMDB, which
// defaults to GOPRIVATE's value when unset — mirroring real go's own
// useSumDB check in modfetch/sumdb.go: `cfg.GOSUMDB != "off" &&
// !module.MatchPrefixPatterns(cfg.GONOSUMDB, mod.Path)`) or when GOSUMDB
// parses fine, whether or not it names the public sum.golang.org. Best-
// effort like its sibling localGovcsConfigError: a failed `go env` call
// doesn't block the real check.
func localGosumdbConfigError(module string) error {
	out, err := exec.Command("go", "env", "GOSUMDB").Output()
	if err != nil {
		return nil
	}
	gosumdb := strings.TrimSpace(string(out))
	if gosumdb == "off" {
		return nil
	}
	if nonsumOut, err := exec.Command("go", "env", "GONOSUMDB").Output(); err == nil {
		if matchesAnyPattern(module, splitPatterns(strings.TrimSpace(string(nonsumOut)))) {
			return nil
		}
	}
	return gosumdbConfigError(gosumdb)
}

// localSumdbSkipped reports whether the local `go` command's effective
// config means it will never consult the public sum.golang.org for module
// at all — GOSUMDB is explicitly set to "off" (globally disables checksum
// database verification, a real setting used by CI/corporate environments
// that trust their proxy or run fully offline), GOSUMDB names a custom
// checksum database instead of the public one (a real, documented setting
// — e.g. an org running its own sumdb to avoid leaking module paths/
// versions to Google — see sumdbName), or GONOSUMDB (which defaults to
// GOPRIVATE's value when unset, confirmed live the same way GONOPROXY does)
// has a pattern matching module.
//
// Confirmed live with `go mod download -x` in an isolated GOMODCACHE: with
// GOSUMDB=off, no sum.golang.org (or proxy.golang.org/sumdb/...) request
// appears in the trace at all, where the default config clearly shows both.
// With a custom GOSUMDB (a valid verifier key naming a different database,
// plus an explicit URL — confirmed with a local HTTP server logging every
// request), every sumdb request went to the custom server; none went to
// sum.golang.org at all, so a sumdb-lag verdict from the public database
// is exactly as meaningless here as under GOSUMDB=off — it's naming a
// different database, not an absent one, but the public one's state still
// can't block a real `go install` in this environment. With GONOSUMDB
// matching a module and GOPRIVATE left unset, the module is still fetched
// normally through proxy.golang.org (confirmed live: the .info/.zip
// fetches go through proxy.golang.org as usual) — only the sumdb lookup is
// skipped. That's different from localModulePrivate (which mirrors
// GONOPROXY and already short-circuits the whole proxy probe earlier in
// run()): a GONOSUMDB-only match still needs the normal proxy-reachability
// check, it just means a sumdb-lag verdict from this tool wouldn't
// actually block a real `go install` in this environment, since
// sum.golang.org's state is irrelevant once the local config has already
// decided not to consult it. Without this, goproxycheck told a
// GOSUMDB=off (or custom-GOSUMDB) user to "retry shortly" for a module
// that was already installable right now.
func localSumdbSkipped(module string) (skipped bool, reason string) {
	if out, err := exec.Command("go", "env", "GOSUMDB").Output(); err == nil {
		gosumdb := strings.TrimSpace(string(out))
		if gosumdb == "off" {
			return true, "GOSUMDB=off"
		}
		if name := sumdbName(gosumdb); name != "sum.golang.org" {
			return true, fmt.Sprintf("custom GOSUMDB %q", name)
		}
	}
	out, err := exec.Command("go", "env", "GONOSUMDB").Output()
	if err != nil {
		return false, "" // best-effort: don't block the real check on this
	}
	patterns := splitPatterns(strings.TrimSpace(string(out)))
	for _, p := range patterns {
		if matchesPrefixPattern(p, module) {
			return true, fmt.Sprintf("GONOSUMDB pattern %q", p)
		}
	}
	return false, ""
}

// gitDescribeTag determines the release tag for HEAD by listing every tag
// that points exactly at it, rather than delegating that choice to `git
// describe --tags --exact-match HEAD` — confirmed live that when more than
// one tag points at the same commit (a real pattern: release automation and
// CI commonly add a second marker tag such as "latest", "stable", or
// "ci-verified" on the same commit as the semver release tag, and a mistaken
// re-tag left in place does the same), `git describe` silently returns just
// one of them, chosen by an internal, undocumented tie-break — empirically
// the alphabetically-first tag ref, completely unrelated to which one is
// actually the semver release — with no error, no warning, and nothing in
// its output to reveal a choice was even made.
//
// Before this existed, goproxycheck's no-argument mode (the shipped GitHub
// Action's default invocation, with no `args` input set) could silently
// probe a non-version marker tag sitting on the exact same commit as the
// real release instead of the release tag itself: verified live with a repo
// carrying both `v1.6.0` and `ci-verified` on HEAD, `git describe --tags
// --exact-match HEAD` returned `ci-verified`, and goproxycheck reported
// "not-yet-indexed... retry in a minute, or use --wait" for a tag that will
// never be indexed (it isn't a module version), while the real v1.6.0
// release — fully live and ready right now — was never checked at all;
// under --wait this polls uselessly to timeout every time.
//
// When there's more than one tag at HEAD, this now picks the one valid Go
// module version among them (via semver.IsValid, the same check `go`
// itself requires of a module version) rather than guessing, and returns a
// clear error asking for an explicit module@version instead of silently
// choosing when that's still ambiguous (none, or more than one, look like a
// real version).
func gitDescribeTag() (string, error) {
	out, err := exec.Command("git", "tag", "--points-at", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git tag --points-at HEAD: %w (HEAD may not be tagged — pass module@version explicitly)", err)
	}
	var tags []string
	for _, t := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if t != "" {
			tags = append(tags, t)
		}
	}
	if len(tags) == 0 {
		return "", fmt.Errorf("no tag points at HEAD (HEAD may not be tagged — pass module@version explicitly)")
	}
	if len(tags) == 1 {
		return tags[0], nil
	}

	var versionTags []string
	for _, t := range tags {
		if semver.IsValid(t) {
			versionTags = append(versionTags, t)
		}
	}
	if len(versionTags) == 1 {
		return versionTags[0], nil
	}
	return "", fmt.Errorf("HEAD has more than one tag (%s) and it's ambiguous which one is the release version — pass module@version explicitly", strings.Join(tags, ", "))
}
