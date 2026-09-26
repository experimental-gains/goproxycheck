package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		"/example.com/mod/@v/v0.1.0.mod":  http.StatusOK,
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
		case "/example.com/mod/@v/v0.1.0.mod":
			// probe() fetches @latest's own .mod unconditionally to check
			// for retract directives (see retraction() in retract.go); this
			// test isn't about that, so serve a plain, unretracted go.mod.
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "module example.com/mod\n")
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

// TestDiagnose_BlocklistedMaliciousVersionOnly covers the case
// TestDiagnose_BlocklistedMalicious doesn't: a block scoped to one
// version, not the whole module. @latest and @v/list are healthy (the
// module has other, unblocked versions), but the specific version's
// @v/<version>.info returns the same 403 blocklist body. Before this fix,
// isBlocklistedMalicious only ever looked at r.latest.body/r.list.body, so
// this case fell through to statusZipBuildError or statusNegativeCache
// instead — both tell the user to retry or cut a new tag, which is wrong
// for a version the Go security team deliberately and permanently blocked.
func TestDiagnose_BlocklistedMaliciousVersionOnly(t *testing.T) {
	const blockedBody = "SECURITY ERROR\nThe module proxy considers this module to be malicious\nand will not serve it."
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/example.com/mod/@latest":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"Version":"v1.4.0","Time":"2026-09-19T00:00:00Z"}`)
		case "/example.com/mod/@v/list":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "v1.0.0\nv1.3.3\nv1.4.0\n")
		case "/example.com/mod/@v/v1.3.3.info":
			w.WriteHeader(http.StatusForbidden)
			_, _ = fmt.Fprint(w, blockedBody)
		case "/example.com/mod/@v/v1.4.0.mod":
			// probe() fetches @latest's own .mod unconditionally to check
			// for retract directives; this test isn't about that.
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "module example.com/mod\n")
		default:
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/example.com/mod@v1.3.3": http.StatusForbidden,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	r := ep.probe("example.com/mod", "v1.3.3")
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

// TestDiagnose_RepoCheckRateLimited is a real-world-testing find: an
// unauthenticated GET to github.com (the repo-reachability check) can get a
// 403 from GitHub's own secondary rate limiting or a 429 under load — most
// realistically when this tool runs frequently as the shipped GitHub
// Action, doing that GET from CI on every push. Before this fix that status
// code was indistinguishable from a genuine 404 through repoReachable's
// bool, so a rate-limited check produced the same "check: is the repo
// public?" module-unknown message as a real typo/never-existed module,
// even though the true answer is "we don't know, GitHub blocked our
// check" — actively misleading, not just imprecise.
func TestDiagnose_RepoCheckRateLimited(t *testing.T) {
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
		w.WriteHeader(http.StatusForbidden)
	}))
	defer repoCheck.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, repoCheckBase: repoCheck.URL, client: proxy.Client()}
	r := ep.probe("github.com/owner/repo", "v0.1.0")
	got := diagnose(r)
	if got.status != statusRepoCheckInconclusive {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusRepoCheckInconclusive, got.message)
	}
	if strings.Contains(got.message, "is the repo public") {
		t.Fatalf("rate-limited repo check must not read like a confirmed module-unknown answer: %s", got.message)
	}
	if !strings.Contains(got.message, "403") {
		t.Fatalf("message should surface the actual status code that made the check inconclusive: %s", got.message)
	}
}

// TestDiagnose_RepoCheckTooManyRequests covers the 429 variant of the same
// inconclusive-check case (sustained load rather than GitHub's specific
// secondary-rate-limit 403).
func TestDiagnose_RepoCheckTooManyRequests(t *testing.T) {
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
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer repoCheck.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, repoCheckBase: repoCheck.URL, client: proxy.Client()}
	r := ep.probe("github.com/owner/repo", "v0.1.0")
	got := diagnose(r)
	if got.status != statusRepoCheckInconclusive {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusRepoCheckInconclusive, got.message)
	}
}

