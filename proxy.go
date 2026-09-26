package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const (
	defaultProxyBase     = "https://proxy.golang.org"
	defaultSumBase       = "https://sum.golang.org"
	defaultRepoCheckBase = "https://github.com"
)

// endpoints holds the base URLs so tests can point at an httptest server.
type endpoints struct {
	proxyBase     string
	sumBase       string
	repoCheckBase string
	client        *http.Client
}

func defaultEndpoints() endpoints {
	return endpoints{
		proxyBase:     defaultProxyBase,
		sumBase:       defaultSumBase,
		repoCheckBase: defaultRepoCheckBase,
		client:        &http.Client{Timeout: 15 * time.Second},
	}
}

// githubRepoPattern matches module paths rooted directly at github.com
// (owner/repo, optionally with a nested/major-version subpath). Only this
// shape gets the live-reachability check in probe(): for any other host
// (gitlab.com, vanity import paths with a go-import redirect, etc.) there's
// no reliable way to know how many path segments form the repo root without
// following VCS discovery, which is out of scope here.
var githubRepoPattern = regexp.MustCompile(`^github\.com/([^/]+)/([^/]+)`)

// probeResult is the outcome of one HTTP GET against the proxy or sumdb.
type probeResult struct {
	ok         bool
	statusCode int
	body       string
	err        error
}

