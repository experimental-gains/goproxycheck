package main

import (
	"bytes"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestModuleFromGoMod(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	_ = os.WriteFile(path, []byte("module github.com/experimental-gains/goproxycheck\n\ngo 1.24\n"), 0o644)

	got, err := moduleFromGoMod(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "github.com/experimental-gains/goproxycheck"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestModuleFromGoMod_TrailingComment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	_ = os.WriteFile(path, []byte("module github.com/foo/bar // the main module\n\ngo 1.24\n"), 0o644)

	got, err := moduleFromGoMod(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "github.com/foo/bar"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestModuleFromGoMod_Quoted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	_ = os.WriteFile(path, []byte(`module "github.com/foo/bar"`+"\n\ngo 1.24\n"), 0o644)

	got, err := moduleFromGoMod(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "github.com/foo/bar"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestModuleFromGoMod_TabSeparator(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	// go.mod's lexer treats any whitespace as a token separator, so a tab
	// between "module" and the path is valid — `go list -m` parses it fine —
	// even though gofmt always normalizes to a single space.
	_ = os.WriteFile(path, []byte("module\tgithub.com/foo/bar\n\ngo 1.24\n"), 0o644)

	got, err := moduleFromGoMod(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "github.com/foo/bar"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestModuleFromGoMod_CommentOnlyValue is a regression test for a real
// bug found via mutation testing (run #125): a "module" line whose
// entire value is a "//" comment (e.g. "module //oops", no actual path
// before it) used to parse to an empty string and return (nil, "") —
// silently succeeding instead of erroring, which would have sent a
// probe request for an empty module path downstream.
func TestModuleFromGoMod_CommentOnlyValue(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	_ = os.WriteFile(path, []byte("module //oops, no path here\n\ngo 1.24\n"), 0o644)

	got, err := moduleFromGoMod(path)
	if err == nil {
		t.Fatalf("expected an error for a comment-only module line, got module %q with no error", got)
	}
}

func TestModuleFromGoMod_Missing(t *testing.T) {
	if _, err := moduleFromGoMod(filepath.Join(t.TempDir(), "go.mod")); err == nil {
		t.Fatal("expected an error for a missing go.mod")
	}
}

func TestResolveTarget_ExplicitArg(t *testing.T) {
	module, version, err := resolveTarget([]string{"example.com/mod@v1.2.3"})
	if err != nil {
		t.Fatal(err)
	}
	if module != "example.com/mod" || version != "v1.2.3" {
		t.Errorf("got (%q, %q)", module, version)
	}
}

func TestResolveTarget_BadArg(t *testing.T) {
	if _, _, err := resolveTarget([]string{"no-at-sign"}); err == nil {
		t.Fatal("expected an error for an argument without '@'")
	}
}

func TestResolveTarget_TooManyArgs(t *testing.T) {
	if _, _, err := resolveTarget([]string{"a@1", "b@2"}); err == nil {
		t.Fatal("expected an error for more than one argument")
	}
}

// TestResolveTarget_WhitespaceInVersion is a regression test for a real bug:
// a version query with a leading, trailing, or embedded space (e.g. picked
// up from copy-paste, a CI variable, or a file read with the newline only
// partly stripped) used to sail through unrejected and get diagnosed as
// statusNotYetIndexed ("retry in a minute, or use --wait") — misleading,
// since no real tag or branch can ever contain whitespace (git disallows it
// in ref names), so waiting could never make it "become indexed." Confirmed
// live against the real proxy: cmd/go itself doesn't reject this input
// up front either (unlike a literal newline, ":", or "?"), it percent-encodes
// the space and gets a 404 like any other nonexistent version.
func TestResolveTarget_WhitespaceInVersion(t *testing.T) {
	for _, version := range []string{"v1.2.3 ", " v1.2.3", "v1.2\t.3", "v1.2.3\nv1.2.4"} {
		t.Run(version, func(t *testing.T) {
			if _, _, err := resolveTarget([]string{"example.com/mod@" + version}); err == nil {
				t.Fatalf("expected an error for version %q containing whitespace", version)
			}
		})
	}
}

func TestResolveTarget_FallbackToGoModAndGitTag(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_ = os.WriteFile("go.mod", []byte("module example.com/fallback\n\ngo 1.24\n"), 0o644)
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run("git", "init", "-q")
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "test")
	run("git", "add", "go.mod")
	run("git", "commit", "-q", "-m", "init")
	run("git", "tag", "v0.9.0")

	module, version, err := resolveTarget(nil)
	if err != nil {
		t.Fatal(err)
	}
	if module != "example.com/fallback" || version != "v0.9.0" {
		t.Errorf("got (%q, %q)", module, version)
	}
}

func TestResolveTarget_NoGoMod(t *testing.T) {
	t.Chdir(t.TempDir())
	if _, _, err := resolveTarget(nil); err == nil {
		t.Fatal("expected an error with no go.mod present")
	}
}

// TestResolveTarget_MultipleTagsAtHead_PicksTheVersionTag is a regression
// test for a real bug: `git describe --tags --exact-match HEAD` silently
// picks just one of several tags pointing at the same commit, by an
// internal tie-break unrelated to which one is the actual semver release —
// confirmed live that it returned a non-version marker tag ("ci-verified")
// ahead of the real "v1.6.0" release tag on the same commit. When exactly
// one of the tags at HEAD is a valid module version, that's the
// unambiguous right answer and should be picked without requiring the
// caller to disambiguate explicitly.
func TestResolveTarget_MultipleTagsAtHead_PicksTheVersionTag(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_ = os.WriteFile("go.mod", []byte("module example.com/fallback\n\ngo 1.24\n"), 0o644)
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run("git", "init", "-q")
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "test")
	run("git", "add", "go.mod")
	run("git", "commit", "-q", "-m", "init")
	// A non-version marker tag alongside the real release tag, both on the
	// same commit — the realistic shape (CI/release automation adding a
	// "latest"/"stable"/"ci-verified" marker tag, or a leftover re-tag).
	run("git", "tag", "ci-verified")
	run("git", "tag", "v1.6.0")

	module, version, err := resolveTarget(nil)
	if err != nil {
		t.Fatal(err)
	}
	if module != "example.com/fallback" || version != "v1.6.0" {
		t.Errorf("got (%q, %q), want (%q, %q) — picked the marker tag instead of the release tag", module, version, "example.com/fallback", "v1.6.0")
	}
}

// TestResolveTarget_MultipleVersionTagsAtHead_Ambiguous covers the case
// where more than one tag at HEAD looks like a real module version (e.g. a
// mistaken re-tag on the same commit) — there's no way to know which one
// the caller meant, so this should return a clear error instead of
// silently guessing one, same as the no-tag-at-all case already does.
func TestResolveTarget_MultipleVersionTagsAtHead_Ambiguous(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	_ = os.WriteFile("go.mod", []byte("module example.com/fallback\n\ngo 1.24\n"), 0o644)
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run("git", "init", "-q")
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "test")
	run("git", "add", "go.mod")
	run("git", "commit", "-q", "-m", "init")
	run("git", "tag", "v1.6.0")
	run("git", "tag", "v1.6.1")

	if _, _, err := resolveTarget(nil); err == nil {
		t.Fatal("expected an error when more than one version-shaped tag points at HEAD")
	}
}

// TestLocalGoproxyOff covers localGoproxyOff's parsing of `go env GOPROXY`
// output, including the comma/pipe list case: confirmed live against the
// real `go` command that "off" only disables lookup when it's the *first*
// entry (GOPROXY=off,direct and off|direct both fail with "module lookup
// disabled by GOPROXY=off"; GOPROXY=direct,off succeeds via direct and
// never reaches the off entry). t.Setenv is safe here — exec.Command
// inherits the process environment, and `go env` reads the env var over
// any GOENV-persisted value.
func TestLocalGoproxyOff(t *testing.T) {
	cases := []struct {
		name  string
		proxy string
		want  bool
	}{
		{"off", "off", true},
		{"off then direct, comma", "off,direct", true},
		{"off then direct, pipe", "off|direct", true},
		{"direct then off", "direct,off", false},
		{"default", "https://proxy.golang.org,direct", false},
		{"direct only", "direct", false},
		// Empty entries (stray/leading separators, e.g. from
		// `GOPROXY="$UNSET_VAR,off"`) don't count as an entry — verified
		// live that real `go` skips them and evaluates the first
		// *non-empty* entry, not the blank ahead of it.
		{"leading empty comma", ",off", true},
		{"leading empty pipe", "|off", true},
		{"leading empty then direct", ",direct", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("GOPROXY", c.proxy)
			if got := localGoproxyOff(); got != c.want {
				t.Errorf("localGoproxyOff() with GOPROXY=%q = %v, want %v", c.proxy, got, c.want)
			}
		})
	}
}

// TestLocalGoproxyNonPublic covers localGoproxyNonPublic's classification
// of `go env GOPROXY` output — the fix for a real bug found via
// live-toolchain differential testing: goproxycheck unconditionally probed
// the hardcoded public proxy.golang.org regardless of the local `go`
// command's actual effective GOPROXY, so a "direct" or custom-proxy
// environment (private mirrors like Athens/Artifactory/goproxy.cn are all
// in real, common use) got a diagnosis based on a proxy `go install` never
// even talks to.
func TestLocalGoproxyNonPublic(t *testing.T) {
	cases := []struct {
		name      string
		proxy     string
		wantKind  string
		wantValue string
	}{
		{"default", "https://proxy.golang.org,direct", "", ""},
		{"default no fallback", "https://proxy.golang.org", "", ""},
		{"default trailing slash", "https://proxy.golang.org/", "", ""},
		{"off", "off", "", ""}, // handled separately by localGoproxyOff
		{"direct only", "direct", "direct", ""},
		{"direct then public", "direct,https://proxy.golang.org", "direct", ""},
		{"custom then direct", "https://goproxy.example.com,direct", "custom", "https://goproxy.example.com"},
		{"custom pipe direct", "https://goproxy.example.com|direct", "custom", "https://goproxy.example.com"},
		{"file proxy", "file:///tmp/fileproxy,off", "custom", "file:///tmp/fileproxy"},
		{"empty", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("GOPROXY", c.proxy)
			gotKind, gotValue := localGoproxyNonPublic()
			if gotKind != c.wantKind || gotValue != c.wantValue {
				t.Errorf("localGoproxyNonPublic() with GOPROXY=%q = (%q, %q), want (%q, %q)", c.proxy, gotKind, gotValue, c.wantKind, c.wantValue)
			}
		})
	}
}

// TestParseGoproxyChain covers parseGoproxyChain's replication of cmd/go's
// own GOPROXY-list walk (cmd/go/internal/modfetch/proxy.go's proxyList,
// verified directly against that source): "," and "|" both separate
// entries (with "|" recorded so callers can tell it means "fall back on
// any error," not just 404/410), and both "off" and "direct" terminate the
// walk outright — an entry placed after either is never actually reachable
// so must not appear in the returned chain.
func TestParseGoproxyChain(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantEntries []string
		wantSeps    string // one byte per separator, as a string for readability
	}{
		{"empty", "", nil, ""},
		{"single", "https://proxy.golang.org", []string{"https://proxy.golang.org"}, ""},
		{"comma chain", "https://a.example,https://b.example", []string{"https://a.example", "https://b.example"}, ","},
		{"pipe chain", "https://a.example|https://b.example", []string{"https://a.example", "https://b.example"}, "|"},
		{"mixed", "https://a.example,https://b.example|https://c.example", []string{"https://a.example", "https://b.example", "https://c.example"}, ",|"},
		{"off truncates", "https://a.example,off,https://b.example", []string{"https://a.example", "off"}, ","},
		{"direct truncates", "https://a.example,direct,https://b.example", []string{"https://a.example", "direct"}, ","},
		{"off first", "off,direct", []string{"off"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			entries, seps := parseGoproxyChain(c.raw)
			if strings.Join(entries, ",") != strings.Join(c.wantEntries, ",") {
				t.Errorf("entries = %v, want %v", entries, c.wantEntries)
			}
			if string(seps) != c.wantSeps {
				t.Errorf("seps = %q, want %q", seps, c.wantSeps)
			}
		})
	}
}

// TestPublicProxyFallback covers publicProxyFallback's detection of the
// documented go.dev/ref/mod#goproxy-protocol fallback-chain pattern
// (GOPROXY=https://corp.example.com,https://proxy.golang.org — that page's
// own worked example): a real bug fix. Before this existed, a GOPROXY chain
// like this made goproxycheck bail out claiming it had "no way to know...
// can't tell you whether it's ready there," even though `go mod download`
// itself falls straight through to the public proxy this tool actually
// probes whenever the earlier custom entry 404s/410s — confirmed live
// against the real go toolchain (a stand-in proxy that 404s everything,
// then real proxy.golang.org, succeeded end to end for an ordinary public
// module).
func TestPublicProxyFallback(t *testing.T) {
	cases := []struct {
		name              string
		proxy             string
		wantPrecedingJoin string // "" means wantOK == false
		wantAnyError      bool
		wantOK            bool
	}{
		{"public first, default", "https://proxy.golang.org,direct", "", false, false},
		{"public only", "https://proxy.golang.org", "", false, false},
		{"custom then public, comma", "https://corp.example.com,https://proxy.golang.org", "https://corp.example.com", false, true},
		{"custom then public, pipe", "https://corp.example.com|https://proxy.golang.org", "https://corp.example.com", true, true},
		{"two custom then public", "https://a.example,https://b.example,https://proxy.golang.org", "https://a.example,https://b.example", false, true},
		{"custom only, no public in chain", "https://corp.example.com,direct", "", false, false},
		{"custom then off, public never reached", "https://corp.example.com,off", "", false, false},
		{"custom then direct then public, public unreachable", "https://corp.example.com,direct,https://proxy.golang.org", "", false, false},
		{"trailing slash public", "https://corp.example.com,https://proxy.golang.org/", "https://corp.example.com", false, true},
		{"empty", "", "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("GOPROXY", c.proxy)
			preceding, anyErr, ok := publicProxyFallback()
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v (preceding=%v anyErr=%v)", ok, c.wantOK, preceding, anyErr)
			}
			if !ok {
				return
			}
			if got := strings.Join(preceding, ","); got != c.wantPrecedingJoin {
				t.Errorf("preceding = %q, want %q", got, c.wantPrecedingJoin)
			}
			if anyErr != c.wantAnyError {
				t.Errorf("anyErrorFallback = %v, want %v", anyErr, c.wantAnyError)
			}
		})
	}
}