func TestDiagnose_SumdbLag(t *testing.T) {
	proxy := fakeProxy(t, map[string]int{
		"/example.com/mod/@latest":        http.StatusOK,
		"/example.com/mod/@v/list":        http.StatusOK,
		"/example.com/mod/@v/v0.1.0.info": http.StatusOK,
		"/example.com/mod/@v/v0.1.0.mod":  http.StatusOK,
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

// TestDiagnose_CanonicalModuleMismatch is the regression for issue #2
// (github.com/experimental-gains/goproxycheck/issues/2, filed by an
// external user): the proxy resolves @latest/@v/list/@v/<version>.info by
// VCS origin discovery against the requested import path, not by checking
// the module directive in that version's go.mod — so an old/renamed import
// path (modeled here on the real github.com/grpc/grpc-go, whose go.mod has
// declared "module google.golang.org/grpc" since it moved off that path)
// reports fully "ready" with no other signal that `go install` will
// actually fail with "module declares its path as: ..." (confirmed live,
// 2026-09-24, against the real proxy and a real `go install`). The .mod
// fetch this test exercises is the only way to catch that.
func TestDiagnose_CanonicalModuleMismatch(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/github.com/grpc/grpc-go/@latest":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"Version":"v1.84.0","Time":"2026-09-17T20:03:25Z"}`)
		case "/github.com/grpc/grpc-go/@v/list":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "v1.84.0\n")
		case "/github.com/grpc/grpc-go/@v/v1.84.0.info":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"Version":"v1.84.0","Time":"2026-09-17T20:03:25Z"}`)
		case "/github.com/grpc/grpc-go/@v/v1.84.0.mod":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "module google.golang.org/grpc\n\ngo 1.25.0\n")
		default:
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/github.com/grpc/grpc-go@v1.84.0": http.StatusOK,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	r := ep.probe("github.com/grpc/grpc-go", "v1.84.0")
	got := diagnose(r)
	// Not statusReady: proxy.golang.org and sum.golang.org both being
	// healthy under this import path (sum is 200 here too) doesn't mean `go
	// install` will actually work — it fails at the go.mod parse step
	// regardless, so this needs to be its own non-zero-exit status, not a
	// footnote on a "ready" verdict a CI script would read as success.
	if got.status != statusWrongImportPath {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusWrongImportPath, got.message)
	}
	if !strings.Contains(got.message, `declares its module path as "google.golang.org/grpc"`) {
		t.Fatalf("expected the canonical-path explanation, got: %s", got.message)
	}
	if !strings.Contains(got.message, "module declares its path as: google.golang.org/grpc\n\tbut was required as: github.com/grpc/grpc-go") {
		t.Fatalf("expected the message to quote go's own real failure text verbatim, got: %s", got.message)
	}
}

// TestDiagnose_CanonicalModuleMatch is the counterpart to
// TestDiagnose_CanonicalModuleMismatch: when the go.mod at the resolved
// version declares the same path that was checked (the overwhelmingly
// common case), no note should be added.
func TestDiagnose_CanonicalModuleMatch(t *testing.T) {
	proxy := fakeProxy(t, map[string]int{
		"/example.com/mod/@latest":        http.StatusOK,
		"/example.com/mod/@v/list":        http.StatusOK,
		"/example.com/mod/@v/v0.1.0.info": http.StatusOK,
	})
	defer proxy.Close()
	// fakeProxy doesn't let us script a distinct body for one path, so use a
	// dedicated handler serving a real go.mod body for the .mod fetch.
	modSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/example.com/mod/@latest", "/example.com/mod/@v/list", "/example.com/mod/@v/v0.1.0.info":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"Version":"v0.1.0","Time":"2026-09-19T00:00:00Z"}`)
		case "/example.com/mod/@v/v0.1.0.mod":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "module example.com/mod\n\ngo 1.21\n")
		default:
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer modSrv.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/example.com/mod@v0.1.0": http.StatusOK,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: modSrv.URL, sumBase: sum.URL, client: modSrv.Client()}
	r := ep.probe("example.com/mod", "v0.1.0")
	got := diagnose(r)
	if got.status != statusReady {
		t.Fatalf("status = %s, want ready; message: %s", got.status, got.message)
	}
	if strings.Contains(got.message, "declares its module path as") {
		t.Fatalf("matching module path shouldn't produce a canonical-path note: %s", got.message)
	}
}

