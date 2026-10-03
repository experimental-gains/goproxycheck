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
	// resolvedVersion carries the concrete version a query (e.g. "latest",
	// a partial version like "v0.19", or a revision identifier) actually
	// resolved to, for the --json output below — see its use there for why.
	// Stays "" for every branch that short-circuits before probing at all
	// (GOPROXY=off, a private module, etc.), since none of those resolve
	// anything.
	var resolvedVersion string
	if localGo111ModuleOff() {
		// Checked before every other local-config branch below, including
		// localGoproxyEmptyListError/localGoproxyMalformedEntryError: see
		// localGo111ModuleOff's doc comment for the live confirmation that
		// GO111MODULE=off preempts all of them — a real `go install`/`go get`
		// never gets far enough to parse GOPROXY, GOVCS, GOAUTH, or GOSUMDB,
		// or to consult GOPRIVATE/GONOPROXY, when module mode itself is off.
		d = diagnosis{statusGo111moduleOffLocally, fmt.Sprintf(
			"your local `GO111MODULE` is set to `off` (via env var or `go env -w`), which disables module mode outright — `go install`/`go get` fails immediately and unconditionally with `go: modules disabled by GO111MODULE=off; see 'go help modules'` the moment it's invoked, entirely offline, before %s@%s (or anything else, including a malformed module path) is ever checked against proxy.golang.org, sum.golang.org, or any VCS host. "+
				"That's your machine's own config, not a proxy-availability problem. Module mode has been the default since Go 1.16 for any directory with a go.mod — unset `GO111MODULE` (or set it to `on`) to use it.",
			module, version)}
	} else if raw, goproxyErr := localGoproxyEmptyListError(); goproxyErr != nil {
		// Checked before every other local-config branch below, including
		// localModulePrivate: confirmed live this failure preempts even the
		// GOPRIVATE/GONOPROXY direct-fetch path (see
		// localGoproxyEmptyListError's doc comment) — a real `go install`
		// never even gets far enough to consult GOPRIVATE, GOVCS, or any
		// proxy/VCS host at all when GOPROXY itself parses to zero entries.
		d = diagnosis{statusGoproxyEmptyLocally, fmt.Sprintf(
			"your local `GOPROXY` is set to %q (via env var or `go env -w`), which parses to zero actual proxy entries once blank entries are dropped — `go install`/`go get` fails outright with `GOPROXY list is not the empty string, but contains no entries` the moment it needs to resolve %s@%s, entirely offline, before consulting any proxy, VCS host, or your GOPRIVATE/GONOPROXY/GOVCS config at all. "+
				"That's your machine's own config, not a proxy-availability problem. Fix `GOPROXY` (see `go help goproxy`) — e.g. `GOPROXY=https://proxy.golang.org,direct` for the default public config, or unset it entirely.",
			raw, module, version)}
	} else if raw, badEntry, entryErr := localGoproxyMalformedEntryError(); entryErr != nil {
		// Checked at the same priority as localGoproxyEmptyListError just
		// above, and for the identical reason: see goproxyMalformedEntryError's
		// doc comment for why this preempts even a matching GOPRIVATE or a
		// GOPROXY resolving to "direct" — real cmd/go's own proxyList parses
		// and validates every entry in the whole GOPROXY string eagerly, in one
		// process-wide pass, before TryProxies ever attempts the first entry
		// (public, custom, or the "noproxy"/"direct" pseudo-entries a private-
		// module match or GOPROXY=direct resolve through) — so one malformed
		// entry anywhere in the list, even one that's only ever meant to be an
		// unreachable fallback after a perfectly healthy first entry, Fatals
		// every module operation in the process outright.
		d = diagnosis{statusGoproxyMalformedLocally, fmt.Sprintf(
			"your local `GOPROXY` is set to %q (via env var or `go env -w`), which contains a malformed entry (%q) — `go install`/`go get` fails outright with `%v` the moment it needs to resolve %s@%s, entirely offline, before consulting any proxy (even a perfectly healthy one listed earlier in the same chain), VCS host, or your GOPRIVATE/GONOPROXY/GOVCS config at all. "+
				"That's your machine's own config, not a proxy-availability problem. Fix `GOPROXY` (see `go help goproxy`) — every entry needs an explicit `https://`/`http://`/`file://` scheme, or at least a dot, colon, or slash so `go` can infer `https://` for you.",
			raw, badEntry, entryErr, module, version)}
	} else if localGoflagsModVendor() {
		// Checked ahead of localModulePrivate/localGoproxyOff/the direct-fetch
		// and public-proxy-probe branches below: see localGoflagsModVendor's
		// doc comment for the live confirmation that `-mod=vendor` (via
		// GOFLAGS, env var or `go env -w`) Fatals real `go install`/`go get`
		// with `cannot query module due to -mod=vendor` regardless of a
		// matching GOPRIVATE/GONOPROXY or GOPROXY=off — the query mechanism
		// itself is disabled process-wide, before any of those are even
		// consulted. Checked after localGoproxyEmptyListError/
		// localGoproxyMalformedEntryError, which still Fatal with their own,
		// different error ahead of this one.
		d = diagnosis{statusGoflagsModVendorLocally, fmt.Sprintf(
			"your local `GOFLAGS` includes `-mod=vendor` (via env var or `go env -w`), which disables the module-query mechanism outright — `go install`/`go get` fails outright with `cannot query module due to -mod=vendor` the moment it needs to resolve %s@%s, entirely offline, before consulting proxy.golang.org, sum.golang.org, any VCS host, or your GOPRIVATE/GONOPROXY/GOPROXY config at all. "+
				"That's your machine's own config, not a proxy-availability problem. Fix `GOFLAGS` (see `go help environment`) — drop the `-mod=vendor` entry, or use `-mod=mod`/`-mod=readonly` if you still want the main module's own build mode controlled explicitly.",
			module, version)}
	} else if priv, pattern := localModulePrivate(module); priv {
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
			// The quoted error text below names githubRepoRoot(module), not
			// module itself: real cmd/go's own "GOVCS disallows using %s for
			// %s %s" error (checkGOVCS) names the VCS-resolved repo root, the
			// same truncated value localGovcsPrivate/localGovcsAllowsGit now
			// classify against (see localGovcsPrivate's doc comment) — for a
			// module with a major-version suffix or monorepo subdirectory,
			// that's shorter than module itself, and this message used to
			// name the untruncated module here, which wouldn't match what a
			// user pastes from their own terminal.
			d = diagnosis{statusGovcsDisallowedLocally, fmt.Sprintf(
				"your local `GOPRIVATE`/`GONOPROXY` config matches %s via the pattern %q, so `go install`/`go get` would normally fetch it directly from its VCS host — but your local `GOVCS` setting disallows git for this (%[3]s) module, so the real command fails outright with `GOVCS disallows using git for %[3]s %[4]s; see 'go help vcs'` instead of succeeding. "+
					"That's your machine's own config, not a proxy-availability problem, and this tool's proxy/sumdb checks don't apply to it either way. Adjust `GOVCS` (or `go env -w GOVCS=...`) if you meant to allow this.",
				module, pattern, govcsWhat(govcsPrivate), githubRepoRoot(module))}
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
			// See the equivalent case in the private-module branch above for
			// why githubRepoRoot(module), not module, is named in the quoted
			// error text.
			d = diagnosis{statusGovcsDisallowedLocally, fmt.Sprintf(
				"your local `GOPROXY` resolves to `direct` (via env var or `go env -w`), so `go install`/`go get` would normally fetch %s@%s straight from its VCS host — but your local `GOVCS` setting disallows git for this (%[3]s) module, so the real command fails outright with `GOVCS disallows using git for %[3]s %[4]s; see 'go help vcs'` instead of succeeding. "+
					"That's your machine's own config, not a proxy-availability problem, and this tool's proxy/sumdb checks don't apply to it either way. Adjust `GOVCS` (or `go env -w GOVCS=...`) if you meant to allow this.",
				module, version, govcsWhat(govcsPrivate), githubRepoRoot(module))}
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
	} else if goauthErr := localGoAuthConfigError(); goauthErr != nil {
		// Checked here, immediately ahead of the branch that actually probes
		// the public proxy over HTTPS: see goAuthConfigError's doc comment
		// for why a malformed GOAUTH Fatals real `go` before its first HTTPS
		// request of the whole process, regardless of module health — and
		// why that's scoped to exactly this branch rather than every branch
		// above it. Confirmed live (2026-10-02) that an identical malformed
		// GOAUTH has zero effect on the GOPRIVATE/GONOPROXY-direct-fetch and
		// GOPROXY=direct branches above: a direct git fetch of a github.com
		// module shells out to the `git` binary via
		// cmd/go/internal/modfetch/codehost, never touching cmd/go's own
		// HTTPS client (and so never touching GOAUTH) at all — confirmed by
		// running `GOAUTH=off;netrc GOPRIVATE=... GOPROXY=direct go install
		// github.com/golang/example@latest`, which succeeds identically with
		// or without the malformed GOAUTH. GOPROXY=off similarly never
		// issues any HTTP request at all (an immediate, different Fatal of
		// its own), so GOAUTH is moot there too. The "custom, no fallback"
		// branch just above is deliberately left unchanged: this tool
		// already declines to probe a non-public custom proxy at all (it has
		// no way to know that proxy's scheme), so there's no basis here for
		// claiming GOAUTH would or wouldn't Fatal before reaching it.
		d = diagnosis{statusGoauthMalformedLocally, fmt.Sprintf(
			"your local `GOAUTH` config is malformed (%v) — real `go install`/`go get`/`go mod download` Fatals outright with this exact error the moment it makes its first HTTPS request of the run, entirely offline and before %s@%s (or anything else) is ever checked against proxy.golang.org or sum.golang.org. "+
				"That's your machine's own config, not a proxy-availability problem. Fix `GOAUTH` (see `go help goauth`) — e.g. `GOAUTH=netrc` for the default, or a lone `GOAUTH=off` (not combined with any other semicolon-separated entry) to disable authentication entirely.",
			goauthErr, module, version)}
	} else {
		deadline := time.Now().Add(*timeout)
		var r report
		for {
			r = ep.probe(module, version)
			d = diagnose(r)
			if d.status == statusSumdbLag || d.status == statusReady || d.status == statusRetracted || d.status == statusDeprecated {
				// Checked before localSumdbSkipped: a custom (non-"off",
				// non-public) $GOSUMDB only genuinely means "verification
				// goes elsewhere, the public sumdb's lag is irrelevant" when
				// it's actually a well-formed checksum-database verifier key
				// — see localGosumdbConfigError's doc comment for why a
				// malformed one is a real, unconditional failure instead, not
				// a safe skip.
				//
				// Gated on all four of these statuses, not just
				// statusSumdbLag: every one of them already claims (either
				// unconditionally for statusReady, or conditionally on
				// r.sum.ok for statusRetracted/statusDeprecated — see their
				// "resolves fine... will succeed" wording in diagnose.go)
				// that "a plain `go install` will work/succeed" based purely
				// on proxy.golang.org and the PUBLIC sum.golang.org this tool
				// actually probes — with no awareness that dbDial()
				// (modfetch/sumdb.go) parses and validates the *local*
				// $GOSUMDB unconditionally, before any specific module's
				// verification is even attempted, regardless of whether the
				// module is otherwise fully live, retracted, or deprecated.
				// Confirmed live (2026-10-01) two ways against a fresh
				// GOMODCACHE with GOSUMDB=sum.example.com (a bare hostname —
				// passes sumdbName's extraction fine, but fails
				// note.NewVerifier, exactly the "malformed verifier id"
				// shape localGosumdbConfigError already detects): `go mod
				// download golang.org/x/mod@v0.41.0` (fully live on both
				// proxy.golang.org and sum.golang.org right now) fails
				// outright with `invalid GOSUMDB: malformed verifier id`,
				// and the identical env against the real, currently-
				// retracted `github.com/mattn/go-sqlite3@v2.0.3+incompatible`
				// fails the exact same way — never even reaching the point
				// of reporting retraction. Before this fix, goproxycheck
				// reported plain statusReady ("a plain `go install` will
				// work") for the first case, and statusRetracted with
				// "resolves fine... will succeed" for the second — both the
				// opposite of what the real command does in this exact
				// environment.
				if err := localGosumdbConfigError(module); err != nil {
					d = diagnosis{statusGosumdbMalformedLocally, fmt.Sprintf(
						"%s is live on proxy.golang.org, but before that would even matter, your local `GOSUMDB` config is itself malformed (%v) — real `go install`/`go get` fails outright with `invalid GOSUMDB: %v` the moment it actually needs to verify this (or any) module against the checksum database, regardless of what sum.golang.org has, or whether this version is otherwise ready, retracted, or deprecated. "+
							"That's your machine's own config, not a proxy-availability problem. Fix `GOSUMDB` (see `go help goproxy`), or set `GOSUMDB=off` if you intend to skip verification entirely.",
						displayTarget(r), err, err)}
				} else if localGosumdbOffBlocksToolchain(module) {
					// See sumdbAlwaysRequired's doc comment: unlike every other
					// module, GOSUMDB=off doesn't mean "skip verification" for
					// golang.org/toolchain — modfetch/sumdb.go's useSumDB hardcodes
					// this one module path as always requiring the checksum
					// database, so dbDial's own "off" check is reached
					// unconditionally and Fatals outright, regardless of proxy or
					// sumdb state. Checked ahead of statusSumdbLag below for the
					// same reason localGosumdbConfigError is: this is a local-config
					// failure that preempts the sumdb-lag question entirely, and it
					// applies just as much when d.status is already statusReady
					// (sum.golang.org has fully caught up) as when it's
					// statusSumdbLag — a real install Fatals on GOSUMDB=off before
					// ever checking either.
					d = diagnosis{statusGosumdbRequiredLocally, fmt.Sprintf(
						"%s is Go's own toolchain-distribution module — real `go install`/`go get`/`go mod download` always consult the checksum database for it, even when your local `GOSUMDB` is `off` or a `GONOSUMDB` pattern matches it (every other module path is exempted by either of those; this one, uniquely, is not — see modfetch/sumdb.go's useSumDB, \"downloaded toolchains cannot be listed in go.sum\"). "+
							"Your local `GOSUMDB` is `off`, so the real command fails outright with `checksum database disabled by GOSUMDB=off` the moment it tries to verify this module, regardless of what proxy.golang.org or sum.golang.org have, or whether this version is otherwise ready. "+
							"That's your machine's own config, not a proxy-availability problem — but unlike every other module, `GOSUMDB=off` is not the fix here: point `GOSUMDB` at a real, reachable checksum database (the default `sum.golang.org` works) to fetch Go toolchains.",
						displayTarget(r))}
				} else if d.status == statusSumdbLag {
					if skipped, reason := localSumdbSkipped(module); skipped {
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
			// statusGosumdbRequiredLocally joins it for the identical
			// reason: GOSUMDB=off blocking golang.org/toolchain (see
			// sumdbAlwaysRequired's doc comment) is just as much a local
			// config problem as a malformed GOSUMDB, and just as
			// unfixable by waiting on either proxy.golang.org or
			// sum.golang.org to catch up.
			// statusNoMatchingVersion joins this list for the same reason as
			// statusUnknownRevision/statusInvalidPseudoVersion right above:
			// a comparison version query (e.g. "<v0.0.1") for which zero
			// published versions satisfy the bound can never resolve no
			// matter how long this polls — the set of already-tagged
			// versions below/above the bound doesn't change just because
			// the proxy catches up on indexing; a real `go get`/`go install`
			// fails immediately and permanently with "no matching versions
			// for query" for the identical reason. See
			// report.comparisonQueryNoMatch's doc comment.
			// statusNegativeCache (the per-version case) belongs in this
			// list too, and was the one permanent-failure status missing
			// from it: diagnose's own message for it says outright "It has
			// been observed not to clear on its own within 30+ minutes.
			// Fix: cut a new patch tag ... rather than waiting" — the
			// identical "waiting is not the fix, don't poll for it"
			// rationale as every other status in this list, just never
			// wired in. Reproduced live before the fix with a fake proxy
			// serving the per-version negative-cache pattern under --wait
			// --timeout=300ms --interval=10ms: it polled ~28 times over the
			// full 300ms instead of returning after the first probe, the
			// same doomed-poll waste already fixed for
			// statusZipBuildError/statusMajorVersionMismatch/etc. above.
			//
			// statusModuleNegativeCache (the whole-module case) is
			// deliberately NOT added alongside it, despite sharing most of
			// its name and code path: its own message reaches the opposite
			// conclusion — "there's no known trick that reliably clears
			// it... Waiting is the only broadly-effective known fix" —
			// because cutting a new tag doesn't help when @latest itself
			// is what's cached negative. Don't generalize the per-version
			// fix to its whole-module sibling; check each status's own
			// diagnosis text for what it actually claims about waiting
			// before assuming they share the same answer.
			// statusGoModUnparseable joins this list for the same reason as
			// statusZipBuildError/statusMajorVersionMismatch above: a go.mod
			// that golang.org/x/mod/modfile.Parse itself rejects (e.g. a "/*
			// */" block comment) is a permanent property of the file
			// committed at this tag — proxy.golang.org and sum.golang.org
			// don't validate go.mod syntax before indexing a version (see
			// this status's own diagnosis text), so no amount of polling
			// either service ever fixes it; only a new tag with a corrected
			// go.mod would.
			if !*wait || d.status == statusReady || d.status == statusModuleUnknown || d.status == statusBlocklistedMalicious || d.status == statusWrongImportPath || d.status == statusRetracted || d.status == statusDeprecated || d.status == statusZipBuildError || d.status == statusMajorVersionMismatch || d.status == statusUnknownRevision || d.status == statusInvalidPseudoVersion || d.status == statusGosumdbMalformedLocally || d.status == statusGosumdbRequiredLocally || d.status == statusNegativeCache || d.status == statusNoMatchingVersion || d.status == statusGoModUnparseable || time.Now().After(deadline) {
				break
			}
			time.Sleep(*interval)
		}
		resolvedVersion = r.resolvedVersion
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
		// See negativeCacheDirectFallbackNote's doc comment: a negative-cache
		// verdict (module-level or per-version) means proxy.golang.org 404s
		// right now, but that alone doesn't mean a real `go install`/`go get`
		// on this machine is stuck — under the ordinary, unmodified default
		// GOPROXY chain (or any chain shaped like it), the exact same 404
		// makes the go command itself retry via a direct VCS fetch, which a
		// real public GitHub repo typically satisfies immediately. Append
		// this as a note rather than replacing the message: the negative
		// cache is still real (and still what every other GOPROXY=off/
		// proxy-only user or CI system sees), this only qualifies what
		// *this* invocation would actually do.
		//
		// statusNotYetIndexed belongs in this same list, not just the two
		// negative-cache statuses: mechanically, cmd/go's own TryProxies
		// (confirmed directly against cmd/go/internal/modfetch/proxy.go)
		// falls back to the next chain entry on *any* fs.ErrNotExist-
		// equivalent error (404/410) from a comma-separated proxy — it has
		// no notion of "negative cache" vs. "not yet indexed" at all, both
		// are just a 404 on the @v/<version>.info request that triggered
		// this diagnosis. By the time diagnose() reaches the not-yet-indexed
		// fallback, every permanent-failure marker (isUnknownRevision,
		// isZipBuildError, majorVersionMismatchMarker, ...) has already been
		// ruled out — confirmed live (2026-09-29) against a real, warm
		// module (golang.org/x/mod) that a genuinely nonexistent version or
		// branch name 404s with the isUnknownRevision marker specifically
		// ("invalid version: unknown revision ..."), so a not-yet-indexed
		// verdict reaching this point is, like the negative-cache case,
		// overwhelmingly a real tag the direct-VCS fallback can already
		// fetch — this tool's own top-of-file comment names exactly this
		// scenario ("just tagged a release, is it live yet") as the reason
		// it exists. Before this, a freshly-pushed tag under the ordinary
		// default GOPROXY chain got the same "retry in a minute, or use
		// --wait" wording with no mention that a plain `go install` right
		// now would likely already succeed via automatic direct fetch,
		// while the mechanically-identical negative-cache case got the note.
		if d.status == statusModuleNegativeCache || d.status == statusNegativeCache || d.status == statusNotYetIndexed {
			if note := negativeCacheDirectFallbackNote(module); note != "" {
				d.message += " " + note
			}
		}
	}

	if *jsonOut {
		out := map[string]string{
			"module":  module,
			"version": version,
			"status":  string(d.status),
			"message": d.message,
		}
		// "version" above is always the literal argument (or the git-tag-
		// derived version in no-argument mode) — deliberately left as the
		// caller's own input even when it was a query, so a caller can always
		// see what it originally asked for. But a query like "latest", a
		// partial version ("v0.19"), or a revision identifier (a branch name
		// or commit hash) resolves to a different, concrete version before
		// this tool checks anything — the text-mode output already surfaces
		// that via displayTarget's "(resolved to vX.Y.Z)", but --json had no
		// field for it at all, so a script consuming --json (the entire
		// point of the flag) had no way to learn the concrete version that
		// was actually confirmed ready short of regexing it back out of the
		// free-text "message" field. Confirmed live: `goproxycheck --json
		// golang.org/x/mod@latest` printed "version": "latest" even though
		// the message said "(resolved to v0.41.0)". Only set when resolution
		// actually happened (resolvedVersion != ""), so an already-literal
		// version argument's JSON is unchanged.
		if resolvedVersion != "" {
			out["resolved_version"] = resolvedVersion
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
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

// isVersionPrefix reports whether v — already confirmed valid semver by the
// caller — is an incomplete ("prefix") version rather than a fully-specified
// one: bare major ("v1"), major.minor ("v1.2"), but not major.minor.patch
// ("v1.2.3") or anything carrying a pre-release/build suffix. Ported
// verbatim from cmd/go's own gover.ModIsPrefix (mod.go), restricted to the
// ordinary-module (non "go"/"toolchain" path) case, since goproxycheck never
// checks either of those special pseudo-modules: fewer than two dots, and no
// '-'/'+' anywhere (a version with either of those is always a complete,
// unambiguous version, never a prefix — see ModIsPrefix's own doc comment,
// "the caller is assumed to have checked that ModIsValid(path, vers) is
// true").
func isVersionPrefix(v string) bool {
	dots := 0
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '-', '+':
			return false
		case '.':
			dots++
			if dots >= 2 {
				return false
			}
		}
	}
	return true
}

// nonASCIIVersionLetterError reports the real, verbatim `go get`/`go install`
// Fatal for a version/revision string containing a non-ASCII Unicode letter
// (e.g. an accented character in a hand-named branch, or a copy-pasted
// smart/non-Latin character) — a genuinely different failure shape than
// resolveTarget's neighboring "disallowed version string" check just below,
// confirmed live and by reading golang.org/x/mod/module's own source
// (module.go, this repo's pinned v0.41.0) directly:
//
// EscapeVersion validates version/revision strings via `checkElem(v,
// filePath)`, which defers to fileNameOK — and fileNameOK's own doc comment
// says plainly "we allow all Unicode letters" (its code: `return
// unicode.IsLetter(r)` for any rune >= utf8.RuneSelf) — so a version like
// "café" or "日本語" passes this validation step cleanly, completely unlike
// an ordinary disallowed character (a colon, question mark, asterisk, ...),
// which checkElem rejects immediately with "disallowed version string" (see
// the sibling check just below). Only EscapeVersion's *second* step,
// escapeString, actually fails on it: that function's own loop treats any
// rune >= utf8.RuneSelf as a case it should never see ("This should be
// disallowed by CheckPath, but diagnose anyway" — a comment written for the
// module-PATH case, EscapePath, where CheckPath really does reject non-ASCII
// outright; it doesn't hold for the version/file-path case EscapeVersion
// shares the same helper with), and returns a bare, unwrapped `"internal
// error: inconsistency in EscapePath"` instead of an *module.
// InvalidVersionError the way every other rejection does.
//
// That type difference propagates all the way out to the real error text:
// cmd/go's own modfetch/proxy.go (proxyRepo.versionError) wraps whatever
// EscapeVersion returned in a fresh outer *module.InvalidVersionError, then
// module.ModuleError.Error() special-cases printing when that outer error's
// own .Err is itself an *InvalidVersionError (the ordinary case) by adding
// the "version %q invalid:" phrase and the quoted version string — but
// skips that extra wrapping entirely when .Err is a plain error (this case),
// printing the bare inner message directly instead. Confirmed live
// (2026-10-03, go1.26.8, entirely offline — this Fatals before any proxy
// contact, as fast as the ordinary-disallowed-character case):
//
//	$ go get golang.org/x/mod@café
//	go: golang.org/x/mod@café: invalid version: internal error: inconsistency in EscapePath
//
// — no quoted "café", no "version ... invalid:" phrase, nothing resembling
// "disallowed version string" at all. Before this check existed,
// resolveTarget's sibling EscapeVersion-failure branch unconditionally
// quoted the ordinary-case wording for every failure alike, so goproxycheck
// told a caller hitting this exact shape that real `go` rejects it with a
// message real `go` never actually prints for this input — actively
// misdirecting anyone trying to match goproxycheck's own quoted error text
// against their terminal's real output, or grep a CI log for it.
func nonASCIIVersionLetterError(module, version string, escapeErr error) error {
	return fmt.Errorf("version %q contains a non-ASCII Unicode letter — a real `go get`/`go install` Fatals immediately and offline on this exact input, but not with the ordinary \"disallowed version string\" wording: golang.org/x/mod/module's own version validation (fileNameOK) explicitly allows Unicode letters, so it passes that check, then fails one step later with a bare internal-error message cmd/go surfaces verbatim: `%s@%s: invalid version: %v`. Either way, this could never resolve no matter how long you --wait or retry — check for an accented character, a non-Latin script, or a stray smart-quote/copy-paste artifact in the version string", version, module, version, escapeErr)
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
		if parts[0] == "go" || parts[0] == "toolchain" {
			// "go" and "toolchain" are not ordinary module paths at all — real
			// cmd/go reserves both literal strings as pseudo-module names it
			// special-cases ahead of any path validation (modfetch/repo.go's
			// lookup: `switch path { case "go", "toolchain": return
			// &toolchainRepo{path, Lookup(ctx, proxy, "golang.org/toolchain")},
			// nil }`), redirecting every query to the real golang.org/toolchain
			// module under the hood. `go help get` documents this directly:
			// "To upgrade the minimum required Go version to the latest
			// released Go version: go get go@latest" / "To upgrade the Go
			// toolchain to the latest patch release of the current Go
			// toolchain: go get toolchain@patch". Confirmed live (2026-10-02,
			// go1.24.4): `go install go@latest` and `go get toolchain@patch`
			// both resolve the module successfully (`go install go@latest`
			// only fails afterward, on "does not contain package go" — the
			// module lookup itself already succeeded by then).
			//
			// modulepkg.CheckPath rejects both strings outright ("missing dot
			// in first path element", since neither looks like a
			// domain-rooted import path) — but a real `go get`/`go install`
			// never reaches that check for these two literal strings; it
			// special-cases them first. Before this check, resolveTarget let
			// CheckPath's ordinary rejection fire here, so goproxycheck told
			// the caller that `go get`/`go install` "rejects this exact
			// string immediately with `malformed module path ...: missing dot
			// in first path element`" — an outright false claim about real
			// tool behavior (the opposite-direction mistake from this tool's
			// usual "the real toolchain would Fatal first" family: here it
			// invented a Fatal that doesn't happen).
			//
			// goproxycheck still doesn't check either target, though: unlike
			// an ordinary module, "go"/"toolchain" versions aren't semver
			// (no "v" prefix — confirmed live `go get go@v1.27.1` itself fails
			// with "invalid go version v1.27.1"), "latest"/"patch" resolve
			// against the *locally running* toolchain's own version as well
			// as a fetched list (cmd/go's internal/gover package; confirmed
			// live that proxy.golang.org's own plain golang.org/toolchain
			// @latest returns a nonsense, years-stale "v0.0.1-go1.9rc2...",
			// unusable as a stand-in), and even a fully-specified version like
			// "1.21rc1" that's older than the locally running toolchain is
			// treated as present without ever consulting the proxy at all
			// (cmd/go's own toolchainRepo.Stat: "pretend to have all earlier
			// Go versions available without network access"). None of that
			// reuses this tool's ordinary module-version machinery safely, so
			// this reports an honest "not supported" instead of a false
			// "invalid".
			return "", "", fmt.Errorf("%q is a reserved Go-toolchain pseudo-module name, not an ordinary module path — `go get %s@%s` (see `go help get`) is real, documented syntax that resolves fine via golang.org/toolchain, not an offline parse rejection. goproxycheck doesn't check Go-toolchain version availability, only ordinary module paths against the public proxy/checksum database", parts[0], parts[0], parts[1])
		}
		if err := modulepkg.CheckPath(parts[0]); err != nil {
			// Confirmed live (2026-09-28) against golang.org/x/mod/module (the
			// same package cmd/go itself uses for this check) and the real `go`
			// toolchain: `go get example.com/foo!bar@v1.0.0` (an invalid
			// character), `go get example.com/foo.@v1.0.0` (a trailing dot),
			// `go get example.com/.foo@v1.0.0` (a leading dot), and `go get
			// example.com/fooé@v1.0.0` (a non-ASCII letter) all fail immediately
			// and unconditionally with `malformed module path %q: %v`, entirely
			// offline, before ever contacting a proxy — even with GOPROXY=off,
			// which still lets an otherwise-valid path (e.g. one with uppercase
			// letters, itself legal — see escapePath's case-encoding) through to
			// its own separate "module lookup disabled" error instead.
			//
			// resolveTarget already ran the mirror-image check on the version
			// half of module@version (see the disallowed-version-string check
			// below, via modulepkg.EscapeVersion) but never validated the
			// module path half the same way. Before this check, goproxycheck
			// sent a module path like this straight to escapePath (which merely
			// case-encodes uppercase ASCII and passes every other character,
			// including one real Go rejects outright, straight through
			// unescaped) and then to the proxy, which 404s with its own "invalid
			// escaped module path" style body that matches none of diagnose's
			// specific markers — so it fell through to the generic
			// statusModuleUnknown verdict ("check: is the repo public? does the
			// module path in go.mod exactly match the repo? ..."), actively
			// misdirecting the user to check for a typo or a GOPRIVATE
			// misconfiguration when the real, offline, unconditional answer is
			// that the module path itself is syntactically invalid and could
			// never resolve no matter what the repo or proxy config look like.
			return "", "", fmt.Errorf("module path %q is not valid — a real `go get`/`go install` rejects this exact string immediately with `%v`, entirely offline, before ever contacting the proxy, so this could never resolve regardless of the repo or proxy config. Check for a stray character, leading/trailing dot, or non-ASCII letter in the module path", parts[0], err)
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
		if parts[1] == "none" {
			// "none" is the documented "empty" version query
			// (go.dev/ref/mod#version-queries): unlike every other query this
			// tool resolves or rejects above/below, it doesn't name any real,
			// fetchable version at all — it means "no version": `go get
			// module@none` removes module's requirement from go.mod (or is
			// simply a no-op if there wasn't one), rather than checking or
			// fetching anything. So there's no proxy/sumdb-availability
			// question here for this tool to answer, unlike "latest"/"upgrade"
			// (resolved against a real version) or "patch" (meaningfully
			// rejected right above for lacking an existing requirement to be
			// relative to).
			//
			// Confirmed live (2026-09-30): `go get module@none` succeeds
			// immediately (exit 0) and touches the network not at all —
			// verified even with GOPROXY=off, and even for a module path that
			// doesn't exist or is completely unreachable
			// (`example.com/totally/nonexistent/module@none` succeeds the exact
			// same way, offline). With an existing requirement already in
			// go.mod, it likewise succeeds instantly under GOPROXY=off and
			// simply drops the requirement line — no proxy or sumdb lookup
			// either way.
			//
			// Before this check, goproxycheck sent "none" to the proxy as a
			// literal version string, which 404s with the identical "invalid
			// version: unknown revision none" body a genuinely bogus
			// revision/branch/commit gets (confirmed live against
			// golang.org/x/mod), so it reported statusUnknownRevision —
			// "proxy.golang.org says this isn't a revision that exists in the
			// module's repository at all ... Check for a typo in the version,
			// tag, or commit hash" and exited 1 — the exact opposite of
			// reality: a real `go get module@none` for this same module always
			// succeeds trivially and instantly, no repository or proxy lookup
			// involved at all.
			return "", "", fmt.Errorf(`version "none" is the documented "empty" version query (go.dev/ref/mod#version-queries) — it removes %s's requirement (or is a no-op if there wasn't one) rather than naming any real, fetchable version, so there's no proxy/sumdb availability question here for this tool to check: a real 'go get %s@none' always succeeds immediately without ever contacting the proxy, regardless of whether %s even exists. Check a concrete version, %s@latest, or %s@upgrade instead`, parts[0], parts[0], parts[0], parts[0], parts[0])
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
			if op, operand, ok := comparisonQuery(parts[1]); ok {
				if !semver.IsValid(operand) {
					return "", "", fmt.Errorf("version %q is not a valid comparison version query (go.dev/ref/mod#version-queries) — %q is not a valid semantic version, so a real `go get`/`go install` rejects this exact string immediately with `invalid semantic version %q in range %q`, entirely offline, before ever contacting the proxy, so this could never resolve no matter how long you --wait or retry", parts[1], operand, operand, parts[1])
				}
				// "<=" and ">" specifically (not "<" or ">=") are also rejected
				// when operand is an incomplete ("prefix") version — missing its
				// patch component, or bare major-only, per cmd/go's own
				// gover.ModIsPrefix (mod.go): fewer than two dots and no
				// pre-release/build suffix. Confirmed live (2026-09-29):
				// `go get golang.org/x/mod@<=v0.19` and `go get golang.org/x/mod@>v0`
				// both fail immediately and unconditionally with `ambiguous
				// semantic version %q in range %q`, entirely offline, before ever
				// contacting the proxy — real cmd/go's own newQueryMatcher
				// (modload/query.go) comment explains why: "@v1.2 might mean
				// v1.2.3", so it refuses to guess whether the bound is meant as
				// exactly vX.Y(.0) or as the whole vX.Y.* line, rather than
				// resolving one way silently. "<" and ">=" have no such ambiguity
				// (excluding/including everything from vX.Y.0 up is unambiguous
				// either way) and were both confirmed live to succeed normally
				// with the identical prefix-shaped operand (`go get
				// golang.org/x/mod@<v0.19` and `@>=v0.19` both resolve and
				// install fine).
				//
				// Before this check, resolveTarget let a comparison query like
				// this through unconditionally (operand is valid semver, so the
				// check above doesn't catch it), so it reached probe()'s
				// resolveComparisonQuery, which happily resolved it against
				// @v/list using plain semver.Compare — no notion of "ambiguous"
				// exists there — and reported the module statusReady with a
				// concrete resolved version, when the real `go get`/`go install`
				// invocation for that exact argument fails outright and installs
				// nothing.
				if (op == "<=" || op == ">") && isVersionPrefix(operand) {
					return "", "", fmt.Errorf("version %q is not a valid comparison version query (go.dev/ref/mod#version-queries) — %q is an incomplete (major or major.minor only) version, and paired with %q a real `go get`/`go install` refuses to guess whether that means exactly %[2]s.0(.0) or the whole %[2]s.* line, rejecting this exact string immediately with `ambiguous semantic version %[2]q in range %[1]q`, entirely offline, before ever contacting the proxy, so this could never resolve no matter how long you --wait or retry. Use a complete major.minor.patch version instead, or switch to `<` / `>=`, neither of which is ambiguous for an incomplete operand", parts[1], operand, op)
				}
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
				if _, ok := err.(*modulepkg.InvalidVersionError); !ok {
					// See nonASCIIVersionLetterError's doc comment: a version
					// string containing a non-ASCII Unicode letter takes a
					// completely different failure path through x/mod/module
					// than every other disallowed-character shape above —
					// EscapeVersion's own checkElem/fileNameOK validation
					// actually ACCEPTS it (fileNameOK's doc comment: "we allow
					// all Unicode letters"), so this isn't that "disallowed
					// version string" rejection at all; it only fails one
					// step later, inside escapeString itself, with an
					// internal-error message real `go` surfaces verbatim and
					// unwrapped — no quoted version string, no "invalid
					// version: version %q invalid:" prefix the way every
					// other case in this function gets.
					return "", "", nonASCIIVersionLetterError(parts[0], parts[1], err)
				}
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
	// A leading UTF-8 byte order mark (hex EF BB BF, U+FEFF) makes the
	// *entire* file unparseable to the real go toolchain — confirmed live
	// (go1.24.4): `go list -m`/`go build` against a go.mod opening with
	// this byte sequence Fatals immediately and offline with
	// `go.mod:1: unexpected input character`, quoting the BOM rune itself,
	// before the module directive, or anything else in the file, is ever
	// evaluated. Windows tooling
	// commonly writes UTF-8-with-BOM by default (pre-6 PowerShell's
	// `Set-Content -Encoding UTF8`, Notepad's "UTF-8" option), so a
	// hand-edited go.mod saved that way is a real, if rare, shape.
	//
	// diagnose.go's statusGoModUnparseable check already catches this same
	// class of problem for the *proxy-served* go.mod of the version being
	// checked (it runs golang.org/x/mod/modfile.Parse, which rejects a BOM
	// exactly like it rejects a "/* */" block comment, ahead of
	// canonicalModulePath/retraction/deprecation). But this function is a
	// separate, earlier code path — the no-argument CLI mode that reads the
	// *local* ./go.mod to discover which module to probe in the first
	// place — and it never goes through that check: it hands the raw bytes
	// straight to moduleDirective's hand-rolled line scanner, whose
	// `strings.CutPrefix(line, "module")` check on the first line silently
	// fails once the BOM is glued onto the front of "module" (strings.
	// TrimSpace does not strip U+FEFF), so the directive is never
	// recognized at all. Without this check, that fell through to the
	// generic "has no 'module' directive" error below — actively
	// misdirecting a user into thinking they need to add a module
	// directive from scratch, instead of the accurate, `go`-shaped answer
	// that the file has an encoding problem a real toolchain Fatals on
	// before even looking for one.
	const utf8BOM = "\xEF\xBB\xBF" // the three raw bytes of a UTF-8 byte order mark (U+FEFF)
	if strings.HasPrefix(string(data), utf8BOM) {
		return "", fmt.Errorf("%s begins with a UTF-8 byte order mark — a real `go list -m`/`go build` Fatals immediately with `go.mod:1: unexpected input character`, entirely offline, before ever contacting the proxy, so this could never resolve regardless of the repo or proxy config. Re-save the file as plain UTF-8 without a byte-order mark and try again", path)
	}
	if lineErr := ignoreDirectiveTooOldError(string(data), localGoVersion()); lineErr != nil {
		// Checked ahead of ignoreDirectiveArgCountError just below: see
		// ignoreDirectiveTooOldError's doc comment for the live confirmation
		// that when the toolchain actually selected to run this file
		// doesn't recognize `ignore` as a verb AT ALL, real go Fatals with
		// `unknown directive: ignore` regardless of the directive's own
		// argument count — an unrecognized verb never reaches argument-count
		// validation in the first place.
		return "", fmt.Errorf("%s %s, entirely offline, before ever contacting the proxy, so this could never resolve regardless of the repo or proxy config", path, lineErr)
	}
	if lineErr := ignoreDirectiveArgCountError(string(data)); lineErr != nil {
		// See ignoreDirectiveArgCountError's doc comment for the live
		// confirmation (go1.26.8, 2026-10-03) that a malformed `ignore`
		// directive anywhere in the file Fatals real `go list -m`/`go build`
		// immediately and entirely offline — before the module directive
		// (checked right below) or anything else is ever resolved, let alone
		// a proxy contacted. moduleDirective's scanner only ever recognizes
		// `module` lines, so without this check a go.mod this broken sailed
		// straight through unflagged.
		return "", fmt.Errorf("%s %s, entirely offline, before ever contacting the proxy, so this could never resolve regardless of the repo or proxy config", path, lineErr)
	}
	mod, err := moduleDirective(string(data))
	if err != nil {
		return "", fmt.Errorf("%s %w", path, err)
	}
	if err := modulepkg.CheckPath(mod); err != nil {
		// Mirrors resolveTarget's identical check on an explicit
		// module@version argument (bca7487) — but that check only ever ran
		// for the args[0] case, not this one. Confirmed live (see
		// TestModuleFromGoMod_InvalidPath) against a real go1.24.4 toolchain:
		// a go.mod whose own `module` directive is a syntactically invalid
		// import path (a stray '!', a leading/trailing dot, a non-ASCII
		// letter, ...) makes `go list -m`/`go build`/every other
		// module-aware command Fatal immediately with `malformed module path
		// %q: %v`, entirely offline, before ever resolving a single
		// requirement or contacting a proxy — the exact same "the real
		// toolchain would Fatal first" shape already checked for the
		// explicit-argument case. Without this check, goproxycheck's
		// no-argument mode (reading ./go.mod, the shipped GitHub Action's
		// default invocation) sent the literal invalid path straight to
		// probe(), which reported the generic statusModuleUnknown ("check:
		// is the repo public? does the module path... typo? GOPRIVATE?") —
		// actively misdirecting the user away from the real, unconditional
		// answer that the module path itself can never resolve, regardless
		// of the repo or proxy config.
		return "", fmt.Errorf("%s has a 'module' directive with an invalid path %q — a real `go list -m`/`go build` rejects this exact string immediately with `%v`, entirely offline, before ever contacting the proxy, so this could never resolve regardless of the repo or proxy config. Check for a stray character, leading/trailing dot, or non-ASCII letter in the module path", path, mod, err)
	}
	return mod, nil
}

// hasIgnoreDirective reports whether data — a go.mod file's raw body —
// contains a top-level `ignore` directive at all, single-line or inside
// its parenthesized block form. A pure presence check (unlike
// ignoreDirectiveArgCountError, which also validates the directive's
// shape) — used only by ignoreDirectiveTooOldError, which needs to know
// whether `ignore` appears at all before it's worth asking `go env
// GOVERSION` which toolchain would actually try to parse it.
func hasIgnoreDirective(data string) bool {
	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		rest, ok := strings.CutPrefix(line, "ignore")
		if ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '(' || strings.HasPrefix(rest, "//")) {
			return true
		}
	}
	return false
}

// goDirectiveVersion extracts a go.mod's own `go` directive version string
// (e.g. "1.26.8" or "1.21") from data, or "" if the file has no `go`
// directive at all. Unlike `module`, the `go` directive (along with
// `toolchain`) is never written in parenthesized block form — confirmed
// against golang.org/x/mod/modfile/rule.go's block-open verb set, which
// lists `module`/`godebug`/`require`/`exclude`/`replace`/`retract`/`tool`/
// `ignore` but not `go`/`toolchain` — so unlike moduleDirective this needs
// no block-tracking at all.
func goDirectiveVersion(data string) string {
	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		rest, ok := strings.CutPrefix(line, "go")
		if ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t') {
			return strings.TrimSpace(stripLineComment(rest))
		}
	}
	return ""
}

// goVersionAtLeast reports whether a go version string (e.g. "1.26.8" or
// "1.21", with or without a leading "go" — any such prefix is stripped
// first) is at least major.minor. Returns false for an empty or
// unparsable version.
func goVersionAtLeast(version string, major, minor int) bool {
	version = strings.TrimPrefix(strings.TrimSpace(version), "go")
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return false
	}
	vMajor, err1 := strconv.Atoi(parts[0])
	vMinor, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	if vMajor != major {
		return vMajor > major
	}
	return vMinor >= minor
}

// localGoVersion returns the version `go env GOVERSION` reports in the
// current directory (e.g. "go1.24.4"), or "" if the command fails for any
// reason. Running `go env` (for any variable, not just this one) already
// performs GOTOOLCHAIN=auto's full toolchain selection — including
// downloading a newer toolchain if this go.mod's own `go`/`toolchain`
// directive demands one — before it ever gets to the deeper go.mod
// semantic parsing where an unrecognized verb like a too-new `ignore`
// directive would Fatal; confirmed live (2026-10-03) that `go env
// GOVERSION` succeeds and reports the correctly-selected toolchain version
// even when the same go.mod's `ignore` directive is itself malformed or
// unrecognized by that exact version.
func localGoVersion() string {
	out, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ignoreDirectiveTooOldError reports an error if data's go.mod contains a
// top-level `ignore` directive (see hasIgnoreDirective) that the toolchain
// actually selected to run this file cannot recognize as a directive at
// all — making every module-aware `go` subcommand Fatal with `go.mod:N:
// unknown directive: ignore` before resolving a single module, regardless
// of whether the directive's own argument count would otherwise be valid
// (see ignoreDirectiveArgCountError, a separate, later-stage check).
//
// `ignore` is a comparatively new go.mod directive: confirmed absent from
// golang.org/x/mod/modfile as vendored into go1.24.4's own cmd/go (no
// "ignore" case anywhere in its verb switch) and present starting
// go1.25.14/go1.26.0 (read directly from each toolchain's own vendored
// modfile/rule.go — matches goprivaudit's independent finding of the same
// cutoff, technique #90/#116 in this project's testing-practice catalog).
// Whether a given go.mod's `ignore` line parses at all depends on which
// toolchain binary ends up running it, governed by GOTOOLCHAIN (default
// "auto"): the effective version is max(the locally installed/selected go
// version, whatever this go.mod's own `go`/`toolchain` directive
// requires) — go never downgrades, and only attempts to download a newer
// toolchain when the file's own stated requirement exceeds what's already
// installed/selected. So this only fires when BOTH halves of that max are
// below 1.25: the file's own `go` directive requires less than 1.25 (an
// absent `go` line counts as unsatisfied too), AND localGoVersion() — the
// toolchain actually installed/selected for this directory, which already
// reflects any GOTOOLCHAIN upgrade this go.mod's own directives would
// trigger — is ALSO below 1.25.
//
// This is a realistic shape, not a contrived one: `go mod edit
// -ignore=path`, run with a newer local toolchain, does NOT bump the
// file's own `go` line to cover the directive it just added — live-
// verified (2026-10-03) running it with a real go1.26.8 toolchain.
// Live-verified end-to-end: a from-scratch go.mod reading only `module
// example.com/foo`, `go 1.21`, and `ignore ./testdata` makes a real
// go1.24.4 (GOTOOLCHAIN=auto, no override — the toolchain this sandbox's
// `go env GOVERSION` actually selects for a `go 1.21` file) Fatal
// instantly with `go.mod:5: unknown directive: ignore`, GOPROXY=off, zero
// network access — even with the directive's own single argument
// perfectly well-formed. Before this fix, moduleFromGoMod's no-argument-
// mode callers had no awareness of this toolchain-gating at all: the
// module path was extracted normally and probed against the live proxy as
// if the file were perfectly ordinary.
//
// localVersion is the toolchain that would actually run data (ordinarily
// localGoVersion()'s live result, taken as a parameter — rather than
// called directly — so tests can pin it instead of depending on this
// sandbox's own ambient toolchain; mirrors goprivaudit's identical
// `-goversion`-override split for the exact same reason, see that tool's
// goModHasIgnoreDirectiveTooOld). An unresolvable value (empty, meaning
// the `go env` call itself failed) fails open here, same convention as
// every other go-env-derived check in this file when the environment fact
// it needs can't be pinned down.
func ignoreDirectiveTooOldError(data, localVersion string) error {
	if !hasIgnoreDirective(data) {
		return nil
	}
	if goVersionAtLeast(goDirectiveVersion(data), 1, 25) {
		return nil
	}
	if localVersion == "" || goVersionAtLeast(localVersion, 1, 25) {
		return nil
	}
	return fmt.Errorf("has an 'ignore' directive, but neither its own `go` directive (%q) nor the locally selected toolchain (%s) is go1.25 or newer — `ignore` wasn't recognized as a go.mod directive before go1.25, so `go list -m`/`go build` Fatals immediately with `unknown directive: ignore`", goDirectiveVersion(data), localVersion)
}

// ignoreDirectiveArgCountError scans data — a go.mod file's raw body — for
// an `ignore` directive, single-line or inside its parenthesized block
// form (both forms accept an optional or missing space before the opening
// paren, exactly like `module`'s own block form — see moduleDirective's
// doc comment — confirmed live, 2026-10-03, go1.26.8, that `ignore(` alone
// on its own line opens a block identically to `ignore (`), carrying
// anything other than exactly one argument.
//
// `ignore` is a comparatively new go.mod directive: golang.org/x/mod/
// modfile v0.41.0 (this repo's own pinned version) parses it, but the
// go1.24.4 toolchain installed on this box does not recognize it at all —
// confirmed live it Fatals instead with `go.mod:N: unknown directive:
// ignore`, a different, unrelated failure this function deliberately
// leaves alone: that toolchain-gating question (is the `go` directive, or
// the toolchain actually selected, too old to know `ignore` exists at
// all) is ignoreDirectiveTooOldError's job, checked ahead of this one in
// moduleFromGoMod — this function only ever runs once that check has
// already confirmed `ignore` IS a recognized verb for this file. Despite
// being new, `ignore`'s own grammar is unremarkable: golang.org/x/mod/
// modfile/rule.go's
// parseVerb gives it the exact same "expects exactly one argument" rule
// as `tool`/`godebug` (case "ignore": `if len(args) != 1 { errorf("ignore
// directive expects exactly one argument") }`) — confirmed live
// (go1.26.8, 2026-10-03) that a go.mod carrying a bare `ignore` line (zero
// arguments), `ignore ./a ./b` (two unquoted tokens on one line), or the
// equivalent two-argument shape written as one entry inside an `ignore
// (...)` block all Fatal `go list -m`/`go build`/`go install` immediately
// and entirely offline with `ignore directive expects exactly one
// argument`, before ever resolving a single requirement or contacting a
// proxy — while a well-formed single-argument `ignore` line (or block
// entry) builds and resolves completely normally.
//
// Before this check, moduleFromGoMod's no-argument-mode callers (the
// shipped GitHub Action's default invocation, reading ./go.mod with no
// CLI argument) had no awareness of the `ignore` directive at all:
// moduleDirective's scanner only ever recognizes lines starting with
// `module`, so a go.mod broken this way sailed straight through
// unflagged — module path extracted normally, then probed against the
// live proxy and reported ready (or whatever the proxy/sumdb state
// happened to say) as if the file were perfectly ordinary, when the real
// toolchain can't even parse it, let alone get as far as resolving a
// single requirement.
//
// Deliberately narrower than a full golang.org/x/mod/modfile.Parse
// delegation would be: it only ever flags an argument-COUNT mismatch
// (matching this function's name), not a malformed quoted-string argument
// (e.g. `ignore "unterminated`, confirmed live to Fatal with its own,
// different lexer-level error, `unexpected newline in string`) — a
// narrower, rarer gap left unfixed here as a deliberate scope boundary,
// the same way moduleLineHasExtraArgs's sibling check for `module`
// deliberately leaves quoted-path content validation to parseModulePath's
// own separate, already-existing check rather than folding every
// character-level go.mod lexer rule into one function.
func ignoreDirectiveArgCountError(data string) error {
	inBlock := false
	lineNo := 0
	for _, raw := range strings.Split(data, "\n") {
		lineNo++
		line := strings.TrimSpace(raw)
		if inBlock {
			content := stripLineComment(line)
			if content == ")" {
				inBlock = false
				continue
			}
			if content != "" {
				if err := ignoreEntryArgCountError(content, lineNo); err != nil {
					return err
				}
			}
			continue
		}
		rest, ok := strings.CutPrefix(line, "ignore")
		if !ok || !(rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '(' || strings.HasPrefix(rest, "//")) {
			continue
		}
		rest = strings.TrimSpace(rest)
		if stripLineComment(rest) == "(" {
			inBlock = true
			continue
		}
		if err := ignoreEntryArgCountError(stripLineComment(rest), lineNo); err != nil {
			return err
		}
	}
	return nil
}

// ignoreEntryArgCountError checks a single `ignore` directive's argument
// text (content after the verb, or a block entry line — either way, with
// any trailing "//" comment already stripped) for the zero-argument or
// more-than-one-argument shapes ignoreDirectiveArgCountError's doc comment
// describes, reusing moduleLineHasExtraArgs's existing quote-aware
// token-count logic (shared across both directives since go.mod's lexer
// treats a quoted string as exactly one token for either verb alike).
func ignoreEntryArgCountError(content string, lineNo int) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("has an 'ignore' directive on line %d with no argument — real `go list -m`/`go build` Fatals immediately with `ignore directive expects exactly one argument`", lineNo)
	}
	if moduleLineHasExtraArgs(content) {
		return fmt.Errorf("has an 'ignore' directive on line %d with more than one argument (%q) — real `go list -m`/`go build` Fatals immediately with `ignore directive expects exactly one argument`", lineNo, content)
	}
	return nil
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
//
// Also treats a bare "module" keyword with nothing (real) after it at all —
// end of line, only trailing whitespace, or a same-line "//" comment with no
// separating space — as the single-line form's already-existing "directive
// present but no path" case, not as "no directive at all." Confirmed live
// (2026-09-30) against golang.org/x/mod/modfile.Parse: a go.mod whose sole
// content is "module" (no newline), "module\n", "module \n" (trailing
// space), "module\t\n" (trailing tab), or "module//no path\n" (a comment
// glued straight onto the keyword, no space) all Fatal identically with
// `usage: module module/path` — the same hard, offline, before-any-network
// parse error the existing "module (\n)" (empty block) and "module //
// comment" (space then comment) cases already produce, just missing their
// own dedicated check. The bug: `line := strings.TrimSpace(raw)` at the top
// of this loop's body strips a bare trailing space/tab BEFORE the CutPrefix
// check ever runs, collapsing "module " and "module\t" down to exactly
// "module" — so `rest` becomes "" and the existing `rest != ""` guard
// rejected them outright, same as an unadorned bare "module" with nothing
// after it to begin with. And a same-line comment with no leading space
// ("module//...") starts with '/', which the guard's byte set (' ', '\t',
// '(') never included, exactly the same "separator-character check misses a
// zero-space variant" shape as the no-space-block-paren fix right above,
// just for a genuinely empty argument instead of a block open. A single '/'
// that ISN'T the start of "//" (e.g. "module/foo", no comment) is
// deliberately NOT included here — confirmed live that real go tokenizes
// that as one unrecognized identifier ("unknown directive: module/foo"), a
// completely different failure this function has no way to diagnose
// (it isn't even about the module directive at that point), not "module
// directive with no path."
//
// Before this fix, moduleFromGoMod's no-argument-mode callers got "go.mod
// has no 'module' directive" for a go.mod that plainly has one — just a
// malformed, argument-less one — misdirecting a user or CI log into
// thinking a module directive needs to be added from scratch, instead of
// the accurate, `go`-shaped answer that an existing directive's path is
// missing or was accidentally deleted.
//
// Also rejects a go.mod carrying more than one 'module' directive — in any
// combination of single-line and parenthesized-block form, including two
// path lines inside a single block — as "repeated module statement",
// mirroring go/toolchain/module directive repeated-more-than-once being a
// Fatal in sibling tool goprivaudit, confirmed to have the exact same shape
// here: a repeated 'go' or 'toolchain' directive already Fatals this way
// (not goproxycheck's concern, it never parses those directives itself),
// and live testing (2026-10-01, go1.24.4) against a real go.mod shows a
// repeated 'module' directive Fatals identically —
//
//	go: errors parsing go.mod:
//	go.mod:3: repeated module statement
//
// — regardless of whether the two directives are both single-line, both
// block form, one of each, or two path lines inside one block. Before this
// fix, this function returned as soon as it finished parsing the FIRST
// 'module' directive it found, so a go.mod with a second (necessarily
// invalid, per the above) 'module' directive never hit this check at all:
// moduleFromGoMod's no-argument-mode callers silently got the first
// directive's path and probed it against the proxy as if the go.mod were
// perfectly ordinary, instead of the accurate, `go`-shaped answer that the
// real toolchain would Fatal immediately and offline, before ever
// resolving anything or contacting a proxy, no matter which of the two (or
// more) module paths a user might expect to be "the real one."
//
// Also rejects a module path wrapped in backticks (or carrying a stray
// unquoted `"`/`'`/backtick anywhere) instead of silently accepting it as a
// Go raw string literal — see parseModulePath's doc comment for the
// live-confirmed real-go Fatal this mirrors and the real bug this closes
// (moduleFromGoMod used to strip the backticks and probe the unwrapped path
// against the live proxy, reporting a misleading module-unknown verdict for
// a go.mod that never had a shot at parsing in the first place).
func moduleDirective(data string) (string, error) {
	inBlock := false
	blockMod := ""
	mod := ""
	foundLine := ""
	for _, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		if inBlock {
			if stripLineComment(line) == ")" {
				if blockMod == "" {
					return "", fmt.Errorf("has a 'module' block with no path")
				}
				if mod != "" {
					return "", fmt.Errorf("has more than one 'module' directive (%q and %q) — real `go list -m`/`go build` Fatals immediately with `repeated module statement`, entirely offline, before ever contacting the proxy", foundLine, line)
				}
				mod, foundLine = blockMod, line
				inBlock = false
				continue
			}
			if content := stripLineComment(line); moduleLineHasExtraArgs(content) {
				return "", fmt.Errorf("has a 'module' block entry with more than one argument: %q — real `go list -m`/`go build` Fatals immediately with `usage: module module/path`, entirely offline, before ever contacting the proxy", line)
			}
			m, perr := parseModulePath(line)
			if perr != nil {
				return "", fmt.Errorf("has a 'module' block entry with an invalid path: %q — real `go list -m`/`go build` Fatals immediately with `%v`, entirely offline, before ever contacting the proxy", line, perr)
			}
			if m != "" {
				// A second non-empty path line inside the same block is
				// just as much a repeated module statement to real go as
				// two separate top-level directives (confirmed live, see
				// this function's doc comment) — report it the same way
				// instead of silently letting the last one win.
				if blockMod != "" {
					return "", fmt.Errorf("has more than one 'module' directive (%q and %q) — real `go list -m`/`go build` Fatals immediately with `repeated module statement`, entirely offline, before ever contacting the proxy", blockMod, line)
				}
				blockMod = m
			}
			continue
		}
		// rest[0] == '(' (no space) is included alongside the ordinary
		// space/tab separator: "module(" is real, accepted go.mod syntax
		// for the block-open line — see this function's doc comment.
		//
		// rest == "" (bare "module", nothing left after trimming) and
		// strings.HasPrefix(rest, "//") (a same-line comment glued directly
		// onto the keyword) are both included for the same reason, one
		// paragraph further down in this function's doc comment: both are
		// real go.mod shapes real `go` Fatals on as a malformed module
		// directive, not evidence there's no directive here at all.
		if rest, ok := strings.CutPrefix(line, "module"); ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t' || rest[0] == '(' || strings.HasPrefix(rest, "//")) {
			if mod != "" {
				return "", fmt.Errorf("has more than one 'module' directive (%q and %q) — real `go list -m`/`go build` Fatals immediately with `repeated module statement`, entirely offline, before ever contacting the proxy", foundLine, line)
			}
			rest = strings.TrimSpace(rest)
			if stripLineComment(rest) == "(" {
				inBlock = true
				blockMod = ""
				continue
			}
			if content := stripLineComment(rest); moduleLineHasExtraArgs(content) {
				// See moduleLineHasExtraArgs's doc comment: this is a
				// directive-argument-count failure ("usage: module
				// module/path"), a different real error than the
				// CheckPath-style "malformed module path" error returned
				// below for a single argument whose own content is
				// invalid — conflating the two used to blame a stray
				// trailing token on the path's characters instead of on
				// there being a second argument at all.
				return "", fmt.Errorf("has a 'module' directive with more than one argument: %q — real `go list -m`/`go build` Fatals immediately with `usage: module module/path`, entirely offline, before ever contacting the proxy", line)
			}
			m, perr := parseModulePath(rest)
			if perr != nil {
				return "", fmt.Errorf("has a 'module' directive with an invalid path: %q — real `go list -m`/`go build` Fatals immediately with `%v`, entirely offline, before ever contacting the proxy", line, perr)
			}
			if m == "" {
				// e.g. a "module" line whose entire value is a "//"
				// comment (found via mutation testing, run #125: the
				// comment-stripping boundary at index 0 is exercised,
				// but nothing checked what it produces). Silently
				// probing an empty module path would send a malformed
				// request to the proxy instead of a clear error.
				return "", fmt.Errorf("has a 'module' directive with no path: %q", line)
			}
			mod, foundLine = m, line
			continue
		}
	}
	if inBlock {
		return "", fmt.Errorf("has an unterminated 'module' block")
	}
	if mod == "" {
		return "", fmt.Errorf("has no 'module' directive")
	}
	return mod, nil
}