// TestRun_GoproxyFallbackChain is an end-to-end regression test for the
// same bug TestPublicProxyFallback covers at the run() level: with a
// GOPROXY chain that reaches the public proxy only after a custom entry,
// the tool must still perform (and report) the real probe against the
// public proxy — via the injected fake endpoints, standing in for
// proxy.golang.org — rather than bailing out as if it were an opaque
// custom proxy it can't check at all.
func TestRun_GoproxyFallbackChain(t *testing.T) {
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

	// The detection side (publicProxyFallback) reads the literal local
	// `go env GOPROXY`, independent of the fake endpoints above standing in
	// for the actual probe target — same decoupling the pre-existing
	// "custom"/"direct" tests rely on.
	t.Setenv("GOPROXY", "https://corp.example.com,https://proxy.golang.org")

	var stdout, stderr bytes.Buffer
	code := run([]string{"example.com/mod@v0.1.0"}, &stdout, &stderr, ep)
	if code != 0 {
		t.Fatalf("run() = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "ready") {
		t.Errorf("expected a ready status, got:\n%s", out)
	}
	if !strings.Contains(out, "https://corp.example.com") {
		t.Errorf("expected the caveat to name the preceding custom entry, got:\n%s", out)
	}
	if !strings.Contains(out, "404/410") {
		t.Errorf("expected the caveat to explain the comma-separated 404/410 fallback trigger, got:\n%s", out)
	}
	if strings.Contains(out, "can't tell you") {
		t.Errorf("expected the real probe result, not the old unconditional bail-out message, got:\n%s", out)
	}
}

// TestLocalModulePrivate covers localModulePrivate's parsing of `go env
// GONOPROXY` output — including the GOPRIVATE-fallback case (GONOPROXY
// unset, GOPRIVATE set: `go env GONOPROXY` already resolves to GOPRIVATE's
// value, confirmed live) and an explicit GONOPROXY override taking
// precedence over an unrelated GOPRIVATE.
func TestLocalModulePrivate(t *testing.T) {
	cases := []struct {
		name             string
		gonoproxy        string
		goprivate        string
		module           string
		wantMatch        bool
		wantPatternIsSet bool
	}{
		{"no config", "", "", "github.com/myorg/foo", false, false},
		{"GOPRIVATE fallback, match", "", "github.com/myorg/*", "github.com/myorg/foo", true, true},
		{"GOPRIVATE fallback, no match", "", "github.com/myorg/*", "github.com/otherorg/foo", false, false},
		{"explicit GONOPROXY, match", "github.com/myorg/*", "", "github.com/myorg/foo", true, true},
		{"explicit GONOPROXY overrides unrelated GOPRIVATE", "github.com/myorg/*", "example.com/other", "github.com/myorg/foo", true, true},
		{"multiple patterns", "example.com/a,github.com/myorg/*", "", "github.com/myorg/foo", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("GONOPROXY", c.gonoproxy)
			t.Setenv("GOPRIVATE", c.goprivate)
			gotMatch, gotPattern := localModulePrivate(c.module)
			if gotMatch != c.wantMatch {
				t.Errorf("localModulePrivate(%q) match = %v, want %v", c.module, gotMatch, c.wantMatch)
			}
			if (gotPattern != "") != c.wantPatternIsSet {
				t.Errorf("localModulePrivate(%q) pattern = %q, want non-empty=%v", c.module, gotPattern, c.wantPatternIsSet)
			}
		})
	}
}

