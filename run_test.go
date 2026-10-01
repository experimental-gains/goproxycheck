package main

import (
	"bytes"
	"encoding/json"
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

// perVersionNegativeCacheEndpoints simulates the per-version negative-cache
// pattern this tool exists to detect: the module is known (@latest/@v/list
// both succeed and list the requested version) but that exact version's
// @v/<version>.info still 404s.
func perVersionNegativeCacheEndpoints(t *testing.T) endpoints {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.2.0"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v0.1.0\nv0.2.0\n"))
		default: // .info, sum lookup
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}
}

// moduleNegativeCacheEndpoints simulates the whole-module negative-cache
// pattern: @latest and @v/list both 404, but the repo-reachability check
// (repoCheckBase) reports the underlying repo as live and public.
func moduleNegativeCacheEndpoints(t *testing.T) endpoints {
	t.Helper()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(proxy.Close)
	repoCheck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(repoCheck.Close)
	return endpoints{proxyBase: proxy.URL, sumBase: proxy.URL, repoCheckBase: repoCheck.URL, client: proxy.Client()}
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

// TestRun_LocalModulePrivate_GovcsUsesRepoRootNotFullModulePath is the
// run()-level regression case for the localGovcsPrivate repo-root bug (see
// TestLocalGovcsPrivate_UsesRepoRootNotFullModulePath in govcs_test.go for
// the live-toolchain verification): a GOPRIVATE pattern that names a
// module's full import path — including a real major-version subdirectory
// like "/v2" — matches that full path but never matches the truncated
// github.com/owner/repo root real cmd/go's GOVCS classification actually
// checks. Confirmed live (2026-09-28): with GOPRIVATE=
// "github.com/googleapis/gax-go/v2" (a real module living in a real "v2"
// subdirectory) and GOVCS="public:off,private:git", `go mod download -x
// github.com/googleapis/gax-go/v2@v2.7.0` fails with "GOVCS disallows using
// git for public github.com/googleapis/gax-go; see 'go help vcs'" — real go
// classified it public. Before this fix, this tool classified it private
// (matching the full module path), evaluated the permissive "private:git"
// rule instead of "public:off", and reported statusPrivateModuleLocally
// ("will fetch it directly ... no problem") for a fetch that actually fails
// outright.
func TestRun_LocalModulePrivate_GovcsUsesRepoRootNotFullModulePath(t *testing.T) {
	t.Setenv("GOPRIVATE", "github.com/googleapis/gax-go/v2")
	t.Setenv("GOVCS", "public:off,private:git")
	var stdout, stderr bytes.Buffer
	code := run([]string{"github.com/googleapis/gax-go/v2@v2.7.0"}, &stdout, &stderr, endpoints{})
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "govcs-disallowed-locally") {
		t.Errorf("stdout = %q, want it to mention govcs-disallowed-locally", stdout.String())
	}
	if !strings.Contains(stdout.String(), "GOVCS disallows using git for public github.com/googleapis/gax-go;") {
		t.Errorf("stdout = %q, want it to quote real go's exact error naming the truncated repo root, not the full module path", stdout.String())
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

// TestRun_LatestQueryJSONIncludesResolvedVersion is the fix for a real bug
// found by testing --json (this tool's documented machine-readable output
// mode) against a "latest"-style query: the text-mode diagnosis already
// names the concrete resolved version via displayTarget ("resolved to
// v0.5.0"), but --json's top-level "version" field echoed back the raw,
// unresolved query string ("latest") with no structured field carrying the
// concrete version at all — a script parsing the JSON (the entire point of
// --json) has no way to learn which version was actually checked without
// regexing it back out of the free-text "message" field, defeating the
// purpose of structured output. Confirmed live against the real
// proxy.golang.org: `goproxycheck --json golang.org/x/mod@latest` prints
// "version": "latest" even though the message says "(resolved to v0.41.0)".
// This server only serves @v/v0.5.0.info and sum for v0.5.0, same shape as
// TestRun_LatestQueryResolves.
func TestRun_LatestQueryJSONIncludesResolvedVersion(t *testing.T) {
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
	code := run([]string{"--json", "example.com/mod@latest"}, &stdout, &stderr, ep)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	var out struct {
		Module          string `json:"module"`
		Version         string `json:"version"`
		Status          string `json:"status"`
		ResolvedVersion string `json:"resolved_version"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout.String(), err)
	}
	if out.Version != "latest" {
		t.Errorf("version = %q, want the literal query %q preserved", out.Version, "latest")
	}
	if out.ResolvedVersion != "v0.5.0" {
		t.Errorf("resolved_version = %q, want %q", out.ResolvedVersion, "v0.5.0")
	}
}

// TestRun_JSONOmitsResolvedVersionForLiteralVersion confirms the sibling,
// already-correct case is unaffected by the fix above: an explicit, already-
// concrete module@version argument never sets report.resolvedVersion (see
// probe()), so --json's output shouldn't grow a "resolved_version" key that
// would just duplicate "version" for the overwhelming majority of
// invocations (an explicit version is the common case; only latest/upgrade/
// partial/comparison/revision queries resolve to something different).
func TestRun_JSONOmitsResolvedVersionForLiteralVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--json", "example.com/mod@v0.1.0"}, &stdout, &stderr, readyEndpoints(t))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	var out map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", stdout.String(), err)
	}
	if _, ok := out["resolved_version"]; ok {
		t.Errorf("stdout = %q, want no resolved_version key for an already-literal version", stdout.String())
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

// negativeCacheEndpoints simulates the per-version negative-cache pattern
// (like perVersionNegativeCacheEndpoints) but also counts .info requests, so
// TestRun_WaitStopsOnNegativeCache can assert on hits instead of elapsed
// time.
func negativeCacheEndpoints(t *testing.T, hits *int) endpoints {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.2.0"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v0.1.0\nv0.2.0\n"))
		case strings.HasSuffix(r.URL.Path, ".info"):
			*hits++
			w.WriteHeader(http.StatusNotFound)
		default: // sum lookup
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}
}

// TestRun_WaitStopsOnNegativeCache guards the run()-level early-break list in
// the --wait polling loop the same way TestRun_WaitStopsOnZipBuildError does
// for statusZipBuildError: before this, statusNegativeCache (the per-version
// case) was missing from that list even though diagnose's own message for it
// says outright "It has been observed not to clear on its own within 30+
// minutes. Fix: cut a new patch tag ... rather than waiting" — the same
// "waiting is not the fix" property every other status in the list already
// has. Reproduced live before the fix: with --wait --timeout=300ms
// --interval=10ms against a fake proxy serving this exact pattern, it polled
// .info about 28 times over the full 300ms instead of returning after the
// first probe. Asserting hits == 1 (not an elapsed-time bound) keeps this
// deterministic instead of timing-flaky, matching the sibling tests above.
func TestRun_WaitStopsOnNegativeCache(t *testing.T) {
	var hits int
	ep := negativeCacheEndpoints(t, &hits)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--wait", "--interval=1ms", "--timeout=5s", "example.com/mod@v0.1.0"}, &stdout, &stderr, ep)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stdout.String(), "negative-cache-suspected") {
		t.Errorf("stdout = %q, want it to mention negative-cache-suspected", stdout.String())
	}
	if hits != 1 {
		t.Fatalf(".info was probed %d times; want exactly 1 — --wait should stop immediately on a negative-cache verdict instead of polling the full 5s timeout", hits)
	}
}

// noMatchingVersionEndpoints simulates a comparison version query (e.g.
// "<v0.5.0") for which no published version satisfies the bound — @v/list
// only lists versions the query excludes. Counts @v/list requests, since a
// real doomed-poll regression here would show up as repeated @v/list hits
// (probe() has no per-version endpoint to poll for this case at all — see
// TestProbe_ComparisonQueryNoMatchSkipsLiteralProbe).
func noMatchingVersionEndpoints(t *testing.T, listHits *int) endpoints {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v1.5.0"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			*listHits++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v1.0.0\nv1.5.0\n"))
		case strings.HasSuffix(r.URL.Path, "/@v/v1.5.0.mod"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("module example.com/mod\n"))
		default:
			t.Errorf("unexpected request for %s — a comparison query with no matching version should never reach a per-version endpoint", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}
}

// TestRun_WaitStopsOnNoMatchingVersion guards the run()-level early-break
// list in the --wait polling loop the same way TestRun_WaitStopsOnZipBuildError
// and TestRun_WaitStopsOnNegativeCache do for their own statuses: a
// comparison query with no satisfying published version at all (e.g.
// "<v0.5.0" against a module whose lowest version is v1.0.0) is a permanent,
// offline-provable failure — real `go get`/`go install` fails immediately
// with "no matching versions for query", the same "waiting is not the fix"
// property the other statuses in this list already have. Before the fix,
// this fell through to statusNotYetIndexed, which was NOT in the early-break
// list, so --wait polled a doomed @v/list for the full --timeout. Asserting
// listHits == 1 (not an elapsed-time bound) keeps this deterministic instead
// of timing-flaky, matching the sibling tests above.
func TestRun_WaitStopsOnNoMatchingVersion(t *testing.T) {
	var listHits int
	ep := noMatchingVersionEndpoints(t, &listHits)
	var stdout, stderr bytes.Buffer
	code := run([]string{"--wait", "--interval=1ms", "--timeout=5s", "example.com/mod@<v0.5.0"}, &stdout, &stderr, ep)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "no-matching-version") {
		t.Errorf("stdout = %q, want it to mention no-matching-version", stdout.String())
	}
	if listHits != 1 {
		t.Fatalf("@v/list was probed %d times; want exactly 1 — --wait should stop immediately on a no-matching-version verdict instead of polling the full 5s timeout", listHits)
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

// retractedEndpoints simulates a module whose checked version (also
// @latest's own version) is covered by a `retract` directive, with
// proxy.golang.org and sum.golang.org both otherwise fully healthy — the
// same shape TestDiagnose_Retracted uses at the diagnose() level, reused
// here for full run()-level end-to-end tests.
func retractedEndpoints(t *testing.T) endpoints {
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
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.1.0"}`))
		case strings.HasSuffix(r.URL.Path, ".mod"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("module example.com/mod\n\nretract v0.1.0 // bad release\n"))
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

// TestRun_ReadyWithMalformedGosumdb, TestRun_RetractedWithMalformedGosumdb,
// and TestRun_DeprecatedWithMalformedGosumdb are regression tests for a real
// bug: the malformed-$GOSUMDB check (TestRun_SumdbLagWithMalformedGosumdb
// above) was only ever consulted when the probe itself produced
// statusSumdbLag — but statusReady, statusRetracted, and statusDeprecated
// all make the identical "a plain `go install` will work/succeed" claim
// based purely on proxy.golang.org and the PUBLIC sum.golang.org, with no
// awareness that a malformed local $GOSUMDB breaks verification
// unconditionally, before any of those three states even come into play.
//
// Confirmed live (2026-10-01) against a fresh GOMODCACHE with
// GOSUMDB=sum.example.com (the identical malformed-verifier-id shape
// TestRun_SumdbLagWithMalformedGosumdb already uses): `go mod download
// golang.org/x/mod@v0.41.0` (fully live on both proxy.golang.org and
// sum.golang.org right now) fails outright with `invalid GOSUMDB: malformed
// verifier id`, and the identical env against the real, currently-retracted
// `github.com/mattn/go-sqlite3@v2.0.3+incompatible` fails the exact same
// way — never even reaching the point of reporting retraction. Before the
// fix, goproxycheck reported plain statusReady for the first case and
// statusRetracted/statusDeprecated with "resolves fine... will succeed" for
// the other two — the opposite of what the real command does.
func TestRun_ReadyWithMalformedGosumdb(t *testing.T) {
	t.Setenv("GOSUMDB", "mycompany.example+abc123 https://sumdb.mycompany.example")
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, readyEndpoints(t))
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
}

func TestRun_RetractedWithMalformedGosumdb(t *testing.T) {
	t.Setenv("GOSUMDB", "mycompany.example+abc123 https://sumdb.mycompany.example")
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, retractedEndpoints(t))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s", code, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "gosumdb-malformed-locally") {
		t.Errorf("stdout = %q, want it to mention gosumdb-malformed-locally, not a retracted-but-will-succeed verdict", out)
	}
	if strings.Contains(out, "will succeed") {
		t.Errorf("stdout = %q, must not claim `go install` will succeed: a real one fails outright on this GOSUMDB", out)
	}
}

