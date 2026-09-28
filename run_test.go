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

// TestRun_LocalModulePrivate_GovcsDisallowed is the fix for a real gap in
// TestRun_LocalModulePrivate above: that test's message unconditionally
// claimed `go install`/`go get` "will fetch it directly from its VCS host"
// once GOPRIVATE/GONOPROXY matches, but confirmed live that a local GOVCS
// setting excluding git for private modules makes the real command fail
// outright instead — `GOPRIVATE=github.com/golang/protobuf
// GOVCS=private:off go mod download github.com/golang/protobuf@v1.5.0`
// fails with "GOVCS disallows using git for private
// github.com/golang/protobuf; see 'go help vcs'", not a successful direct
// fetch. Scoped to github.com (see githubRepoPattern) since that's the
// only host this tool knows the VCS is git for certain.
func TestRun_LocalModulePrivate_GovcsDisallowed(t *testing.T) {
	t.Setenv("GOPRIVATE", "github.com/golang/protobuf")
	t.Setenv("GOVCS", "private:off")
	var stdout, stderr bytes.Buffer
	code := run([]string{"github.com/golang/protobuf@v1.5.0"}, &stdout, &stderr, endpoints{})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "govcs-disallowed-locally") {
		t.Errorf("stdout = %q, want it to mention govcs-disallowed-locally", stdout.String())
	}
	if strings.Contains(stdout.String(), "private-module-locally") {
		t.Errorf("stdout = %q, want it NOT to fall back to the plain private-module-locally message", stdout.String())
	}
}

// TestRun_LocalModulePrivate_GovcsMalformed covers a gap in
// TestRun_LocalModulePrivate_GovcsDisallowed just above: that test's
// govcs-disallowed-locally message is only reachable at all if the local
// GOVCS string parses in the first place. Confirmed live (2026-09-27,
// GOPROXY=direct against a fresh GOMODCACHE): with an unrelated, malformed
// entry placed *ahead* of an otherwise fully permissive "public:git|hg" rule
// in GOVCS, `go mod download -x` for a public module fails outright with
// `malformed entry in GOVCS (missing colon): "badrule"` — never even
// reaching the permissive rule — while goproxycheck's govcsAllowsGit (by
// design, see its own doc comment) skips the unparseable rule and falls
// through, so before this fix it reported statusPrivateModuleLocally
// ("will fetch it directly ... succeed") for a config that actually makes
// every direct-VCS fetch fail. See govcsConfigError's doc comment for the
// four other malformed shapes confirmed the same way.
func TestRun_LocalModulePrivate_GovcsMalformed(t *testing.T) {
	t.Setenv("GOPRIVATE", "github.com/golang/protobuf")
	t.Setenv("GOVCS", "badrule,public:git|hg")
	var stdout, stderr bytes.Buffer
	code := run([]string{"github.com/golang/protobuf@v1.5.0"}, &stdout, &stderr, endpoints{})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "govcs-malformed-locally") {
		t.Errorf("stdout = %q, want it to mention govcs-malformed-locally", stdout.String())
	}
	if strings.Contains(stdout.String(), "private-module-locally") || strings.Contains(stdout.String(), "govcs-disallowed-locally") {
		t.Errorf("stdout = %q, want it NOT to fall back to private-module-locally or govcs-disallowed-locally", stdout.String())
	}
}

