package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
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
	// resolvedVersion is set when version == "latest" and the proxy
	// resolved it to a real tagged version (or pseudo-version) via @latest.
	// The module proxy protocol has no @v/latest.info endpoint — "latest"
	// is a version *query*, resolved only through @latest — so versionInfo
	// and sum below are probed against this resolved version, not the
	// literal string "latest". See probe().
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

	ver := escapePath(checkVersion)
	r.versionInfo = e.get(fmt.Sprintf("%s/%s/@v/%s.info", e.proxyBase, mod, ver))
	r.sum = e.get(fmt.Sprintf("%s/lookup/%s@%s", e.sumBase, mod, ver))

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
