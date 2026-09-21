package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeProxy serves a scripted set of responses keyed by exact path, so each
// test can simulate one specific proxy/sumdb state without hitting the network.
func fakeProxy(t *testing.T, routes map[string]int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code, ok := routes[r.URL.Path]
		if !ok {
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
		w.WriteHeader(code)
		if code == http.StatusOK {
			_, _ = fmt.Fprint(w, `{"Version":"v0.1.0","Time":"2026-09-19T00:00:00Z"}`)
		}
	}))
	return srv
}

func TestDiagnose_Ready(t *testing.T) {
	proxy := fakeProxy(t, map[string]int{
		"/example.com/mod/@latest":        http.StatusOK,
		"/example.com/mod/@v/list":        http.StatusOK,
		"/example.com/mod/@v/v0.1.0.info": http.StatusOK,
	})
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/example.com/mod@v0.1.0": http.StatusOK,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	r := ep.probe("example.com/mod", "v0.1.0")
	got := diagnose(r)
	if got.status != statusReady {
		t.Fatalf("status = %s, want ready; message: %s", got.status, got.message)
	}
}

func TestDiagnose_NegativeCache(t *testing.T) {
	// @latest/@v/list know about the module and list this exact version,
	// but the per-version .info 404s — the poisoned-cache signature.
	proxy := fakeProxy(t, map[string]int{
		"/example.com/mod/@latest":        http.StatusOK,
		"/example.com/mod/@v/list":        http.StatusOK,
		"/example.com/mod/@v/v0.1.0.info": http.StatusNotFound,
	})
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/example.com/mod@v0.1.0": http.StatusNotFound,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	// @v/list needs a real body listing the version for the "known but
	// poisoned" branch to trigger; fake it via a second server variant.
	listSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/example.com/mod/@latest":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"Version":"v0.1.0"}`)
		case "/example.com/mod/@v/list":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "v0.1.0\n")
		case "/example.com/mod/@v/v0.1.0.info":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer listSrv.Close()
	ep.proxyBase = listSrv.URL

	r := ep.probe("example.com/mod", "v0.1.0")
	got := diagnose(r)
	if got.status != statusNegativeCache {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusNegativeCache, got.message)
	}
}

// TestDiagnose_ZipBuildError covers a real proxy.golang.org response found
// by testing against github.com/torvalds/linux: @v/list includes the
// version (it's a real, permanently tagged commit) but @v/<version>.info
// 404s with a golang.org/x/mod/zip "create zip: ... case-insensitive file
// name collision" error. The old code classified any @v/list-hit +
// versionInfo-404 as the negative-cache bug and told the user to cut a new
// tag — wrong advice here, since the zip will never build until the file
// collision itself is fixed.
func TestDiagnose_ZipBuildError(t *testing.T) {
	listSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/example.com/mod/@latest":
			w.WriteHeader(http.StatusNotFound)
		case "/example.com/mod/@v/list":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "v1.0.0\n")
		case "/example.com/mod/@v/v1.0.0.info":
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprint(w, `not found: create zip: case-insensitive file name collision: "FOO.go" and "foo.go"`)
		default:
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer listSrv.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/example.com/mod@v1.0.0": http.StatusNotFound,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: listSrv.URL, sumBase: sum.URL, client: listSrv.Client()}
	r := ep.probe("example.com/mod", "v1.0.0")
	got := diagnose(r)
	if got.status != statusZipBuildError {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusZipBuildError, got.message)
	}
}

// TestDiagnose_BlocklistedMalicious is the regression for a real, verified
// misdiagnosis (run #122): proxy.golang.org returns 403 with a distinctive
// plain-text body when it has flagged a specific module as malicious.
// Before this fix, that 403 fell straight through to statusModuleUnknown
// ("check: is the repo public? does the path match?"), which is actively
// wrong guidance for a module the Go security team deliberately blocked —
// there's no typo to fix and nothing to wait out. Verified live against
// three real modules that return exactly this body: github.com/shopsprint/
// decimal, github.com/boltdb-go/bolt, github.com/xinfeisoft/crypto.
func TestDiagnose_BlocklistedMalicious(t *testing.T) {
	const blockedBody = "SECURITY ERROR\nThe module proxy considers this module to be malicious\nand will not serve it."
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/github.com/shopsprint/decimal/@latest", "/github.com/shopsprint/decimal/@v/list", "/github.com/shopsprint/decimal/@v/v1.3.3.info":
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, blockedBody)
		default:
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/github.com/shopsprint/decimal@v1.3.3": http.StatusForbidden,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	r := ep.probe("github.com/shopsprint/decimal", "v1.3.3")
	got := diagnose(r)
	if got.status != statusBlocklistedMalicious {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusBlocklistedMalicious, got.message)
	}
}

