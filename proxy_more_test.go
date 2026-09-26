package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseVersionInfo(t *testing.T) {
	v, err := parseVersionInfo(`{"Version":"v0.1.0","Time":"2026-09-19T00:00:00Z"}`)
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != "v0.1.0" || v.Time != "2026-09-19T00:00:00Z" {
		t.Errorf("got %+v", v)
	}
}

func TestParseVersionInfo_Invalid(t *testing.T) {
	if _, err := parseVersionInfo("not json"); err == nil {
		t.Fatal("expected an error for invalid JSON")
	}
}

func TestCheckVersion(t *testing.T) {
	cases := []struct {
		name string
		r    report
		want string
	}{
		{"literal version, no resolution", report{version: "v1.2.3"}, "v1.2.3"},
		{"latest resolved", report{version: "latest", resolvedVersion: "v1.2.3"}, "v1.2.3"},
	}
	for _, c := range cases {
		if got := c.r.checkVersion(); got != c.want {
			t.Errorf("%s: checkVersion() = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestProbe_LatestQuery_UnknownModule confirms probe() doesn't try to
// resolve "latest" when the module itself is unknown (@latest 404s) — it
// must fall through to probing the literal "latest" string like any other
// unresolvable version, not panic or resolve to an empty version.
func TestProbe_LatestQuery_UnknownModule(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	ep := endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}

	r := ep.probe("example.com/nope", "latest")
	if r.resolvedVersion != "" {
		t.Errorf("resolvedVersion = %q, want empty for an unknown module", r.resolvedVersion)
	}
	if r.moduleKnown() {
		t.Error("moduleKnown() = true, want false")
	}
}

// TestProbe_VersionQueryResolvesForSumLookup is the fix for a real bug found
// by testing goproxycheck against real documented Go version-query forms
// other than "latest" — a partial version like "v0.19" or a revision
// identifier like a branch name. Confirmed live against proxy.golang.org:
// unlike "latest" (no @v/latest.info endpoint at all), these queries DO
// resolve directly against @v/<query>.info, which returns the resolved
// canonical version in its body — but sum.golang.org's lookup endpoint only
// accepts a canonical version and 400s on the literal query string. This
// fake server serves @v/v0.19.info as a query that resolves to v0.5.0, and
// only serves the sum lookup for the canonical v0.5.0 — if probe() used the
// literal "v0.19" for the sum lookup (the pre-fix behavior), it would 404.
func TestProbe_VersionQueryResolvesForSumLookup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.5.0"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v0.5.0\n"))
		case strings.HasSuffix(r.URL.Path, "/@v/v0.19.info"):
			// The proxy resolves the query server-side and reports the
			// canonical version it resolved to, distinct from the literal
			// query string requested.
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

	r := ep.probe("example.com/mod", "v0.19")
	if r.resolvedVersion != "v0.5.0" {
		t.Errorf("resolvedVersion = %q, want %q", r.resolvedVersion, "v0.5.0")
	}
	if !r.sum.ok {
		t.Error("sum.ok = false, want true (sum lookup should use the resolved canonical version, not the literal query)")
	}
	if !r.ready() {
		t.Error("ready() = false, want true")
	}
}

// TestProbe_LatestModFileScopedPastSelfRetractingLatest reproduces
// github.com/jayconrod/retract, the Go team's own canonical example of a
// version retracting itself: v1.0.1 retracts both itself and v1.0.0, so
// @latest resolves past both of them to v0.9.9 — a version tagged years
// before the `retract` directive existed, whose go.mod carries no retract
// block at all (confirmed live against the real proxy.golang.org, 2026-09).
// Before this fix, probe() always fetched @latest's own version's
// (v0.9.9's) go.mod into latestModFile, so a go.mod requiring v1.0.0 of
// this module produced no retraction finding — reproduced end to end via
// diagnose() below, TestDiagnose_RetractedPastSelfRetractingLatest.
func TestProbe_LatestModFileScopedPastSelfRetractingLatest(t *testing.T) {
	const module = "github.com/jayconrod/retract"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.9.9","Time":"2021-01-26T16:46:49Z"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v1.0.0\nv0.9.9\nv1.0.1\n"))
		case strings.HasSuffix(r.URL.Path, "/@v/v0.9.9.mod"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("module " + module + "\n\ngo 1.16\n"))
		case strings.HasSuffix(r.URL.Path, "/@v/v1.0.1.mod"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("module " + module + "\n\ngo 1.16\n\nretract (\n\tv1.0.0 // Published accidentally.\n\tv1.0.1 // For retractions only.\n)\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ep := endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}

	r := ep.probe(module, "v1.0.0")
	if !r.latestModFile.ok {
		t.Fatalf("latestModFile.ok = false, want true")
	}
	if !strings.Contains(r.latestModFile.body, "retract") {
		t.Errorf("latestModFile.body = %q, want it to be v1.0.1's go.mod (the highest tag, past self-retracting @latest), which carries the retract directive", r.latestModFile.body)
	}
}

// TestProbe_LatestModFileScopedToMajorLine is the non-regression
// counterpart of TestProbe_LatestModFileScopedPastSelfRetractingLatest,
// modeled on github.com/mattn/go-sqlite3: its highest tag overall is an
// abandoned, bare v2.0.3+incompatible experiment with no retract block,
// while the retraction covering that exact version lives in the
// still-active v1.x line's go.mod (v1.14.52, @latest's own version here).
// Scoping the "highest tag" search to @latest's own major-version line
// (collapsing v0/v1) must still land on v1.14.52, not the higher but
// unrelated v2 tag.
func TestProbe_LatestModFileScopedToMajorLine(t *testing.T) {
	const module = "github.com/mattn/go-sqlite3"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v1.14.52"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v1.14.52\nv2.0.3+incompatible\n"))
		case strings.HasSuffix(r.URL.Path, "/@v/v1.14.52.mod"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("module " + module + "\n\ngo 1.21\n\nretract (\n\t[v2.0.0+incompatible, v2.0.7+incompatible] // Accidental.\n)\n"))
		case strings.HasSuffix(r.URL.Path, "/@v/v2.0.3+incompatible.mod"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("module " + module + "\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ep := endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}

	r := ep.probe(module, "v2.0.3+incompatible")
	if !r.latestModFile.ok {
		t.Fatalf("latestModFile.ok = false, want true")
	}
	if !strings.Contains(r.latestModFile.body, "retract") {
		t.Errorf("latestModFile.body = %q, want v1.14.52's go.mod (same major line as @latest), not the unrelated higher v2 tag's bare go.mod", r.latestModFile.body)
	}
}

func TestReady(t *testing.T) {
	cases := []struct {
		name string
		r    report
		want bool
	}{
		{"both ok", report{versionInfo: probeResult{ok: true}, sum: probeResult{ok: true}}, true},
		{"versionInfo not ok", report{versionInfo: probeResult{ok: false}, sum: probeResult{ok: true}}, false},
		{"sum not ok", report{versionInfo: probeResult{ok: true}, sum: probeResult{ok: false}}, false},
	}
	for _, c := range cases {
		if got := c.r.ready(); got != c.want {
			t.Errorf("%s: ready() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestFirstErr(t *testing.T) {
	if err := firstErr(nil, nil, nil); err != nil {
		t.Errorf("got %v, want nil", err)
	}
	want := errBoom
	if got := firstErr(nil, want, nil); got != want {
		t.Errorf("got %v, want %v", got, want)
	}
}

var errBoom = &boomErr{}

type boomErr struct{}

func (*boomErr) Error() string { return "boom" }
