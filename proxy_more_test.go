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

// TestComparisonQuery checks the operator/operand split for the four
// documented comparison version-query forms (go.dev/ref/mod#version-queries),
// including that "<=" and ">=" aren't mis-split as "<"/">" with a leading
// "=" left in the operand.
func TestComparisonQuery(t *testing.T) {
	cases := []struct {
		v           string
		op, operand string
		ok          bool
	}{
		{"<v1.2.3", "<", "v1.2.3", true},
		{"<=v1.2.3", "<=", "v1.2.3", true},
		{">v1.2.3", ">", "v1.2.3", true},
		{">=v1.2.3", ">=", "v1.2.3", true},
		{"v1.2.3", "", "", false},
		{"latest", "", "", false},
	}
	for _, c := range cases {
		op, operand, ok := comparisonQuery(c.v)
		if op != c.op || operand != c.operand || ok != c.ok {
			t.Errorf("comparisonQuery(%q) = (%q, %q, %v), want (%q, %q, %v)", c.v, op, operand, ok, c.op, c.operand, c.ok)
		}
	}
}

// TestResolveComparisonQuery checks the picked version for each operator
// against a fixed @v/list, plus the two failure cases (invalid operand, no
// listed version satisfies the comparison).
func TestResolveComparisonQuery(t *testing.T) {
	listed := []string{"v1.0.0", "v1.2.0", "v1.5.0", "v2.0.0"}
	cases := []struct {
		op, operand string
		want        string
		wantOK      bool
	}{
		{"<", "v1.5.0", "v1.2.0", true},
		{"<=", "v1.5.0", "v1.5.0", true},
		{">", "v1.2.0", "v1.5.0", true},
		{">=", "v1.2.0", "v1.2.0", true},
		{"<", "v1.0.0", "", false}, // nothing listed is lower
		{">", "v2.0.0", "", false}, // nothing listed is higher
		{"<", "not-a-version", "", false},
	}
	for _, c := range cases {
		got, ok := resolveComparisonQuery(c.op, c.operand, listed)
		if got != c.want || ok != c.wantOK {
			t.Errorf("resolveComparisonQuery(%q, %q, listed) = (%q, %v), want (%q, %v)", c.op, c.operand, got, ok, c.want, c.wantOK)
		}
	}
}

// TestResolveComparisonQuery_PrefersReleaseOverHigherPrerelease is the fix
// for a real bug found by testing goproxycheck against a real module's
// actual @v/list: resolveComparisonQuery used to pick purely by raw
// semver.Compare across every listed version, with no regard for whether a
// candidate was a tagged release or a prerelease. Real cmd/go
// (modload/query.go's queryMatcher.filterVersions/Query) never does that —
// it splits candidates into releases and prereleases up front and only
// ever considers prereleases when zero releases satisfy the comparison at
// all, per go.dev/ref/mod#version-queries ("... prefers the latest release
// version").
//
// Confirmed live (2026-09-28) against google.golang.org/grpc, whose real
// @v/list includes a prerelease marker tag (v1.86.0-dev, an in-progress
// next-minor placeholder with no released counterpart) that raw-semver-compares
// higher than its actual highest release (v1.84.0): `go get -x
// google.golang.org/grpc@<v2.0.0` resolves to v1.84.0 (confirmed via the
// `go: added google.golang.org/grpc v1.84.0` trace line), never touching
// v1.86.0-dev even though it satisfies the identical "<v2.0.0" bound and
// compares higher. This fixture reproduces that exact shape: a release
// lower than a prerelease, both satisfying the bound.
func TestResolveComparisonQuery_PrefersReleaseOverHigherPrerelease(t *testing.T) {
	listed := []string{"v1.60.0", "v1.84.0", "v1.86.0-dev"}
	cases := []struct {
		op, operand string
		want        string
		wantOK      bool
	}{
		// A release exists satisfying the bound: pick the highest release,
		// never the higher-raw-semver prerelease.
		{"<", "v2.0.0", "v1.84.0", true},
		{"<=", "v2.0.0", "v1.84.0", true},
		// No release satisfies ">v1.84.0" (only the prerelease does): fall
		// back to the prerelease since releases is empty for this bound.
		{">", "v1.84.0", "v1.86.0-dev", true},
		{">=", "v1.86.0-dev", "v1.86.0-dev", true},
	}
	for _, c := range cases {
		got, ok := resolveComparisonQuery(c.op, c.operand, listed)
		if got != c.want || ok != c.wantOK {
			t.Errorf("resolveComparisonQuery(%q, %q, listed) = (%q, %v), want (%q, %v)", c.op, c.operand, got, ok, c.want, c.wantOK)
		}
	}
}