// TestDiagnose_Retracted is modeled on a real, live-verified case:
// github.com/mattn/go-sqlite3's go.mod (at its latest tag, v1.14.52)
// retracts the version range [v2.0.0+incompatible, v2.0.7+incompatible]
// with the rationale "Accidental; no major changes or features." Confirmed
// live (2026-09-25) that proxy.golang.org and sum.golang.org both serve
// v2.0.3+incompatible cleanly, and `go mod download
// github.com/mattn/go-sqlite3@v2.0.3+incompatible` succeeds outright with
// no warning anywhere in its trace — retraction never blocks a fetch, only
// `go list -m -u` surfaces it. Before this diagnosis existed, goproxycheck
// reported exactly this version as plain statusReady.
func TestDiagnose_Retracted(t *testing.T) {
	const module = "github.com/mattn/go-sqlite3"
	const version = "v2.0.3+incompatible"
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + module + "/@latest":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"Version":"v1.14.52","Time":"2026-06-05T00:00:00Z"}`)
		case "/" + module + "/@v/list":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, "%s\nv1.14.52\n", version)
		case "/" + module + "/@v/" + version + ".info":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"Version":%q,"Time":"2020-01-28T10:25:19Z"}`, version)
		case "/" + module + "/@v/" + version + ".mod":
			// Confirmed live: a pre-modules +incompatible tag like this one
			// has no go.mod of its own — the proxy synthesizes this exact
			// bare one-liner. The retraction is NOT visible here; it only
			// shows up in the module's actual latest release below, which
			// is why probe() fetches that separately as latestModFile.
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "module github.com/mattn/go-sqlite3\n")
		case "/" + module + "/@v/v1.14.52.mod":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "module github.com/mattn/go-sqlite3\n\ngo 1.21\n\nretract (\n\t[v2.0.0+incompatible, v2.0.7+incompatible] // Accidental; no major changes or features.\n)\n")
		default:
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/" + module + "@" + version: http.StatusOK,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	r := ep.probe(module, version)
	got := diagnose(r)
	if got.status != statusRetracted {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusRetracted, got.message)
	}
	if !strings.Contains(got.message, `rationale given: "Accidental; no major changes or features."`) {
		t.Fatalf("expected the retraction rationale to be quoted, got: %s", got.message)
	}
	if !strings.Contains(got.message, "go install") {
		t.Fatalf("expected the message to note `go install` still succeeds despite the retraction, got: %s", got.message)
	}
}

// TestDiagnose_RetractedPastSelfRetractingLatest is the end-to-end
// counterpart of TestProbe_LatestModFileScopedPastSelfRetractingLatest
// (proxy_more_test.go): reproduces github.com/jayconrod/retract, the Go
// team's own canonical self-retraction example, at the diagnose() level.
// v1.0.1 retracts both itself and v1.0.0, so @latest resolves past both to
// v0.9.9, whose go.mod predates the `retract` directive and carries none.
// `go list -m -u` still reports v1.0.0 as retracted (confirmed live,
// 2026-09) by reading v1.0.1's go.mod instead — before this fix,
// goproxycheck fetched v0.9.9's go.mod unconditionally as latestModFile and
// reported plain statusReady for a version the maintainer explicitly
// retracted.
func TestDiagnose_RetractedPastSelfRetractingLatest(t *testing.T) {
	const module = "github.com/jayconrod/retract"
	const version = "v1.0.0"
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + module + "/@latest":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"Version":"v0.9.9","Time":"2021-01-26T16:46:49Z"}`)
		case "/" + module + "/@v/list":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "v1.0.0\nv0.9.9\nv1.0.1\n")
		case "/" + module + "/@v/" + version + ".info":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"Version":%q,"Time":"2021-01-01T00:00:00Z"}`, version)
		case "/" + module + "/@v/" + version + ".mod":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "module "+module+"\n\ngo 1.16\n")
		case "/" + module + "/@v/v0.9.9.mod":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "module "+module+"\n\ngo 1.16\n")
		case "/" + module + "/@v/v1.0.1.mod":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "module "+module+"\n\ngo 1.16\n\nretract (\n\tv1.0.0 // Published accidentally.\n\tv1.0.1 // For retractions only.\n)\n")
		default:
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/" + module + "@" + version: http.StatusOK,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	r := ep.probe(module, version)
	got := diagnose(r)
	if got.status != statusRetracted {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusRetracted, got.message)
	}
	if !strings.Contains(got.message, `rationale given: "Published accidentally."`) {
		t.Fatalf("expected the retraction rationale to be quoted, got: %s", got.message)
	}
}