func TestRun_DeprecatedWithMalformedGosumdb(t *testing.T) {
	t.Setenv("GOSUMDB", "mycompany.example+abc123 https://sumdb.mycompany.example")
	var hits int
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, deprecatedEndpoints(t, &hits))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s", code, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "gosumdb-malformed-locally") {
		t.Errorf("stdout = %q, want it to mention gosumdb-malformed-locally, not a deprecated-but-will-succeed verdict", out)
	}
	if strings.Contains(out, "will succeed") {
		t.Errorf("stdout = %q, must not claim `go install` will succeed: a real one fails outright on this GOSUMDB", out)
	}
}

// TestRun_NegativeCache_DirectFallbackNote is the fix for a real gap in this
// tool's core negative-cache diagnosis: it told the user flatly that
// nothing but waiting (or, for the per-version case, cutting a new tag)
// would help, and that `GOPROXY=direct` was a deliberate opt-in workaround
// — but real cmd/go's own proxy-list fallback (confirmed directly against
// cmd/go/internal/modfetch's TryProxies/lookup source, and live
// end-to-end: with GOPROXY="<a proxy that 404s everything>,direct", `go
// install github.com/experimental-gains/goproxycheck@v0.1.55` — a real,
// live tag — still succeeds via an automatic direct git fetch) retries the
// *entire* module lookup against the next entry in the chain on a 404, not
// just that one request. The default GOPROXY value nearly every install
// leaves untouched, "https://proxy.golang.org,direct", already ends in
// exactly that fallback — so a plain `go install` on an ordinary,
// unmodified machine will very likely still succeed right now, without
// waiting for anything. Before this fix, goproxycheck's message actively
// implied the opposite: that only a deliberate GOPROXY override would work
// around it.
func TestRun_NegativeCache_DirectFallbackNote(t *testing.T) {
	t.Setenv("GOPROXY", "https://proxy.golang.org,direct")
	t.Setenv("GOVCS", "")
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GONOPROXY", "")
	var stdout, stderr bytes.Buffer
	code := run([]string{"github.com/example-gains-test/negcache@v0.1.0"}, &stdout, &stderr, perVersionNegativeCacheEndpoints(t))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "negative-cache-suspected") {
		t.Fatalf("stdout = %q, want it to mention negative-cache-suspected", stdout.String())
	}
	if !strings.Contains(stdout.String(), "falls back to `direct`") {
		t.Errorf("stdout = %q, want it to mention the automatic direct-fallback note", stdout.String())
	}
}