// TestRun_LocalModulePrivate_GovcsUsesGoprivateNotGonoproxy is the fix for a
// real gap in TestRun_LocalModulePrivate_GovcsDisallowed above: that test's
// GOPRIVATE and GONOPROXY patterns always matched the same module, so it
// couldn't catch this tool passing the *wrong* private/public bool into the
// GOVCS check. Real cmd/go's own "public"/"private" classification for GOVCS
// (internal/vcs/vcs.go's checkGOVCS) always checks GOPRIVATE specifically —
// never GONOPROXY, despite GONOPROXY defaulting to GOPRIVATE's value when
// unset — regardless of *why* a direct VCS fetch is happening. Confirmed
// live (2026-09-27) with GOPRIVATE and GONOPROXY deliberately set to
// different, non-overlapping patterns: GOPRIVATE=nonmatching.example/*
// (does NOT match), GONOPROXY=github.com/golang/protobuf (matches, forcing
// this tool's localModulePrivate short-circuit), GOVCS="public:off,
// private:git" — `go mod download -x github.com/golang/protobuf@v1.5.0`
// fails with "GOVCS disallows using git for *public* github.com/golang/
// protobuf", i.e. real go classified it public (per GOPRIVATE) despite
// GONOPROXY's match, so the permissive "private:git" rule never applies.
// Before this fix, run() hardcoded `true` for this branch's private bool
// (matching GONOPROXY, not GOPRIVATE), so it evaluated "private:git"
// instead and reported statusPrivateModuleLocally ("will fetch it directly
// ... succeed") for a config a real `go install` refuses outright.
func TestRun_LocalModulePrivate_GovcsUsesGoprivateNotGonoproxy(t *testing.T) {
	t.Setenv("GOPRIVATE", "nonmatching.example/*")
	t.Setenv("GONOPROXY", "github.com/golang/protobuf")
	t.Setenv("GOVCS", "public:off,private:git")
	var stdout, stderr bytes.Buffer
	code := run([]string{"github.com/golang/protobuf@v1.5.0"}, &stdout, &stderr, endpoints{})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "govcs-disallowed-locally") {
		t.Errorf("stdout = %q, want it to mention govcs-disallowed-locally", stdout.String())
	}
	if !strings.Contains(stdout.String(), "public") {
		t.Errorf("stdout = %q, want it to say the module was classified public (per GOPRIVATE, not GONOPROXY)", stdout.String())
	}
	if strings.Contains(stdout.String(), "private-module-locally") {
		t.Errorf("stdout = %q, want it NOT to fall back to the plain private-module-locally message", stdout.String())
	}
}

// TestRun_LocalModulePrivate_LeadingSpaceDoesNotMatch is the run()-level
// regression case for the splitPatterns leading-space bug (see
// pattern_test.go's TestSplitPatterns_LeadingSpaceBreaksMatch for the
// live-toolchain verification): GOPRIVATE="nomatch/*, example.com/mod" has
// a space before the second pattern — a natural way to write a comma list
// by hand — which real `go` never matches against the unspaced module
// path "example.com/mod" (confirmed live with `go mod download -x`
// against the analogous golang.org/x/text case: it fetches through
// proxy.golang.org rather than going direct to VCS). Before the fix,
// goproxycheck's own trimming matched it anyway and short-circuited with a
// false private-module-locally, skipping the real proxy/sumdb probe
// entirely. This test uses readyEndpoints (not the panic-if-reached
// endpoints{} the true-positive tests above use) specifically to prove the
// probe *did* run.
func TestRun_LocalModulePrivate_LeadingSpaceDoesNotMatch(t *testing.T) {
	t.Setenv("GOPRIVATE", "nomatch/*, example.com/mod")
	ep := readyEndpoints(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, ep)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "private-module-locally") {
		t.Errorf("stdout = %q, want it NOT to short-circuit as private-module-locally (the leading-space pattern shouldn't match)", stdout.String())
	}
	if !strings.Contains(stdout.String(), "ready") {
		t.Errorf("stdout = %q, want it to mention ready (the probe should have run)", stdout.String())
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

// TestRun_LocalGoproxyDirect_GovcsDisallowed is the fix for the same gap as
// TestRun_LocalModulePrivate_GovcsDisallowed above, for the GOPROXY=direct
// path instead of the GOPRIVATE path: the goproxy-direct-locally message
// used to only *mention* GOVCS as an unchecked caveat ("A real failure
// here ... would show up as its own error straight from go"), when it can
// check GOVCS directly instead. Confirmed live: `GOPROXY=direct
// GOVCS=public:hg go mod download github.com/golang/protobuf@v1.5.0` fails
// with "GOVCS disallows using git for public github.com/golang/protobuf",
// not a successful direct fetch.
func TestRun_LocalGoproxyDirect_GovcsDisallowed(t *testing.T) {
	t.Setenv("GOPROXY", "direct")
	t.Setenv("GOVCS", "public:hg")
	var stdout, stderr bytes.Buffer
	code := run([]string{"github.com/golang/protobuf@v1.5.0"}, &stdout, &stderr, endpoints{})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "govcs-disallowed-locally") {
		t.Errorf("stdout = %q, want it to mention govcs-disallowed-locally", stdout.String())
	}
	if strings.Contains(stdout.String(), "goproxy-direct-locally") {
		t.Errorf("stdout = %q, want it NOT to fall back to the plain goproxy-direct-locally message", stdout.String())
	}
}

