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
		w.Write([]byte(`{"Version":"v0.1.0"}`))
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
			w.Write([]byte(`{"Version":"v0.0.9"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("v0.0.9\n"))
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
			w.Write([]byte(`{"Version":"v0.1.0"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			if ready {
				w.Write([]byte("v0.1.0\n"))
			} else {
				w.Write([]byte("v0.0.9\n"))
			}
		default: // .info, sum lookup
			if ready {
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"Version":"v0.1.0"}`))
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