func TestDiagnose_ModuleUnknown(t *testing.T) {
	proxy := fakeProxy(t, map[string]int{
		"/example.com/nope/@latest":        http.StatusNotFound,
		"/example.com/nope/@v/list":        http.StatusNotFound,
		"/example.com/nope/@v/v0.1.0.info": http.StatusNotFound,
	})
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/example.com/nope@v0.1.0": http.StatusNotFound,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	r := ep.probe("example.com/nope", "v0.1.0")
	got := diagnose(r)
	if got.status != statusModuleUnknown {
		t.Fatalf("status = %s, want %s", got.status, statusModuleUnknown)
	}
}

// TestDiagnose_ModuleNegativeCache reproduces the run #63 finding: a repo
// that was queried by the proxy while private, then made public. @latest
// and @v/list both keep 404ing (the module-level analog of the per-version
// negative cache this tool otherwise detects), even though the repo itself
// is live. Verified against the real proxy.golang.org and github.com/
// experimental-gains/proxytest-pseudo before this test was written.
func TestDiagnose_ModuleNegativeCache(t *testing.T) {
	proxy := fakeProxy(t, map[string]int{
		"/github.com/owner/repo/@latest":        http.StatusNotFound,
		"/github.com/owner/repo/@v/list":        http.StatusNotFound,
		"/github.com/owner/repo/@v/v0.1.0.info": http.StatusNotFound,
	})
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/github.com/owner/repo@v0.1.0": http.StatusNotFound,
	})
	defer sum.Close()
	repoCheck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/owner/repo" {
			t.Fatalf("unexpected repo-check request to %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer repoCheck.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, repoCheckBase: repoCheck.URL, client: proxy.Client()}
	r := ep.probe("github.com/owner/repo", "v0.1.0")
	got := diagnose(r)
	if got.status != statusModuleNegativeCache {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusModuleNegativeCache, got.message)
	}
	if strings.Contains(got.message, "extra path segments") {
		t.Fatalf("plain owner/repo module message shouldn't carry the nested-path caveat: %s", got.message)
	}
}

// TestDiagnose_ModuleNegativeCache_NestedPath is a real-world-testing find
// (not from a fixture): probing github.com/gin-gonic/gin/nonexistentsubpath
// live against proxy.golang.org and github.com returns exactly this shape —
// module unknown to the proxy, but the *repo root* (github.com/gin-gonic/
// gin) reachable — and the tool reported it as module-negative-cache-
// suspected with no hedge, even though the true cause here is a bogus
// nested module path, not poisoning. The repoReachable check can only ever
// confirm the repo root: GitHub's bare (non-/tree/) URLs 404 on any nested
// path whether or not it's real, including the common major-version-on-a-
// branch layout (github.com/redis/go-redis/v9 lives at the repo root, not
// a /v9 directory) — so a nested module path can't be verified either way,
// and the message needs to say so instead of asserting the specific nested
// URL was itself confirmed reachable.
func TestDiagnose_ModuleNegativeCache_NestedPath(t *testing.T) {
	proxy := fakeProxy(t, map[string]int{
		"/github.com/owner/repo/sub/@latest":        http.StatusNotFound,
		"/github.com/owner/repo/sub/@v/list":        http.StatusNotFound,
		"/github.com/owner/repo/sub/@v/v0.1.0.info": http.StatusNotFound,
	})
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/github.com/owner/repo/sub@v0.1.0": http.StatusNotFound,
	})
	defer sum.Close()
	repoCheck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/owner/repo" {
			t.Fatalf("unexpected repo-check request to %s (should check the repo root, not the nested path)", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer repoCheck.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, repoCheckBase: repoCheck.URL, client: proxy.Client()}
	r := ep.probe("github.com/owner/repo/sub", "v0.1.0")
	got := diagnose(r)
	if got.status != statusModuleNegativeCache {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusModuleNegativeCache, got.message)
	}
	if !strings.Contains(got.message, "https://github.com/owner/repo is reachable") {
		t.Fatalf("message should credit the repo *root* as what was checked, not the nested module path: %s", got.message)
	}
	if strings.Contains(got.message, "https://github.com/owner/repo/sub is reachable") {
		t.Fatalf("message must not claim the nested path itself was confirmed reachable — it wasn't checked: %s", got.message)
	}
	if !strings.Contains(got.message, "double-check the module path") {
		t.Fatalf("message should hedge that the nested module path itself could be wrong: %s", got.message)
	}
}