// TestRun_LocalGoproxyDirect_GovcsMalformed is the same gap as
// TestRun_LocalModulePrivate_GovcsMalformed above, for the GOPROXY=direct
// path instead of the GOPRIVATE path — see that test's comment for the live
// verification (the same malformed-GOVCS shape applies regardless of why the
// fetch needs to go direct).
func TestRun_LocalGoproxyDirect_GovcsMalformed(t *testing.T) {
	t.Setenv("GOPROXY", "direct")
	t.Setenv("GOVCS", "badrule,public:git|hg")
	var stdout, stderr bytes.Buffer
	code := run([]string{"github.com/golang/protobuf@v1.5.0"}, &stdout, &stderr, endpoints{})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "govcs-malformed-locally") {
		t.Errorf("stdout = %q, want it to mention govcs-malformed-locally", stdout.String())
	}
	if strings.Contains(stdout.String(), "goproxy-direct-locally") || strings.Contains(stdout.String(), "govcs-disallowed-locally") {
		t.Errorf("stdout = %q, want it NOT to fall back to goproxy-direct-locally or govcs-disallowed-locally", stdout.String())
	}
}

// TestRun_LocalGoproxyDirect_GovcsUsesGoprivateNotGonoproxy is the mirror
// case of TestRun_LocalModulePrivate_GovcsUsesGoprivateNotGonoproxy above,
// for the GOPROXY=direct path instead of the GONOPROXY-match path: real
// go's GOVCS private/public classification checks GOPRIVATE regardless of
// *why* the fetch went direct, including when it's GOPROXY=direct itself
// (an unrelated reason) rather than a GONOPROXY match. Confirmed live
// (2026-09-27) with GOPROXY=direct, GOPRIVATE=github.com/golang/protobuf
// (matches), GONOPROXY=nonmatching.example/* (deliberately set to NOT
// match, overriding its usual GOPRIVATE fallback) and GOVCS="private:off,
// public:git": `go mod download -x github.com/golang/protobuf@v1.5.0`
// fails with "GOVCS disallows using git for *private* github.com/golang/
// protobuf" — real go classified it private purely off GOPRIVATE, even
// though GONOPROXY (the pattern this tool's GOPROXY=direct branch used to
// check against, via a hardcoded `false`) didn't match at all. Before this
// fix, run() hardcoded `false` for this branch's private bool, so it
// evaluated "public:git" instead and reported statusGoproxyDirectLocally
// ("will fetch it ... never through proxy.golang.org") for a config a real
// `go install` refuses outright.
func TestRun_LocalGoproxyDirect_GovcsUsesGoprivateNotGonoproxy(t *testing.T) {
	t.Setenv("GOPROXY", "direct")
	t.Setenv("GOPRIVATE", "github.com/golang/protobuf")
	t.Setenv("GONOPROXY", "nonmatching.example/*")
	t.Setenv("GOVCS", "private:off,public:git")
	var stdout, stderr bytes.Buffer
	code := run([]string{"github.com/golang/protobuf@v1.5.0"}, &stdout, &stderr, endpoints{})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "govcs-disallowed-locally") {
		t.Errorf("stdout = %q, want it to mention govcs-disallowed-locally", stdout.String())
	}
	if !strings.Contains(stdout.String(), "private") {
		t.Errorf("stdout = %q, want it to say the module was classified private (per GOPRIVATE, not GONOPROXY)", stdout.String())
	}
	if strings.Contains(stdout.String(), "goproxy-direct-locally") {
		t.Errorf("stdout = %q, want it NOT to fall back to the plain goproxy-direct-locally message", stdout.String())
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

// TestRun_UpgradeQueryResolves is the fix for a real bug found by testing
// goproxycheck against the "upgrade" version query (go.dev/ref/mod#version-
// queries), by analogy to the standard `go get module@upgrade` idiom.
// Confirmed live against cmd/go's own modload/query.go (newQueryMatcher,
// case query == "upgrade"): with no existing requirement — always
// goproxycheck's situation for a bare module@version CLI argument —
// "upgrade" resolves via the identical Latest lookup as "latest"
// (mayUseLatest = true), verified by matching `go get -x` request traces for
// `golang.org/x/mod@upgrade` and `golang.org/x/mod@latest`. Before this fix,
// "upgrade" was sent to the proxy as a literal version string, which live
// testing against proxy.golang.org shows 404s "not found: invalid version"
// for every module regardless of health, misdiagnosing a fully ready module
// as statusNotYetIndexed. This server only serves @v/v0.5.0.info and sum for
// v0.5.0 — if probe() still used the literal "upgrade" string, every request
// but @latest itself would 404.
func TestRun_UpgradeQueryResolves(t *testing.T) {
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
	code := run([]string{"example.com/mod@upgrade"}, &stdout, &stderr, ep)
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

// zipBuildErrorEndpoints simulates a tag the proxy can never turn into a
// module zip (e.g. a case-insensitive filename collision) — a permanent
// property of the tagged tree, not indexing lag, so --wait must stop on the
// first probe instead of polling it for the full --timeout.
func zipBuildErrorEndpoints(t *testing.T, hits *int) endpoints {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.1.0"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v0.1.0\n"))
		case strings.HasSuffix(r.URL.Path, ".info"):
			*hits++
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("fetch: create zip: some/BAD and some/bad differ only by case"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}
}