// TestDiagnose_RetractedRangeDoesNotCoverVersion checks the negative case:
// a go.mod with a retract directive that exists but doesn't cover the
// checked version (the common case for any module with retractions at
// all, e.g. the same go-sqlite3 go.mod checked against its own v1.14.52
// latest tag, which isn't in the retracted v2.x range) reports plain
// statusReady, not a false positive.
func TestDiagnose_RetractedRangeDoesNotCoverVersion(t *testing.T) {
	const module = "github.com/mattn/go-sqlite3"
	const version = "v1.14.52"
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + module + "/@latest", "/" + module + "/@v/list", "/" + module + "/@v/" + version + ".info":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"Version":%q,"Time":"2026-06-05T00:00:00Z"}`, version)
		case "/" + module + "/@v/" + version + ".mod":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "module github.com/mattn/go-sqlite3\n\ngo 1.21\n\nretract (\n\t[v2.0.0+incompatible, v2.0.7+incompatible] // Accidental; no major changes or features.\n)\n")
		default:
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/" + module + "@" + version: http.StatusOK,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	r := ep.probe(module, version)
	got := diagnose(r)
	if got.status != statusReady {
		t.Fatalf("status = %s, want ready (retract range doesn't cover this version); message: %s", got.status, got.message)
	}
}

// TestDiagnose_RetractedRangeCoversNeverPublishedVersion is modeled on a
// real, live-verified case: github.com/mattn/go-sqlite3's go.mod retract
// range [v2.0.0+incompatible, v2.0.7+incompatible] covers v2.0.4-v2.0.7, but
// @v/list (confirmed live, 2026-09-26) shows only v2.0.0-v2.0.3+incompatible
// were ever tagged — a retract range is written in version-number order, not
// against the set of versions that actually exist. Querying
// v2.0.5+incompatible must NOT report statusRetracted: @v/v2.0.5+incompatible
// .info 404s for real ("unknown revision v2.0.5"), so a plain `go install`
// fails outright, the opposite of what the pre-fix statusRetracted message
// ("resolves fine through proxy.golang.org and sum.golang.org — a plain `go
// install` will succeed") claimed.
func TestDiagnose_RetractedRangeCoversNeverPublishedVersion(t *testing.T) {
	const module = "github.com/mattn/go-sqlite3"
	const version = "v2.0.5+incompatible"
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/" + module + "/@latest":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, `{"Version":"v1.14.52","Time":"2026-06-05T00:00:00Z"}`)
		case "/" + module + "/@v/list":
			// v2.0.4-v2.0.7+incompatible were never tagged; only the four
			// listed here (plus the unrelated v1.x line) were.
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "v2.0.0+incompatible\nv2.0.1+incompatible\nv2.0.2+incompatible\nv2.0.3+incompatible\nv1.14.52\n")
		case "/" + module + "/@v/" + version + ".info":
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, "not found: %s@%s: invalid version: unknown revision v2.0.5", module, version)
		case "/" + module + "/@v/v1.14.52.mod":
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "module github.com/mattn/go-sqlite3\n\ngo 1.21\n\nretract (\n\t[v2.0.0+incompatible, v2.0.7+incompatible] // Accidental; no major changes or features.\n)\n")
		default:
			t.Fatalf("unexpected request to %s", r.URL.Path)
		}
	}))
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/" + module + "@" + version: http.StatusNotFound,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	r := ep.probe(module, version)
	got := diagnose(r)
	if got.status == statusRetracted {
		t.Fatalf("status = %s, want anything but retracted for a version that was never published; message: %s", got.status, got.message)
	}
	if got.status != statusNotYetIndexed {
		t.Fatalf("status = %s, want not-yet-indexed (not in @v/list, never published); message: %s", got.status, got.message)
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
	// Pins the actual client timeout value (found LIVED by mutation
	// testing, run #127: the existing check only asserted client !=
	// nil, never the 15s Timeout itself).
	if e.client.Timeout != 15*time.Second {
		t.Errorf("client.Timeout = %s, want 15s", e.client.Timeout)
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
		case "/example.com/mod/@v/v0.0.9.mod":
			// probe() fetches @latest's own .mod unconditionally to check
			// for retract directives; this test isn't about that.
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "module example.com/mod\n")
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

// TestDiagnose_ProxyErrorStatus_ModuleUnknown reproduces the run #380 find:
// per go.dev/ref/mod#goproxy-protocol, only 404/410 mean "not found" — any
// other 4xx/5xx (429, 500, 502, 503, ...) is a terminal protocol error the
// go command does NOT fall back or wait on. Pre-fix, @latest/@v/list both
// returning 503 was indistinguishable from a clean double-404 and got
// misdiagnosed as statusModuleUnknown ("check for a typo"/negative-cache),
// actively wrong advice for a proxy-side outage that has nothing to do with
// whether the module exists.
func TestDiagnose_ProxyErrorStatus_ModuleUnknown(t *testing.T) {
	proxy := fakeProxy(t, map[string]int{
		"/example.com/mod/@latest":        http.StatusServiceUnavailable,
		"/example.com/mod/@v/list":        http.StatusServiceUnavailable,
		"/example.com/mod/@v/v0.1.0.info": http.StatusServiceUnavailable,
		"/lookup/example.com/mod@v0.1.0":  http.StatusServiceUnavailable,
	})
	defer proxy.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: proxy.URL, client: proxy.Client()}
	r := ep.probe("example.com/mod", "v0.1.0")
	got := diagnose(r)
	if got.status != statusProxyError {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusProxyError, got.message)
	}
	if !strings.Contains(got.message, "503") || !strings.Contains(got.message, "@latest") {
		t.Fatalf("message should name the actual endpoint and status code: %s", got.message)
	}
	if strings.Contains(got.message, "check for a typo") {
		t.Fatalf("a proxy outage must not read like a confirmed module-unknown/typo answer: %s", got.message)
	}
}