// TestProbe_ComparisonQueryResolvesViaList is the fix for a real bug found
// by testing goproxycheck against real documented Go version-query forms:
// a comparison query like "<v0.20.0" (go.dev/ref/mod#version-queries).
// Confirmed live against proxy.golang.org and a real `go get -x`: unlike a
// partial version ("v0.19") or a revision identifier (a branch name), which
// DO resolve directly against @v/<query>.info, a comparison query is never
// sent to the proxy as a literal per-version query at all — `go get
// golang.org/x/mod@<v0.20.0` resolves it locally from @v/list (to v0.19.0,
// the highest listed version below v0.20.0) and never issues any request
// for a "<v0.20.0"-shaped path. Querying the literal string directly
// against the proxy — what this tool used to do — 404s with
// proxy.golang.org's real body "bad request: invalid escaped version
// \"<v0.20.0\": invalid char '<'" (confirmed live), which matched none of
// diagnose's specific error markers and fell through to the generic
// not-yet-indexed fallback: "retry in a minute, or use --wait" for a query
// that could never succeed as a literal string no matter how long it was
// retried.
//
// This fake server 404s the literal "<v0.20.0" per-version path with that
// exact real body (so the test fails loudly, via a wrong diagnosis, if the
// fix regresses and probe() ever falls back to probing it literally again)
// and only serves .info/sum for the correctly resolved v0.19.0.
func TestProbe_ComparisonQueryResolvesViaList(t *testing.T) {
	const badEscapeBody = `bad request: invalid escaped version "<v0.20.0": invalid char '<'`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.21.0"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v0.19.0\nv0.20.0\nv0.21.0\n"))
		case strings.HasSuffix(r.URL.Path, "/@v/v0.19.0.info"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.19.0"}`))
		case strings.Contains(r.URL.Path, "/lookup/") && strings.HasSuffix(r.URL.Path, "@v0.19.0"):
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "<v0.20.0"):
			// The literal, unresolved comparison query — real
			// proxy.golang.org rejects this outright; it should never be
			// requested once comparison queries are resolved up front.
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(badEscapeBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ep := endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}

	r := ep.probe("example.com/mod", "<v0.20.0")
	if r.resolvedVersion != "v0.19.0" {
		t.Errorf("resolvedVersion = %q, want %q", r.resolvedVersion, "v0.19.0")
	}
	if strings.Contains(r.versionInfo.body, "invalid escaped version") {
		t.Errorf("versionInfo.body = %q, want the resolved v0.19.0 info, not the doomed literal-query error", r.versionInfo.body)
	}
	if !r.ready() {
		t.Errorf("ready() = false, want true (versionInfo.ok=%v sum.ok=%v)", r.versionInfo.ok, r.sum.ok)
	}
	if d := diagnose(r); d.status != statusReady {
		t.Errorf("diagnose(r).status = %q, want %q (message: %s)", d.status, statusReady, d.message)
	}
}

// TestProbe_ComparisonQueryNoMatchSkipsLiteralProbe is a regression test for
// a real bug found by testing goproxycheck against a comparison version
// query with a bound no published version satisfies at all — e.g. "<v0.5.0"
// for a module whose lowest published version is v1.0.0. Confirmed live
// (2026-09-29) that `go get -x golang.org/x/mod@<v0.0.1` (golang.org/x/mod's
// lowest published version is well above v0.0.1) issues only @v/list
// requests and then fails immediately with `no matching versions for query
// "<v0.0.1"` — it never issues any request for a "<v0.0.1"-shaped path.
//
// Before this fix, probe() fell back to sending the literal, never-
// satisfiable comparison string to the proxy exactly like the doomed-request
// bug TestProbe_ComparisonQueryResolvesViaList already fixed for the
// *resolvable* case just above — this fake server fails the test if that
// literal path is ever requested, or if @v/list is polled more than once.
func TestProbe_ComparisonQueryNoMatchSkipsLiteralProbe(t *testing.T) {
	var listHits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v1.5.0"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			listHits++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v1.0.0\nv1.5.0\n"))
		case strings.HasSuffix(r.URL.Path, "/@v/v1.5.0.mod"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("module example.com/mod\n"))
		default:
			// The literal, never-satisfiable comparison query ("<v0.5.0") —
			// real cmd/go never requests any path shaped like this; it fails
			// locally from @v/list alone. If probe() still fell back to
			// probing it, this 404 (real proxy.golang.org's actual body for
			// an un-percent-encoded '<') would land here.
			t.Errorf("unexpected request for %s — the doomed literal comparison query should never reach the proxy", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`bad request: invalid escaped version "<v0.5.0": invalid char '<'`))
		}
	}))
	defer srv.Close()
	ep := endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}

	r := ep.probe("example.com/mod", "<v0.5.0")
	if !r.comparisonQueryNoMatch {
		t.Errorf("comparisonQueryNoMatch = false, want true (no listed version satisfies \"<v0.5.0\")")
	}
	if r.versionInfo.ok || r.versionInfo.statusCode != 0 {
		t.Errorf("versionInfo = %+v, want a zero value — no per-version probe should have been made", r.versionInfo)
	}
	if r.sum.ok || r.sum.statusCode != 0 {
		t.Errorf("sum = %+v, want a zero value — no sum lookup should have been made", r.sum)
	}
	if d := diagnose(r); d.status != statusNoMatchingVersion {
		t.Errorf("diagnose(r).status = %q, want %q (message: %s)", d.status, statusNoMatchingVersion, d.message)
	}
	if listHits != 1 {
		t.Errorf("@v/list was requested %d times, want exactly 1", listHits)
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