// TestRun_WaitStopsOnZipBuildError guards the run()-level early-break list in
// the --wait polling loop: before this, statusZipBuildError was missing from
// it (unlike statusModuleUnknown/statusBlocklistedMalicious/
// statusWrongImportPath/statusRetracted, the other permanent, waiting-can't-
// fix-this statuses), so `--wait` polled a doomed zip-build error at the
// full --interval cadence for the entire --timeout — reproduced live before
// the fix with a 300ms timeout that polled the full duration instead of
// returning after the first probe. Asserting hits == 1 (not an elapsed-time
// bound) keeps this deterministic instead of timing-flaky.
func TestRun_WaitStopsOnZipBuildError(t *testing.T) {
	var hits int
	ep := zipBuildErrorEndpoints(t, &hits)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--wait", "--interval=1ms", "--timeout=5s", "example.com/mod@v0.1.0"}, &stdout, &stderr, ep)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "zip-build-error") {
		t.Errorf("stdout = %q, want it to mention zip-build-error", stdout.String())
	}
	if hits != 1 {
		t.Fatalf(".info was probed %d times; want exactly 1 — --wait should stop immediately on a permanent zip-build-error instead of polling the full 5s timeout", hits)
	}
}

// majorVersionMismatchEndpoints simulates a version whose go.mod exists but
// lacks the /vN major-version suffix the proxy requires (the real
// github.com/osrg/gobgp@v2.16.0 shape) — a permanent property of that tag,
// not indexing lag, so --wait must stop on the first probe instead of
// polling it for the full --timeout.
func majorVersionMismatchEndpoints(t *testing.T, hits *int) endpoints {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.1.0"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v0.1.0\n"))
		case strings.HasSuffix(r.URL.Path, ".info"):
			*hits++
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`not found: example.com/mod@v2.0.0: invalid version: module contains a go.mod file, so module path must match major version ("example.com/mod/v2")`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}
}

// TestRun_WaitStopsOnMajorVersionMismatch guards the run()-level early-break
// list in the --wait polling loop the same way TestRun_WaitStopsOnZipBuildError
// does for statusZipBuildError: before statusMajorVersionMismatch was added
// to that list, `--wait` polled a doomed major-version-suffix mismatch at
// the full --interval cadence for the entire --timeout instead of returning
// after the first probe, since nothing about the tag's go.mod can ever
// change by waiting.
func TestRun_WaitStopsOnMajorVersionMismatch(t *testing.T) {
	var hits int
	ep := majorVersionMismatchEndpoints(t, &hits)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--wait", "--interval=1ms", "--timeout=5s", "example.com/mod@v2.0.0"}, &stdout, &stderr, ep)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "major-version-mismatch") {
		t.Errorf("stdout = %q, want it to mention major-version-mismatch", stdout.String())
	}
	if hits != 1 {
		t.Fatalf(".info was probed %d times; want exactly 1 — --wait should stop immediately on a permanent major-version-mismatch instead of polling the full 5s timeout", hits)
	}
}

// unknownRevisionEndpoints simulates a version query naming a revision that
// doesn't exist in the module's repository at all (a fabricated
// pseudo-version, a bogus branch, or a mistyped commit hash) — confirmed
// live (2026-09-27) that proxy.golang.org 404s this with "invalid version:
// unknown revision <name>" and never resolves it no matter how long you
// wait, since there's no such revision to eventually pick up.
func unknownRevisionEndpoints(t *testing.T, hits *int) endpoints {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.1.0"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v0.1.0\n"))
		case strings.HasSuffix(r.URL.Path, ".info"):
			*hits++
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`not found: example.com/mod@bogus-branch: invalid version: unknown revision bogus-branch`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}
}