// A 403 that ISN'T the documented malicious-block body (just some other
// forbidden response, e.g. a corporate WAF or a misconfigured mirror) must
// still be told apart from the specific blocklisted-malicious diagnosis.
func TestDiagnose_ProxyErrorStatus_PlainForbidden(t *testing.T) {
	proxy := fakeProxy(t, map[string]int{
		"/example.com/mod/@latest":        http.StatusForbidden,
		"/example.com/mod/@v/list":        http.StatusForbidden,
		"/example.com/mod/@v/v0.1.0.info": http.StatusForbidden,
		"/lookup/example.com/mod@v0.1.0":  http.StatusForbidden,
	})
	defer proxy.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: proxy.URL, client: proxy.Client()}
	r := ep.probe("example.com/mod", "v0.1.0")
	got := diagnose(r)
	if got.status != statusProxyError {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusProxyError, got.message)
	}
}

// The malicious-block diagnosis (body-based) must still win over the new
// generic status-based check when both a 403 status AND the marker body
// are present — it's strictly more informative.
func TestDiagnose_ProxyErrorStatus_DoesNotShadowMaliciousBlock(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, "go.sum database server considers this module to be malicious")
	}))
	defer proxy.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: proxy.URL, client: proxy.Client()}
	r := ep.probe("example.com/mod", "v0.1.0")
	got := diagnose(r)
	if got.status != statusBlocklistedMalicious {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusBlocklistedMalicious, got.message)
	}
}