// TestDefaultFlagDurations pins the exact --timeout/--interval flag
// defaults (found LIVED by mutation testing, run #127: every --wait
// test explicitly overrides both flags, nothing exercised what happens
// if a user passes --wait alone). Doesn't actually run a 5-minute poll
// — an invalid flag makes fs.Parse fail before that, and flag's own
// Usage/PrintDefaults renders the literal default values it was
// constructed with into the error output, which is enough to pin them
// without waiting.
func TestDefaultFlagDurations(t *testing.T) {
	var stdout, stderr bytes.Buffer
	run([]string{"--this-flag-does-not-exist"}, &stdout, &stderr, defaultEndpoints())
	out := stderr.String()
	if !strings.Contains(out, "default 5m0s") {
		t.Errorf("expected the --timeout default (5m0s) in usage output, got:\n%s", out)
	}
	if !strings.Contains(out, "default 15s") {
		t.Errorf("expected the --interval default (15s) in usage output, got:\n%s", out)
	}
}

// TestSumdbName pins sumdbName's field-splitting and the
// "sum.golang.google.cn" alias special case against real cmd/go behavior
// (confirmed against modfetch/sumdb.go's dbDial: it rewrites that literal
// alias to "sum.golang.org https://sum.golang.google.cn" before parsing,
// since it's a China-reachable mirror of the same public tree, not a
// different database).
func TestSumdbName(t *testing.T) {
	cases := []struct {
		gosumdb string
		want    string
	}{
		{"", "sum.golang.org"},
		{"sum.golang.org", "sum.golang.org"},
		{"sum.golang.google.cn", "sum.golang.org"},
		{"sum.golang.org+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8", "sum.golang.org"},
		{"mycompany.example+abc123 https://sumdb.mycompany.example", "mycompany.example"},
		{"mycompany.example", "mycompany.example"},
	}
	for _, c := range cases {
		if got := sumdbName(c.gosumdb); got != c.want {
			t.Errorf("sumdbName(%q) = %q, want %q", c.gosumdb, got, c.want)
		}
	}
}