// TestRun_WaitStopsOnUnknownRevision guards the run()-level early-break list
// in the --wait polling loop the same way TestRun_WaitStopsOnMajorVersionMismatch
// does for statusMajorVersionMismatch: before statusUnknownRevision was added
// to that list, `--wait` polled a doomed nonexistent-revision query at the
// full --interval cadence for the entire --timeout instead of returning
// after the first probe, since no amount of polling makes a revision that
// was never real start existing.
func TestRun_WaitStopsOnUnknownRevision(t *testing.T) {
	var hits int
	ep := unknownRevisionEndpoints(t, &hits)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--wait", "--interval=1ms", "--timeout=5s", "example.com/mod@bogus-branch"}, &stdout, &stderr, ep)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "unknown-revision") {
		t.Errorf("stdout = %q, want it to mention unknown-revision", stdout.String())
	}
	if hits != 1 {
		t.Fatalf(".info was probed %d times; want exactly 1 — --wait should stop immediately on a permanent unknown-revision error instead of polling the full 5s timeout", hits)
	}
}

// invalidPseudoVersionEndpoints simulates a pseudo-version whose encoded
// timestamp doesn't match the real commit's — confirmed live (2026-09-28)
// that proxy.golang.org 404s this with "invalid pseudo-version: does not
// match version-control timestamp (expected ...)" and never resolves it no
// matter how long you wait, since the real commit's timestamp can never
// retroactively match a fabricated one.
func invalidPseudoVersionEndpoints(t *testing.T, hits *int) endpoints {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.1.0"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v0.1.0\n"))
		case strings.HasSuffix(r.URL.Path, ".info"):
			*hits++
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`not found: example.com/mod@v0.0.0-20200101000000-abcdef123456: invalid pseudo-version: does not match version-control timestamp (expected 20260101000000)`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}
}

// TestRun_WaitStopsOnInvalidPseudoVersion guards the run()-level early-break
// list in the --wait polling loop the same way TestRun_WaitStopsOnUnknownRevision
// does for statusUnknownRevision: before statusInvalidPseudoVersion was added
// to that list, `--wait` polled a doomed malformed-pseudo-version query at
// the full --interval cadence for the entire --timeout instead of returning
// after the first probe.
func TestRun_WaitStopsOnInvalidPseudoVersion(t *testing.T) {
	var hits int
	ep := invalidPseudoVersionEndpoints(t, &hits)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--wait", "--interval=1ms", "--timeout=5s", "example.com/mod@v0.0.0-20200101000000-abcdef123456"}, &stdout, &stderr, ep)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "invalid-pseudo-version") {
		t.Errorf("stdout = %q, want it to mention invalid-pseudo-version", stdout.String())
	}
	if hits != 1 {
		t.Fatalf(".info was probed %d times; want exactly 1 — --wait should stop immediately on a permanent invalid-pseudo-version error instead of polling the full 5s timeout", hits)
	}
}

// deprecatedEndpoints simulates a module whose go.mod deprecates it —
// otherwise fully healthy (both .info and sum lookup succeed) — to check
// that --wait stops on the first probe instead of polling a permanent
// deprecation notice for the full --timeout, the same waste
// TestRun_WaitStopsOnZipBuildError guards against for statusZipBuildError.
func deprecatedEndpoints(t *testing.T, hits *int) endpoints {
	t.Helper()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.1.0"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v0.1.0\n"))
		case strings.HasSuffix(r.URL.Path, ".info"):
			*hits++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.1.0"}`))
		case strings.HasSuffix(r.URL.Path, ".mod"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("// Deprecated: use example.com/mod2 instead.\nmodule example.com/mod\n"))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(proxy.Close)
	sum := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(sum.Close)
	return endpoints{proxyBase: proxy.URL, sumBase: sum.URL, client: proxy.Client()}
}

