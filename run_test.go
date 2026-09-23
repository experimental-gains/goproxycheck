package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func readyEndpoints(t *testing.T) endpoints {
	t.Helper()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"Version":"v0.1.0"}`))
	}))
	t.Cleanup(proxy.Close)
	sum := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sum.Close)
	return endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
}

func unknownEndpoints(t *testing.T) endpoints {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}
}

// notYetIndexedEndpoints simulates a module the proxy knows about, but whose
// specific version never shows up in @v/list or .info — the case that keeps
// a --wait poll looping until it times out rather than short-circuiting on
// statusReady/statusModuleUnknown like the other fake servers do.
func notYetIndexedEndpoints(t *testing.T) endpoints {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.0.9"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v0.0.9\n"))
		default: // .info, sum lookup
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}
}

func TestRun_ReadyText(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, readyEndpoints(t))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "ready") {
		t.Errorf("stdout = %q, want it to mention ready", stdout.String())
	}
}

func TestRun_ReadyJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--json", "example.com/mod@v0.1.0"}, &stdout, &stderr, readyEndpoints(t))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `"status": "ready"`) {
		t.Errorf("stdout = %q, want JSON with status:ready", stdout.String())
	}
}

func TestRun_ModuleUnknownNonZeroExit(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/nope@v0.1.0"}, &stdout, &stderr, unknownEndpoints(t))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "module-unknown") {
		t.Errorf("stdout = %q, want it to mention module-unknown", stdout.String())
	}
}

func TestRun_BadFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--not-a-flag"}, &stdout, &stderr, endpoints{})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}

func TestRun_ResolveTargetError(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"no-at-sign"}, &stdout, &stderr, endpoints{})
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "goproxycheck:") {
		t.Errorf("stderr = %q, want the resolveTarget error prefixed", stderr.String())
	}
}

func TestRun_WaitPollsUntilReady(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		ready := n > 4 // first full probe (4 requests) reports not-ready, then flips
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.1.0"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			if ready {
				_, _ = w.Write([]byte("v0.1.0\n"))
			} else {
				_, _ = w.Write([]byte("v0.0.9\n"))
			}
		default: // .info, sum lookup
			if ready {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"Version":"v0.1.0"}`))
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		}
	}))
	defer srv.Close()
	ep := endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}

	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := run([]string{"--wait", "--interval=1ms", "--timeout=2s", "example.com/mod@v0.1.0"}, &stdout, &stderr, ep)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("run took %s, want it to stop polling once ready", elapsed)
	}
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "ready") {
		t.Errorf("stdout = %q, want it to mention ready", stdout.String())
	}
}

// TestRun_LocalGoproxyOff verifies run() short-circuits on a local
// GOPROXY=off before ever probing the proxy/sumdb — passing endpoints{}
// (a nil client) means any attempt to actually probe would panic, so a
// clean, non-crashing statusGoproxyOffLocally result proves the probe
// loop was skipped entirely. This is the fix for a real bug found via
// live-toolchain differential testing: goproxycheck used to report
// "ready" for a module@version while GOPROXY=off was set, when the real
// `go install` in that exact environment fails outright with "module
// lookup disabled by GOPROXY=off".
func TestRun_LocalGoproxyOff(t *testing.T) {
	t.Setenv("GOPROXY", "off")
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, endpoints{})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "goproxy-off-locally") {
		t.Errorf("stdout = %q, want it to mention goproxy-off-locally", stdout.String())
	}
}

// TestRun_LocalModulePrivate verifies run() short-circuits on a local
// GOPRIVATE/GONOPROXY match before ever probing the proxy/sumdb — passing
// endpoints{} means any attempt to actually probe would panic, so a clean,
// non-crashing statusPrivateModuleLocally result proves the probe loop was
// skipped entirely. This is the fix for a real bug found by live-toolchain
// differential testing: with GOPRIVATE set to a pattern matching the target
// module, `go install`/`go mod download` fetch it directly from its VCS
// host and never contact proxy.golang.org at all (confirmed with `go mod
// download -x`), so probing the public proxy for it would report false
// module-unknown/not-yet-indexed verdicts regardless of whether the real
// install actually works right now.
func TestRun_LocalModulePrivate(t *testing.T) {
	t.Setenv("GOPRIVATE", "example.com/*")
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, endpoints{})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "private-module-locally") {
		t.Errorf("stdout = %q, want it to mention private-module-locally", stdout.String())
	}
}