// TestRun_ModuleNegativeCache_DirectFallbackNote covers the same gap as
// TestRun_NegativeCache_DirectFallbackNote above, for the whole-module
// negative-cache verdict (statusModuleNegativeCache) instead of the
// per-version one — a separate code path in diagnose() that needed the
// same note.
func TestRun_ModuleNegativeCache_DirectFallbackNote(t *testing.T) {
	t.Setenv("GOPROXY", "https://proxy.golang.org,direct")
	t.Setenv("GOVCS", "")
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GONOPROXY", "")
	var stdout, stderr bytes.Buffer
	code := run([]string{"github.com/example-gains-test/negcache@v0.1.0"}, &stdout, &stderr, moduleNegativeCacheEndpoints(t))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "module-negative-cache-suspected") {
		t.Fatalf("stdout = %q, want it to mention module-negative-cache-suspected", stdout.String())
	}
	if !strings.Contains(stdout.String(), "falls back to `direct`") {
		t.Errorf("stdout = %q, want it to mention the automatic direct-fallback note", stdout.String())
	}
}

// TestRun_NegativeCache_NoDirectFallbackNote_NonGithub guards against
// negativeCacheDirectFallbackNote overclaiming for a module this tool has
// no VCS-type certainty for (see githubRepoPattern's doc comment) — a
// non-github.com host might not resolve via direct-fetch discovery at all,
// so no fallback claim should be made for it.
func TestRun_NegativeCache_NoDirectFallbackNote_NonGithub(t *testing.T) {
	t.Setenv("GOPROXY", "https://proxy.golang.org,direct")
	t.Setenv("GOVCS", "")
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GONOPROXY", "")
	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, perVersionNegativeCacheEndpoints(t))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "falls back to `direct`") {
		t.Errorf("stdout = %q, want it NOT to claim a direct fallback for a non-github.com module", stdout.String())
	}
}