func TestRun_WaitStopsOnDeprecated(t *testing.T) {
	var hits int
	ep := deprecatedEndpoints(t, &hits)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--wait", "--interval=1ms", "--timeout=5s", "example.com/mod@v0.1.0"}, &stdout, &stderr, ep)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "deprecated") {
		t.Errorf("stdout = %q, want it to mention deprecated", stdout.String())
	}
	if hits != 1 {
		t.Fatalf(".info was probed %d times; want exactly 1 — --wait should stop immediately on a permanent deprecation notice instead of polling the full 5s timeout", hits)
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

// TestRun_SumdbLagWithCustomGosumdb is the fix for a real bug found by
// checking whether the run #253 goprivaudit fix (custom multi-field GOSUMDB
// form, e.g. "name+key url") had been ported to this tool too — it hadn't.
// Confirmed live with a local HTTP server logging every request: pointing
// GOSUMDB at a custom name+key+URL made every sumdb request go to that
// server, none to sum.golang.org at all — same "public sumdb state is
// irrelevant" situation as GOSUMDB=off, just naming a different database
// instead of no database. Before this fix, goproxycheck reported
// statusSumdbLag ("retry shortly") based purely on the fake sum server here
// (standing in for a real sumdb-lag window) never having caught up, even
// though a real `go install` in this exact environment would never consult
// it at all.
//
// The GOSUMDB fixture here is a genuinely well-formed verifier key (built
// with golang.org/x/mod/sumdb/note.GenerateKey, name "sumdb.mycompany.example")
// plus a URL — confirmed live that `go install` accepts this exact value
// during GOSUMDB validation and only then fails on an unrelated network
// error dialing the (nonexistent) custom host, never on "invalid GOSUMDB".
// This test used to use "mycompany.example+abc123 https://sumdb.mycompany.example"
// instead — see TestRun_SumdbLagWithMalformedGosumdb below for why that
// fixture was itself a real bug this file's own tests were carrying: "abc123"
// isn't a valid key hash, so a real `go install` under that exact value
// fails outright with "invalid GOSUMDB: malformed verifier id", the opposite
// of "ready."
func TestRun_SumdbLagWithCustomGosumdb(t *testing.T) {
	t.Setenv("GOSUMDB", "sumdb.mycompany.example+18034219+ARh1MwsDARWl2XLlkBuE9hyxjXsSk5sX709QEBIDy21S https://sumdb.mycompany.example")
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, sumdbLagEndpoints(t))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "ready") {
		t.Errorf("stdout = %q, want it to mention ready", stdout.String())
	}
	if !strings.Contains(stdout.String(), `custom GOSUMDB "sumdb.mycompany.example"`) {
		t.Errorf("stdout = %q, want it to explain the custom-GOSUMDB reason", stdout.String())
	}
}

// TestRun_SumdbLagWithMalformedGosumdb is a regression test for a real bug:
// a custom (non-"off", non-public) $GOSUMDB that doesn't actually parse as a
// valid checksum-database verifier key used to be treated exactly like a
// genuinely working custom database — see the now-fixed fixture in
// TestRun_SumdbLagWithCustomGosumdb above, which used this exact value.
//
// Confirmed live (2026-09-28) against a fresh, isolated GOMODCACHE: `GOSUMDB=
// "mycompany.example+abc123 https://sumdb.mycompany.example" go install
// golang.org/x/text@v0.14.0` (a module not already recorded in any local
// go.sum, so verification is actually attempted) fails outright with
// "invalid GOSUMDB: malformed verifier id" — real `go install` never
// reaches the custom database at all, let alone treats the public sumdb's
// lag as irrelevant. Before this fix, goproxycheck reported statusReady
// ("a plain `go install` will work right now") for exactly this
// configuration — the opposite of what a real `go install` does.
func TestRun_SumdbLagWithMalformedGosumdb(t *testing.T) {
	t.Setenv("GOSUMDB", "mycompany.example+abc123 https://sumdb.mycompany.example")
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, sumdbLagEndpoints(t))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s", code, stdout.String())
	}
	out := stdout.String()
	if strings.Contains(out, "\nready\n") || strings.Contains(out, ": ready\n") {
		t.Errorf("stdout = %q, must not report ready: a real `go install` under this exact GOSUMDB fails outright", out)
	}
	if !strings.Contains(out, "gosumdb-malformed-locally") {
		t.Errorf("stdout = %q, want it to mention gosumdb-malformed-locally", out)
	}
	if !strings.Contains(out, "invalid GOSUMDB: malformed verifier id") {
		t.Errorf("stdout = %q, want it to quote the real `go` error", out)
	}
}

// TestRun_SumdbLagWithGoogleCnAlias pins that the documented
// "sum.golang.google.cn" alias (a China-reachable mirror of the *same*
// public sum.golang.org tree, not a different database — cmd/go's own
// dbDial rewrites it internally before ever dialing) is NOT treated as a
// custom database: sumdb-lag should still be reported, since a real `go`
// under this config still consults the same underlying tree the fake sum
// server here stands in for.
func TestRun_SumdbLagWithGoogleCnAlias(t *testing.T) {
	t.Setenv("GOSUMDB", "sum.golang.google.cn")
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, sumdbLagEndpoints(t))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), "sumdb-lag") {
		t.Errorf("stdout = %q, want it to mention sumdb-lag", stdout.String())
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
