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

// TestModuleFromGoMod_ParenBlock is a regression test for a real bug: the
// `module` directive's parenthesized block form ("module (\n\tpath\n)")
// isn't shown in go.dev/ref/mod#go-mod-file-module's prose, but real
// golang.org/x/mod/modfile accepts it exactly like require/replace/tool/
// exclude's own block forms — confirmed live (2026-09-27) that a go.mod
// written this way builds, `go list -m` reports the correct module path,
// and `go mod tidy` rewrites it to the single-line form. Before this
// fix, moduleDirective's single-line branch matched the "module (" line
// with rest "(" (not a valid quoted string), returning the bogus module
// path "(" and silently dropping the block's real path line — sending
// goproxycheck's no-argument mode probing a nonsense module path instead
// of the real one.
func TestModuleFromGoMod_ParenBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	_ = os.WriteFile(path, []byte("module (\n\tgithub.com/foo/bar\n)\n\ngo 1.24\n"), 0o644)

	got, err := moduleFromGoMod(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "github.com/foo/bar"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestModuleFromGoMod_ParenBlock_NoSpaceBeforeParen is a regression test for
// a real bug: "module(" (no space before the opening paren) used to match
// neither the block-open check nor the single-line path check in
// moduleDirective (both required the very next byte after "module" to be a
// space or tab), so this line fell through unmatched entirely, the block's
// real path line was never reached, and moduleFromGoMod reported "has no
// 'module' directive" — confirmed live (2026-09-28) that this is real,
// accepted go.mod syntax: golang.org/x/mod/modfile.Parse/ParseLax both
// resolve it to the same module path as the spaced form, and a real `go
// list -m`/`go mod verify` against a go.mod written exactly this way
// succeed and report the correct module path. Go's lexer tokenizes "module"
// and "(" independently of whitespace, the same way it does for every
// other verb/block-open pair.
func TestModuleFromGoMod_ParenBlock_NoSpaceBeforeParen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	_ = os.WriteFile(path, []byte("module(\n\tgithub.com/foo/bar\n)\n\ngo 1.24\n"), 0o644)

	got, err := moduleFromGoMod(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "github.com/foo/bar"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestModuleFromGoMod_ParenBlock_QuotedAndComment covers the same block
// form with a quoted path and a trailing comment inside the block, and a
// comment on the closing paren — both valid go.mod syntax for the
// single-line form already (see TestModuleFromGoMod_Quoted/
// TestModuleFromGoMod_TrailingComment), now checked inside the block too.
func TestModuleFromGoMod_ParenBlock_QuotedAndComment(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	_ = os.WriteFile(path, []byte("module (\n\t\"github.com/foo/bar\" // the main module\n) // end\n\ngo 1.24\n"), 0o644)

	got, err := moduleFromGoMod(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "github.com/foo/bar"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestModuleFromGoMod_ParenBlock_Empty is a defensive check that an
// (invalid — real go errors on this) empty block reports a clear error
// instead of silently returning an empty module path.
func TestModuleFromGoMod_ParenBlock_Empty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	_ = os.WriteFile(path, []byte("module (\n)\n\ngo 1.24\n"), 0o644)

	if _, err := moduleFromGoMod(path); err == nil {
		t.Fatal("expected an error for an empty 'module' block, got none")
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

// TestModuleFromGoMod_InvalidPath is a regression test for a real bug:
// moduleFromGoMod (the no-argument mode's read of ./go.mod) never validated
// the extracted module path via golang.org/x/mod/module.CheckPath, the same
// check resolveTarget already runs for an explicit `module@version` CLI
// argument (see TestResolveTarget's CheckPath coverage). Confirmed live
// against a real go1.24.4 toolchain that `go list -m`/`go build` both Fatal
// immediately with `malformed module path "example.com/foo!bar": invalid
// char '!'` for a go.mod written exactly this way — entirely offline, before
// ever resolving a requirement or contacting a proxy. Before this fix,
// moduleFromGoMod returned the invalid path unchanged, and goproxycheck's
// no-argument mode went on to probe the real proxy with it, reporting the
// generic "module-unknown... check for a typo or GOPRIVATE" verdict instead
// of the real, unconditional, already-broken-before-any-network-call answer.
func TestModuleFromGoMod_InvalidPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	_ = os.WriteFile(path, []byte("module example.com/foo!bar\n\ngo 1.24\n"), 0o644)

	_, err := moduleFromGoMod(path)
	if err == nil {
		t.Fatal("expected an error for a go.mod whose module directive is a syntactically invalid import path, got none")
	}
	if !strings.Contains(err.Error(), "invalid path") {
		t.Errorf("error %q doesn't mention the invalid path", err)
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

// TestResolveTarget_MalformedModulePathRejected is a regression test for a
// real bug: resolveTarget validated the version half of "module@version"
// against golang.org/x/mod/module (via modulepkg.EscapeVersion) but never
// validated the module path half the same way. Confirmed live (2026-09-28)
// against the real go1.24.4 toolchain: `go get example.com/foo!bar@v1.0.0`
// (an invalid character), `go get example.com/foo.@v1.0.0` (a trailing dot),
// `go get example.com/.foo@v1.0.0` (a leading dot), and `go get
// example.com/fooé@v1.0.0` (a non-ASCII letter) all fail immediately and
// unconditionally with `malformed module path %q: %v`, entirely offline,
// before ever contacting a proxy — even with GOPROXY=off. Before this check,
// goproxycheck sent a module path like this straight to the proxy (via
// escapePath, which only case-encodes uppercase ASCII and passes every other
// character straight through unescaped) and reported the generic
// statusModuleUnknown verdict ("check: is the repo public? does the module
// path in go.mod exactly match the repo? ..."), actively misdirecting the
// user toward a typo/GOPRIVATE explanation instead of the real, offline,
// unconditional answer that the module path itself is syntactically invalid.
func TestResolveTarget_MalformedModulePathRejected(t *testing.T) {
	for _, module := range []string{
		"example.com/foo!bar", // invalid char '!'
		"example.com/foo.",    // trailing dot in path element
		"example.com/.foo",    // leading dot in path element
		"example.com/fooé",    // non-ASCII letter
		"example.com/foo bar", // embedded space
	} {
		t.Run(module, func(t *testing.T) {
			_, _, err := resolveTarget([]string{module + "@v1.0.0"})
			if err == nil {
				t.Fatalf("expected an error for malformed module path %q", module)
			}
			if !strings.Contains(err.Error(), "malformed module path") {
				t.Errorf("error should mention the real `go` wording (\"malformed module path\"): %v", err)
			}
		})
	}
}

// TestResolveTarget_ValidUppercaseModulePathNotRejected guards against an
// over-broad fix to the check above: uppercase ASCII letters are legal in a
// real module path (e.g. github.com/Masterminds/squirrel) — that's the whole
// reason escapePath's case-encoding scheme exists — so module.CheckPath must
// not reject them.
func TestResolveTarget_ValidUppercaseModulePathNotRejected(t *testing.T) {
	module, version, err := resolveTarget([]string{"github.com/Masterminds/squirrel@v1.5.4"})
	if err != nil {
		t.Fatalf("resolveTarget rejected a valid uppercase module path: %v", err)
	}
	if module != "github.com/Masterminds/squirrel" || version != "v1.5.4" {
		t.Errorf("got module %q version %q, want github.com/Masterminds/squirrel v1.5.4", module, version)
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

// TestResolveTarget_PatchVersionRejected is a regression test for a real
// bug found by testing goproxycheck against real documented Go version
// queries beyond "latest"/"upgrade": unlike "upgrade" (see probe()'s
// "upgrade" handling), the "patch" query is only ever meaningful relative to
// a version some go.mod already requires. Confirmed live against cmd/go's
// own modload/query.go (NoPatchBaseError) that `go get module@patch` fails
// immediately and unconditionally with `can't query version "patch" of
// module <path>: no existing version is required` when there's no existing
// requirement — even with GOPROXY=off, and even for a module that doesn't
// exist — since goproxycheck's bare module@version argument never has such a
// requirement to be relative to. Before this check, goproxycheck sent
// "patch" to the proxy as a literal version string, which live testing shows
// 404s with a bare "not found: invalid version" body (no "unknown revision"
// marker) for every module, and fell through to statusNotYetIndexed —
// telling the caller to retry or --wait for a query that can never succeed
// no matter how long it's probed.
func TestResolveTarget_PatchVersionRejected(t *testing.T) {
	_, _, err := resolveTarget([]string{"example.com/mod@patch"})
	if err == nil {
		t.Fatal("expected an error for version \"patch\" with no existing requirement")
	}
	if !strings.Contains(err.Error(), "patch") {
		t.Errorf("error should mention the patch query: %v", err)
	}
}

// TestResolveTarget_DisallowedVersionCharsRejected is a regression test for a
// real bug: a version string containing a character real `go` disallows
// outright (a colon, question mark, semicolon, asterisk, pipe, backslash,
// exclamation mark, a trailing dot, or a Windows-reserved name like "NUL")
// used to sail through resolveTarget unrejected and get probed against the
// real proxy, where it either 404s with a body ("bad request: invalid
// escaped version ...") that matches none of diagnose's specific markers
// (falling through to statusNotYetIndexed, "retry in a minute, or use
// --wait" — a doomed --wait poll, since cmd/go itself rejects the identical
// string immediately and offline with "invalid version: version %q invalid:
// disallowed version string", confirmed live 2026-09-27 against
// golang.org/x/mod for every one of these), or — for an un-percent-encoded
// "?" specifically — gets mangled entirely by net/url treating it as the
// start of a query string before the request is even sent.
func TestResolveTarget_DisallowedVersionCharsRejected(t *testing.T) {
	for _, version := range []string{
		"v0.1:9",  // path separator, confirmed live: "invalid version: version \"v0.1:9\" invalid: disallowed version string"
		"v0.1?9",  // shell-special; also mangled by net/url as a query-string separator
		"v0.1;9",  // bare semicolon
		"v0.1*9",  // shell-special glob char
		"v0.1|9",  // shell-special pipe
		"v0.1\\9", // path separator
		"v0.1!9",  // literal '!' collides with the proxy's own case-escaping marker
		"v0.1.0.", // trailing dot
		"NUL",     // Windows-reserved element name
	} {
		t.Run(version, func(t *testing.T) {
			_, _, err := resolveTarget([]string{"example.com/mod@" + version})
			if err == nil {
				t.Fatalf("expected an error for disallowed version string %q", version)
			}
			if !strings.Contains(err.Error(), "disallowed version string") {
				t.Errorf("error should mention the real `go` wording (\"disallowed version string\"): %v", err)
			}
		})
	}
}

// TestResolveTarget_ComparisonVersionQueryNotRejected is a regression test
// guarding the isComparisonVersionQuery exclusion added alongside the
// disallowed-version-character check above: a version-range query like
// "<v1.2.3" or ">=v0.9.0" (go.dev/ref/mod#version-queries) is valid,
// documented syntax real `go` accepts fine — it's resolved by comparing
// against @v/list locally, never sent to the proxy as a literal version —
// but it necessarily starts with '<' or '>', characters that are themselves
// disallowed in an ordinary version string. Without excluding this shape,
// the disallowed-version-character check above would wrongly reject every
// comparison query resolveTarget previously accepted, before it's ever
// probed.
func TestResolveTarget_ComparisonVersionQueryNotRejected(t *testing.T) {
	for _, version := range []string{"<v1.2.3", "<=v1.2.3", ">v1.2.3", ">=v1.2.3"} {
		t.Run(version, func(t *testing.T) {
			_, gotVersion, err := resolveTarget([]string{"example.com/mod@" + version})
			if err != nil {
				t.Fatalf("resolveTarget rejected valid comparison query %q: %v", version, err)
			}
			if gotVersion != version {
				t.Errorf("got version %q, want %q", gotVersion, version)
			}
		})
	}
}

// TestResolveTarget_InvalidComparisonOperandRejected is a regression test
// for a real bug: a comparison-query version (go.dev/ref/mod#version-queries)
// whose operand isn't itself a valid semantic version — e.g. "<1.2.3"
// (missing the required "v" prefix), ">=badversion", "<v1.2.3-" (a trailing
// hyphen with no pre-release identifier), "<=v1.2.3.4" (four components), or
// the bare operator "<" alone — used to sail through resolveTarget
// unrejected (isComparisonVersionQuery only checks the '<'/'>' prefix, not
// whether the rest is a valid version) and get probed against the real
// proxy as a literal query string, which can never succeed: confirmed live
// (2026-09-28) that `go get golang.org/x/mod@<1.2.3` and the other examples
// above all fail immediately and unconditionally with `invalid semantic
// version %q in range %q`, entirely offline, before cmd/go ever contacts a
// proxy. Before this check, goproxycheck instead diagnosed statusNotYetIndexed
// ("retry in a minute, or use --wait") — a doomed poll, since a query shaped
// like this never appears in @v/list no matter how long it's retried.
func TestResolveTarget_InvalidComparisonOperandRejected(t *testing.T) {
	for _, version := range []string{
		"<1.2.3",       // missing "v" prefix
		">=badversion", // not a version at all
		"<v1.2.3-",     // trailing hyphen, no pre-release identifier
		"<=v1.2.3.4",   // four components
		"<",            // bare operator, empty operand
	} {
		t.Run(version, func(t *testing.T) {
			_, _, err := resolveTarget([]string{"example.com/mod@" + version})
			if err == nil {
				t.Fatalf("expected an error for invalid comparison operand %q", version)
			}
			if !strings.Contains(err.Error(), "invalid semantic version") {
				t.Errorf("error should mention the real `go` wording (\"invalid semantic version\"): %v", err)
			}
		})
	}
}

// TestResolveTarget_AmbiguousComparisonPrefixRejected is a regression test
// for a real bug: "<=" and ">" (unlike "<" and ">=") paired with an
// incomplete ("prefix") semantic-version operand — bare major ("v1") or
// major.minor ("v1.2"), missing the patch component — used to sail through
// resolveTarget unrejected (the operand is valid semver, just incomplete, so
// the invalid-operand check above doesn't catch it) and get resolved against
// @v/list by probe()'s resolveComparisonQuery, which has no notion of
// "ambiguous" and just picks a concrete version via plain semver.Compare.
// Confirmed live (2026-09-29) against cmd/go's own newQueryMatcher
// (modload/query.go): `go get golang.org/x/mod@<=v0.19` and `go get
// golang.org/x/mod@>v0` both fail immediately and unconditionally with
// `ambiguous semantic version %q in range %q`, entirely offline, before ever
// contacting the proxy — real go refuses to guess whether the bound means
// exactly vX.Y(.0) or the whole vX.Y.* line. Before this check, goproxycheck
// instead reported these as statusReady with a concrete resolved version, the
// opposite of what a real `go get`/`go install` does for that exact argument.
func TestResolveTarget_AmbiguousComparisonPrefixRejected(t *testing.T) {
	for _, version := range []string{
		"<=v0.19", // major.minor only
		">v0.19",
		"<=v1", // bare major
		">v1",
	} {
		t.Run(version, func(t *testing.T) {
			_, _, err := resolveTarget([]string{"example.com/mod@" + version})
			if err == nil {
				t.Fatalf("expected an error for ambiguous comparison query %q", version)
			}
			if !strings.Contains(err.Error(), "ambiguous") {
				t.Errorf("error should mention the real `go` wording (\"ambiguous\"): %v", err)
			}
		})
	}
}

// TestResolveTarget_UnambiguousComparisonPrefixNotRejected guards the two
// shapes TestResolveTarget_AmbiguousComparisonPrefixRejected's fix must NOT
// reject: "<" and ">=" paired with the identical incomplete-version operand
// shape are NOT ambiguous (excluding/including everything from vX.Y.0 up
// reads the same way either way real `go` could interpret the prefix), and a
// *complete* major.minor.patch operand paired with "<=" or ">" isn't
// incomplete at all, so neither triggers the new check. Confirmed live
// (2026-09-29): `go get golang.org/x/mod@<v0.19` and `go get
// golang.org/x/mod@>=v0.19` both resolve and install successfully, matching
// the exact operator/operand combinations exercised here.
func TestResolveTarget_UnambiguousComparisonPrefixNotRejected(t *testing.T) {
	for _, version := range []string{
		"<v0.19",       // "<" with a prefix operand: unambiguous
		">=v0.19",      // ">=" with a prefix operand: unambiguous
		"<=v1.2.3",     // "<=" with a complete operand: not a prefix at all
		">v1.2.3",      // ">" with a complete operand: not a prefix at all
		"<=v1.2.3-rc1", // "<=" with a complete pre-release operand: not a prefix
	} {
		t.Run(version, func(t *testing.T) {
			_, gotVersion, err := resolveTarget([]string{"example.com/mod@" + version})
			if err != nil {
				t.Fatalf("resolveTarget wrongly rejected unambiguous comparison query %q: %v", version, err)
			}
			if gotVersion != version {
				t.Errorf("got version %q, want %q", gotVersion, version)
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
		// Confirmed live (2026-09-27): `go env GOPROXY` echoes back the raw,
		// un-normalized config string ("proxy.golang.org", not
		// "https://proxy.golang.org"), but real cmd/go's own proxyList
		// (modfetch/proxy.go) implicitly prepends "https://" to any bare-host
		// entry before ever using it — confirmed with `go mod download -x
		// golang.org/x/mod@v0.19.0` under GOPROXY=proxy.golang.org (no
		// scheme): the trace hits https://proxy.golang.org, byte-for-byte the
		// same public proxy the default config uses.
		{"no scheme", "proxy.golang.org", "", ""},
		{"no scheme trailing slash", "proxy.golang.org/", "", ""},
		{"no scheme then direct", "proxy.golang.org,direct", "", ""},
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
		// Same no-scheme normalization gap as TestLocalGoproxyNonPublic's
		// "no scheme" case, but for the later-in-the-chain fallback path:
		// confirmed live the same way, `go mod download -x` for a chain
		// with a bare "proxy.golang.org" second entry still falls through
		// to the exact public proxy this tool probes.
		{"custom then no-scheme public", "https://corp.example.com,proxy.golang.org", "https://corp.example.com", false, true},
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

// TestGosumdbConfigError is a regression test for a real bug: sumdbName
// above only extracts *which* database a raw GOSUMDB value names, it never
// checks that the value actually parses as a real checksum-database
// verifier key the way cmd/go's own dbDial (modfetch/sumdb.go) does before
// ever dialing one. A bare custom hostname with no key at all, or a
// name+key pair whose fields don't form a valid key, sails through
// sumdbName as if it named a real, working custom database.
//
// Confirmed live (2026-09-28) against a fresh, isolated GOMODCACHE with
// `go install golang.org/x/text@v0.14.0` (a module not already recorded in
// any local go.sum, so verification is actually attempted):
//   - GOSUMDB=sum.example.com (a bare hostname, no key) fails outright with
//     "invalid GOSUMDB: malformed verifier id"
//   - GOSUMDB="mycompany.example+abc123 https://sumdb.mycompany.example"
//     (the exact fixture TestRun_SumdbLagWithCustomGosumdb used to use)
//     fails the same way — "abc123" is only 6 hex chars, not the 8-hex-
//     digit key hash note.NewVerifier requires, so it's not a valid key
//     either
//   - GOSUMDB="a b c" (too many fields) fails with "invalid GOSUMDB: too
//     many fields"
//   - a genuinely well-formed name+hash+key pair generated by
//     note.GenerateKey, plus a URL, passes GOSUMDB validation cleanly (the
//     install then fails on an unrelated network error dialing the
//     nonexistent custom host, never on "invalid GOSUMDB")
//   - the default "sum.golang.org" and its full expanded key both parse
//     fine, as does the documented "sum.golang.google.cn" alias
func TestGosumdbConfigError(t *testing.T) {
	cases := []struct {
		name       string
		gosumdb    string
		wantErr    bool
		wantSubstr string
	}{
		{"off", "off", false, ""},
		{"default public name", "sum.golang.org", false, ""},
		{"full public key", "sum.golang.org+033de0ae+Ac4zctda0e5eza+HJyk9SxEdh+s3Ux18htTTAD8OuAn8", false, ""},
		{"google cn alias", "sum.golang.google.cn", false, ""},
		{"bare custom hostname, no key", "sum.example.com", true, "malformed verifier id"},
		{"short truncated hash", "mycompany.example+abc123 https://sumdb.mycompany.example", true, "malformed verifier id"},
		{"too many fields", "a b c", true, "too many fields"},
		{"valid generated key with url", "sumdb.mycompany.example+18034219+ARh1MwsDARWl2XLlkBuE9hyxjXsSk5sX709QEBIDy21S https://sumdb.mycompany.example", false, ""},
		// Regression test: a verifier key whose embedded name carries a
		// trailing slash (e.g. copy-pasted from a URL instead of a bare
		// host[/path]) — generated live via note.GenerateKey(rand.Reader,
		// "example.com/"), which accepts it fine (isValidName has no
		// opinion on a trailing slash). Confirmed live (2026-09-28): `go
		// get golang.org/x/text@v0.14.0` with GOSUMDB set to this exact
		// value, against a fresh GOMODCACHE, fails outright with "invalid
		// sumdb name (must be host[/path]): example.com/ ...", never
		// reaching the network — see validSumdbName's doc comment.
		{"generated key with trailing-slash name", "example.com/+6b8bd748+AfzT6Rs7/F20IZqPIzDwguH7VNYE5d+tqj+5f+EpTQep", true, "invalid sumdb name (must be host[/path])"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := gosumdbConfigError(c.gosumdb)
			if c.wantErr && err == nil {
				t.Fatalf("gosumdbConfigError(%q) = nil, want an error", c.gosumdb)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("gosumdbConfigError(%q) = %v, want nil", c.gosumdb, err)
			}
			if c.wantSubstr != "" && !strings.Contains(err.Error(), c.wantSubstr) {
				t.Errorf("gosumdbConfigError(%q) = %v, want it to contain %q", c.gosumdb, err, c.wantSubstr)
			}
		})
	}
}