// TestRun_LocalGoproxyDirect verifies run() short-circuits when the local
// GOPROXY resolves to "direct" before ever probing the proxy/sumdb — same
// panic-if-reached proof as TestRun_LocalGoproxyOff. This is the fix for a
// real bug found via live-toolchain differential testing: with
// GOPROXY=direct, `go mod download` fetches straight from the module's VCS
// host and never contacts proxy.golang.org at all (confirmed live with `go
// mod download -x`, which showed only a `git ls-remote` trace and no
// proxy.golang.org request), so probing the public proxy in this case
// answers a question `go install` never asks.
func TestRun_LocalGoproxyDirect(t *testing.T) {
	t.Setenv("GOPROXY", "direct")
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, endpoints{})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "goproxy-direct-locally") {
		t.Errorf("stdout = %q, want it to mention goproxy-direct-locally", stdout.String())
	}
}

// TestRun_LocalGoproxyCustom verifies run() short-circuits when the local
// GOPROXY resolves to a custom (non-public) proxy URL before ever probing
// proxy.golang.org — same panic-if-reached proof as TestRun_LocalGoproxyOff.
// This is the fix for a real bug found via live-toolchain differential
// testing: with GOPROXY pointed at a working private/custom proxy (Athens,
// Artifactory, goproxy.cn, and similar are all in real, common use) serving
// a module the public proxy has never heard of, `go mod download` succeeds
// in that exact environment (confirmed live against a hand-built
// file://-proxy fixture) while goproxycheck, before this fix, unconditionally
// probed proxy.golang.org and reported a false "module-unknown" telling the
// user to check for a typo — nothing was wrong, it was just asking the
// wrong proxy.
func TestRun_LocalGoproxyCustom(t *testing.T) {
	t.Setenv("GOPROXY", "https://goproxy.example.com,direct")
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, endpoints{})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "goproxy-custom-locally") {
		t.Errorf("stdout = %q, want it to mention goproxy-custom-locally", stdout.String())
	}
	if !strings.Contains(stdout.String(), "https://goproxy.example.com") {
		t.Errorf("stdout = %q, want it to name the custom proxy", stdout.String())
	}
}

// TestRun_DefaultGoproxyStillProbes is a sanity guard for the
// localGoproxyNonPublic short-circuit above: the ordinary default GOPROXY
// (the public proxy followed by direct fallback) must still take the
// normal probing path, not get misclassified as "custom" just because the
// full value isn't a bare "https://proxy.golang.org" string.
func TestRun_DefaultGoproxyStillProbes(t *testing.T) {
	t.Setenv("GOPROXY", "https://proxy.golang.org,direct")
	ep := readyEndpoints(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, ep)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "ready") {
		t.Errorf("stdout = %q, want it to mention ready", stdout.String())
	}
}

// TestRun_LatestQueryResolves is the fix for a real bug found by mirroring
// the standard `go install module@latest` idiom: the module proxy protocol
// has no @v/latest.info endpoint (confirmed live against
// golang.org/x/mod@latest, which 404s "invalid version" there), so probing
// the literal string "latest" as a version misdiagnosed a perfectly healthy
// module as not-yet-indexed and, under --wait, would poll until timeout
// since "latest" never appears in @v/list. This server only serves
// @v/v0.5.0.info and sum for v0.5.0 — if probe() still used the literal
// "latest" string, every request but @latest itself would 404.
func TestRun_LatestQueryResolves(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.5.0"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v0.5.0\n"))
		case strings.HasSuffix(r.URL.Path, "/@v/v0.5.0.info"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.5.0"}`))
		case strings.Contains(r.URL.Path, "/lookup/") && strings.HasSuffix(r.URL.Path, "@v0.5.0"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ep := endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}

	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@latest"}, &stdout, &stderr, ep)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "ready") {
		t.Errorf("stdout = %q, want it to mention ready", stdout.String())
	}
	if !strings.Contains(stdout.String(), "resolved to v0.5.0") {
		t.Errorf("stdout = %q, want it to mention the resolved version", stdout.String())
	}
}

// TestRun_PartialVersionQueryResolves is the fix for a real bug found by
// testing goproxycheck against real documented Go version-query forms beyond
// "latest" (go.dev/ref/mod#version-queries): a partial version like "v0.19"
// or a revision identifier like a branch name. Confirmed live against
// proxy.golang.org that, unlike "latest", these DO resolve directly against
// @v/<query>.info (returning the resolved canonical version in the body),
// but sum.golang.org's lookup endpoint only accepts a canonical version and
// 400s on the literal query string — so before this fix, `goproxycheck
// mod@v0.19` (or `mod@master`) misdiagnosed a fully ready module as
// sumdb-lag forever, since the literal query never resolves via sum lookup.
// This server only serves the sum lookup for the resolved v0.5.0, not the
// literal "v0.19" queried.
func TestRun_PartialVersionQueryResolves(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.5.0"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v0.5.0\n"))
		case strings.HasSuffix(r.URL.Path, "/@v/v0.19.info"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.5.0"}`))
		case strings.Contains(r.URL.Path, "/lookup/") && strings.HasSuffix(r.URL.Path, "@v0.5.0"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ep := endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}

	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.19"}, &stdout, &stderr, ep)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "ready") {
		t.Errorf("stdout = %q, want it to mention ready", stdout.String())
	}
	if !strings.Contains(stdout.String(), "resolved to v0.5.0") {
		t.Errorf("stdout = %q, want it to mention the resolved version", stdout.String())
	}
}