// TestDiagnose_ProxyErrorStatus_VersionInfo covers the module-known,
// version-unresolvable path: @latest/@v/list are healthy but the specific
// version's @v/<version>.info returns a genuine error status instead of a
// 404. Pre-fix this fell into the not-yet-indexed/negative-cache fallback
// ("retry in a minute, or use --wait"), which under --wait would poll to
// the full timeout on what's actually a terminal per-request error.
func TestDiagnose_ProxyErrorStatus_VersionInfo(t *testing.T) {
	proxy := fakeProxy(t, map[string]int{
		"/example.com/mod/@latest":        http.StatusOK,
		"/example.com/mod/@v/list":        http.StatusOK,
		"/example.com/mod/@v/v0.1.0.info": http.StatusTooManyRequests,
		"/example.com/mod/@v/v0.1.0.mod":  http.StatusOK,
	})
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/example.com/mod@v0.1.0": http.StatusNotFound,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	r := ep.probe("example.com/mod", "v0.1.0")
	got := diagnose(r)
	if got.status != statusProxyError {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusProxyError, got.message)
	}
	if !strings.Contains(got.message, "429") {
		t.Fatalf("message should surface the actual status code: %s", got.message)
	}
}

// TestDiagnose_ProxyErrorStatus_Sum covers the sum.golang.org side: proxy
// has the version, but the sumdb lookup returns a genuine error status
// rather than a clean 404. Pre-fix this was indistinguishable from
// ordinary sumdb-lag ("retry shortly") — the wrong advice for a terminal
// error the go command itself won't just wait out.
func TestDiagnose_ProxyErrorStatus_Sum(t *testing.T) {
	proxy := fakeProxy(t, map[string]int{
		"/example.com/mod/@latest":        http.StatusOK,
		"/example.com/mod/@v/list":        http.StatusOK,
		"/example.com/mod/@v/v0.1.0.info": http.StatusOK,
		"/example.com/mod/@v/v0.1.0.mod":  http.StatusOK,
	})
	defer proxy.Close()
	sum := fakeProxy(t, map[string]int{
		"/lookup/example.com/mod@v0.1.0": http.StatusInternalServerError,
	})
	defer sum.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
	r := ep.probe("example.com/mod", "v0.1.0")
	got := diagnose(r)
	if got.status != statusProxyError {
		t.Fatalf("status = %s, want %s; message: %s", got.status, statusProxyError, got.message)
	}
	if !strings.Contains(got.message, "sum.golang.org") || !strings.Contains(got.message, "500") {
		t.Fatalf("message should name sum.golang.org and the actual status code: %s", got.message)
	}
}

// A clean 410 (Gone) — the documented alternative to 404 — must still be
// treated as an ordinary "not found," not caught by the new error-status
// check.
func TestDiagnose_Gone_TreatedAsNotFound(t *testing.T) {
	proxy := fakeProxy(t, map[string]int{
		"/example.com/mod/@latest":        http.StatusGone,
		"/example.com/mod/@v/list":        http.StatusGone,
		"/example.com/mod/@v/v0.1.0.info": http.StatusGone,
		"/lookup/example.com/mod@v0.1.0":  http.StatusGone,
	})
	defer proxy.Close()

	ep := endpoints{proxyBase: proxy.URL, sumBase: proxy.URL, client: proxy.Client()}
	r := ep.probe("example.com/mod", "v0.1.0")
	got := diagnose(r)
	if got.status == statusProxyError {
		t.Fatalf("410 Gone is a documented not-found status, must not be diagnosed as a proxy error: %s", got.message)
	}
}