// TestRun_NegativeCache_NoDirectFallbackNote_GovcsDisallows guards against
// overclaiming when the local GOVCS setting would itself block a direct
// git fetch of this (public) module — the same rule real `go` applies via
// checkGOVCS, mirrored here by localGovcsAllowsGit.
func TestRun_NegativeCache_NoDirectFallbackNote_GovcsDisallows(t *testing.T) {
	t.Setenv("GOPROXY", "https://proxy.golang.org,direct")
	t.Setenv("GOVCS", "public:off")
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GONOPROXY", "")
	var stdout, stderr bytes.Buffer
	code := run([]string{"github.com/example-gains-test/negcache@v0.1.0"}, &stdout, &stderr, perVersionNegativeCacheEndpoints(t))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "falls back to `direct`") {
		t.Errorf("stdout = %q, want it NOT to claim a direct fallback when GOVCS disallows git for public modules", stdout.String())
	}
}

// TestRun_NegativeCache_NoDirectFallbackNote_NoDirectInChain guards against
// overclaiming when the local GOPROXY chain simply doesn't have "direct"
// as the next entry after the public proxy (here: no next entry at all) —
// the note only applies to a chain actually shaped like the documented
// default.
func TestRun_NegativeCache_NoDirectFallbackNote_NoDirectInChain(t *testing.T) {
	t.Setenv("GOPROXY", "https://proxy.golang.org")
	t.Setenv("GOVCS", "")
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GONOPROXY", "")
	var stdout, stderr bytes.Buffer
	code := run([]string{"github.com/example-gains-test/negcache@v0.1.0"}, &stdout, &stderr, perVersionNegativeCacheEndpoints(t))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "falls back to `direct`") {
		t.Errorf("stdout = %q, want it NOT to claim a direct fallback when GOPROXY has no further entry", stdout.String())
	}
}