// TestDiagnose_ModuleUnknown_GithubRepoAlsoUnreachable makes sure a genuine
// typo/never-existed github.com path still gets the plain module-unknown
// diagnosis, not the negative-cache one, when the repo-reachability check
// also 404s.
func TestDiagnose_ModuleUnknown_GithubRepoAlsoUnreachable(t *testing.T) {
	proxy := fakeProxy(t, map[string]int{
		"/github.com/owner/typo/@latest":        http.StatusNotFound,
		"/github.com/owner/typo/@v/list":        http.StatusNotFound,
		"/github.com/owner/typo/@v/v0.1.0.info": http.StatusNotFound,
	})
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/github.com/owner/typo@v0.1.0": http.StatusNotFound,
	})
	defer sum.Close()
	repoCheck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer repoCheck.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, repoCheckBase: repoCheck.URL, client: proxy.Client()}
	r := ep.probe("github.com/owner/typo", "v0.1.0")
	got := diagnose(r)
	if got.status != statusModuleUnknown {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusModuleUnknown, got.message)
	}
}

func TestDiagnose_SumdbLag(t *testing.T) {
	proxy := fakeProxy(t, map[string]int{
		"/example.com/mod/@latest":        http.StatusOK,
		"/example.com/mod/@v/list":        http.StatusOK,
		"/example.com/mod/@v/v0.1.0.info": http.StatusOK,
	})
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/example.com/mod@v0.1.0": http.StatusNotFound,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	r := ep.probe("example.com/mod", "v0.1.0")
	got := diagnose(r)
	if got.status != statusSumdbLag {
		t.Fatalf("status = %s, want %s", got.status, statusSumdbLag)
	}
}

func TestDiagnose_NetworkError(t *testing.T) {
	r := report{
		module:  "example.com/mod",
		version: "v0.1.0",
		latest:  probeResult{err: fmt.Errorf("dial tcp: connection refused")},
	}
	got := diagnose(r)
	if got.status != statusNetworkError {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusNetworkError, got.message)
	}
}

// A transport error on versionInfo or sum looks identical to a clean 404
// (ok=false, empty body) unless err is checked too — it used to be
// misdiagnosed as the tool's own negative-cache/sumdb-lag verdicts instead
// of "we couldn't actually complete the request."
func TestDiagnose_NetworkError_VersionInfo(t *testing.T) {
	r := report{
		module:      "example.com/mod",
		version:     "v0.1.0",
		latest:      probeResult{ok: true},
		list:        probeResult{ok: true, body: "v0.1.0\n"},
		versionInfo: probeResult{err: fmt.Errorf("dial tcp: i/o timeout")},
	}
	got := diagnose(r)
	if got.status != statusNetworkError {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusNetworkError, got.message)
	}
}

func TestDiagnose_NetworkError_Sum(t *testing.T) {
	r := report{
		module:      "example.com/mod",
		version:     "v0.1.0",
		latest:      probeResult{ok: true},
		list:        probeResult{ok: true, body: "v0.1.0\n"},
		versionInfo: probeResult{ok: true, body: `{"Version":"v0.1.0"}`},
		sum:         probeResult{err: fmt.Errorf("dial tcp: connection refused")},
	}
	got := diagnose(r)
	if got.status != statusNetworkError {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusNetworkError, got.message)
	}
}

func TestDefaultEndpoints(t *testing.T) {
	e := defaultEndpoints()
	if e.proxyBase != defaultProxyBase || e.sumBase != defaultSumBase || e.client == nil {
		t.Errorf("got %+v", e)
	}
}

func TestDiagnose_NotYetIndexed(t *testing.T) {
	// Module known (has other versions) but this version isn't in @v/list
	// at all yet — ordinary indexing lag, distinct from negative-cache.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/example.com/mod/@latest":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"Version":"v0.0.9"}`)
		case "/example.com/mod/@v/list":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "v0.0.9\n")
		case "/example.com/mod/@v/v0.1.0.info":
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/example.com/mod@v0.1.0": http.StatusNotFound,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	r := ep.probe("example.com/mod", "v0.1.0")
	got := diagnose(r)
	if got.status != statusNotYetIndexed {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusNotYetIndexed, got.message)
	}
}
