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

// TestProbe_HashInVersionReachesFullPath is a regression test for a real bug
// found by testing goproxycheck's URL construction against a version string
// containing '#' — a character golang.org/x/mod/module.EscapeVersion (the
// same offline check resolveTarget already uses to reject a disallowed
// version string like '?', see TestResolveTarget's disallowed-character
// cases) explicitly allows: EscapeVersion validates against fileNameOK,
// whose documented allowed-punctuation set includes '#'. And it's realistic
// input, not just a technicality: real git accepts a tag/branch name
// containing it (confirmed live with `git check-ref-format --branch
// release#123`, a plausible way to fold an issue/PR number into a branch
// name), so a revision-identifier version query shaped like this sails past
// every existing check in resolveTarget and reaches probe()'s URL
// construction completely unescaped.
//
// Before this fix, probe() built every proxy/sumdb URL as a plain
// fmt.Sprintf'd string handed to (*http.Client).Get, which parses it with
// url.Parse — and url.Parse treats an unescaped '#' as the start of a URL
// fragment, silently discarding it and everything after it before the
// request is ever sent. Confirmed live: building the URL
// ".../example.com/mod/@v/v1.2.3#issue456.info" this way and issuing it
// actually reached the server as a GET for only
// ".../example.com/mod/@v/v1.2.3" — not even the ".info" suffix survived.
// This fake server only answers the correctly-escaped, full path (decoded
// server-side back to ".../@v/v1.2.3#issue456.info", exactly what a real
// `go get` would request per cmd/go's own pathEscape) with a real 200 — any
// other path, including the truncated one this tool sent pre-fix, gets a
// 404, so this test fails pre-fix and passes post-fix.
func TestProbe_HashInVersionReachesFullPath(t *testing.T) {
	const version = "v1.2.3#issue456"
	wantInfoPath := "/example.com/mod/@v/v1.2.3#issue456.info"
	wantSumPath := "/lookup/example.com/mod@v1.2.3#issue456"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case wantInfoPath:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v1.2.3#issue456"}`))
		case wantSumPath:
			w.WriteHeader(http.StatusOK)
		default:
			// Includes @latest, @v/list, and (pre-fix) the truncated
			// ".../@v/v1.2.3" path url.Parse actually sent.
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ep := endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}

	r := ep.probe("example.com/mod", version)
	if !r.versionInfo.ok {
		t.Errorf("versionInfo.ok = false, want true — the request should have reached %s (got body %q)", wantInfoPath, r.versionInfo.body)
	}
	if !r.sum.ok {
		t.Errorf("sum.ok = false, want true — the sum lookup should have reached %s", wantSumPath)
	}
	if !r.ready() {
		t.Errorf("ready() = false, want true")
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
		got, ok := resolveComparisonQuery(c.op, c.operand, listed, nil)
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
		got, ok := resolveComparisonQuery(c.op, c.operand, listed, nil)
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

// TestResolveComparisonQuery_SkipsRetractedVersions is the fix for a real
// bug found by testing goproxycheck's comparison-query resolution against
// github.com/jayconrod/retract, the Go team's own canonical self-retraction
// example (already used by TestProbe_LatestModFileScopedPastSelfRetractingLatest
// above): v1.0.0 and v1.0.1 are both retracted, leaving only v0.9.9
// unretracted. resolveComparisonQuery used to pick purely from @v/list with
// no notion of retraction at all — see this function's own doc comment and
// its caller in probe() for the full live verification against a real `go
// get`, which never automatically selects a retracted version for any
// comparison query, exactly like it never does for "latest"/"upgrade".
func TestResolveComparisonQuery_SkipsRetractedVersions(t *testing.T) {
	listed := []string{"v0.9.9", "v1.0.0", "v1.0.1"}
	retracted := func(v string) bool { return v == "v1.0.0" || v == "v1.0.1" }
	cases := []struct {
		op, operand string
		want        string
		wantOK      bool
	}{
		// v1.0.1 is the highest listed version satisfying either bound by
		// raw semver, but it's retracted (and so is v1.0.0) — v0.9.9 is the
		// only real candidate, matching real `go get`'s resolution to it.
		{"<", "v2.0.0", "v0.9.9", true},
		{"<=", "v1.0.1", "v0.9.9", true},
		// Every version satisfying this bound (v1.0.0, v1.0.1) is
		// retracted — real `go get ...@>=v1.0.0` fails outright with "no
		// matching versions for query", not a silent pick of a retracted
		// version.
		{">=", "v1.0.0", "", false},
	}
	for _, c := range cases {
		got, ok := resolveComparisonQuery(c.op, c.operand, listed, retracted)
		if got != c.want || ok != c.wantOK {
			t.Errorf("resolveComparisonQuery(%q, %q, listed, retracted) = (%q, %v), want (%q, %v)", c.op, c.operand, got, ok, c.want, c.wantOK)
		}
	}

	// A nil predicate must behave exactly like the pre-fix code (no
	// filtering at all) — every existing caller that has no retract data
	// available (e.g. latestModFile itself failed to fetch) relies on this.
	if got, ok := resolveComparisonQuery("<", "v2.0.0", listed, nil); got != "v1.0.1" || !ok {
		t.Errorf("resolveComparisonQuery with a nil predicate = (%q, %v), want (%q, %v) (no filtering)", got, ok, "v1.0.1", true)
	}
}

// TestProbe_ComparisonQuerySkipsRetractedVersion is the end-to-end version
// of TestResolveComparisonQuery_SkipsRetractedVersions: reproduces
// github.com/jayconrod/retract's exact shape (v1.0.0 and v1.0.1 both
// retracted, only v0.9.9 isn't) through the full probe()/diagnose() path,
// confirming the resolved version — and therefore the reported status — is
// the real, installable v0.9.9, not the retracted v1.0.1 a raw semver.Compare
// walk would have picked. Confirmed live (2026-09-30): `go get
// github.com/jayconrod/retract@<v2.0.0` resolves to v0.9.9 (`go: added
// github.com/jayconrod/retract v0.9.9`), never v1.0.1.
func TestProbe_ComparisonQuerySkipsRetractedVersion(t *testing.T) {
	const module = "github.com/jayconrod/retract"
	const retractModFile = "module " + module + "\n\ngo 1.16\n\nretract (\n\tv1.0.0 // Published accidentally.\n\tv1.0.1 // For retractions only.\n)\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.9.9","Time":"2021-01-26T16:46:49Z"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v1.0.0\nv0.9.9\nv1.0.1\n"))
		case strings.HasSuffix(r.URL.Path, "/@v/v1.0.1.mod"):
			// The highest tag in the major line — probe()'s latestModFile
			// fetch, same as TestProbe_LatestModFileScopedPastSelfRetractingLatest.
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(retractModFile))
		case strings.HasSuffix(r.URL.Path, "/@v/v0.9.9.info"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.9.9"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/v0.9.9.mod"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("module " + module + "\n\ngo 1.16\n"))
		case strings.Contains(r.URL.Path, "/lookup/") && strings.HasSuffix(r.URL.Path, "@v0.9.9"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/@v/v1.0.1.info"), strings.HasSuffix(r.URL.Path, "/@v/v1.0.0.info"):
			// The two retracted versions must never be probed as the
			// resolved checkVersion — if resolveComparisonQuery regresses to
			// picking one of them, this test should fail loudly via a wrong
			// resolvedVersion/status rather than silently succeeding against
			// the wrong version's .info.
			t.Errorf("unexpected request for a retracted version's .info: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ep := endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}

	r := ep.probe(module, "<v2.0.0")
	if r.resolvedVersion != "v0.9.9" {
		t.Errorf("resolvedVersion = %q, want %q (the only unretracted candidate — real `go get` resolves here, never to the retracted v1.0.1)", r.resolvedVersion, "v0.9.9")
	}
	if d := diagnose(r); d.status != statusReady {
		t.Errorf("diagnose(r).status = %q, want %q (message: %s)", d.status, statusReady, d.message)
	}
}

// TestProbe_ComparisonQueryAllCandidatesRetracted covers the other real
// `go get` outcome confirmed live for github.com/jayconrod/retract: a bound
// where every listed version satisfying it is retracted (only v1.0.0 and
// v1.0.1 satisfy ">=v1.0.0", and both are retracted) fails immediately with
// "no matching versions for query ">=v1.0.0"" — the same offline-provable
// failure as a bound nothing at all satisfies, not a silent pick of a
// retracted version.
func TestProbe_ComparisonQueryAllCandidatesRetracted(t *testing.T) {
	const module = "github.com/jayconrod/retract"
	const retractModFile = "module " + module + "\n\ngo 1.16\n\nretract (\n\tv1.0.0 // Published accidentally.\n\tv1.0.1 // For retractions only.\n)\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v0.9.9","Time":"2021-01-26T16:46:49Z"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v1.0.0\nv0.9.9\nv1.0.1\n"))
		case strings.HasSuffix(r.URL.Path, "/@v/v1.0.1.mod"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(retractModFile))
		default:
			t.Errorf("unexpected request for %s — nothing satisfying \">=v1.0.0\" should ever be probed once every candidate is known to be retracted", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ep := endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}

	r := ep.probe(module, ">=v1.0.0")
	if !r.comparisonQueryNoMatch {
		t.Errorf("comparisonQueryNoMatch = false, want true (every version satisfying \">=v1.0.0\" is retracted)")
	}
	if d := diagnose(r); d.status != statusNoMatchingVersion {
		t.Errorf("diagnose(r).status = %q, want %q (message: %s)", d.status, statusNoMatchingVersion, d.message)
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

// TestProbe_LatestModFilePrefersReleaseOverHigherPrerelease reproduces a
// real, live shape on google.golang.org/grpc (2026-09-29): @latest correctly
// returns v1.84.0 (the actual latest release), but @v/list also carries
// v1.84.0-dev/v1.85.0-dev/v1.86.0-dev — pre-release "next minor" marker tags
// that all raw-semver-compare *higher* than v1.84.0 despite having no
// release behind them yet (confirmed live: `curl
// https://proxy.golang.org/google.golang.org/grpc/@v/list` lists all four
// alongside v1.84.0, and `.../@latest` still names v1.84.0).
//
// probe()'s latestModFile search exists to walk past a self-retracting
// @latest to the module's real highest tag (see
// TestProbe_LatestModFileScopedPastSelfRetractingLatest), but before this
// fix it did that with a plain semver.Compare walk over every listed
// version, release or prerelease alike — so on a module shaped like this,
// it would walk right past the correct v1.84.0 target to the higher-raw-
// semver v1.86.0-dev prerelease tag instead. That's the same
// release-preferred-over-prerelease rule already fixed three times in this
// tool family for version-query resolution (resolveComparisonQuery here,
// modslop's Lookup and its own resolveComparisonQuery) — go.dev/ref/mod's
// own "latest" query semantics say "the latest available, allowed tagged
// version, with non-prereleases preferred over prereleases" even when
// retraction is deliberately ignored (cmd/go/internal/modload/query.go's
// queryLatestVersionIgnoringRetractions still resolves via the ordinary
// "latest" query, which always prefers any release over any prerelease
// through filterVersions/lookup — confirmed reading that source directly),
// so fetching the prerelease tag's go.mod instead of the release's is a
// real divergence, not just an internal implementation detail: a retract or
// deprecation notice present only in the true latest release's go.mod (and
// not yet copied into the interim dev tag, or vice versa) would be read
// from the wrong version entirely.
func TestProbe_LatestModFilePrefersReleaseOverHigherPrerelease(t *testing.T) {
	const module = "google.golang.org/grpc"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/@latest"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"Version":"v1.84.0"}`))
		case strings.HasSuffix(r.URL.Path, "/@v/list"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("v1.84.0\nv1.84.0-dev\nv1.85.0-dev\nv1.86.0-dev\n"))
		case strings.HasSuffix(r.URL.Path, "/@v/v1.84.0.mod"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("module " + module + "\n\ngo 1.25\n\n// REALRELEASE\n"))
		case strings.HasSuffix(r.URL.Path, "/@v/v1.86.0-dev.mod"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("module " + module + "\n\ngo 1.25\n\n// WRONGPRERELEASE\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ep := endpoints{proxyBase: srv.URL, sumBase: srv.URL, client: srv.Client()}

	r := ep.probe(module, "v1.84.0")
	if !r.latestModFile.ok {
		t.Fatalf("latestModFile.ok = false, want true")
	}
	if !strings.Contains(r.latestModFile.body, "REALRELEASE") || strings.Contains(r.latestModFile.body, "WRONGPRERELEASE") {
		t.Errorf("latestModFile.body = %q, want v1.84.0's go.mod (the highest *release* tag, matching real cmd/go's own \"latest\" query semantics), not the higher-raw-semver v1.86.0-dev prerelease tag's go.mod", r.latestModFile.body)
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