func TestRun_WaitTimesOut(t *testing.T) {
	ep := notYetIndexedEndpoints(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--wait", "--interval=1ms", "--timeout=10ms", "example.com/mod@v0.1.0"}, &stdout, &stderr, ep)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "not-yet-indexed") {
		t.Errorf("stdout = %q, want it to mention not-yet-indexed", stdout.String())
	}
}

// sumdbLagEndpoints simulates the module proxy already having the version
// (@latest/@v/list/.info all 200) while sum.golang.org hasn't caught up yet
// (lookup 404) — the same shape TestDiagnose_SumdbLag uses at the diagnose()
// level, reused here for full run()-level end-to-end tests.
func sumdbLagEndpoints(t *testing.T) endpoints {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.1.0"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v0.1.0\n"))
		case strings.HasSuffix(r.URL.Path, "/@v/v0.1.0.info"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.1.0"}`))
		default: // sum lookup
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}
}

// TestRun_SumdbLagDefault pins the pre-existing behavior (no local sumdb
// exemption configured): still reports sumdb-lag, not ready. Guards against
// the GOSUMDB=off/GONOSUMDB fix below over-firing for the ordinary case.
func TestRun_SumdbLagDefault(t *testing.T) {
	t.Setenv("GOSUMDB", "")
	t.Setenv("GONOSUMDB", "")
	t.Setenv("GOPRIVATE", "")
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, sumdbLagEndpoints(t))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "sumdb-lag") {
		t.Errorf("stdout = %q, want it to mention sumdb-lag", stdout.String())
	}
}

// TestRun_SumdbLagWithGosumdbOff is the fix for a real bug found via
// live-toolchain differential testing: confirmed with `go mod download -x`
// against the real golang.org/x/mod module in an isolated GOMODCACHE that
// GOSUMDB=off makes `go install`/`go mod download` skip contacting
// sum.golang.org entirely (no sum.golang.org or
// proxy.golang.org/sumdb/... request appears in the trace at all, where the
// default config clearly shows both). Before this fix, goproxycheck
// reported statusSumdbLag ("retry shortly") for a module@version whose
// proxy listing was already live, purely because the fake sum server here
// (standing in for a real sumdb-lag window) hadn't caught up — actively
// wrong advice for a GOSUMDB=off environment, where a plain `go install`
// already succeeds right now.
func TestRun_SumdbLagWithGosumdbOff(t *testing.T) {
	t.Setenv("GOSUMDB", "off")
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, sumdbLagEndpoints(t))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "ready") {
		t.Errorf("stdout = %q, want it to mention ready", stdout.String())
	}
	if !strings.Contains(stdout.String(), "GOSUMDB=off") {
		t.Errorf("stdout = %q, want it to explain the GOSUMDB=off reason", stdout.String())
	}
}

// TestRun_SumdbLagWithGonosumdbPattern covers the narrower, GOPRIVATE-free
// case: confirmed live the same way (`go mod download -x` with only
// GONOSUMDB set, no GOPRIVATE/GONOPROXY) that a matching GONOSUMDB pattern
// still fetches the module normally through proxy.golang.org (unlike
// GOPRIVATE/GONOPROXY, which skip the proxy entirely and are already
// handled by localModulePrivate's short-circuit before this code is ever
// reached) — only the sum.golang.org lookup for that module is skipped, so
// this needs its own check independent of localModulePrivate.
func TestRun_SumdbLagWithGonosumdbPattern(t *testing.T) {
	t.Setenv("GOSUMDB", "")
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GONOSUMDB", "example.com/*")
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, sumdbLagEndpoints(t))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "ready") {
		t.Errorf("stdout = %q, want it to mention ready", stdout.String())
	}
	if !strings.Contains(stdout.String(), "GONOSUMDB pattern") {
		t.Errorf("stdout = %q, want it to explain the GONOSUMDB reason", stdout.String())
	}
}