// TestRun_NotYetIndexed_DirectFallbackNote covers a real gap left behind by
// the negative-cache fix above (TestRun_NegativeCache_DirectFallbackNote):
// statusNotYetIndexed is mechanically the same situation from cmd/go's own
// perspective as statusNegativeCache/statusModuleNegativeCache — a 404 on
// the @v/<version>.info request that triggered this diagnosis. Real cmd/go's
// TryProxies (cmd/go/internal/modfetch/proxy.go, confirmed directly against
// that source) falls back to the next GOPROXY chain entry on any
// fs.ErrNotExist-equivalent error, with no distinction between "the proxy
// cached a stale failure" and "the proxy just hasn't indexed this tag yet" —
// both are just a 404. By the time diagnose() reaches the not-yet-indexed
// fallback, every permanent-failure marker (isUnknownRevision and friends)
// has already been ruled out, so — like the negative-cache case — this is
// overwhelmingly a real, just-pushed tag the automatic direct-VCS fallback
// can already fetch: confirmed live (2026-09-29) against a real, warm
// module (golang.org/x/mod/@v/v0.999.999.info and .../@v/totallyfake...info)
// that a genuinely nonexistent version/branch 404s with the distinct
// isUnknownRevision marker instead, so it would never reach this fallback in
// the first place. Before this fix, a freshly-tagged release under the
// ordinary default GOPROXY chain ("https://proxy.golang.org,direct") got
// "retry in a minute, or use --wait" with no mention that a plain
// `go install` right now would likely already succeed via direct git fetch —
// exactly the caveat the negative-cache statuses already got.
func TestRun_NotYetIndexed_DirectFallbackNote(t *testing.T) {
	t.Setenv("GOPROXY", "https://proxy.golang.org,direct")
	t.Setenv("GOVCS", "")
	t.Setenv("GOPRIVATE", "")
	t.Setenv("GONOPROXY", "")
	var stdout, stderr bytes.Buffer
	code := run([]string{"github.com/example-gains-test/freshtag@v0.1.0"}, &stdout, &stderr, notYetIndexedEndpoints(t))
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "not-yet-indexed") {
		t.Fatalf("stdout = %q, want it to mention not-yet-indexed", stdout.String())
	}
	if !strings.Contains(stdout.String(), "falls back to `direct`") {
		t.Errorf("stdout = %q, want it to mention the automatic direct-fallback note", stdout.String())
	}
}