func (e endpoints) get(url string) probeResult {
	resp, err := e.client.Get(url)
	if err != nil {
		return probeResult{err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return probeResult{
		ok:         resp.StatusCode == http.StatusOK,
		statusCode: resp.StatusCode,
		body:       string(body),
	}
}

// report is every probe made against one module@version, plus the raw list
// of published versions (for the "was this version ever released" check).
type report struct {
	module, version string
	latest          probeResult
	list            probeResult
	versionInfo     probeResult
	sum             probeResult
	// modFile is the go.mod file body the proxy serves for the resolved
	// version (@v/<version>.mod), fetched only when versionInfo is a
	// confirmed 200 — see canonicalModuleNote in diagnose.go for why.
	modFile probeResult
	// latestModFile is the go.mod body of the highest tagged version in
	// the module's *current* major-version line — not necessarily
	// @latest's own version — independent of whichever version was
	// actually requested, and fetched whenever @latest succeeds. Real `go`
	// finds retract directives by loading go.mod from the version @latest
	// would resolve to *before* retractions are considered
	// (go.dev/ref/mod#go-mod-file-retract), not from @latest's own,
	// already-retraction-filtered result — those two only diverge when the
	// highest tag in the current major-version line is itself retracted.
	// Confirmed live (2026-09) against github.com/jayconrod/retract, the
	// Go team's own canonical self-retraction example: v1.0.1 retracts
	// both itself and v1.0.0, so @latest resolves past both to v0.9.9,
	// whose go.mod predates the retract directive and carries none —
	// fetching info.Version's go.mod unconditionally (the old behavior)
	// silently missed the retraction `go list -m -u` still correctly
	// reports by reading v1.0.1's go.mod instead. See probe()'s
	// normalizedMajor-scoped search and retraction() in retract.go.
	//
	// "Highest tag" is scoped to @latest's own major-version line
	// (collapsing v0/v1, matching golang.org/x/mod/semver.Major), not the
	// true global maximum across @v/list: confirmed live against
	// github.com/mattn/go-sqlite3@v2.0.3+incompatible — its highest tag
	// overall is an abandoned, bare v2 experiment with no retract block of
	// its own, while the retraction covering that exact version actually
	// lives in the still-active v1.x line's go.mod (v1.14.52). Scoping by
	// major line keeps both live-verified cases correct at once; using the
	// unscoped global max would have regressed this one instead.
	//
	// Known limitation: this only ever looks at the current major-version
	// line's highest tag, not at every version in between — real `go`'s
	// own resolution walks the module graph more thoroughly, so a version
	// retracted only by a later release that itself got superseded/retracted
	// in turn is a case this could miss. Not worth chasing: this already
	// catches the realistic cases confirmed live above.
	latestModFile probeResult
	// repoReachable is set only when latest/list both failed and the module
	// path is rooted at github.com: it distinguishes "the proxy has really
	// never heard of this" from "the repo is live and public right now, but
	// the proxy's own module-level negative cache hasn't cleared" — see
	// diagnose's statusModuleNegativeCache.
	repoReachable *bool
	// repoCheckedNestedPath is true when repoReachable was only able to
	// confirm the github.com/owner/repo root, not the exact module path —
	// true for any module with path segments past owner/repo (a major-
	// version subdirectory like /v2, a multi-module-repo subpackage, or a
	// plain typo). GitHub's bare (non-/tree/) URLs 404 for *any* such
	// nested path regardless of whether the segment is real, including the
	// common major-version-on-a-branch layout (e.g. github.com/redis/
	// go-redis/v9 lives at the repo root, not a /v9 subdirectory) — so
	// there's no reliable way to verify the nested segment itself here,
	// only the repo root. See diagnose's statusModuleNegativeCache.
	repoCheckedNestedPath bool
	// repoCheckStatusCode is the raw HTTP status of the repoReachable probe
	// (0 if no check was made, e.g. a transport error or a non-github.com
	// module path). A plain 404 means GitHub itself said the repo/path
	// doesn't exist; a 403 or 429 means the unauthenticated scrape request
	// this tool makes got rate-limited or blocked by GitHub's own abuse
	// protection, which looks identical to "not reachable" via .ok but
	// means the check is inconclusive, not a real negative answer. This
	// matters most when goproxycheck runs as the shipped GitHub Action: CI
	// runners doing frequent unauthenticated github.com GETs are a
	// realistic way to hit that limit. See diagnose's
	// statusRepoCheckInconclusive.
	repoCheckStatusCode int
	// resolvedVersion is set when version was a query (e.g. "latest", a
	// partial version like "v0.19", a comparison like "<v1.2.3", or a
	// revision identifier such as a branch name or commit hash — see
	// go.dev/ref/mod#version-queries) that the proxy resolved to a
	// concrete, canonical version. sum is probed against this resolved
	// version, not the literal query string. See probe().
	resolvedVersion string
}

func (e endpoints) probe(module, version string) report {
	mod := escapePath(module)
	r := report{
		module:  module,
		version: version,
		latest:  e.get(fmt.Sprintf("%s/%s/@latest", e.proxyBase, mod)),
		list:    e.get(fmt.Sprintf("%s/%s/@v/list", e.proxyBase, mod)),
	}

	if r.latest.ok {
		if info, err := parseVersionInfo(r.latest.body); err == nil && info.Version != "" {
			modVersion := info.Version
			if r.list.ok {
				wantMajor := normalizedMajor(modVersion)
				for _, v := range r.listedVersions() {
					if normalizedMajor(v) == wantMajor && semver.Compare(v, modVersion) > 0 {
						modVersion = v
					}
				}
			}
			r.latestModFile = e.get(fmt.Sprintf("%s/%s/@v/%s.mod", e.proxyBase, mod, escapePath(modVersion)))
		}
	}

	checkVersion := version
	if version == "latest" && r.latest.ok {
		// Confirmed live: `goproxycheck somemodule@latest` — the natural
		// invocation by analogy to `go install somemodule@latest`, the
		// standard Go idiom — used to probe @v/latest.info literally, which
		// the real proxy 404s with "invalid version" even for a perfectly
		// healthy module (verified against golang.org/x/mod@latest). That
		// misdiagnosed a completely ready module as statusNotYetIndexed and
		// told the user to retry or --wait, which would poll until timeout
		// since the literal string "latest" never appears in @v/list.
		if info, err := parseVersionInfo(r.latest.body); err == nil && info.Version != "" {
			checkVersion = info.Version
			r.resolvedVersion = info.Version
		}
	}

	r.versionInfo = e.get(fmt.Sprintf("%s/%s/@v/%s.info", e.proxyBase, mod, escapePath(checkVersion)))

	// Unlike "latest" (handled above, since it has no @v/latest.info
	// endpoint at all), other documented version queries — a partial
	// version like "v0.19", a comparison like "<v1.2.3", or a revision
	// identifier such as a branch name or commit hash — DO resolve
	// directly against @v/<query>.info: confirmed live that
	// proxy.golang.org accepts the literal query there and returns the
	// resolved canonical version in the response body (e.g. querying
	// .../@v/v0.19.info for golang.org/x/mod returns
	// {"Version":"v0.19.0",...}; .../@v/master.info similarly resolves to
	// whatever the current tip tag is). sum.golang.org's lookup endpoint,
	// unlike the proxy, only accepts a canonical version and returns 400
	// for a query string — so without this, any such query got a
	// permanently-failing sum.golang.org probe misdiagnosed as
	// statusSumdbLag ("retry shortly", and under --wait, polls to
	// timeout), when the module was actually fully ready right now via
	// its resolved canonical version. Verified live end-to-end: `go get
	// golang.org/x/mod@v0.19` and `go get golang.org/x/mod@master` both
	// succeed immediately, and their -x traces show the sum.golang.org
	// lookup made against the *resolved* canonical version, never the
	// literal query string.
	if r.versionInfo.ok && checkVersion == version {
		if info, err := parseVersionInfo(r.versionInfo.body); err == nil && info.Version != "" && info.Version != checkVersion {
			checkVersion = info.Version
			r.resolvedVersion = info.Version
		}
	}

	r.sum = e.get(fmt.Sprintf("%s/lookup/%s@%s", e.sumBase, mod, escapePath(checkVersion)))

	if r.versionInfo.ok {
		// The proxy resolves @latest/@v/<version>.info by VCS origin
		// discovery against the requested import path, not by checking
		// the module directive in that version's go.mod — confirmed live
		// (2026-09) that github.com/grpc/grpc-go/@latest and
		// google.golang.org/grpc/@latest return the identical
		// Version/Origin, even though grpc-go's go.mod has declared
		// "module google.golang.org/grpc" since it moved off the
		// github.com path. Fetching the actual .mod file here is the only
		// way to catch that: see canonicalModuleNote.
		r.modFile = e.get(fmt.Sprintf("%s/%s/@v/%s.mod", e.proxyBase, mod, escapePath(checkVersion)))
	}

	if !r.moduleKnown() {
		if m := githubRepoPattern.FindStringSubmatch(module); m != nil {
			checkResult := e.get(fmt.Sprintf("%s/%s/%s", e.repoCheckBase, m[1], m[2]))
			reachable := checkResult.ok
			r.repoReachable = &reachable
			r.repoCheckedNestedPath = m[0] != module
			r.repoCheckStatusCode = checkResult.statusCode
		}
	}
	return r
}

// moduleKnown reports whether the proxy has heard of the module at all,
// independent of the specific version being checked.
func (r report) moduleKnown() bool {
	if r.latest.ok {
		return true
	}
	// @v/list returns 200 with an empty body for a module with zero
	// tagged releases, so any 200 counts even if list.body is empty.
	return r.list.ok
}

// checkVersion is the concrete version actually probed against @v/<version>.info
// and sum.golang.org/lookup — the literal argument, unless it was a "latest"
// query resolved to a real version (see probe()).
func (r report) checkVersion() string {
	if r.resolvedVersion != "" {
		return r.resolvedVersion
	}
	return r.version
}

func (r report) listedVersions() []string {
	if !r.list.ok || strings.TrimSpace(r.list.body) == "" {
		return nil
	}
	return strings.Split(strings.TrimSpace(r.list.body), "\n")
}

func (r report) ready() bool {
	return r.versionInfo.ok && r.sum.ok
}

// versionInfoBody mirrors the JSON the proxy returns from an @v/<version>.info
// or @latest request. Only used to surface the resolved version in output.
type versionInfoBody struct {
	Version string
	Time    string
}

func parseVersionInfo(body string) (versionInfoBody, error) {
	var v versionInfoBody
	err := json.Unmarshal([]byte(body), &v)
	return v, err
}

// normalizedMajor returns v's major-version-compatibility line, the way
// the go command's own major-version-suffix rules group them: v0 and v1
// collapse into the same implicit line (neither ever carries a path
// suffix or +incompatible marker), while v2 and above are each their own
// line. semver.Major returns "" for a string that isn't valid semver at
// all; that can't happen for real proxy.golang.org data, but grouping it
// with the v0/v1 line rather than treating it as a line of its own is the
// conservative choice — it can only ever suppress a comparison, not
// wrongly promote an invalid string to "highest tag."
func normalizedMajor(v string) string {
	switch m := semver.Major(v); m {
	case "v0", "v1", "":
		return "v1"
	default:
		return m
	}
}