// moduleLineHasExtraArgs reports whether content — a go.mod module-directive
// line (or a line inside its parenthesized block form) with any trailing
// "//" comment already stripped — carries more than the single argument
// real go's own lexer allows. Confirmed directly against
// golang.org/x/mod/modfile's rule.go: the "module" verb's case in its
// top-level directive switch Fatals with "usage: module module/path"
// whenever it's handed anything other than exactly one token.
//
// A quoted Go string literal (double-quoted or a raw backtick string) is
// lexed as exactly one token regardless of any whitespace it contains, so a
// quoted path with an embedded space isn't mistaken for two arguments here
// — only content after the closing quote, or more than one unquoted
// whitespace-separated field, counts as a second argument. That embedded-
// space case is still correctly caught, just by the existing
// modulepkg.CheckPath validation once parseModulePath unquotes it down to
// a single (invalid) path string — a genuinely different real error from
// this one, confirmed live below.
//
// Confirmed live (2026-09-30) against a real go1.26.8 toolchain, three
// ways: `module example.com/foo extra` (single-line) and
// `example.com/foo extra` inside a `module (...)` block both Fatal with
// `usage: module module/path` (an argument-count error) — while `module
// "example.com/foo bar"` (one quoted argument, containing a literal space)
// Fatals instead with `malformed module path "example.com/foo bar":
// invalid char ' '`, proving the two failures are genuinely distinct, not
// the same thing phrased two ways. Before this check, moduleDirective
// passed a two-token line's entire raw text straight through to
// modulepkg.CheckPath as if it were one literal path — for `module
// example.com/foo extra`, CheckPath naturally rejects the embedded space
// and this tool reported `malformed module path "example.com/foo extra":
// invalid char ' '`, actively misdirecting a user toward stripping
// characters from the module path instead of removing the stray trailing
// token that was never part of it.
func moduleLineHasExtraArgs(content string) bool {
	content = strings.TrimSpace(content)
	if content == "" {
		return false
	}
	if content[0] == '"' || content[0] == '`' {
		prefix, err := strconv.QuotedPrefix(content)
		if err != nil {
			// Not a well-formed quoted string after all; let the existing
			// unquote-or-literal fallback in parseModulePath handle it as
			// a single (malformed) argument instead of a count problem.
			return false
		}
		return strings.TrimSpace(content[len(prefix):]) != ""
	}
	return len(strings.Fields(content)) > 1
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
// as a double-quoted Go string literal (also valid go.mod syntax, just
// rarer).
//
// Mirrors golang.org/x/mod/modfile's own parseString (rule.go) exactly,
// confirmed by reading that function directly: a token is only ever
// unquoted via strconv.Unquote when it starts with a literal '"' — any
// other token (quoted or not) that contains a '"', '\”, or '`' ANYWHERE is
// an unconditional parse Fatal, with real go's own comment explaining why:
// "Other quotes are reserved both for possible future expansion and to
// avoid confusion." Before this fix, this function instead ran s straight
// through strconv.Unquote regardless of which quote character (if any)
// opened it — strconv.Unquote treats a backtick-delimited string as an
// ordinary Go raw string literal and happily unquotes it, since Go SOURCE
// CODE does support backtick raw strings; go.mod's own grammar does not
// extend that same allowance, confirmed live (2026-10-02) against a real
// go1.24.4 toolchain: a go.mod whose module directive reads
//
//	module `example.com/foo`
//
// makes `go list -m`/`go build` Fatal immediately and offline with
// `go.mod:1: invalid quoted string: unquoted string cannot contain quote`
// — never resolving anything, let alone contacting a proxy. Before this
// fix, moduleFromGoMod's no-argument-mode callers instead silently stripped
// the backticks, accepted "example.com/foo" as the module path, and probed
// it against the real proxy.golang.org, reporting the actively misleading
// statusModuleUnknown verdict ("check: is the repo public? does the module
// path in go.mod exactly match the repo? is it covered by a
// GOPRIVATE/GONOSUMDB pattern? ...") for a go.mod that never had a shot at
// resolving at all, since it doesn't even parse.
//
// Also fixes a narrower, same-root-cause case one call site already caught
// but misreported: a double-quoted path with a malformed Go-string escape
// (e.g. `module "example.com\xZZ"`, an invalid two-digit hex escape) was
// already rejected — strconv.Unquote fails on it too — but the old
// "return s unchanged" fallback handed modulepkg.CheckPath the raw,
// still-quoted literal, which blamed the embedded `"` characters
// ("malformed module path ...: invalid char '\"'") instead of citing the
// real, earlier go.mod-parse Fatal real go actually raises here too:
// confirmed live the identical file fails with `invalid quoted string:
// invalid syntax` (strconv.Unquote's own error), never reaching CheckPath's
// validation at all.
func parseModulePath(s string) (string, error) {
	s = stripLineComment(s)
	if strings.HasPrefix(s, `"`) {
		unquoted, err := strconv.Unquote(s)
		if err != nil {
			return "", fmt.Errorf("invalid quoted string: %v", err)
		}
		return unquoted, nil
	}
	if strings.ContainsAny(s, "\"'`") {
		return "", fmt.Errorf("invalid quoted string: unquoted string cannot contain quote")
	}
	return s, nil
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

// localGo111ModuleOff reports whether the local `go` command's effective
// GO111MODULE disables module mode outright. Reads it via `go env
// GO111MODULE` rather than os.Getenv("GO111MODULE") directly, the same
// go-env-reading shape as firstGoproxyEntry and every other local*
// check in this file, so a value persisted with `go env -w` is picked up
// too.
//
// Confirmed live (2026-10-02, go1.24.4): `GO111MODULE=off go install
// golang.org/x/example/hello@latest` fails immediately and unconditionally
// with `go: modules disabled by GO111MODULE=off; see 'go help modules'`,
// entirely offline, before proxy.golang.org, sum.golang.org, or any VCS
// host is ever contacted — reproduced identically for `go get`, and for a
// module path that's already known to be syntactically invalid (a stray
// '!' character) or already blocklisted, proving this Fatal preempts even
// the module-path-validation check resolveTarget already performs, not
// just the proxy probe. This isn't module-specific or even GOPROXY-shaped
// local config: it precedes every other local-config gate this tool already
// models — confirmed live that combining it with a malformed GOPROXY
// (`GOPROXY=,`), a malformed GOVCS (`GOVCS=badrule`), or a malformed GOAUTH
// (`GOAUTH="off;netrc"`) always surfaces the identical GO111MODULE error,
// never any of those others, since go never gets as far as parsing any of
// them when module mode itself is off.
//
// GO111MODULE is legacy (module mode has been the unconditional default
// since Go 1.16 for any directory with a go.mod — `go help modules`), but
// "off" is not a no-op relic the way GONOSUMCHECK is: it's still read and
// still Fatal on every module-aware command in every currently supported Go
// release, including the one this tool's own go.mod requires. A stray
// GO111MODULE=off is realistic, not just theoretical: it's exactly the kind
// of var a legacy GOPATH-era shell profile, Makefile, or CI job still
// exports out of habit long after the project itself moved to modules.
//
// Before this check existed, goproxycheck had no detection for this at
// all: with GO111MODULE=off set and an otherwise fully live, healthy
// module@version, it probed proxy.golang.org/sum.golang.org directly and
// reported plain statusReady ("a plain `go install` will work") — the
// opposite of reality, the same "local config error, not a proxy-
// availability problem" shape already handled for GOPROXY=off
// (localGoproxyOff) and every other local*ConfigError in this file, just
// for the one gate that, uniquely, precedes all of them.
func localGo111ModuleOff() bool {
	out, err := exec.Command("go", "env", "GO111MODULE").Output()
	if err != nil {
		return false // best-effort: don't block the real check on this
	}
	return strings.TrimSpace(string(out)) == "off"
}

// localGoflagsModVendor reports whether the local `go` command's effective
// GOFLAGS (env var or persisted via `go env -w`) carries a `-mod=vendor` (or
// `--mod=vendor`) entry. GOFLAGS is documented ("go help environment") as "a
// space-separated list of -flag=value settings to apply to go commands by
// default, when the given flag is known by the current command" — cmd/go
// itself splits it on whitespace (strings.Fields) before matching each token
// against the flags the invoked subcommand actually registers, so this
// mirrors that: any whitespace-separated token, dash-trimmed, equal to
// exactly "mod=vendor" counts, regardless of how many other flags share the
// string or how much extra whitespace surrounds it.
//
// Confirmed live (2026-10-02, go1.26.8) that this Fatals `go install`/`go
// get module@version` immediately and unconditionally with `cannot query
// module due to -mod=vendor` — entirely offline, zero network trace under
// `go install -x` — the moment it would otherwise need to resolve the
// module, even in a directory with no go.mod and no vendor/ directory at
// all (this isn't a vendor-consistency check against a real vendor tree;
// `-mod=vendor` disables the module-query mechanism itself, unconditionally,
// for any target). This holds via a bare env var and via `go env -w
// GOFLAGS=-mod=vendor`, with `--mod=vendor` (double-dash) and extra
// leading/trailing/interior whitespace (`"  -mod=vendor  "`, `"-x
// -mod=vendor"`) all equally effective — but `-mod=mod`/`-mod=readonly`
// (every other `-mod` value) are not, so this must match the literal
// "vendor" value, not just the flag's presence.
//
// Verified the ordering empirically against real go, each case in its own
// fresh GOMODCACHE to rule out module-cache contamination: `GO111MODULE=off`
// and a malformed/empty-list `GOPROXY` both still Fatal with their own error
// ahead of this one (so those checks, positioned earlier in run()'s dispatch
// chain, correctly stay there) — but a matching `GOPRIVATE`/`GONOPROXY` (the
// localModulePrivate branch below, which would otherwise claim "fetches
// directly from VCS, never touches the proxy") and `GOPROXY=off` are both
// still preempted by `-mod=vendor`: real `go install` Fatals on the
// `-mod=vendor` query error regardless of either, so this check is placed
// ahead of both in run()'s chain.
//
// Before this check existed, goproxycheck had no detection for this at all:
// with GOFLAGS=-mod=vendor set and an otherwise fully live, healthy
// module@version, it probed proxy.golang.org/sum.golang.org directly and
// reported plain statusReady ("a plain `go install` will work") — the
// opposite of reality, the same "local config error, not a proxy-
// availability problem" shape already handled for GO111MODULE=off
// (localGo111ModuleOff) and every other local*ConfigError in this file.
func localGoflagsModVendor() bool {
	out, err := exec.Command("go", "env", "GOFLAGS").Output()
	if err != nil {
		return false // best-effort: don't block the real check on this
	}
	for _, tok := range strings.Fields(string(out)) {
		if strings.TrimLeft(tok, "-") == "mod=vendor" {
			return true
		}
	}
	return false
}

// goproxyEmptyListError reports the error real cmd/go's own GOPROXY-list
// parsing (proxyList, cmd/go/internal/modfetch/proxy.go, verified directly
// against that source) raises for raw when every entry parses away to
// nothing: once blank entries (from a leading/trailing/doubled separator, or
// a whitespace-only entry) are dropped, and anything after an "off"/"direct"
// terminator is discarded, zero entries are left at all. nil when at least
// one real entry survives — including just "off" or "direct" alone, both of
// which are themselves valid single entries, not an empty list.
//
// Confirmed live (2026-10-01): `GOPROXY=, go install golang.org/x/mod@v0.19.0`
// and `GOPROXY=" " go install golang.org/x/mod@v0.19.0` both fail immediately
// and unconditionally with `go: golang.org/x/mod@v0.19.0: GOPROXY list is not
// the empty string, but contains no entries` — entirely offline, before ever
// contacting proxy.golang.org or any VCS host. This holds regardless of
// GOPRIVATE/GONOPROXY: `GOPROXY=, GOPRIVATE=golang.org/x/mod go install
// golang.org/x/mod@v0.19.0` fails the exact same way, even though the module
// matches GOPRIVATE and would otherwise be fetched directly, never touching
// any proxy at all — reading cmd/go's own proxyList source directly explains
// why: it adds an implicit "noproxy" pseudo-entry when GONOPROXY is set, but
// still treats a list containing only that one pseudo-entry as empty for
// this exact error, so a private-module match never rescues an otherwise-
// empty GOPROXY from this failure.
//
// Before this check existed, goproxycheck had no detection for this case at
// all: a bare `GOPROXY=,` run against a fully healthy, live module reported
// plain statusReady ("a plain `go install` will work"), and the identical
// config with GOPRIVATE additionally matching the module reported
// statusPrivateModuleLocally ("will fetch it directly from its VCS host
// here... this tool's proxy/sumdb checks don't apply to it") — both the
// opposite of reality: the real command never reaches the proxy, sumdb, or
// VCS host at all, failing outright on the GOPROXY config itself first, the
// same "local config error, not a proxy-availability problem" shape already
// handled for a malformed GOVCS (govcsConfigError) and GOSUMDB
// (gosumdbConfigError).
func goproxyEmptyListError(raw string) error {
	if entries, _ := parseGoproxyChain(raw); len(entries) == 0 {
		return fmt.Errorf("GOPROXY list is not the empty string, but contains no entries")
	}
	return nil
}

// localGoproxyEmptyListError is the go-env-reading wrapper around
// goproxyEmptyListError, the same shape as localGovcsConfigError/
// localGosumdbConfigError: it reads the local `go` command's actual
// effective GOPROXY (via `go env GOPROXY`, so a value persisted with `go env
// -w` is picked up too, not just an explicit env var) rather than assuming
// os.Getenv("GOPROXY") directly reflects it. raw is returned alongside err
// so callers can quote the exact configured value in their message. Best-
// effort like its siblings: a failed `go env` call doesn't block the real
// check (returns "", nil).
func localGoproxyEmptyListError() (raw string, err error) {
	out, cmdErr := exec.Command("go", "env", "GOPROXY").Output()
	if cmdErr != nil {
		return "", nil
	}
	raw = strings.TrimSpace(string(out))
	return raw, goproxyEmptyListError(raw)
}

// goproxyEntrySchemeError mirrors the URL-shape validation real cmd/go's own
// newProxyRepo (modfetch/proxy.go, verified directly against that source)
// performs on *every* GOPROXY entry it actually tries to use — not just the
// first, and not gated on whether that entry is ever reached by a
// particular module's fetch — before ever issuing a request to it: after
// the same implicit-"https://"-prefix normalization normalizeGoproxyURL
// already applies, the (possibly normalized) entry must parse as a URL
// whose scheme is "http", "https", or a "file" URL with no extra non-path
// components (no host, userinfo, query, or fragment). Any other outcome —
// no scheme at all, a scheme that isn't one of those three, or a file://
// URL carrying something beyond a bare path — fails this exact way,
// entirely offline, regardless of network reachability or which module is
// being fetched.
//
// Confirmed live (2026-10-02), all three shapes, both go1.24.4 and
// go1.27.1: `GOPROXY=localhost go mod download golang.org/x/text@v0.14.0`
// (a single bare word — no dot, colon, or slash, so normalizeGoproxyURL's
// own implicit-https rule doesn't apply, the same "natural misconfiguration
// by analogy to GOPRIVATE's bare-host patterns" shape documented on
// normalizeGoproxyURL, but this time for a host with no TLD at all, e.g. a
// local dev proxy a user forgets to give a port/scheme) fails outright with
// `invalid proxy URL missing scheme: localhost`; `GOPROXY=ftp://example.com`
// fails with `invalid proxy URL scheme (must be https, http, file):
// ftp://example.com`; `GOPROXY=file:///tmp/x?y=1` fails with `invalid
// file:// proxy URL with non-path elements: file:///tmp/x?y=1`.
func goproxyEntrySchemeError(entry string) error {
	base, err := url.Parse(normalizeGoproxyURL(entry))
	if err != nil {
		return err
	}
	switch base.Scheme {
	case "http", "https":
		return nil
	case "file":
		if *base != (url.URL{Scheme: base.Scheme, Path: base.Path, RawPath: base.RawPath}) {
			return fmt.Errorf("invalid file:// proxy URL with non-path elements: %s", base.Redacted())
		}
		return nil
	case "":
		return fmt.Errorf("invalid proxy URL missing scheme: %s", base.Redacted())
	default:
		return fmt.Errorf("invalid proxy URL scheme (must be https, http, file): %s", base.Redacted())
	}
}

// goproxyMalformedEntryError reports the first entry in raw's GOPROXY chain
// (left to right, stopping at — and never validating past — a terminating
// "off"/"direct", exactly like parseGoproxyChain already models: see its own
// doc comment) that fails goproxyEntrySchemeError, along with the error it
// fails with. Returns ("", nil) when every entry actually reached parses
// cleanly.
//
// This matters for an entry anywhere in the chain, not just the first: real
// cmd/go's own proxyList (modfetch/proxy.go) builds and validates its
// *entire* entry list eagerly, inside one process-wide sync.Once, before
// TryProxies ever attempts the first one — so one malformed entry breaks
// every subsequent `go` module operation in the process outright, for every
// module, regardless of whether an earlier entry in the same list is a
// perfectly healthy proxy.golang.org that would otherwise have answered
// first. Confirmed live (2026-10-02): `GOPROXY="https://proxy.golang.org,localhost"
// go mod download golang.org/x/text@v0.14.0` — a fully healthy public-proxy
// entry *first*, with a malformed trailing entry only ever meant as a
// fallback — fails outright with `invalid proxy URL missing scheme:
// localhost`, never even attempting the public proxy; the identical failure
// also preempts a GOPRIVATE match that would otherwise fetch directly from
// VCS (`GOPROXY="https://proxy.golang.org,localhost" GOPRIVATE=<module> go
// mod download <module>@latest` fails the exact same way, never reaching
// the VCS fetch), since TryProxies calls proxyList() — and so hits this same
// eager, whole-string parse — before ever trying the "noproxy"/"direct"
// pseudo-entries a private-module match resolves through. A GOPROXY entry
// placed *after* a terminating "off"/"direct" is never actually reached by
// that parse (confirmed live: `GOPROXY=off,localhost` fails with the
// ordinary `module lookup disabled by GOPROXY=off`, not a scheme error;
// `GOPROXY=direct,localhost` succeeds fetching an ordinary public module via
// direct VCS, `localhost` never consulted at all) — mirrored here by
// stopping at the same point parseGoproxyChain already stops.
//
// Before this check existed, goproxycheck had no detection for this shape
// at all: a GOPROXY chain like "https://proxy.golang.org,localhost" against
// a fully live, healthy module reported plain statusReady ("a plain `go
// install` will work"), the opposite of reality — the real command never
// even attempts the public proxy, let alone succeeds against it. The same
// "local config error, not a proxy-availability problem" shape already
// handled for a malformed GOVCS/GOSUMDB/GOAUTH (govcsConfigError,
// gosumdbConfigError, goAuthConfigError) and for a GOPROXY that parses to
// zero entries (goproxyEmptyListError, right above).
func goproxyMalformedEntryError(raw string) (entry string, err error) {
	entries, _ := parseGoproxyChain(raw)
	for _, e := range entries {
		if e == "off" || e == "direct" {
			return "", nil
		}
		if schemeErr := goproxyEntrySchemeError(e); schemeErr != nil {
			return e, schemeErr
		}
	}
	return "", nil
}

// localGoproxyMalformedEntryError is the go-env-reading wrapper around
// goproxyMalformedEntryError, the same shape as localGoproxyEmptyListError
// right above: it reads the local `go` command's actual effective GOPROXY
// (via `go env GOPROXY`, so a value persisted with `go env -w` is picked up
// too) rather than assuming os.Getenv("GOPROXY") directly reflects it.
// Best-effort like its siblings: a failed `go env` call doesn't block the
// real check (returns "", "", nil).
func localGoproxyMalformedEntryError() (raw, entry string, err error) {
	out, cmdErr := exec.Command("go", "env", "GOPROXY").Output()
	if cmdErr != nil {
		return "", "", nil
	}
	raw = strings.TrimSpace(string(out))
	entry, err = goproxyMalformedEntryError(raw)
	return raw, entry, err
}

// goAuthConfigError reports the error real cmd/go's own GOAUTH parsing
// (runGoAuth, cmd/go/internal/auth/auth.go, verified directly against that
// source) Fatals with for raw before the very *first* HTTPS request of the
// entire process — confirmed directly against cmd/go/internal/web/http.go's
// fetch closure, which calls `auth.AddCredentials(client, req, nil, "")`
// unconditionally ahead of `client.Do(req)` for every URL with an "https"
// scheme, and auth.AddCredentials parses the whole GOAUTH value (via a
// `sync.Once`) the first time it's ever called in the process. Since the
// default (and overwhelmingly common) GOPROXY is `https://proxy.golang.org`,
// and sum.golang.org lookups are HTTPS too, this Fatals before proxy.golang.org
// or sum.golang.org are ever contacted — entirely offline, regardless of
// whether the target module is otherwise perfectly healthy.
//
// nil when raw parses cleanly: "netrc" alone (the default), "off" alone, a
// custom auth command, a well-formed "git <absolute-dir>", or any
// semicolon-separated combination of those that isn't one of the specific
// Fatal shapes below.
//
// Confirmed live (2026-10-02), entirely offline (a deliberately
// unreachable `https://` GOPROXY returns in ~3ms instead of timing out, vs.
// 5+ seconds for the identical setup with a well-formed GOAUTH): both
// `GOAUTH="off;netrc"` and `GOAUTH="netrc;;netrc"` (a stray double
// semicolon — a natural copy-paste/templating typo) Fatal `go
// install`/`go get`/`go mod download` of a perfectly healthy, live module
// against the real public proxy.golang.org, before sending a single
// request — the same "go itself would Fatal first" shape already modeled
// for a malformed GOVCS/GOPROXY/GOSUMDB (see govcsConfigError,
// goproxyEmptyListError, gosumdbConfigError). GOAUTH itself was only added
// in Go 1.24 (`go help goauth`), and goproxycheck had no detection for it
// at all before this: a malformed GOAUTH with an otherwise fully healthy
// target module used to report plain statusReady.
//
// Deliberately only two of runGoAuth's Fatal conditions are modeled as
// "empty command"/"off combined with other commands" above the switch, plus
// the three "git <dir>" argument-shape ones below — every one of them is a
// property of the GOAUTH string (plus, for "git", the local filesystem)
// alone, checkable with zero network access, exactly mirroring what
// govcsConfigError already does for GOVCS. The "netrc"/custom-command cases
// are deliberately NOT modeled: a bad netrc file or a failing custom auth
// command is collected and only reported later, non-fatally, if that
// specific credential set was actually needed and still missing — not an
// unconditional offline Fatal the way these are.
func goAuthConfigError(raw string) error {
	cmds := strings.Split(raw, ";")
	// Real go processes commands in reverse order (runGoAuth's own comment:
	// "GOAUTH commands are processed in reverse order to prioritize
	// credentials in the order they were specified") — mirrored here so
	// that, when more than one entry is independently malformed, this
	// reports the same one a real `go` run would Fatal on first.
	for i := len(cmds) - 1; i >= 0; i-- {
		command := strings.TrimSpace(cmds[i])
		words := strings.Fields(command)
		if len(words) == 0 {
			return fmt.Errorf("GOAUTH encountered an empty command (GOAUTH=%s)", raw)
		}
		switch words[0] {
		case "off":
			if len(cmds) != 1 {
				return fmt.Errorf("GOAUTH=off cannot be combined with other authentication commands (GOAUTH=%s)", raw)
			}
			return nil
		case "git":
			if len(words) != 2 {
				return fmt.Errorf("GOAUTH=git dir method requires an absolute path to the git working directory")
			}
			dir := words[1]
			if !filepath.IsAbs(dir) {
				return fmt.Errorf("GOAUTH=git dir method requires an absolute path to the git working directory, dir is not absolute")
			}
			fi, statErr := os.Stat(dir)
			if statErr != nil {
				return fmt.Errorf("GOAUTH=git encountered an error; cannot stat %s: %v", dir, statErr)
			}
			if !fi.IsDir() {
				return fmt.Errorf("GOAUTH=git dir method requires an absolute path to the git working directory, dir is not a directory")
			}
		}
	}
	return nil
}

// localGoAuthConfigError is the go-env-reading wrapper around
// goAuthConfigError, the same shape as localGoproxyEmptyListError/
// localGovcsConfigError: reads the local `go` command's actual effective
// GOAUTH (via `go env GOAUTH`, so a value persisted with `go env -w` is
// picked up too) rather than assuming os.Getenv("GOAUTH") reflects it.
// Best-effort like its siblings: a failed `go env` call doesn't block the
// real check.
func localGoAuthConfigError() error {
	out, err := exec.Command("go", "env", "GOAUTH").Output()
	if err != nil {
		return nil
	}
	return goAuthConfigError(strings.TrimSpace(string(out)))
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

// localGoproxyDirectFallback reports whether the local `go` command's
// effective GOPROXY chain places "direct" as the very next entry right
// after the public proxy.golang.org entry this tool actually probes. This
// is deliberately narrow (only the *immediately following* entry, not
// "direct appears somewhere later"): an intervening custom proxy this tool
// has no way to probe could itself swallow the 404 differently, so only
// the unambiguous, directly-adjacent case is claimed here.
//
// This matters because of how real cmd/go's own proxy-list fallback
// actually behaves (confirmed directly against cmd/go/internal/modfetch's
// TryProxies/lookup source, and live end-to-end): a 404 from one proxy in
// the chain doesn't just fail that one HTTP request, it makes the go
// command retry the *entire* module lookup (Stat/GoMod/Zip — everything
// this tool's negative-cache diagnoses are built on) against the next
// entry in the chain. Reproduced live (2026-09-29): with
// GOPROXY="<a proxy that 404s everything>,direct", `go install
// github.com/experimental-gains/goproxycheck@v0.1.55` (a real, live tag)
// still succeeds outright — go falls back to a direct git fetch and
// installs the binary normally, exactly as it would for a module that's
// genuinely negative-cached on the real proxy.golang.org. The overwhelming
// majority of installs never override GOPROXY at all, and the *default*
// value is exactly this shape: "https://proxy.golang.org,direct" (`go help
// goproxy`).
//
// Before this existed, a negative-cache verdict (module-level or
// per-version) told the user flatly that nothing but waiting or a new tag
// would help and that "GOPROXY=direct works around it for your own local
// build" as if that required deliberately overriding GOPROXY — when in
// fact the ordinary, completely unmodified default config a plain `go
// install` uses out of the box already ends in ",direct" and would very
// likely succeed right now, no override needed.
func localGoproxyDirectFallback() bool {
	out, err := exec.Command("go", "env", "GOPROXY").Output()
	if err != nil {
		return false // best-effort: don't block the real check on this
	}
	entries, _ := parseGoproxyChain(strings.TrimSpace(string(out)))
	for i, url := range entries {
		switch normalizeGoproxyURL(url) {
		case defaultProxyBase, defaultProxyBase + "/":
			return i+1 < len(entries) && entries[i+1] == "direct"
		}
	}
	return false
}

// negativeCacheDirectFallbackNote returns an additional caveat to append to
// a negative-cache diagnosis (module-level or per-version) when a plain
// `go install`/`go get` run on this exact machine right now would likely
// still succeed via cmd/go's own automatic direct-VCS fallback, despite the
// proxy negative cache — see localGoproxyDirectFallback's doc comment.
// Returns "" when that isn't actually established: either the GOPROXY
// chain doesn't fall back to direct immediately after the public proxy, the
// module isn't rooted at github.com (the only host this tool has VCS-type
// certainty for — see githubRepoPattern's doc comment), or the local GOVCS
// setting would itself block or fail to parse for a direct git fetch of
// this module (mirroring the same two-stage GOVCS check used elsewhere in
// run(): a malformed GOVCS blocks every direct fetch outright, regardless
// of whether any one rule would otherwise have allowed this module).
func negativeCacheDirectFallbackNote(module string) string {
	if !localGoproxyDirectFallback() {
		return ""
	}
	if !githubRepoPattern.MatchString(module) {
		return ""
	}
	if localGovcsConfigError() != nil {
		return ""
	}
	if !localGovcsAllowsGit(module, localGovcsPrivate(module)) {
		return ""
	}
	return "Note: your local `GOPROXY` chain falls back to `direct` immediately after proxy.golang.org (this is the out-of-the-box default, unless you've deliberately changed it) — real cmd/go retries the *entire* lookup against the next entry on a 404 like this one, not just this single request, so a plain `go install`/`go get` run here right now will likely still succeed via a direct git fetch, without waiting for the cache to clear or cutting a new tag. This is specific to this exact local/CI config, though: it says nothing about what someone with GOPROXY=off, a proxy-only chain, or a GOVCS rule blocking git would see from the same negative cache."
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
//
// Matches against githubRepoRoot(module), NOT module itself: real cmd/go's
// checkGOVCS (internal/vcs/vcs.go) computes `private` from
// `module.MatchPrefixPatterns(cfg.GOPRIVATE, root)`, where root comes from
// VCS-root resolution (vcsPaths' github.com regexp, `^(?P<root>github\.com/
// [\w.\-]+/[\w.\-]+)(/[\w.\-]+)*$`) — always exactly two path segments after
// "github.com", discarding anything past it, including a major-version
// suffix like "/v2" or a monorepo subdirectory. A GOPRIVATE pattern that's
// more specific than the repo root (e.g. it names the versioned or nested
// import path itself, not just the bare owner/repo) can match the full
// module path while never matching the truncated root real `go` actually
// checks. Confirmed live (2026-09-28): with GOPRIVATE=
// "github.com/googleapis/gax-go/v2" (naming the exact module path of a real
// module that lives in an actual "v2" subdirectory of its repo) and
// GOVCS="public:off,private:git", `go mod download -x
// github.com/googleapis/gax-go/v2@v2.7.0` fails with "GOVCS disallows using
// git for *public* github.com/googleapis/gax-go" — real go classified it
// public (root "github.com/googleapis/gax-go" doesn't match the pattern)
// despite the GOPRIVATE pattern matching the full module path exactly. This
// function used to match against module directly, so it reported private
// here, making localGovcsAllowsGit evaluate the "private:git" rule instead
// of the "public:off" rule real go actually applies — this tool then
// reported "will fetch it directly, no problem" for a fetch that fails
// outright.
func localGovcsPrivate(module string) bool {
	out, err := exec.Command("go", "env", "GOPRIVATE").Output()
	if err != nil {
		return false // best-effort: don't block the real check on this
	}
	root := module
	if r := githubRepoRoot(module); r != "" {
		root = r
	}
	return matchesAnyPattern(root, splitPatterns(strings.TrimSpace(string(out))))
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
// of scope), so it's gated on that at the call site in run(), not here. That
// same precondition means githubRepoRoot(module) is always non-empty here.
//
// Passes githubRepoRoot(module), not module itself, to govcsAllowsGit's own
// pattern-glob matching (the non-public/non-private "default:" case in its
// rule loop): real cmd/go's checkGOVCS calls govcs.allow(root, ...) with the
// same VCS-resolved root used for the private/public classification (see
// localGovcsPrivate's doc comment for why that's truncated to just
// owner/repo), so a GOVCS host pattern as specific as the full module path
// (e.g. "github.com/googleapis/gax-go/v2:off") would match module directly
// but never match the root real go actually checks it against.
func localGovcsAllowsGit(module string, private bool) bool {
	out, err := exec.Command("go", "env", "GOVCS").Output()
	if err != nil {
		return true // best-effort: don't block the real check on this
	}
	return govcsAllowsGit(githubRepoRoot(module), private, strings.TrimSpace(string(out)))
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

// sumdbAlwaysRequired reports whether module is cmd/go's one hardcoded
// exception to every GOSUMDB/GONOSUMDB skip rule the rest of this file
// models — confirmed directly against modfetch/sumdb.go's own useSumDB:
// for module path "golang.org/toolchain" specifically, it sets `must :=
// true` and returns true unconditionally, ahead of (and regardless of) the
// ordinary `cfg.GOSUMDB != "off" && !module.MatchPrefixPatterns(cfg.GONOSUMDB,
// mod.Path)` check every other module path goes through — "Downloaded
// toolchains cannot be listed in go.sum, so we require checksum database
// lookups even if GOSUMDB=off or GONOSUMDB matches the pattern," per that
// function's own comment. (Its own two further exceptions — a
// `file://`-only GOPROXY used for distpack testing, or GIT_HTTP_USER_AGENT
// naming proxy.golang.org — are internal Go-infrastructure signals this
// tool has no way to read and no realistic end-user/CI config would set,
// so they're deliberately not modeled here.)
//
// Confirmed live (2026-10-02), both directions, against a fresh, isolated
// GOMODCACHE: `GOSUMDB=off go mod download
// golang.org/toolchain@v0.0.1-go1.23.0.linux-amd64` fails outright with
// `checksum database disabled by GOSUMDB=off` — the literal dbDial() error
// for GOSUMDB=off — while the identical GOSUMDB=off against an ordinary
// module (golang.org/x/text@v0.14.0) succeeds, with zero sum.golang.org
// requests in its -x trace. And with GONOSUMDB="golang.org/toolchain" (an
// exact match) plus a deliberately malformed custom GOSUMDB,
// golang.org/toolchain still fails with "invalid GOSUMDB: malformed
// verifier id", while golang.org/x/text under that identical GONOSUMDB
// (matching it instead) succeeds untouched — proving the GONOSUMDB match
// that exempts every ordinary module from verification does nothing at all
// for this one path. See localGosumdbConfigError and localSumdbSkipped,
// both of which used to treat this module exactly like any other.
func sumdbAlwaysRequired(module string) bool {
	return module == "golang.org/toolchain"
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
//
// The GONOSUMDB-match exemption is skipped entirely when
// sumdbAlwaysRequired(module) — see its doc comment: a GONOSUMDB pattern
// matching "golang.org/toolchain" doesn't exempt it from verification the
// way it would any other module, so a malformed custom $GOSUMDB still
// Fatals for it even then. The separate GOSUMDB=off case for this same
// module is deliberately NOT handled here (see localGosumdbOffBlocksToolchain
// instead): off parses fine and isn't "malformed" — it just doesn't mean
// what it means for every other module — so it needs its own honestly
// worded diagnosis rather than this function's "invalid GOSUMDB" framing.
func localGosumdbConfigError(module string) error {
	out, err := exec.Command("go", "env", "GOSUMDB").Output()
	if err != nil {
		return nil
	}
	gosumdb := strings.TrimSpace(string(out))
	if gosumdb == "off" {
		return nil
	}
	if !sumdbAlwaysRequired(module) {
		if nonsumOut, err := exec.Command("go", "env", "GONOSUMDB").Output(); err == nil {
			if matchesAnyPattern(module, splitPatterns(strings.TrimSpace(string(nonsumOut)))) {
				return nil
			}
		}
	}
	return gosumdbConfigError(gosumdb)
}

// localGosumdbOffBlocksToolchain reports whether the local `go` command's
// effective GOSUMDB is "off" while module is sumdbAlwaysRequired — the one
// case where that ordinarily-harmless, common setting instead makes every
// real `go install`/`go get`/`go mod download` of module Fatal outright,
// regardless of proxy or sumdb state. See sumdbAlwaysRequired's doc
// comment for the live confirmation. Best-effort like its siblings: a
// failed `go env` call doesn't block the real check.
func localGosumdbOffBlocksToolchain(module string) bool {
	if !sumdbAlwaysRequired(module) {
		return false
	}
	out, err := exec.Command("go", "env", "GOSUMDB").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "off"
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
//
// None of this applies when sumdbAlwaysRequired(module) — see its doc
// comment: for "golang.org/toolchain" specifically, GOSUMDB=off blocks the
// install outright instead of skipping verification (handled as its own
// diagnosis by localGosumdbOffBlocksToolchain, not a "skip" here) and a
// matching GONOSUMDB pattern does nothing at all, so neither reason this
// function would otherwise return true for actually holds for it. A
// well-formed custom (non-public, non-"off") GOSUMDB is unaffected by this
// exception and still counts as skipped below: real go redirects
// verification to that database instead of the public sum.golang.org this
// tool probes either way, for every module including this one.
func localSumdbSkipped(module string) (skipped bool, reason string) {
	if out, err := exec.Command("go", "env", "GOSUMDB").Output(); err == nil {
		gosumdb := strings.TrimSpace(string(out))
		if gosumdb == "off" {
			if !sumdbAlwaysRequired(module) {
				return true, "GOSUMDB=off"
			}
		} else if name := sumdbName(gosumdb); name != "sum.golang.org" {
			return true, fmt.Sprintf("custom GOSUMDB %q", name)
		}
	}
	if sumdbAlwaysRequired(module) {
		return false, ""
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
//
// A module defined in a repository subdirectory needs one more step before
// that semver check: real Go module versioning (go.dev/ref/mod#vcs-version)
// requires such a module's tags to be written "<subdir>/vX.Y.Z", with the
// subdirectory path (relative to the repo root) as a literal prefix — the
// module version itself is only the "vX.Y.Z" part after it, never the raw
// tag. Confirmed live against a real nested module, golang.org/x/tools/gopls
// (module path golang.org/x/tools/gopls, defined in the "gopls" subdirectory
// of github.com/golang/tools): proxy.golang.org's own @latest response for
// it names the underlying tag "refs/tags/gopls/v0.23.0" for module version
// "v0.23.0" — the "gopls/" prefix is part of the git tag, not part of the
// version the proxy indexes it under (confirmed the mismatch is fatal to a
// direct request too: GET .../golang.org/x/tools/gopls/@v/gopls/v0.23.0.info
// 404s with "bad request: invalid escaped version ...: invalid char '/'",
// while .../@v/v0.23.0.info succeeds). Before this, gitDescribeTag knew
// nothing about the module's subdirectory and returned a lone matching tag
// exactly as git wrote it — so goproxycheck's no-argument mode, run from
// inside such a subdirectory right after tagging a real, already-live
// release, reported "not-yet-indexed ... retry in a minute, or use --wait"
// for a version that could never be indexed under that literal (prefixed)
// spelling no matter how long it was polled.
//
// gitTagPrefix (git rev-parse --show-prefix) gives exactly this
// subdirectory, already in the "dir/" form real Go's tag convention uses,
// empty at the repo root. Root-module behavior (the common case, and the
// only case any existing test covers) is deliberately unchanged: this only
// narrows the semver-shaped candidate set to those additionally carrying the
// module's own prefix, then re-applies the exact same
// zero/one/many decision the root case already used — including the
// existing single-arbitrary-tag pass-through (return the lone tag exactly
// as written) when nothing at HEAD looks like a real version tag for this
// module either way, preserving support for checking an arbitrary
// non-semver revision tag.
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

	prefix := gitTagPrefix()

	var versionTags []string
	for _, t := range tags {
		if prefix == "" {
			if semver.IsValid(t) {
				versionTags = append(versionTags, t)
			}
			continue
		}
		if rest, ok := strings.CutPrefix(t, prefix); ok && semver.IsValid(rest) {
			versionTags = append(versionTags, rest)
		}
	}

	switch len(versionTags) {
	case 1:
		return versionTags[0], nil
	case 0:
		if len(tags) == 1 {
			return tags[0], nil
		}
		return "", fmt.Errorf("HEAD has more than one tag (%s) and it's ambiguous which one is the release version — pass module@version explicitly", strings.Join(tags, ", "))
	default:
		return "", fmt.Errorf("HEAD has more than one tag (%s) and it's ambiguous which one is the release version — pass module@version explicitly", strings.Join(tags, ", "))
	}
}

// gitTagPrefix returns the module's own version-tag prefix, per real Go's
// subdirectory-tag convention (see gitDescribeTag's doc comment): "" for a
// module at the repository root, or "<subdir>/" for one defined in a
// subdirectory. Backed by `git rev-parse --show-prefix`, which already
// reports the path from the repo root to the current directory in exactly
// that "dir/" form (confirmed live: empty output at the repo root, "sub/
// dir/" — trailing slash included — from within ./sub/dir). Errors are
// deliberately swallowed to "" (root-module behavior, this function's
// existing, well-tested behavior before subdirectory-awareness existed)
// rather than failing gitDescribeTag outright — by the time this is called,
// `git tag --points-at HEAD` already succeeded, so this realistically only
// fails for a git old enough to lack --show-prefix, not a validity problem
// worth surfacing as a hard error.
func gitTagPrefix() string {
	out, err := exec.Command("git", "rev-parse", "--show-prefix").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
