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
	defer resp.Body.Close()
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
}

func (e endpoints) probe(module, version string) report {
	mod := escapePath(module)
	ver := escapePath(version)
	r := report{
		module:      module,
		version:     version,
		latest:      e.get(fmt.Sprintf("%s/%s/@latest", e.proxyBase, mod)),
		list:        e.get(fmt.Sprintf("%s/%s/@v/list", e.proxyBase, mod)),
		versionInfo: e.get(fmt.Sprintf("%s/%s/@v/%s.info", e.proxyBase, mod, ver)),
		sum:         e.get(fmt.Sprintf("%s/lookup/%s@%s", e.sumBase, mod, ver)),
	}
	if !r.moduleKnown() {
		if m := githubRepoPattern.FindStringSubmatch(module); m != nil {
			reachable := e.get(fmt.Sprintf("%s/%s/%s", e.repoCheckBase, m[1], m[2])).ok
			r.repoReachable = &reachable
			r.repoCheckedNestedPath = m[0] != module
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
