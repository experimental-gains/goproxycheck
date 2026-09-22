package main

import (
	"bytes"
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
