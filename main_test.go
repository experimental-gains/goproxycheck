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

// TestModuleFromGoMod_BareModuleDirective is a regression test for a real
// bug: a "module" keyword with nothing real after it — end of line, only
// trailing whitespace, or a same-line "//" comment glued directly onto the
// keyword with no separating space — fell through moduleDirective's
// single-line-form check entirely (the leading strings.TrimSpace(raw)
// silently collapses "module " / "module\t" down to exactly "module" before
// the check ever runs, and a bare "//" right after the keyword starts with
// '/', a byte the check's separator set didn't include) and was
// misdiagnosed as "has no 'module' directive" — implying the file is
// missing the directive entirely. Confirmed live (2026-09-30) against
// golang.org/x/mod/modfile.Parse that every one of these shapes instead
// Fatals with `usage: module module/path`: a directive that IS present,
// just missing its required path argument, the identical failure the
// already-existing "module // comment" (space then comment) and "module
// (\n)" (empty block) cases correctly report as "has a 'module' directive
// with no path" rather than "no directive at all."
func TestModuleFromGoMod_BareModuleDirective(t *testing.T) {
	for name, content := range map[string]string{
		"bare, no trailing newline": "module",
		"bare, with blank line":     "module\n\ngo 1.24\n",
		"trailing space only":       "module \n\ngo 1.24\n",
		"trailing tab only":         "module\t\n\ngo 1.24\n",
		"no-space comment":          "module//oops, no path here\n\ngo 1.24\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "go.mod")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := moduleFromGoMod(path)
			if err == nil {
				t.Fatalf("expected an error for a bare 'module' directive with no path, got module %q with no error", got)
			}
			if strings.Contains(err.Error(), "has no 'module' directive") {
				t.Errorf("error %q wrongly claims there's no module directive at all, for a go.mod that has one (just malformed, missing its path) — real go Fatals with `usage: module module/path`, not \"no directive\"", err)
			}
			if !strings.Contains(err.Error(), "has a 'module' directive with no path") {
				t.Errorf("error %q doesn't match the existing 'directive with no path' wording used for the same underlying failure shape", err)
			}
		})
	}
}

// TestModuleFromGoMod_SingleSlashIsNotACommentSeparator makes sure the fix
// for TestModuleFromGoMod_BareModuleDirective stays narrowly scoped to a
// genuine "//" comment glued onto the keyword, not any byte starting with
// '/'. Confirmed live (2026-09-30) that real go tokenizes "module/foo" (one
// slash, not a comment) as a single unrecognized identifier token —
// `unknown directive: module/foo` — a completely different failure this
// function was never meant to diagnose, not "module directive with no
// path." moduleDirective can't distinguish "unknown directive" from "no
// module directive" either way (it only recognizes the module verb, not a
// general directive-verb checklist), so this just confirms the fix doesn't
// misfire into the wrong specific error for this shape.
func TestModuleFromGoMod_SingleSlashIsNotACommentSeparator(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	_ = os.WriteFile(path, []byte("module/foo\n\ngo 1.24\n"), 0o644)

	_, err := moduleFromGoMod(path)
	if err == nil {
		t.Fatal("expected an error for a go.mod with no real 'module' directive, got none")
	}
	if strings.Contains(err.Error(), "has a 'module' directive with no path") {
		t.Errorf("error %q wrongly claims a 'module' directive was found with a missing path — \"module/foo\" is a single unrecognized token to real go, not the module directive at all", err)
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

// TestModuleFromGoMod_TooManyArgs is a regression test for a real bug:
// moduleDirective passed a module-directive line's entire raw text straight
// to modulepkg.CheckPath even when the line carried a second,
// whitespace-separated argument after the path — real go's own lexer
// (golang.org/x/mod/modfile's rule.go) Fatals a "module" directive with
// anything other than exactly one argument with `usage: module module/path`,
// an argument-count error, never reaching CheckPath's module-path validation
// at all. Confirmed live (2026-09-30) against a real go1.26.8 toolchain:
// `module example.com/foo extra-token` (single-line form) and
// `example.com/foo extra` inside a `module (...)` block both Fatal with
// `usage: module module/path`. Before this fix, moduleFromGoMod instead
// reported `malformed module path "example.com/foo extra-token": invalid
// char ' '` — CheckPath's genuine reaction to being handed the whole
// two-token line as if it were one literal path — actively misdirecting a
// user toward stripping a character from the module path instead of
// removing the stray trailing token that was never part of it. A quoted
// path with an embedded space (`module "example.com/foo bar"`) is
// deliberately NOT part of this bug: that's one lexical argument whose own
// content is invalid, and real go Fatals with the CheckPath-style
// `malformed module path ...: invalid char ' '` instead — confirmed live
// the two cases produce genuinely different real errors, not the same
// failure phrased two ways (see TestModuleFromGoMod_QuotedPathWithSpace).
func TestModuleFromGoMod_TooManyArgs(t *testing.T) {
	for name, content := range map[string]string{
		"single-line, unquoted extra token": "module example.com/foo extra-token\n\ngo 1.24\n",
		"single-line, quoted path plus extra token": `module "example.com/foo" extra

go 1.24
`,
		"block form, extra token on the path line": "module (\n\texample.com/foo extra\n)\n\ngo 1.24\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "go.mod")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := moduleFromGoMod(path)
			if err == nil {
				t.Fatal("expected an error for a 'module' directive with more than one argument, got none")
			}
			if strings.Contains(err.Error(), "malformed module path") {
				t.Errorf("error %q wrongly blames the module path's own characters (CheckPath's error) for an argument-COUNT problem — real go Fatals with `usage: module module/path`, never reaching path validation at all", err)
			}
			if !strings.Contains(err.Error(), "more than one argument") {
				t.Errorf("error %q doesn't describe this as an extra-argument problem", err)
			}
		})
	}
}

// TestModuleFromGoMod_QuotedPathWithSpace confirms the fix for
// TestModuleFromGoMod_TooManyArgs stays narrowly scoped to a genuine second
// argument, not any whitespace appearing anywhere on the line: a single
// quoted argument containing an embedded space is one lexical token to real
// go's parser (confirmed live: `module "example.com/foo bar"` Fatals with
// `malformed module path "example.com/foo bar": invalid char ' '`, the
// CheckPath-style error, not `usage: module module/path`), so this must
// still be reported as an invalid path, not an extra-argument error.
func TestModuleFromGoMod_QuotedPathWithSpace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	content := "module \"example.com/foo bar\"\n\ngo 1.24\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := moduleFromGoMod(path)
	if err == nil {
		t.Fatal("expected an error for a module path containing a space, got none")
	}
	if strings.Contains(err.Error(), "more than one argument") {
		t.Errorf("error %q wrongly treats a single quoted argument with an embedded space as two arguments", err)
	}
	if !strings.Contains(err.Error(), "invalid path") {
		t.Errorf("error %q doesn't mention the invalid path", err)
	}
}

// TestModuleFromGoMod_RepeatedModuleDirective is a regression test for a
// real bug: moduleDirective returned as soon as it finished parsing the
// FIRST 'module' directive in a go.mod, so a go.mod carrying a second
// 'module' directive never got flagged at all — its first path was
// silently returned and probed against the proxy as an ordinary module.
// Confirmed live (2026-10-01, go1.24.4) against a real go.mod: `go list -m`
// Fatals immediately and offline with "go.mod:N: repeated module
// statement" for every one of these shapes — two single-line directives,
// two block directives, one of each, and two path lines inside a single
// block — never reaching the proxy at all, regardless of which of the two
// (or more) module paths a user might expect to be "the real one."
func TestModuleFromGoMod_RepeatedModuleDirective(t *testing.T) {
	for name, content := range map[string]string{
		"two single-line directives": "module example.com/foo\n\nmodule example.com/bar\n\ngo 1.21\n",
		"two block directives":       "module (\n\texample.com/foo\n)\n\nmodule (\n\texample.com/bar\n)\n\ngo 1.21\n",
		"single-line then block":     "module example.com/foo\n\nmodule (\n\texample.com/bar\n)\n\ngo 1.21\n",
		"block then single-line":     "module (\n\texample.com/foo\n)\n\nmodule example.com/bar\n\ngo 1.21\n",
		"two paths inside one block": "module (\n\texample.com/foo\n\texample.com/bar\n)\n\ngo 1.21\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "go.mod")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err := moduleFromGoMod(path)
			if err == nil {
				t.Fatal("expected an error for a go.mod with more than one 'module' directive, got none")
			}
			if !strings.Contains(err.Error(), "repeated module statement") {
				t.Errorf("error %q doesn't describe this as a repeated module statement", err)
			}
		})
	}
}

// TestModuleFromGoMod_RepeatedGoDirective is a regression test for a real
// bug: moduleFromGoMod detected a repeated 'module' directive (see
// TestModuleFromGoMod_RepeatedModuleDirective above) but had no equivalent
// check for a repeated 'go' directive, even though real go Fatals on both
// identically. Confirmed live (2026-10-05, go1.24.4): a go.mod with two
// top-level `go` lines Fatals `go list -m`/`go build` immediately and
// entirely offline with "go.mod:N: repeated go statement" — and this fires
// at parse time, ahead of any toolchain-satisfiability logic: even a second
// `go` line naming an unsatisfiable version (e.g. "go 1.99") under
// GOTOOLCHAIN=auto, which would otherwise attempt a network fetch, still
// Fatals with the same offline parse error and never attempts a fetch.
// Before this fix, moduleDirective's own doc comment even asserted this was
// "not goproxycheck's concern" since it allegedly never parsed `go`/
// `toolchain` directives — a claim later invalidated by goDirectiveVersion
// (added for goDirectiveUnsatisfiableError/ignoreDirectiveTooOldError/etc.),
// which silently returns only the FIRST `go` line, ignoring a conflicting
// second one; a go.mod this broken sailed straight through to probe() and
// got a generic module-unknown-style verdict instead of the accurate,
// `go`-shaped answer that it could never resolve regardless of repo or
// proxy config.
func TestModuleFromGoMod_RepeatedGoDirective(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	content := "module example.com/foo\n\ngo 1.21\n\ngo 1.22\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := moduleFromGoMod(path)
	if err == nil {
		t.Fatal("expected an error for a go.mod with more than one 'go' directive, got none")
	}
	if !strings.Contains(err.Error(), "repeated go statement") {
		t.Errorf("error %q doesn't describe this as a repeated go statement", err)
	}
}

// TestModuleFromGoMod_RepeatedToolchainDirective is
// TestModuleFromGoMod_RepeatedGoDirective's exact sibling for the
// 'toolchain' directive. Confirmed live (2026-10-05, go1.24.4): a go.mod
// with two top-level `toolchain` lines Fatals `go list -m`/`go build`
// immediately and entirely offline with "go.mod:N: repeated toolchain
// statement".
func TestModuleFromGoMod_RepeatedToolchainDirective(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	content := "module example.com/foo\n\ngo 1.21\n\ntoolchain go1.22.0\n\ntoolchain go1.23.0\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := moduleFromGoMod(path)
	if err == nil {
		t.Fatal("expected an error for a go.mod with more than one 'toolchain' directive, got none")
	}
	if !strings.Contains(err.Error(), "repeated toolchain statement") {
		t.Errorf("error %q doesn't describe this as a repeated toolchain statement", err)
	}
}

func TestModuleFromGoMod_Missing(t *testing.T) {
	if _, err := moduleFromGoMod(filepath.Join(t.TempDir(), "go.mod")); err == nil {
		t.Fatal("expected an error for a missing go.mod")
	}
}

// TestModuleFromGoMod_BOM is a regression test for a real bug: a go.mod
// whose first bytes are the UTF-8 byte order mark (hex EF BB BF, Unicode
// code point U+FEFF) makes a real `go list -m`/`go build` Fatal
// immediately and offline with `go.mod:1: unexpected input character`
// (quoting the BOM rune itself) — confirmed live (go1.24.4) — before the
// module directive, or anything else in the file, is ever evaluated.
// Windows tooling commonly writes UTF-8-with-BOM by default (pre-6
// PowerShell's `Set-Content -Encoding UTF8`, Notepad's "UTF-8" option), so
// a hand-edited go.mod saved that way is a real, if rare, shape.
//
// diagnose.go's statusGoModUnparseable check (added for a different go.mod
// defect, a "/* */" block comment — see that status's doc comment) already
// catches this same class of problem for the *proxy-served* go.mod of the
// version being checked, by running the real golang.org/x/mod/modfile.Parse
// ahead of canonicalModulePath/retraction/deprecation. But moduleFromGoMod
// is a separate, earlier code path: the no-argument CLI mode that reads the
// *local* ./go.mod to discover which module to probe in the first place,
// and it never goes through that check at all — it calls moduleDirective's
// hand-rolled line scanner directly.
//
// Before this fix, moduleDirective's `strings.CutPrefix(line, "module")`
// check on the first line silently failed (the line reads the BOM code
// point glued onto the front of "module", and strings.TrimSpace does not
// strip that code point), so the directive was never recognized at all and
// moduleFromGoMod reported the generic "go.mod has no 'module' directive"
// — actively misdirecting a user into thinking they need to add a module
// directive from scratch, instead of the accurate, `go`-shaped answer that
// the file has an encoding problem a real toolchain Fatals on before even
// looking for one.
func TestModuleFromGoMod_BOM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	_ = os.WriteFile(path, []byte("\xEF\xBB\xBFmodule github.com/foo/bar\n\ngo 1.24\n"), 0o644)

	_, err := moduleFromGoMod(path)
	if err == nil {
		t.Fatal("expected an error for a go.mod beginning with a UTF-8 byte order mark, got none")
	}
	if strings.Contains(err.Error(), "has no 'module' directive") {
		t.Errorf("error %q wrongly claims there's no module directive at all — real go Fatals with a BOM-specific parse error (`unexpected input character '\\ufeff'`) before it ever looks for one", err)
	}
	if !strings.Contains(err.Error(), "byte order mark") || !strings.Contains(err.Error(), "unexpected input character") {
		t.Errorf("error %q doesn't mention the byte order mark and real go's own Fatal message", err)
	}
}

// TestModuleFromGoMod_BacktickQuoted is a regression test for a real bug:
// parseModulePath ran a module-directive's raw text straight through
// strconv.Unquote regardless of which quote character (if any) opened it.
// strconv.Unquote treats a backtick-delimited string as an ordinary Go raw
// string literal and happily unquotes it — Go SOURCE CODE does support
// backtick raw strings — but go.mod's own grammar does not extend that same
// allowance: confirmed by reading golang.org/x/mod/modfile/rule.go's
// parseString directly, a token is only ever unquoted when it starts with a
// literal '"'; any other token containing a '"', '\”, or '`' anywhere
// (including one fully wrapped in matching backticks) is an unconditional
// parse Fatal, with real go's own comment explaining why: "Other quotes are
// reserved both for possible future expansion and to avoid confusion."
// Confirmed live (2026-10-02, go1.24.4): a go.mod whose module directive
// reads `module `+"`"+`example.com/foo`+"`"+` makes `go list -m`/`go build`
// Fatal immediately and offline with `go.mod:1: invalid quoted string:
// unquoted string cannot contain quote`.
//
// Before this fix, moduleFromGoMod's no-argument-mode callers instead
// silently stripped the backticks, accepted "example.com/foo" as the module
// path, and probed it against the real proxy.golang.org — reporting the
// actively misleading statusModuleUnknown verdict ("check: is the repo
// public? does the module path... typo? GOPRIVATE?") for a go.mod that
// never had a shot at resolving at all, since it doesn't even parse.
func TestModuleFromGoMod_BacktickQuoted(t *testing.T) {
	for name, content := range map[string]string{
		"single-line, whole path backtick-quoted":  "module `example.com/foo`\n\ngo 1.24\n",
		"block form, path line backtick-quoted":    "module (\n\t`example.com/foo`\n)\n\ngo 1.24\n",
		"empty backtick string":                    "module ``\n\ngo 1.24\n",
		"stray backtick in an otherwise bare path": "module exa`mple.com/foo\n\ngo 1.24\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "go.mod")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := moduleFromGoMod(path)
			if err == nil {
				t.Fatalf("expected an error for a module directive involving a backtick, got module %q", got)
			}
			if !strings.Contains(err.Error(), "invalid quoted string") || !strings.Contains(err.Error(), "cannot contain quote") {
				t.Errorf("error %q doesn't cite real go's own `invalid quoted string: unquoted string cannot contain quote` Fatal", err)
			}
		})
	}
}

// TestModuleFromGoMod_MalformedEscape confirms parseModulePath's rewrite
// (TestModuleFromGoMod_BacktickQuoted above) also fixes a narrower,
// same-root-cause case one call site already caught but misreported: a
// double-quoted path with a malformed Go-string escape (e.g. an invalid
// two-digit hex escape) was already rejected pre-fix too — strconv.Unquote
// fails on it either way — but the old "return s unchanged on Unquote
// failure" fallback handed modulepkg.CheckPath the raw, still-quoted
// literal, which blamed the embedded '"' characters ("malformed module path
// ...: invalid char '\"'") instead of citing the real, earlier go.mod-parse
// Fatal real go actually raises: confirmed live (2026-10-02, go1.24.4) the
// identical file fails with `invalid quoted string: invalid syntax`
// (strconv.Unquote's own error), never reaching CheckPath's validation at
// all.
func TestModuleFromGoMod_MalformedEscape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	content := "module \"example.com\\xZZ\"\n\ngo 1.24\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := moduleFromGoMod(path)
	if err == nil {
		t.Fatal("expected an error for a module path with a malformed Go-string escape, got none")
	}
	if strings.Contains(err.Error(), "malformed module path") {
		t.Errorf("error %q wrongly blames CheckPath's module-path validation for a go.mod-parse-time Fatal that happens first in real go", err)
	}
	if !strings.Contains(err.Error(), "invalid quoted string") {
		t.Errorf("error %q doesn't cite real go's own `invalid quoted string: ...` Fatal", err)
	}
}

// TestModuleFromGoMod_MalformedIgnoreDirective is a regression test for a
// real bug: the `ignore` go.mod directive (golang.org/x/mod/modfile v0.41.0
// parses it; go1.24.4 — the system toolchain on this box — does not even
// recognize it, confirmed live) Fatals real `go list -m`/`go build`
// immediately and entirely offline with `ignore directive expects exactly
// one argument` whenever it's given zero or more than one argument — in
// either its single-line form or inside its parenthesized block form.
// Confirmed live (go1.26.8, 2026-10-03) for every shape in this table.
//
// Before this fix, moduleFromGoMod had no awareness of `ignore` at all —
// moduleDirective's scanner only ever recognizes `module` lines — so a
// go.mod broken this way sailed straight through unflagged: the module
// path was extracted normally and probed against the live proxy as if the
// file were perfectly ordinary, when the real toolchain can't even parse
// it, let alone resolve a single requirement.
func TestModuleFromGoMod_MalformedIgnoreDirective(t *testing.T) {
	for name, content := range map[string]string{
		"bare ignore, no argument": "module example.com/foo\n\ngo 1.26\n\nignore\n",
		"two unquoted arguments":   "module example.com/foo\n\ngo 1.26\n\nignore ./a ./b\n",
		"trailing comment doesn't count as an arg, this one's still bare": "module example.com/foo\n\ngo 1.26\n\nignore // nothing here\n",
		"block form, one entry with two arguments":                        "module example.com/foo\n\ngo 1.26\n\nignore (\n\t./a ./b\n)\n",
		"block form, one empty-looking entry":                             "module example.com/foo\n\ngo 1.26\n\nignore (\n\t./a\n\t\n\t./b ./c\n)\n",
		"no-space-before-paren block, bad entry":                          "module example.com/foo\n\ngo 1.26\n\nignore(\n\t./a ./b\n)\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "go.mod")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := moduleFromGoMod(path)
			if err == nil {
				t.Fatalf("expected an error for a malformed 'ignore' directive, got module %q", got)
			}
			if !strings.Contains(err.Error(), "ignore directive expects exactly one argument") {
				t.Errorf("error %q doesn't cite real go's own `ignore directive expects exactly one argument` Fatal", err)
			}
		})
	}
}

// TestModuleFromGoMod_WellFormedIgnoreDirectiveNotRejected confirms the fix
// for TestModuleFromGoMod_MalformedIgnoreDirective stays narrowly scoped to
// a genuine argument-count mismatch: a well-formed single-argument `ignore`
// line, in every shape real go accepts (single-line, block form, block form
// with no space before the opening paren, multiple one-argument block
// entries), must not be rejected.
func TestModuleFromGoMod_WellFormedIgnoreDirectiveNotRejected(t *testing.T) {
	for name, content := range map[string]string{
		"single-line":                 "module example.com/foo\n\ngo 1.26\n\nignore ./vendor\n",
		"single-line with comment":    "module example.com/foo\n\ngo 1.26\n\nignore ./vendor // generated\n",
		"block form":                  "module example.com/foo\n\ngo 1.26\n\nignore (\n\t./vendor\n)\n",
		"block form, two entries":     "module example.com/foo\n\ngo 1.26\n\nignore (\n\t./vendor\n\t./testdata\n)\n",
		"no-space-before-paren block": "module example.com/foo\n\ngo 1.26\n\nignore(\n\t./vendor\n)\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "go.mod")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := moduleFromGoMod(path)
			if err != nil {
				t.Fatalf("unexpected error for a well-formed 'ignore' directive: %v", err)
			}
			if want := "example.com/foo"; got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}
}

// TestModuleFromGoMod_UnknownDirective is a regression test for a real
// bug: moduleDirective's scanner only ever recognizes `module` lines, so
// an ordinary typo'd top-level directive anywhere else in the file (not
// just the already-separately-checked `ignore`) sailed straight through
// unflagged. Confirmed live (go1.24.4 and go1.26.8): a go.mod otherwise
// reading only `module example.com/foo` / `go 1.21` plus one extra,
// unrecognized line Fatals real `go list -m all`/`go build` immediately
// and entirely offline with `go.mod:N: unknown directive: <verb>` (or
// `unknown block type: <verb>` for the parenthesized-block-open form) —
// before the module directive, or anything else in the file, is ever
// resolved, let alone a proxy contacted.
//
// Before this fix, moduleFromGoMod extracted "example.com/foo" normally
// from each of these files and probed it against the live proxy as if the
// file were perfectly ordinary, reporting the actively misleading
// statusModuleUnknown verdict for a go.mod that could never resolve via
// any real `go` command in the first place, for a completely unrelated
// reason.
func TestModuleFromGoMod_UnknownDirective(t *testing.T) {
	for name, content := range map[string]string{
		"bare unknown verb, no arguments":                     "module example.com/foo\n\ngo 1.21\n\nbogusverb\n",
		"unknown verb with arguments":                         "module example.com/foo\n\ngo 1.21\n\nbogusverb something here\n",
		"unknown verb, wrong-case known verb (Require)":       "module example.com/foo\n\ngo 1.21\n\nRequire example.com/bar v1.0.0\n",
		"unknown verb opening a block, no space before paren": "module example.com/foo\n\ngo 1.21\n\nbogusverb(\n\tx\n)\n",
		"unknown verb before the module directive":            "bogusverb x\n\nmodule example.com/foo\n\ngo 1.21\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "go.mod")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := moduleFromGoMod(path)
			if err == nil {
				t.Fatalf("expected an error for an unrecognized go.mod directive, got module %q", got)
			}
			if !strings.Contains(err.Error(), "unknown directive") && !strings.Contains(err.Error(), "unknown block type") {
				t.Errorf("error %q doesn't cite real go's own `unknown directive`/`unknown block type` Fatal", err)
			}
		})
	}
}

// TestModuleFromGoMod_UnknownDirectiveBlockVsSingleLine pins the two
// distinct real-go error messages for an unknown verb depending on whether
// it opens a parenthesized block (`unknown block type: %s`) or not
// (`unknown directive: %s`) — confirmed live they're genuinely different
// wording, not just this tool's own invention, so a caller scraping either
// substring out of goproxycheck's own error text gets the right one for
// the shape that was actually in the file.
func TestModuleFromGoMod_UnknownDirectiveBlockVsSingleLine(t *testing.T) {
	dir := t.TempDir()

	singleLine := filepath.Join(dir, "single.mod")
	if err := os.WriteFile(singleLine, []byte("module example.com/foo\n\ngo 1.21\n\nbogusverb x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := moduleFromGoMod(singleLine); err == nil || !strings.Contains(err.Error(), "unknown directive: bogusverb") {
		t.Errorf("single-line unknown verb: got error %v, want one citing `unknown directive: bogusverb`", err)
	}

	block := filepath.Join(dir, "block.mod")
	if err := os.WriteFile(block, []byte("module example.com/foo\n\ngo 1.21\n\nbogusverb (\n\tx\n)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := moduleFromGoMod(block); err == nil || !strings.Contains(err.Error(), "unknown block type: bogusverb") {
		t.Errorf("block-opening unknown verb: got error %v, want one citing `unknown block type: bogusverb`", err)
	}
}

// TestModuleFromGoMod_KnownDirectivesNotFlaggedAsUnknown confirms the
// TestModuleFromGoMod_UnknownDirective fix stays narrowly scoped: every
// real go.mod verb (including the ones with their own block form, and
// comments inside a block) must still resolve normally, not get
// misidentified as an unknown directive by goModUnknownDirectiveError.
func TestModuleFromGoMod_KnownDirectivesNotFlaggedAsUnknown(t *testing.T) {
	content := "module example.com/foo\n\ngo 1.21\n\n" +
		"// a top-level comment\n" +
		"require golang.org/x/mod v0.19.0\n\n" +
		"exclude (\n\t// comment inside a block\n\tgolang.org/x/mod v0.1.0\n)\n\n" +
		"replace golang.org/x/mod => golang.org/x/mod v0.19.0\n\n" +
		"retract v0.0.1\n\n" +
		"tool golang.org/x/mod\n\n" +
		"godebug default=go1.21\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := moduleFromGoMod(path)
	if err != nil {
		t.Fatalf("unexpected error for a go.mod using every known directive: %v", err)
	}
	if want := "example.com/foo"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestModuleFromGoMod_InvalidQuotedTokenInOtherDirectives is a regression
// test for a real bug: parseModulePath already rejected a backtick- or
// single-quote-involving token in the module directive's own path
// (TestModuleFromGoMod_BacktickQuoted above), but that fix was scoped to
// moduleDirective's module-only scanner — a stray quote character in any
// OTHER go.mod directive's own argument (require/exclude/replace/retract/
// tool/godebug/ignore) sailed straight through unflagged, since
// moduleDirective only ever looks for "module" lines.
//
// Confirmed live (go1.24.4 and go1.26.8, GOPROXY=off): a go.mod otherwise
// reading only `module example.com/foo` / `go 1.21` plus one extra line
// like a backtick-quoted require version Fatals real `go list -m`/`go
// build` immediately and entirely offline with `invalid quoted string:
// unquoted string cannot contain quote`, before the module directive (or
// anything else in the file) is ever resolved, let alone a proxy
// contacted — confirmed for a backtick-quoted, single-quote-quoted, and
// bare-glued-quote shape, across require, exclude, replace, tool, and a
// require block entry.
//
// Before this fix, moduleFromGoMod extracted "example.com/foo" normally
// from each of these files and probed it against the live proxy as if
// the file were perfectly ordinary, reporting whatever the module's real
// proxy/sumdb state happened to be for a go.mod that could never resolve
// via any real `go` command in the first place, for a completely
// unrelated reason.
func TestModuleFromGoMod_InvalidQuotedTokenInOtherDirectives(t *testing.T) {
	for name, content := range map[string]string{
		"require, backtick-quoted version":       "module example.com/foo\n\ngo 1.21\n\nrequire example.com/bar `v1.0.0`\n",
		"require block entry, backtick-quoted":   "module example.com/foo\n\ngo 1.21\n\nrequire (\n\texample.com/bar `v1.0.0`\n)\n",
		"exclude, single-quote-quoted version":   "module example.com/foo\n\ngo 1.21\n\nexclude example.com/bar 'v1.0.0'\n",
		"replace, backtick-quoted new version":   "module example.com/foo\n\ngo 1.21\n\nreplace example.com/bar => example.com/baz `v1.0.0`\n",
		"tool, bare glued backtick, no verb sep": "module example.com/foo\n\ngo 1.21\n\ntool example.com/bar`baz`\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "go.mod")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := moduleFromGoMod(path)
			if err == nil {
				t.Fatalf("expected an error for a directive argument involving a stray quote, got module %q", got)
			}
			if !strings.Contains(err.Error(), "invalid quoted string") || !strings.Contains(err.Error(), "cannot contain quote") {
				t.Errorf("error %q doesn't cite real go's own `invalid quoted string: unquoted string cannot contain quote` Fatal", err)
			}
		})
	}
}

// TestModuleFromGoMod_InvalidQuotedTokenNotOverTriggered confirms the
// TestModuleFromGoMod_InvalidQuotedTokenInOtherDirectives fix stays
// narrowly scoped: a go.mod using every known directive, including a
// legitimately double-quoted require path/version, a retract range, and
// comments, must still resolve normally.
func TestModuleFromGoMod_InvalidQuotedTokenNotOverTriggered(t *testing.T) {
	content := "module example.com/foo\n\ngo 1.21\n\n" +
		"require (\n\t\"example.com/bar\" v1.0.0 // comment\n\texample.com/baz v1.2.3\n)\n\n" +
		"exclude example.com/old v0.1.0\n\n" +
		"replace example.com/bar => example.com/fork v1.0.0\n\n" +
		"retract [v1.0.0, v1.9.9]\n\n" +
		"tool example.com/foo/cmd/gen\n\n" +
		"godebug http2client=0\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := moduleFromGoMod(path)
	if err != nil {
		t.Fatalf("unexpected error for a go.mod with no stray quote characters: %v", err)
	}
	if want := "example.com/foo"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestModuleFromGoMod_RequireExcludeArgCount confirms moduleFromGoMod
// rejects a `require`/`exclude` directive carrying anything other than
// exactly two arguments (module path, version) — the version accidentally
// dropped by a hand-edit or merge conflict is the realistic shape, but a
// stray extra token is covered too — live-verified against a real
// go1.24.4 toolchain to Fatal `go list -m`/`go build` immediately and
// entirely offline with `usage: require/exclude module/path v1.2.3`,
// before ever contacting the proxy. Before this check, moduleFromGoMod's
// module-path scanner (moduleDirective) never looks at require/exclude
// lines at all, so a go.mod broken this way was extracted and probed
// normally, reporting whatever the module's real proxy state happened to
// be — confirmed live, a plain statusReady — for a go.mod that can never
// build in the first place.
func TestModuleFromGoMod_RequireExcludeArgCount(t *testing.T) {
	for name, content := range map[string]string{
		"require, version dropped":             "module example.com/foo\n\ngo 1.21\n\nrequire github.com/pkg/errors\n",
		"require, extra trailing token":        "module example.com/foo\n\ngo 1.21\n\nrequire github.com/pkg/errors v0.9.1 extra\n",
		"require block entry, version dropped": "module example.com/foo\n\ngo 1.21\n\nrequire (\n\tgithub.com/pkg/errors\n)\n",
		"exclude, version dropped":             "module example.com/foo\n\ngo 1.21\n\nexclude github.com/pkg/errors\n",
		"exclude block entry, version dropped": "module example.com/foo\n\ngo 1.21\n\nexclude (\n\tgithub.com/pkg/errors\n)\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "go.mod")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := moduleFromGoMod(path)
			if err == nil {
				t.Fatalf("expected an error for a require/exclude directive with the wrong argument count, got module %q", got)
			}
			if !strings.Contains(err.Error(), "usage:") || !strings.Contains(err.Error(), "module/path v1.2.3") {
				t.Errorf("error %q doesn't cite real go's own `usage: require/exclude module/path v1.2.3` Fatal", err)
			}
		})
	}
}

// TestModuleFromGoMod_RequireExcludeArgCountNotOverTriggered confirms the
// TestModuleFromGoMod_RequireExcludeArgCount fix stays narrowly scoped: an
// ordinary well-formed require (including one carrying a trailing "//
// indirect" comment, the common `go mod tidy` shape) and exclude line,
// single-line and inside a block, must still resolve normally.
func TestModuleFromGoMod_RequireExcludeArgCountNotOverTriggered(t *testing.T) {
	content := "module example.com/foo\n\ngo 1.21\n\n" +
		"require golang.org/x/mod v0.41.0 // indirect\n\n" +
		"require (\n\tgithub.com/pkg/errors v0.9.1\n\tgolang.org/x/sync v0.5.0 // indirect\n)\n\n" +
		"exclude example.com/old v0.1.0\n\n" +
		"exclude (\n\texample.com/older v0.0.1\n)\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := moduleFromGoMod(path)
	if err != nil {
		t.Fatalf("unexpected error for a go.mod with well-formed require/exclude directives: %v", err)
	}
	if want := "example.com/foo"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestModuleFromGoMod_ReplaceArgCount is a regression test for a real bug
// (testing-practice technique #201): moduleFromGoMod had no model at all of
// `replace`'s own argument grammar — real golang.org/x/mod/modfile's
// parseReplace (rule.go) Fatals unless the token right after the module
// path (and optional old version) is a literal "=>", with exactly the
// right number of arguments around it. requireExcludeDirectiveArgCountError
// deliberately left this verb out of its own "exactly two arguments" check
// (see its doc comment) since replace's grammar doesn't fit that shape at
// all — leaving it completely unchecked.
//
// Live-verified (2026-10-04, go1.24.4, GOPROXY=off): both shapes below
// Fatal real `go list -m`/`go build` immediately and entirely offline with
// `go.mod:N: usage: replace module/path [v1.2.3] => other/module v1.4`,
// before moduleDirective's own module-path extraction (or anything else in
// the file) is ever resolved. Before this fix, moduleFromGoMod extracted
// the module path normally and probed the live proxy as if the file were
// ordinary — confirmed live, a plain statusModuleUnknown verdict — for a
// go.mod that can never build at all.
func TestModuleFromGoMod_ReplaceArgCount(t *testing.T) {
	for name, content := range map[string]string{
		"missing arrow entirely":       "module example.com/foo\n\ngo 1.21\n\nreplace example.com/bar v1.0.0\n",
		"arrow present, nothing after": "module example.com/foo\n\ngo 1.21\n\nreplace example.com/bar => \n",
		"block entry, missing arrow":   "module example.com/foo\n\ngo 1.21\n\nreplace (\n\texample.com/bar v1.0.0\n)\n",
		"too many tokens after arrow":  "module example.com/foo\n\ngo 1.21\n\nreplace example.com/bar => example.com/baz v1.0.0 extra\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "go.mod")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := moduleFromGoMod(path)
			if err == nil {
				t.Fatalf("expected an error for a malformed replace directive, got module %q", got)
			}
			if !strings.Contains(err.Error(), "usage: replace module/path") {
				t.Errorf("error %q doesn't cite real go's own `usage: replace module/path ...` Fatal", err)
			}
		})
	}
}

// TestModuleFromGoMod_ReplaceArgCountNotOverTriggered confirms the above
// fix stays narrowly scoped: well-formed replace directives — with and
// without an old version, with and without a new version, single-line and
// inside a block, and spelling the target as either another module or a
// local directory path — must still resolve normally.
func TestModuleFromGoMod_ReplaceArgCountNotOverTriggered(t *testing.T) {
	content := "module example.com/foo\n\ngo 1.21\n\n" +
		"replace example.com/bar v1.0.0 => example.com/baz v1.0.0\n\n" +
		"replace example.com/qux => ../local/qux\n\n" +
		"replace (\n" +
		"\texample.com/a v1.0.0 => example.com/b v1.0.0\n" +
		"\texample.com/c => ../local/c\n" +
		")\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := moduleFromGoMod(path)
	if err != nil {
		t.Fatalf("unexpected error for a go.mod with well-formed replace directives: %v", err)
	}
	if want := "example.com/foo"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestModuleFromGoMod_RetractArgCount is a regression test for a real bug
// (testing-practice technique #201), the `retract` sibling of
// TestModuleFromGoMod_ReplaceArgCount: real golang.org/x/mod/modfile's
// parseVersionInterval (rule.go) requires retract's argument to be either a
// single bare version, or a bracketed "[low, high]" interval, with nothing
// else on the line — a shape requireExcludeDirectiveArgCountError's own doc
// comment deliberately leaves out of its "exactly two arguments" check.
//
// Live-verified (2026-10-04, go1.24.4, GOPROXY=off): `retract v1.0.0
// v2.0.0` (a bare range, missing the required brackets — a realistic typo)
// Fatals with `go.mod:N: unexpected token after version: "v2.0.0"`, and a
// bare `retract` with no argument Fatals with `go.mod:N: expected '[' or
// version` — both entirely offline, before moduleDirective or anything
// else in the file is ever resolved. Before this fix, moduleFromGoMod
// extracted the module path normally and probed the live proxy, reporting
// a plain statusModuleUnknown verdict for a go.mod that can never build.
func TestModuleFromGoMod_RetractArgCount(t *testing.T) {
	for name, content := range map[string]string{
		"bare range missing brackets":   "module example.com/foo\n\ngo 1.21\n\nretract v1.0.0 v2.0.0\n",
		"no argument at all":            "module example.com/foo\n\ngo 1.21\n\nretract\n",
		"unclosed bracket":              "module example.com/foo\n\ngo 1.21\n\nretract [v1.0.0\n",
		"missing comma in range":        "module example.com/foo\n\ngo 1.21\n\nretract [v1.0.0 v2.0.0]\n",
		"missing closing bracket":       "module example.com/foo\n\ngo 1.21\n\nretract [v1.0.0, v2.0.0\n",
		"block entry, missing brackets": "module example.com/foo\n\ngo 1.21\n\nretract (\n\tv1.0.0 v2.0.0\n)\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "go.mod")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := moduleFromGoMod(path)
			if err == nil {
				t.Fatalf("expected an error for a malformed retract directive, got module %q", got)
			}
			if !strings.Contains(err.Error(), "retract") {
				t.Errorf("error %q doesn't mention the malformed retract directive", err)
			}
		})
	}
}

// TestModuleFromGoMod_RetractArgCountNotOverTriggered confirms the above
// fix stays narrowly scoped: a well-formed bare-version retract and a
// well-formed bracketed-range retract — with and without spaces around the
// brackets/comma, single-line and inside a block, with and without a
// trailing rationale comment — must still resolve normally.
func TestModuleFromGoMod_RetractArgCountNotOverTriggered(t *testing.T) {
	content := "module example.com/foo\n\ngo 1.21\n\n" +
		"retract v1.0.0\n\n" +
		"retract [v1.1.0, v1.2.0]\n\n" +
		"retract [v1.3.0,v1.4.0]\n\n" +
		"retract (\n" +
		"\tv1.5.0 // mistake\n" +
		"\t[v1.6.0, v1.7.0] // bad release\n" +
		")\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := moduleFromGoMod(path)
	if err != nil {
		t.Fatalf("unexpected error for a go.mod with well-formed retract directives: %v", err)
	}
	if want := "example.com/foo"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestModuleFromGoMod_BlockComment is a regression test for a real bug:
// go.mod has no block-comment syntax at all — golang.org/x/mod/modfile's
// lexer Fatals unconditionally the moment it sees "/*" outside a quoted
// string, regardless of whether it ever finds a closing "*/". Confirmed
// live (go1.24.4 and go1.26.8, GOPROXY=off): every shape below Fatals real
// `go list -m`/`go build` immediately and entirely offline with
// `go.mod:N: mod files must use // comments (not /* */ comments)`, before
// resolving the module directive or anything else.
//
// Before this fix, moduleFromGoMod had no model of block comments at all: a
// bare "/*" line was misdiagnosed by goModUnknownDirectiveError as an
// "unknown directive: /*" (a real goproxycheck error message, just the
// wrong one — real go never reaches its unknown-directive check for this
// input), and a "/*"/"*/" pair sitting inside a require(...) block's own
// entries was misdiagnosed even more confusingly by
// requireExcludeDirectiveArgCountError as a bogus "3 argument(s) instead of
// exactly two" arg-count mismatch, treating the comment delimiters as
// ordinary tokens.
func TestModuleFromGoMod_BlockComment(t *testing.T) {
	for name, content := range map[string]string{
		"bare block comment on its own line":        "module example.com/foo\n\ngo 1.21\n\n/*\nrequire example.com/bar v1.0.0\n*/\n",
		"block comment before the module directive": "/* leading */\nmodule example.com/foo\n\ngo 1.21\n",
		"block comment trailing a require line":     "module example.com/foo\n\ngo 1.21\n\nrequire example.com/bar v1.0.0 /* trailing */\n",
		"unterminated block comment":                "module example.com/foo\n\ngo 1.21\n/* never closed\n",
		"block comment inside a require(...) block": "module example.com/foo\n\ngo 1.21\n\nrequire (\n\texample.com/bar v1.0.0\n\t/* comment */\n\texample.com/baz v2.0.0\n)\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "go.mod")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := moduleFromGoMod(path)
			if err == nil {
				t.Fatalf("expected an error for a go.mod containing a \"/* */\" block comment, got module %q", got)
			}
			if strings.Contains(err.Error(), "unknown directive") || strings.Contains(err.Error(), "instead of exactly two") {
				t.Errorf("error %q blames the wrong check (unknown-directive or arg-count) instead of real go's own block-comment Fatal", err)
			}
			if !strings.Contains(err.Error(), "mod files must use // comments") {
				t.Errorf("error %q doesn't cite real go's own `mod files must use // comments (not /* */ comments)` Fatal", err)
			}
		})
	}
}

// TestModuleFromGoMod_BlockCommentNotOverTriggered confirms the
// TestModuleFromGoMod_BlockComment fix stays narrowly scoped: a "/*"-looking
// substring that's either inside a quoted string or past a "//" line
// comment on the same line must not be mistaken for a live block comment.
func TestModuleFromGoMod_BlockCommentNotOverTriggered(t *testing.T) {
	for name, content := range map[string]string{
		"a line comment whose own text happens to contain '/*'": "module example.com/foo\n\ngo 1.21\n\n// has /* inside a line comment\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "go.mod")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := moduleFromGoMod(path)
			if err != nil {
				t.Fatalf("unexpected error for a go.mod with no live block comment: %v", err)
			}
			if want := "example.com/foo"; got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}

	// A "/*"-looking substring fully inside a quoted module path is real
	// go's own CheckPath problem ("invalid char '*'"), not a block-comment
	// Fatal — confirmed live the quoted-string lexer case never re-enters
	// the "/*"-checking scan until the matching close quote.
	t.Run("a '/*'-looking substring inside a quoted module path", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "go.mod")
		content := "module \"example.com/foo/*oddpath*/bar\"\n\ngo 1.21\n"
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}

		_, err := moduleFromGoMod(path)
		if err == nil {
			t.Fatal("expected an error (an invalid module path), got none")
		}
		if strings.Contains(err.Error(), "mod files must use // comments") {
			t.Errorf("error %q wrongly treated a quoted-string substring as a live block comment", err)
		}
		if !strings.Contains(err.Error(), "invalid char") {
			t.Errorf("error %q doesn't cite real go's own CheckPath `invalid char` Fatal", err)
		}
	})
}

// TestGoModBlockCommentError_Priority pins the real-go-confirmed ordering
// between a block comment and every other syntax-level condition this file
// checks for: a lexer-level error ("/*") always wins over a parse-level one
// (an unknown directive, a malformed quoted token, a wrong argument count),
// regardless of which line each sits on. Only a leading UTF-8 BOM — checked
// ahead of goModBlockCommentError in moduleFromGoMod, since it breaks the
// very first character the lexer reads — outranks it.
func TestGoModBlockCommentError_Priority(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{
			name:    "unknown directive on an earlier line than the block comment",
			content: "module example.com/foo\n\ngo 1.21\nbogusverb something\n/*\nfoo\n*/\n",
		},
		{
			name:    "block comment on an earlier line than the unknown directive",
			content: "module example.com/foo\n\ngo 1.21\n/*\nfoo\n*/\nbogusverb something\n",
		},
		{
			name:    "require missing its version on an earlier line than the block comment",
			content: "module example.com/foo\n\ngo 1.21\nrequire example.com/bar\n/*\nfoo\n*/\n",
		},
		{
			name:    "invalid quoted token on an earlier line than the block comment",
			content: "module example.com/foo\n\ngo 1.21\nrequire example.com/bar `v1.0.0`\n/*\nfoo\n*/\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "go.mod")
			if err := os.WriteFile(path, []byte(c.content), 0o644); err != nil {
				t.Fatal(err)
			}

			got, err := moduleFromGoMod(path)
			if err == nil {
				t.Fatalf("expected an error, got module %q", got)
			}
			if !strings.Contains(err.Error(), "mod files must use // comments") {
				t.Errorf("error %q doesn't cite the block-comment Fatal, which real go always reports first regardless of source order", err)
			}
		})
	}
}

// TestHasIgnoreDirective pins hasIgnoreDirective's presence-only scan,
// independent of the argument-shape questions TestModuleFromGoMod_
// MalformedIgnoreDirective/WellFormedIgnoreDirectiveNotRejected already
// cover.
func TestHasIgnoreDirective(t *testing.T) {
	cases := []struct {
		name string
		data string
		want bool
	}{
		{"no ignore directive at all", "module example.com/foo\n\ngo 1.26\n", false},
		{"single-line", "module example.com/foo\n\ngo 1.26\n\nignore ./vendor\n", true},
		{"block form", "module example.com/foo\n\ngo 1.26\n\nignore (\n\t./vendor\n)\n", true},
		{"no-space-before-paren block", "module example.com/foo\n\ngo 1.26\n\nignore(\n\t./vendor\n)\n", true},
		{"bare, no argument", "module example.com/foo\n\ngo 1.26\n\nignore\n", true},
		{"a module path that merely starts with the substring 'ignore'", "module example.com/ignoreme\n\ngo 1.26\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasIgnoreDirective(c.data); got != c.want {
				t.Errorf("hasIgnoreDirective(%q) = %v, want %v", c.data, got, c.want)
			}
		})
	}
}

// TestGoDirectiveVersion pins goDirectiveVersion's extraction of a go.mod's
// own `go` directive version string.
func TestGoDirectiveVersion(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"ordinary", "module example.com/foo\n\ngo 1.26.8\n", "1.26.8"},
		{"two-component version", "module example.com/foo\n\ngo 1.21\n", "1.21"},
		{"trailing comment", "module example.com/foo\n\ngo 1.21 // pinned\n", "1.21"},
		{"tab separator", "module example.com/foo\n\ngo\t1.21\n", "1.21"},
		{"no go directive at all", "module example.com/foo\n", ""},
		{"godebug is not mistaken for go", "module example.com/foo\n\ngodebug httpmux=1\n", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := goDirectiveVersion(c.data); got != c.want {
				t.Errorf("goDirectiveVersion(%q) = %q, want %q", c.data, got, c.want)
			}
		})
	}
}

// TestGoVersionAtLeast pins goVersionAtLeast's major.minor comparison,
// including the "go"-prefixed form `go env GOVERSION` actually returns.
func TestGoVersionAtLeast(t *testing.T) {
	cases := []struct {
		version      string
		major, minor int
		want         bool
	}{
		{"1.25.0", 1, 25, true},
		{"go1.25.0", 1, 25, true},
		{"1.26.8", 1, 25, true},
		{"1.25", 1, 25, true},
		{"1.24.4", 1, 25, false},
		{"go1.24.4", 1, 25, false},
		{"2.0", 1, 25, true},
		{"0.9", 1, 25, false},
		{"", 1, 25, false},
		{"1", 1, 25, false},
		{"garbage", 1, 25, false},
		// Regression cases for a real bug (run #683): a Go prerelease
		// version glues its "rc"/"beta" suffix directly onto the minor
		// number with no separating dot (internal/gover's own doc comment:
		// "Go toolchain versions like 1.21.0 and 1.21rc1") — confirmed
		// live (2026-10-03) that `go env GOVERSION` for a real, officially
		// downloaded go1.25rc1 toolchain prints exactly "go1.25rc1", and
		// that toolchain's own golang.org/x/mod/modfile already recognizes
		// the `ignore` go.mod directive (`go list -m all` succeeds against
		// a go.mod with `go 1.21` and an `ignore` line). Pre-fix, a plain
		// strconv.Atoi on the glued "25rc1"/"26rc2" segment failed
		// outright, so every one of these returned false regardless of how
		// new the prerelease actually was.
		{"go1.25rc1", 1, 25, true},
		{"go1.25beta1", 1, 25, true},
		{"go1.26rc2", 1, 25, true},
		{"1.25rc1", 1, 25, true},
		// A prerelease of an OLDER line must still compare as older —
		// confirming the fix doesn't just treat any "rc"/"beta" suffix as
		// unconditionally satisfying.
		{"go1.24rc1", 1, 25, false},
	}
	for _, c := range cases {
		t.Run(c.version, func(t *testing.T) {
			if got := goVersionAtLeast(c.version, c.major, c.minor); got != c.want {
				t.Errorf("goVersionAtLeast(%q, %d, %d) = %v, want %v", c.version, c.major, c.minor, got, c.want)
			}
		})
	}
}

// TestLeadingInt directly exercises goVersionAtLeast's new helper, covering
// the glued-prerelease-suffix shape (see goVersionAtLeast's doc comment) in
// isolation from the version-comparison logic built on top of it.
func TestLeadingInt(t *testing.T) {
	cases := []struct {
		s      string
		want   int
		wantOk bool
	}{
		{"25", 25, true},
		{"25rc1", 25, true},
		{"26beta2", 26, true},
		{"0", 0, true},
		{"", 0, false},
		{"rc1", 0, false},
		{"abc", 0, false},
	}
	for _, c := range cases {
		t.Run(c.s, func(t *testing.T) {
			n, ok := leadingInt(c.s)
			if n != c.want || ok != c.wantOk {
				t.Errorf("leadingInt(%q) = (%d, %v), want (%d, %v)", c.s, n, ok, c.want, c.wantOk)
			}
		})
	}
}

// --- goDirectiveUnsatisfiableError: can the go.mod's own `go` directive
// even be satisfied by what's locally resolvable, independent of whether
// ignore/tool/godebug are used at all ---
//
// Regression tests for a real bug: real cmd/go resolves which toolchain
// will run a module-aware command (GOTOOLCHAIN, default "auto") as its
// very first step, before parsing a single go.mod directive. A go.mod
// whose own `go` line can't be satisfied here Fatals immediately and
// entirely offline, for a reason that has nothing to do with
// ignore/tool/godebug — but moduleFromGoMod's no-argument-mode callers had
// no general-purpose check for this at all pre-fix, only the
// directive-specific ignoreDirectiveTooOldError/toolDirectiveTooOldError/
// godebugDirectiveTooOldError family below, none of which fire for a plain
// go.mod with no such directive. Live-verified (2026-10-05): a go.mod
// reading only `module golang.org/x/mod` / `go 1.99.0` (no
// ignore/tool/godebug anywhere) makes real `go list -m`/`go build` Fatal
// identically under both GOPROXY=off and the real default GOPROXY with
// `go: download go1.99.0 for linux/amd64: toolchain not available`, and
// under GOTOOLCHAIN=local with `go: go.mod requires go >= 1.99.0 (running
// go 1.24.4; GOTOOLCHAIN=local)` — while pre-fix goproxycheck sailed past
// both and reported `golang.org/x/mod@<tag> is live on both
// proxy.golang.org and sum.golang.org — a plain go install will work` for
// a directory where the user's own `go install` never even reaches that
// module.

// TestGoDirectiveUnsatisfiableError_NoGoDirective confirms a go.mod with no
// `go` directive at all is never flagged — this check has nothing to say
// about a file it doesn't apply to.
func TestGoDirectiveUnsatisfiableError_NoGoDirective(t *testing.T) {
	if err := goDirectiveUnsatisfiableError("module example.com/foo\n", "go1.24.4", "auto"); err != nil {
		t.Errorf("unexpected error for a go.mod with no 'go' directive: %v", err)
	}
}

// TestGoDirectiveUnsatisfiableError_LocalVersionSatisfies confirms the
// ordinary case — the go.mod's own declared `go` version is at or below
// what's locally selected — is never flagged.
func TestGoDirectiveUnsatisfiableError_LocalVersionSatisfies(t *testing.T) {
	if err := goDirectiveUnsatisfiableError("module example.com/foo\n\ngo 1.21\n", "go1.24.4", "auto"); err != nil {
		t.Errorf("unexpected error when the locally selected toolchain (go1.24.4) already satisfies the declared go1.21: %v", err)
	}
}

// TestGoDirectiveUnsatisfiableError_UnresolvableLocalVersion is a
// regression test for the "GOTOOLCHAIN=auto couldn't obtain a toolchain new
// enough" shape: an empty localVersion means `go env GOVERSION` itself
// Fataled, which only happens when the go.mod's own `go` directive demands
// a toolchain auto-switching couldn't resolve (no network, GOPROXY can't
// serve golang.org/toolchain, or the version isn't real at all) — live-
// verified (2026-10-05, go1.24.4) with a go.mod declaring `go 1.99.0`.
func TestGoDirectiveUnsatisfiableError_UnresolvableLocalVersion(t *testing.T) {
	err := goDirectiveUnsatisfiableError("module golang.org/x/mod\n\ngo 1.99.0\n", "", "auto")
	if err == nil {
		t.Fatal("expected an error when the go.mod's own `go` directive couldn't be resolved locally, got none")
	}
	if !strings.Contains(err.Error(), "toolchain not available") {
		t.Errorf("error %q doesn't cite real go's own `toolchain not available` Fatal", err)
	}
}

// TestGoDirectiveUnsatisfiableError_LocalVersionTooOld is a regression test
// for the "GOTOOLCHAIN restricted (local/path), running toolchain too old"
// shape: `go env GOVERSION` succeeds (no network attempted at all), but
// reports a version strictly older than the go.mod's own declared minimum.
// Live-verified (2026-10-05, GOTOOLCHAIN=local) real go Fatals with
// `go.mod requires go >= 1.99.0 (running go 1.24.4; GOTOOLCHAIN=local)`.
func TestGoDirectiveUnsatisfiableError_LocalVersionTooOld(t *testing.T) {
	err := goDirectiveUnsatisfiableError("module golang.org/x/mod\n\ngo 1.99.0\n", "go1.24.4", "local")
	if err == nil {
		t.Fatal("expected an error when the locally selected toolchain is older than the go.mod's own declared minimum, got none")
	}
	if !strings.Contains(err.Error(), "go.mod requires go >= 1.99.0 (running go 1.24.4; GOTOOLCHAIN=local)") {
		t.Errorf("error %q doesn't cite real go's own `go.mod requires go >= ...` Fatal verbatim", err)
	}
}

// TestModuleFromGoMod_GoDirectiveUnresolvableToolchain is the
// moduleFromGoMod-level counterpart of
// TestGoDirectiveUnsatisfiableError_UnresolvableLocalVersion, confirming
// the check is wired into the real no-argument CLI entry point. GOPROXY is
// pinned to "off" so the real toolchain-switch attempt Fatals fast and
// deterministically without depending on network access in this test run
// (confirmed live that GOPROXY=off produces the identical `toolchain not
// available` Fatal as a real, reachable GOPROXY would for an unreal
// version like go1.99.0).
func TestModuleFromGoMod_GoDirectiveUnresolvableToolchain(t *testing.T) {
	t.Setenv("GOPROXY", "off")
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "go.mod")
	content := "module golang.org/x/mod\n\ngo 1.99.0\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := moduleFromGoMod(path)
	if err == nil {
		t.Fatalf("expected an error for a go.mod whose own `go` directive can't be resolved locally, got module %q", got)
	}
	if !strings.Contains(err.Error(), "toolchain not available") {
		t.Errorf("error %q doesn't cite real go's own `toolchain not available` Fatal", err)
	}
}

// TestModuleFromGoMod_GoDirectiveTooNewForRestrictedToolchain is the
// moduleFromGoMod-level counterpart of
// TestGoDirectiveUnsatisfiableError_LocalVersionTooOld, confirming the
// "GOTOOLCHAIN restricted, running toolchain too old" shape is also wired
// into the real entry point, with GOTOOLCHAIN=local pinned so this runs
// fully offline (no toolchain-switch attempt at all).
func TestModuleFromGoMod_GoDirectiveTooNewForRestrictedToolchain(t *testing.T) {
	if systemPath := systemGoDir(t); systemPath != "" {
		t.Setenv("PATH", systemPath+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	t.Setenv("GOTOOLCHAIN", "local")
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "go.mod")
	content := "module golang.org/x/mod\n\ngo 1.99.0\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := moduleFromGoMod(path)
	if err == nil {
		t.Fatalf("expected an error for a go.mod requiring a newer go than GOTOOLCHAIN=local allows, got module %q", got)
	}
	if !strings.Contains(err.Error(), "go.mod requires go >=") {
		t.Errorf("error %q doesn't cite real go's own `go.mod requires go >= ...` Fatal", err)
	}
}

// TestIgnoreDirectiveTooOldError_PrereleaseLocalToolchainSatisfies is a
// regression test for the same real bug as TestGoVersionAtLeast's
// "go1.25rc1"-shaped cases, exercised through ignoreDirectiveTooOldError
// itself (the only caller of goVersionAtLeast, and the function a
// prerelease-toolchain CI run would actually hit): a go.mod whose own `go`
// line stays below 1.25 must NOT be flagged when the toolchain actually
// selected to run it is a go1.25+ prerelease — live-verified (2026-10-03)
// that a real go1.25rc1 toolchain parses this exact file fine (see
// goVersionAtLeast's doc comment for the full live transcript). Pre-fix,
// this returned a false "unknown directive: ignore" error for a file the
// real, currently-selected toolchain handles correctly right now.
func TestIgnoreDirectiveTooOldError_PrereleaseLocalToolchainSatisfies(t *testing.T) {
	if err := ignoreDirectiveTooOldError("module example.com/foo\n\ngo 1.21\n\nignore ./testdata\n", "go1.25rc1"); err != nil {
		t.Errorf("unexpected error when the locally selected toolchain (go1.25rc1, a real go1.25+ prerelease) already recognizes 'ignore': %v", err)
	}
}

// TestIgnoreDirectiveTooOldError_NoIgnoreDirective confirms a go.mod with no
// `ignore` directive at all is never flagged, regardless of its own `go`
// line or the (deliberately low, to prove it's irrelevant here) local
// version passed in — this check has nothing to say about a file it
// doesn't apply to.
func TestIgnoreDirectiveTooOldError_NoIgnoreDirective(t *testing.T) {
	if err := ignoreDirectiveTooOldError("module example.com/foo\n\ngo 1.21\n", "go1.20.0"); err != nil {
		t.Errorf("unexpected error for a go.mod with no 'ignore' directive: %v", err)
	}
}

// TestIgnoreDirectiveTooOldError_OwnGoDirectiveSatisfies confirms a go.mod
// whose own `go` directive already requires go1.25 or newer is never
// flagged — passing a deliberately low localVersion ("go1.20.0") proves the
// own-`go`-directive short-circuit fires before localVersion is even
// consulted.
func TestIgnoreDirectiveTooOldError_OwnGoDirectiveSatisfies(t *testing.T) {
	if err := ignoreDirectiveTooOldError("module example.com/foo\n\ngo 1.26\n\nignore ./vendor\n", "go1.20.0"); err != nil {
		t.Errorf("unexpected error for a go.mod whose own 'go' directive (1.26) already satisfies go1.25+: %v", err)
	}
}

// TestIgnoreDirectiveTooOldError_TooOld is a regression test for a real bug
// shared with sibling tool goprivaudit (fixed there in v0.1.82, this
// project's testing-practice technique #116): a go.mod whose own `go`
// directive stays below 1.25 falls back to the toolchain that would
// actually run it (ordinarily localGoVersion()'s live `go env GOVERSION`
// result, pinned here to a literal string instead — see
// ignoreDirectiveTooOldError's doc comment for why this function takes
// that as a parameter rather than calling localGoVersion() itself) — and
// when that's also below 1.25, `ignore` isn't recognized as a go.mod
// directive at all: a real `go list -m`/`go build` Fatals with `unknown
// directive: ignore`, entirely offline, before resolving anything — even
// though the directive's own single argument is perfectly well-formed.
// Live-verified (2026-10-03): a from-scratch go.mod reading only `module
// example.com/foo`, `go 1.21`, and `ignore ./vendor` makes a real go1.24.4
// (GOTOOLCHAIN=auto, no override) Fatal instantly this exact way,
// GOPROXY=off, zero network access. `go mod edit -ignore=path`, run with a
// newer local toolchain, is a realistic way to produce exactly this
// shape: it does not bump the file's own `go` line to cover the directive
// it just added (also live-verified, with a real go1.26.8 toolchain).
func TestIgnoreDirectiveTooOldError_TooOld(t *testing.T) {
	err := ignoreDirectiveTooOldError("module example.com/foo\n\ngo 1.21\n\nignore ./vendor\n", "go1.24.4")
	if err == nil {
		t.Fatal("expected an error for an 'ignore' directive older than both the file's own `go` line and the local toolchain, got none")
	}
	if !strings.Contains(err.Error(), "unknown directive: ignore") {
		t.Errorf("error %q doesn't cite real go's own `unknown directive: ignore` Fatal", err)
	}
}

// TestIgnoreDirectiveTooOldError_LocalVersionSatisfies is the mirror image
// of TestIgnoreDirectiveTooOldError_TooOld: the file's own `go` directive
// is too low, but the toolchain actually selected to run it (e.g. because
// GOTOOLCHAIN=auto picked a newer one, or `-toolchain` pinned one) is
// go1.25+, so `ignore` resolves fine and there's nothing to flag.
func TestIgnoreDirectiveTooOldError_LocalVersionSatisfies(t *testing.T) {
	if err := ignoreDirectiveTooOldError("module example.com/foo\n\ngo 1.21\n\nignore ./vendor\n", "go1.26.8"); err != nil {
		t.Errorf("unexpected error when the locally selected toolchain (go1.26.8) satisfies go1.25+: %v", err)
	}
}

// TestIgnoreDirectiveTooOldError_UnresolvableLocalVersion confirms the
// fail-open convention every other go-env-derived check in this file
// already uses: an empty localVersion (meaning the `go env GOVERSION` call
// itself failed) must not be treated as "too old."
func TestIgnoreDirectiveTooOldError_UnresolvableLocalVersion(t *testing.T) {
	if err := ignoreDirectiveTooOldError("module example.com/foo\n\ngo 1.21\n\nignore ./vendor\n", ""); err != nil {
		t.Errorf("unexpected error for an unresolvable local version: %v", err)
	}
}

// TestModuleFromGoMod_IgnoreDirectiveTooOldForToolchain is the
// moduleFromGoMod-level counterpart of TestIgnoreDirectiveTooOldError_
// TooOld, confirming the check is actually wired into the no-argument CLI
// mode's real entry point (via the real localGoVersion(), not a pinned
// string), not just reachable in isolation.
//
// Runs `go env GOVERSION` for real, so — like TestResolveTarget_
// FallbackToGoModAndGitTag just below — it `t.Chdir`s into a tempdir
// holding only the fixture go.mod, rather than running from this
// repository's own directory: goproxycheck's own go.mod pins go1.26.8,
// and GOTOOLCHAIN=auto's toolchain switch prepends that toolchain's own
// bin directory to PATH for the rest of the process tree — which would
// otherwise mask the exact "ambient toolchain is below go1.25" case this
// test needs, regardless of which directory `go env GOVERSION` itself
// runs in. PATH is reset to drop that prepended entry so the plain
// system `go` (confirmed live, 2026-10-03, as go1.24.4 on this box) is
// the one actually exec'd.
func TestModuleFromGoMod_IgnoreDirectiveTooOldForToolchain(t *testing.T) {
	if systemPath := systemGoDir(t); systemPath != "" {
		t.Setenv("PATH", systemPath+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "go.mod")
	content := "module example.com/foo\n\ngo 1.21\n\nignore ./vendor\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := moduleFromGoMod(path)
	if err == nil {
		t.Fatalf("expected an error for an 'ignore' directive too new for the selected toolchain, got module %q", got)
	}
	if !strings.Contains(err.Error(), "unknown directive: ignore") {
		t.Errorf("error %q doesn't cite real go's own `unknown directive: ignore` Fatal", err)
	}
}

// systemGoDir returns the directory containing the plain system `go`
// binary (resolved via the symlink chain from /usr/bin/go), or "" if that
// path doesn't exist — used only to put the real system toolchain ahead
// of any GOTOOLCHAIN-prepended entry already in PATH, see
// TestModuleFromGoMod_IgnoreDirectiveTooOldForToolchain.
func systemGoDir(t *testing.T) string {
	t.Helper()
	const systemGo = "/usr/bin/go"
	resolved, err := filepath.EvalSymlinks(systemGo)
	if err != nil {
		t.Skipf("no system go at %s to isolate from this repo's own GOTOOLCHAIN-selected one: %v", systemGo, err)
		return ""
	}
	return filepath.Dir(resolved)
}

// --- tool/godebug toolchain-gating (mirrors the `ignore` family above) ---
//
// goModKnownVerbs already listed `tool`/`godebug` as unconditionally
// recognized, with no equivalent of ignoreDirectiveTooOldError's
// toolchain-version gating at all — a go.mod with a `tool`/`godebug`
// directive and a `go` line too old, audited by a locally selected
// toolchain also too old, sailed through moduleFromGoMod unflagged, when
// the real toolchain Fatals immediately with `unknown directive: tool`/
// `unknown directive: godebug` before resolving anything. Live-verified
// against real installed golang.org/dl toolchain binaries (go1.21.0,
// go1.22.0, go1.23.0, go1.24.4) — see toolDirectiveTooOldError's and
// godebugDirectiveTooOldError's own doc comments for the exact transcripts.

// TestToolDirectiveTooOldError_NoToolDirective confirms a go.mod with no
// `tool` directive at all is never flagged, regardless of its own `go` line
// or the (deliberately low, to prove it's irrelevant here) local version.
func TestToolDirectiveTooOldError_NoToolDirective(t *testing.T) {
	if err := toolDirectiveTooOldError("module example.com/foo\n\ngo 1.20\n", "go1.20.0"); err != nil {
		t.Errorf("unexpected error for a go.mod with no 'tool' directive: %v", err)
	}
}

// TestToolDirectiveTooOldError_OwnGoDirectiveSatisfies confirms a go.mod
// whose own `go` directive already requires go1.24 or newer is never
// flagged — a deliberately low localVersion proves the own-`go`-directive
// short-circuit fires before localVersion is even consulted.
func TestToolDirectiveTooOldError_OwnGoDirectiveSatisfies(t *testing.T) {
	if err := toolDirectiveTooOldError("module example.com/foo\n\ngo 1.24\n\ntool example.com/foo/cmd/bar\n", "go1.20.0"); err != nil {
		t.Errorf("unexpected error for a go.mod whose own 'go' directive (1.24) already satisfies go1.24+: %v", err)
	}
}

// TestToolDirectiveTooOldError_TooOld is the direct regression test: a
// go.mod whose own `go` directive stays below 1.24 falls back to the
// toolchain that would actually run it, and when that's also below 1.24,
// `tool` isn't recognized as a go.mod directive at all. Live-verified
// (2026-10-04): a from-scratch go.mod reading only `module example.com/foo`,
// `go 1.20`, and `tool example.com/foo/cmd/bar` makes real go1.21.0/
// go1.22.0/go1.23.0 (GOTOOLCHAIN=local, GOPROXY=off) Fatal instantly with
// `go.mod:5: unknown directive: tool`, zero network access, while the
// identical file parses clean under go1.24.4.
func TestToolDirectiveTooOldError_TooOld(t *testing.T) {
	err := toolDirectiveTooOldError("module example.com/foo\n\ngo 1.20\n\ntool example.com/foo/cmd/bar\n", "go1.23.0")
	if err == nil {
		t.Fatal("expected an error for a 'tool' directive older than both the file's own `go` line and the local toolchain, got none")
	}
	if !strings.Contains(err.Error(), "unknown directive: tool") {
		t.Errorf("error %q doesn't cite real go's own `unknown directive: tool` Fatal", err)
	}
}

// TestToolDirectiveTooOldError_LocalVersionSatisfies is the mirror image of
// TestToolDirectiveTooOldError_TooOld: the file's own `go` directive is too
// low, but the toolchain actually selected to run it is go1.24+, so `tool`
// resolves fine.
func TestToolDirectiveTooOldError_LocalVersionSatisfies(t *testing.T) {
	if err := toolDirectiveTooOldError("module example.com/foo\n\ngo 1.20\n\ntool example.com/foo/cmd/bar\n", "go1.24.4"); err != nil {
		t.Errorf("unexpected error when the locally selected toolchain (go1.24.4) satisfies go1.24+: %v", err)
	}
}

// TestToolDirectiveTooOldError_UnresolvableLocalVersion confirms the
// fail-open convention every other go-env-derived check in this file
// already uses: an empty localVersion must not be treated as "too old."
func TestToolDirectiveTooOldError_UnresolvableLocalVersion(t *testing.T) {
	if err := toolDirectiveTooOldError("module example.com/foo\n\ngo 1.20\n\ntool example.com/foo/cmd/bar\n", ""); err != nil {
		t.Errorf("unexpected error for an unresolvable local version: %v", err)
	}
}

// TestHasToolDirective_ToolchainNotMistakenForTool is a false-positive
// guard: "toolchain" shares "tool" as a literal prefix, but must never be
// mistaken for a `tool` directive by hasToolDirective's CutPrefix check.
func TestHasToolDirective_ToolchainNotMistakenForTool(t *testing.T) {
	if hasToolDirective("module example.com/foo\n\ngo 1.24\n\ntoolchain go1.24.4\n") {
		t.Error("hasToolDirective should not match a 'toolchain' directive")
	}
}

// TestGodebugDirectiveTooOldError_NoGodebugDirective confirms a go.mod with
// no `godebug` directive at all is never flagged.
func TestGodebugDirectiveTooOldError_NoGodebugDirective(t *testing.T) {
	if err := godebugDirectiveTooOldError("module example.com/foo\n\ngo 1.17\n", "go1.17.0"); err != nil {
		t.Errorf("unexpected error for a go.mod with no 'godebug' directive: %v", err)
	}
}

// TestGodebugDirectiveTooOldError_OwnGoDirectiveSatisfies confirms a go.mod
// whose own `go` directive already requires go1.23 or newer is never
// flagged.
func TestGodebugDirectiveTooOldError_OwnGoDirectiveSatisfies(t *testing.T) {
	if err := godebugDirectiveTooOldError("module example.com/foo\n\ngo 1.23\n\ngodebug default=go1.20\n", "go1.20.0"); err != nil {
		t.Errorf("unexpected error for a go.mod whose own 'go' directive (1.23) already satisfies go1.23+: %v", err)
	}
}

// TestGodebugDirectiveTooOldError_TooOld is the direct regression test:
// live-verified (2026-10-04) that a from-scratch go.mod reading only
// `module example.com/foo`, `go 1.20`, and `godebug default=go1.20` makes
// real go1.21.0/go1.22.0 (GOTOOLCHAIN=local, GOPROXY=off) Fatal instantly
// with `go.mod:5: unknown directive: godebug`, zero network access, while
// the identical file parses clean under go1.23.0/go1.24.4 — `godebug`'s
// real boundary is go1.23, not go1.21 (an earlier version of this file's
// own goModKnownVerbs comment claimed go1.21+ without checking against a
// real pre-1.23 toolchain).
func TestGodebugDirectiveTooOldError_TooOld(t *testing.T) {
	err := godebugDirectiveTooOldError("module example.com/foo\n\ngo 1.20\n\ngodebug default=go1.20\n", "go1.22.0")
	if err == nil {
		t.Fatal("expected an error for a 'godebug' directive older than both the file's own `go` line and the local toolchain, got none")
	}
	if !strings.Contains(err.Error(), "unknown directive: godebug") {
		t.Errorf("error %q doesn't cite real go's own `unknown directive: godebug` Fatal", err)
	}
}

// TestGodebugDirectiveTooOldError_LocalVersionSatisfies is the mirror image
// of TestGodebugDirectiveTooOldError_TooOld: the file's own `go` directive
// is too low, but the toolchain actually selected is go1.23+, so `godebug`
// resolves fine.
func TestGodebugDirectiveTooOldError_LocalVersionSatisfies(t *testing.T) {
	if err := godebugDirectiveTooOldError("module example.com/foo\n\ngo 1.20\n\ngodebug default=go1.20\n", "go1.23.0"); err != nil {
		t.Errorf("unexpected error when the locally selected toolchain (go1.23.0) satisfies go1.23+: %v", err)
	}
}

// TestGodebugDirectiveTooOldError_UnresolvableLocalVersion confirms the
// fail-open convention every other go-env-derived check in this file
// already uses.
func TestGodebugDirectiveTooOldError_UnresolvableLocalVersion(t *testing.T) {
	if err := godebugDirectiveTooOldError("module example.com/foo\n\ngo 1.20\n\ngodebug default=go1.20\n", ""); err != nil {
		t.Errorf("unexpected error for an unresolvable local version: %v", err)
	}
}

// oldGoDir returns the bin directory of a golang.org/dl-downloaded Go SDK
// version (e.g. "go1.22.0"), found at the standard `$HOME/sdk/<version>`
// install location that `go run golang.org/dl/go1.22.0@latest && go1.22.0
// download` leaves it in, or Skips the calling test if that SDK isn't
// present on this machine. Needed because — unlike `ignore`'s own
// moduleFromGoMod-level test above, which could rely on this sandbox's
// plain system go (go1.24.4) already sitting below ignore's 1.25 boundary
// — go1.24.4 is NOT below tool's (1.24) or godebug's (1.23) own boundary,
// so exercising the real too-old path end-to-end (as opposed to the
// pinned-localVersion unit tests above, which cover the same logic without
// needing any particular toolchain installed) needs a genuinely older real
// toolchain binary.
func oldGoDir(t *testing.T, version string) string {
	t.Helper()
	dir := filepath.Join(os.Getenv("HOME"), "sdk", version, "bin")
	if _, err := os.Stat(filepath.Join(dir, "go")); err != nil {
		t.Skipf("no %s SDK at %s to exercise the real too-old-toolchain path end-to-end (download via `go run golang.org/dl/%s@latest && %s download`); the pinned-localVersion unit tests above already cover the same logic without it: %v", version, dir, version, version, err)
		return ""
	}
	return dir
}

// TestModuleFromGoMod_ToolDirectiveTooOldForToolchain is the
// moduleFromGoMod-level counterpart of TestToolDirectiveTooOldError_TooOld,
// confirming the check is actually wired into the no-argument CLI mode's
// real entry point via a real, genuinely-too-old go1.22.0 toolchain binary
// (not a pinned string) put ahead of PATH.
func TestModuleFromGoMod_ToolDirectiveTooOldForToolchain(t *testing.T) {
	oldGo := oldGoDir(t, "go1.22.0")
	t.Setenv("PATH", oldGo+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "go.mod")
	content := "module example.com/foo\n\ngo 1.20\n\ntool example.com/foo/cmd/bar\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := moduleFromGoMod(path)
	if err == nil {
		t.Fatalf("expected an error for a 'tool' directive too new for the selected toolchain, got module %q", got)
	}
	if !strings.Contains(err.Error(), "unknown directive: tool") {
		t.Errorf("error %q doesn't cite real go's own `unknown directive: tool` Fatal", err)
	}
}

// TestModuleFromGoMod_ToolDirectiveModernToolchainStillOk proves the fix
// doesn't overreach: the identical shape of go.mod, but with this
// sandbox's plain system go (go1.24.4, which already recognizes `tool`)
// ahead of PATH, must resolve with no error at all.
func TestModuleFromGoMod_ToolDirectiveModernToolchainStillOk(t *testing.T) {
	if systemPath := systemGoDir(t); systemPath != "" {
		t.Setenv("PATH", systemPath+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "go.mod")
	content := "module example.com/foo\n\ngo 1.20\n\ntool example.com/foo/cmd/bar\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := moduleFromGoMod(path)
	if err != nil {
		t.Fatalf("unexpected error for a 'tool' directive on a modern (go1.24+) toolchain: %v", err)
	}
	if got != "example.com/foo" {
		t.Errorf("got module %q, want example.com/foo", got)
	}
}

// TestModuleFromGoMod_GodebugDirectiveTooOldForToolchain is the
// moduleFromGoMod-level counterpart of
// TestGodebugDirectiveTooOldError_TooOld, using a real, genuinely-too-old
// go1.22.0 toolchain binary (below godebug's own 1.23 boundary).
func TestModuleFromGoMod_GodebugDirectiveTooOldForToolchain(t *testing.T) {
	oldGo := oldGoDir(t, "go1.22.0")
	t.Setenv("PATH", oldGo+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "go.mod")
	content := "module example.com/foo\n\ngo 1.20\n\ngodebug default=go1.20\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := moduleFromGoMod(path)
	if err == nil {
		t.Fatalf("expected an error for a 'godebug' directive too new for the selected toolchain, got module %q", got)
	}
	if !strings.Contains(err.Error(), "unknown directive: godebug") {
		t.Errorf("error %q doesn't cite real go's own `unknown directive: godebug` Fatal", err)
	}
}

// TestModuleFromGoMod_GodebugDirectiveModernToolchainStillOk proves the fix
// doesn't overreach, the godebug counterpart of
// TestModuleFromGoMod_ToolDirectiveModernToolchainStillOk.
func TestModuleFromGoMod_GodebugDirectiveModernToolchainStillOk(t *testing.T) {
	if systemPath := systemGoDir(t); systemPath != "" {
		t.Setenv("PATH", systemPath+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join(dir, "go.mod")
	content := "module example.com/foo\n\ngo 1.20\n\ngodebug default=go1.20\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := moduleFromGoMod(path)
	if err != nil {
		t.Fatalf("unexpected error for a 'godebug' directive on a modern (go1.23+) toolchain: %v", err)
	}
	if got != "example.com/foo" {
		t.Errorf("got module %q, want example.com/foo", got)
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

// TestResolveTarget_GoToolchainPseudoModuleNotFalselyRejected is a regression
// test for a real bug: "go" and "toolchain" are reserved pseudo-module names
// real cmd/go special-cases ahead of any ordinary module-path validation
// (modfetch/repo.go's lookup: `switch path { case "go", "toolchain": ...
// }`), redirecting to the real golang.org/toolchain module under the hood —
// `go help get` documents `go get go@latest` and `go get toolchain@patch`
// directly as the way to bump a go.mod's minimum Go version or toolchain.
// Confirmed live (2026-10-02, go1.24.4): `go install go@latest` and `go get
// toolchain@patch` both resolve the module successfully (`go install
// go@latest` only fails afterward, on "does not contain package go" — the
// module lookup itself already succeeded).
//
// Before this check, resolveTarget ran modulepkg.CheckPath("go") /
// ("toolchain") like any ordinary module path, which fails with "missing dot
// in first path element" (neither looks like a domain-rooted import path) —
// so goproxycheck told the caller that a real `go get`/`go install` "rejects
// this exact string immediately with `malformed module path ...: missing dot
// in first path element`", an outright false claim: real go never runs that
// check for these two literal strings at all, since it recognizes them
// first. This test only guards against the false "malformed module path"
// claim and confirms the error is distinct for the two cases, not that
// goproxycheck actually resolves a go/toolchain target — it still doesn't
// (see the fix's own doc comment in resolveTarget for why: Go-toolchain
// versions aren't semver, and "latest"/"patch" resolution depends on the
// locally running toolchain's own version, neither of which this tool's
// ordinary module-version machinery can safely reuse).
func TestResolveTarget_GoToolchainPseudoModuleNotFalselyRejected(t *testing.T) {
	for _, module := range []string{"go", "toolchain"} {
		t.Run(module, func(t *testing.T) {
			_, _, err := resolveTarget([]string{module + "@1.27.1"})
			if err == nil {
				t.Fatalf("expected resolveTarget to decline %q@version (goproxycheck doesn't check it), got no error", module)
			}
			if strings.Contains(err.Error(), "malformed module path") || strings.Contains(err.Error(), "missing dot in first path element") {
				t.Errorf("error falsely claims real `go` rejects %q as a malformed module path, but `go get %s@1.27.1` is real, documented syntax (go help get) that resolves fine: %v", module, module, err)
			}
			if !strings.Contains(err.Error(), "go get "+module+"@1.27.1") {
				t.Errorf("error should name the real, working `go get %s@1.27.1` equivalent so the caller knows this isn't a syntax error: %v", module, err)
			}
		})
	}
}

// TestResolveTarget_UppercaseGoNotTreatedAsToolchainPseudoModule guards
// against an over-broad fix to the check above: cmd/go's special-casing of
// "go"/"toolchain" is an exact, case-sensitive string match (confirmed live:
// `go install Go@latest` fails with the ordinary `malformed module path
// "Go": missing dot in first path element`, since real go does NOT
// special-case "Go"), so resolveTarget must not treat "Go" or "Toolchain" as
// the reserved pseudo-module names either.
func TestResolveTarget_UppercaseGoNotTreatedAsToolchainPseudoModule(t *testing.T) {
	_, _, err := resolveTarget([]string{"Go@1.27.1"})
	if err == nil {
		t.Fatal(`expected an error for "Go@1.27.1" (not a valid module path either way)`)
	}
	if !strings.Contains(err.Error(), "malformed module path") {
		t.Errorf("error should still cite the ordinary malformed-module-path rejection for \"Go\" (not special-cased like \"go\"): %v", err)
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

// TestResolveTarget_NoneVersionRejected is a regression test for a real bug
// found by testing goproxycheck against real documented Go version queries
// beyond "latest"/"upgrade"/"patch": "none" (go.dev/ref/mod#version-queries)
// is the "empty" version query — it removes a module's requirement (or is a
// no-op if there wasn't one) rather than naming any real, fetchable version.
// Confirmed live (2026-09-30) that `go get module@none` succeeds immediately
// and touches the network not at all, even under GOPROXY=off and even for a
// module that doesn't exist, so there's no proxy/sumdb-availability question
// for this tool to answer. Before this check, goproxycheck sent "none" to
// the proxy as a literal version string, which 404s with the same "invalid
// version: unknown revision none" body a genuinely bogus revision gets, and
// reported statusUnknownRevision ("Check for a typo in the version, tag, or
// commit hash...") and exited 1 — the opposite of reality, since a real `go
// get module@none` for that same module always succeeds trivially.
func TestResolveTarget_NoneVersionRejected(t *testing.T) {
	_, _, err := resolveTarget([]string{"example.com/mod@none"})
	if err == nil {
		t.Fatal(`expected an error for version "none", which this tool has nothing to check`)
	}
	if !strings.Contains(err.Error(), "none") {
		t.Errorf("error should mention the none query: %v", err)
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

// TestResolveTarget_NonASCIIVersionLetterGetsAccurateMessage is a regression
// test for a real bug: a version/revision string containing a non-ASCII
// Unicode letter (e.g. an accented character or a non-Latin script) is NOT
// rejected by golang.org/x/mod/module's own validation the way an ordinary
// disallowed character is — fileNameOK's doc comment says plainly "we allow
// all Unicode letters" — so it passes that check cleanly and only fails one
// step later, inside EscapeVersion's own escapeString helper, with a bare
// "internal error: inconsistency in EscapePath" that cmd/go surfaces
// verbatim and UNWRAPPED, with no quoted version string and no "version %q
// invalid:" phrase at all. Confirmed live (2026-10-03, go1.26.8, entirely
// offline, before any proxy contact):
//
//	$ go get golang.org/x/mod@café
//	go: golang.org/x/mod@café: invalid version: internal error: inconsistency in EscapePath
//
// Before this check, resolveTarget's sibling EscapeVersion-failure branch
// (TestResolveTarget_DisallowedVersionCharsRejected, just below) claimed
// every EscapeVersion failure alike gets real go's "disallowed version
// string" wording — false for this exact input shape, actively misdirecting
// anyone trying to match goproxycheck's own quoted error text against their
// terminal's real output.
func TestResolveTarget_NonASCIIVersionLetterGetsAccurateMessage(t *testing.T) {
	for _, version := range []string{
		"v1.2.3-café", // accented Latin letter
		"日本語",         // non-Latin script
	} {
		t.Run(version, func(t *testing.T) {
			_, _, err := resolveTarget([]string{"example.com/mod@" + version})
			if err == nil {
				t.Fatalf("expected an error for non-ASCII version %q", version)
			}
			if strings.Contains(err.Error(), "invalid: disallowed version string") {
				t.Errorf("error wrongly quotes the ordinary \"...invalid: disallowed version string\" wording as what real `go` prints, which it does not for a non-ASCII Unicode letter: %v", err)
			}
			if !strings.Contains(err.Error(), "internal error: inconsistency in EscapePath") {
				t.Errorf("error should quote real go's actual, different verbatim message (\"internal error: inconsistency in EscapePath\"): %v", err)
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

// TestResolveTarget_NestedModuleSubdirTag_StripsPrefix is a regression test
// for a real bug: gitDescribeTag used to return a module-in-subdirectory's
// git tag exactly as written, including its required subdirectory prefix
// (real Go module versioning, go.dev/ref/mod#vcs-version, requires such
// tags to be named "<subdir>/vX.Y.Z") — but the *module version* real `go`
// and proxy.golang.org actually index it under is only the "vX.Y.Z" part.
// Confirmed live against golang.org/x/tools/gopls (a real nested module):
// proxy.golang.org's own @latest response names the tag
// "refs/tags/gopls/v0.23.0" for module version "v0.23.0", and a direct
// request for "gopls/v0.23.0" as a literal version 404s ("invalid char
// '/'") while "v0.23.0" alone succeeds. Before the fix, running with no
// arguments from inside such a subdirectory right after tagging a real,
// already-live release reported "not-yet-indexed ... retry in a minute, or
// use --wait" — a doomed poll, since the raw prefixed string could never be
// indexed under that spelling no matter how long it was retried.
func TestResolveTarget_NestedModuleSubdirTag_StripsPrefix(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run("git", "init", "-q")
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "test")
	if err := os.MkdirAll(filepath.Join(dir, "gopls"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "gopls", "go.mod"), []byte("module golang.org/x/tools/gopls\n\ngo 1.21\n"), 0o644)
	run("git", "add", "gopls/go.mod")
	run("git", "commit", "-q", "-m", "init")
	run("git", "tag", "gopls/v0.23.0")

	t.Chdir(filepath.Join(dir, "gopls"))
	module, version, err := resolveTarget(nil)
	if err != nil {
		t.Fatal(err)
	}
	if module != "golang.org/x/tools/gopls" || version != "v0.23.0" {
		t.Errorf("got (%q, %q), want (%q, %q) — the subdirectory prefix should have been stripped from the tag", module, version, "golang.org/x/tools/gopls", "v0.23.0")
	}
}

// TestResolveTarget_NestedModuleSubdirTag_IgnoresForeignRootTag covers the
// case where a root-level version tag and this nested module's own
// correctly-prefixed tag both point at the same commit (e.g. a monorepo
// cutting simultaneous releases) — only the prefixed one is this module's
// own version; the bare root tag belongs to a different module entirely and
// must not be picked (and must not make the answer ambiguous either).
func TestResolveTarget_NestedModuleSubdirTag_IgnoresForeignRootTag(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run("git", "init", "-q")
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "test")
	if err := os.MkdirAll(filepath.Join(dir, "gopls"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module golang.org/x/tools\n\ngo 1.21\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "gopls", "go.mod"), []byte("module golang.org/x/tools/gopls\n\ngo 1.21\n"), 0o644)
	run("git", "add", "go.mod", "gopls/go.mod")
	run("git", "commit", "-q", "-m", "init")
	run("git", "tag", "v1.0.0")        // the repo-root module's own tag
	run("git", "tag", "gopls/v0.23.0") // the nested module's own tag

	t.Chdir(filepath.Join(dir, "gopls"))
	module, version, err := resolveTarget(nil)
	if err != nil {
		t.Fatal(err)
	}
	if module != "golang.org/x/tools/gopls" || version != "v0.23.0" {
		t.Errorf("got (%q, %q), want (%q, %q) — should have picked this module's own prefixed tag, not the sibling root tag", module, version, "golang.org/x/tools/gopls", "v0.23.0")
	}
}

// TestResolveTarget_NestedModuleSubdirTag_SoleForeignRootTagRejected is a
// regression test for a real bug in gitDescribeTag's single-tag fallback
// (the `case 0: if len(tags) == 1 { return tags[0], nil }` branch meant to
// treat a lone non-version tag as a revision identifier, e.g. "nightly"):
// that fallback ran unconditionally, with no awareness of gitTagPrefix()
// at all — unlike the len(versionTags) >= 1 branches right above it, which
// already require a tag to carry *this* module's own subdirectory prefix
// (see TestResolveTarget_NestedModuleSubdirTag_IgnoresForeignRootTag right
// above). So when HEAD's *only* tag is a sibling module's bare, unprefixed
// release tag (e.g. the repo-root module's "v1.0.0", with no
// "gopls/"-prefixed tag at HEAD at all — a realistic shape: the root
// module tagged a release on a commit the nested module hasn't tagged
// yet), the fallback silently treated that foreign tag as this module's own
// version instead of erroring out, exactly the "ambiguous root module" risk
// the sibling test already covers for the multiple-tag case — just via a
// different code path this fallback left open.
//
// Confirmed live (pre-fix) with the actual built binary in this exact
// layout: `goproxycheck --json` inside gopls/ reported `"version":
// "v1.0.0"` — golang.org/x/tools/gopls was never tagged v1.0.0 (its real
// release in the companion test is "gopls/v0.23.0"), so a real `go install
// golang.org/x/tools/gopls@v1.0.0` could never have matched what this tool
// just checked.
func TestResolveTarget_NestedModuleSubdirTag_SoleForeignRootTagRejected(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run("git", "init", "-q")
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "test")
	if err := os.MkdirAll(filepath.Join(dir, "gopls"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module golang.org/x/tools\n\ngo 1.21\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "gopls", "go.mod"), []byte("module golang.org/x/tools/gopls\n\ngo 1.21\n"), 0o644)
	run("git", "add", "go.mod", "gopls/go.mod")
	run("git", "commit", "-q", "-m", "init")
	run("git", "tag", "v1.0.0") // only the repo-root module's own tag — nothing "gopls/"-prefixed at all

	t.Chdir(filepath.Join(dir, "gopls"))
	_, _, err := resolveTarget(nil)
	if err == nil {
		t.Fatal("expected an error — HEAD's only tag belongs to a different module in this repo, not this one")
	}
}

// TestResolveTarget_NestedModuleSubdirTag_SoleNonVersionOwnTagStripsPrefix
// covers the legitimate counterpart to the regression test right above:
// gitDescribeTag's single-tag fallback should still treat a lone
// non-semver tag as a revision identifier for *this* module (the original,
// still-valid intent the fallback exists for at all) when that tag does
// carry this module's own subdirectory prefix — just with the prefix
// stripped first, the same way every version-shaped tag already is,
// instead of returning the raw, still-prefixed string.
func TestResolveTarget_NestedModuleSubdirTag_SoleNonVersionOwnTagStripsPrefix(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	run("git", "init", "-q")
	run("git", "config", "user.email", "test@example.com")
	run("git", "config", "user.name", "test")
	if err := os.MkdirAll(filepath.Join(dir, "gopls"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "gopls", "go.mod"), []byte("module golang.org/x/tools/gopls\n\ngo 1.21\n"), 0o644)
	run("git", "add", "gopls/go.mod")
	run("git", "commit", "-q", "-m", "init")
	run("git", "tag", "gopls/nightly") // this module's own tag, but not version-shaped

	t.Chdir(filepath.Join(dir, "gopls"))
	module, version, err := resolveTarget(nil)
	if err != nil {
		t.Fatal(err)
	}
	if module != "golang.org/x/tools/gopls" || version != "nightly" {
		t.Errorf("got (%q, %q), want (%q, %q) — should have stripped this module's own prefix from its non-version tag", module, version, "golang.org/x/tools/gopls", "nightly")
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

// TestGoproxyEmptyListError covers goproxyEmptyListError against the shapes
// confirmed live (2026-10-01) to make real cmd/go's own GOPROXY-list parsing
// (proxyList, cmd/go/internal/modfetch/proxy.go) fail outright with "GOPROXY
// list is not the empty string, but contains no entries": `GOPROXY=,` and
// `GOPROXY=" "` both fail `go install golang.org/x/mod@v0.19.0` this exact
// way, entirely offline, where the default config and a bare "off"/"direct"
// (each a single valid entry on its own, not an empty list) succeed past
// this check fine. A false positive here would make run() claim a fetch
// fails when it wouldn't, so the well-formed cases must report no error.
func TestGoproxyEmptyListError(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"default is well-formed", "https://proxy.golang.org,direct", false},
		{"off alone is one entry, not empty", "off", false},
		{"direct alone is one entry, not empty", "direct", false},
		{"custom proxy is well-formed", "https://goproxy.example.com", false},
		{"lone comma has zero entries", ",", true},
		{"lone pipe has zero entries", "|", true},
		{"whitespace only has zero entries", " ", true},
		{"doubled comma has zero entries", ",,", true},
		{"empty string has zero entries", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := goproxyEmptyListError(c.raw)
			if (err != nil) != c.wantErr {
				t.Errorf("goproxyEmptyListError(%q) = %v, want error: %v", c.raw, err, c.wantErr)
			}
		})
	}
}

// TestLocalGoproxyEmptyListError covers the go-env-reading wrapper the same
// way TestLocalGovcsConfigError covers localGovcsConfigError.
func TestLocalGoproxyEmptyListError(t *testing.T) {
	t.Setenv("GOPROXY", ",")
	if _, err := localGoproxyEmptyListError(); err == nil {
		t.Error("localGoproxyEmptyListError with GOPROXY=\",\" = nil, want an empty-list error")
	}

	t.Setenv("GOPROXY", "https://proxy.golang.org,direct")
	if _, err := localGoproxyEmptyListError(); err != nil {
		t.Errorf("localGoproxyEmptyListError with the default GOPROXY = %v, want nil", err)
	}
}

// TestGoproxyEntrySchemeError covers goproxyEntrySchemeError against the
// three shapes confirmed live (2026-10-02, both go1.24.4 and go1.27.1) to
// make real cmd/go's own newProxyRepo (modfetch/proxy.go) fail outright
// before ever issuing a request: a single bare word with no dot/colon/slash
// (so normalizeGoproxyURL's implicit-https rule doesn't apply) fails with
// "invalid proxy URL missing scheme"; a non-http(s)/file scheme fails with
// "invalid proxy URL scheme (must be https, http, file)"; a file:// URL
// carrying a query string (or other non-path component) fails with "invalid
// file:// proxy URL with non-path elements". A well-formed entry — the
// default public proxy, a bare host with a dot (normalized to https://), a
// custom https:// URL, or a bare file:// path — must report no error, or
// run() would wrongly Fatal a perfectly fetchable config.
func TestGoproxyEntrySchemeError(t *testing.T) {
	cases := []struct {
		name    string
		entry   string
		wantErr bool
	}{
		{"default public proxy", "https://proxy.golang.org", false},
		{"bare host with dot", "proxy.golang.org", false},
		{"custom https", "https://goproxy.example.com", false},
		{"custom http", "http://goproxy.example.com", false},
		{"file URL", "file:///tmp/fileproxy", false},
		{"bare word no dot", "localhost", true},
		{"bare word no dot, another", "myproxy", true},
		{"bare absolute path, no file scheme", "/tmp/fileproxy", true},
		{"non-http(s)/file scheme", "ftp://example.com", true},
		{"file URL with query string", "file:///tmp/fileproxy?x=1", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := goproxyEntrySchemeError(c.entry)
			if (err != nil) != c.wantErr {
				t.Errorf("goproxyEntrySchemeError(%q) = %v, want error: %v", c.entry, err, c.wantErr)
			}
		})
	}
}

// TestGoproxyMalformedEntryError covers goproxyMalformedEntryError's walk
// over the whole GOPROXY chain, not just the first entry: a real bug fix.
// Confirmed live (2026-10-02) that `GOPROXY="https://proxy.golang.org,localhost"
// go mod download golang.org/x/text@v0.14.0` — a fully healthy public-proxy
// entry first, a malformed one only ever meant as a fallback — fails
// outright with "invalid proxy URL missing scheme: localhost", never even
// attempting the healthy first entry; the same failure happens regardless of
// separator ("," or "|") or whether the malformed entry comes first or
// second. An entry placed after a terminating "off"/"direct" is never
// actually reached by real cmd/go's own parse (confirmed live:
// GOPROXY=off,localhost fails with the ordinary GOPROXY=off error, not a
// scheme error; GOPROXY=direct,localhost succeeds fetching an ordinary
// public module via direct VCS) and must not be flagged here either.
func TestGoproxyMalformedEntryError(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantEntry string
	}{
		{"default is well-formed", "https://proxy.golang.org,direct", ""},
		{"off alone", "off", ""},
		{"direct alone", "direct", ""},
		{"malformed only entry", "localhost", "localhost"},
		{"healthy first, malformed fallback, comma", "https://proxy.golang.org,localhost", "localhost"},
		{"healthy first, malformed fallback, pipe", "https://proxy.golang.org|localhost", "localhost"},
		{"malformed first, healthy fallback", "localhost,https://proxy.golang.org", "localhost"},
		{"malformed then off, off never changes the outcome", "localhost,off", "localhost"},
		{"off then malformed: off truncates first, never reached", "off,localhost", ""},
		{"direct then malformed: direct truncates first, never reached", "direct,localhost", ""},
		{"wrong scheme", "ftp://example.com,direct", "ftp://example.com"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			entry, err := goproxyMalformedEntryError(c.raw)
			if entry != c.wantEntry {
				t.Errorf("goproxyMalformedEntryError(%q) entry = %q, want %q", c.raw, entry, c.wantEntry)
			}
			if (err != nil) != (c.wantEntry != "") {
				t.Errorf("goproxyMalformedEntryError(%q) err = %v, want error: %v", c.raw, err, c.wantEntry != "")
			}
		})
	}
}

// TestLocalGoproxyMalformedEntryError covers the go-env-reading wrapper the
// same way TestLocalGoproxyEmptyListError covers localGoproxyEmptyListError.
func TestLocalGoproxyMalformedEntryError(t *testing.T) {
	t.Setenv("GOPROXY", "https://proxy.golang.org,localhost")
	if _, entry, err := localGoproxyMalformedEntryError(); err == nil || entry != "localhost" {
		t.Errorf("localGoproxyMalformedEntryError with a trailing malformed entry = (entry %q, err %v), want entry \"localhost\" and a non-nil error", entry, err)
	}

	t.Setenv("GOPROXY", "https://proxy.golang.org,direct")
	if _, entry, err := localGoproxyMalformedEntryError(); err != nil || entry != "" {
		t.Errorf("localGoproxyMalformedEntryError with the default GOPROXY = (entry %q, err %v), want (\"\", nil)", entry, err)
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

// TestGoAuthConfigError covers goAuthConfigError against the GOAUTH shapes
// confirmed live (2026-10-02) to make real cmd/go's own GOAUTH parsing
// (runGoAuth, cmd/go/internal/auth/auth.go) Fatal outright — entirely
// offline, before the process's first HTTPS request — and the well-formed
// shapes that must NOT trigger a false positive here (a false positive
// would make run() claim a perfectly good config is broken).
func TestGoAuthConfigError(t *testing.T) {
	absDir := t.TempDir()
	absFile := absDir + "/not-a-dir"
	if err := os.WriteFile(absFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		raw        string
		wantErr    bool
		wantSubstr string
	}{
		{"default netrc", "netrc", false, ""},
		{"off alone", "off", false, ""},
		{"custom command alone", "mycompany-auth-helper --flag", false, ""},
		{"netrc then custom command", "netrc;mycompany-auth-helper", false, ""},
		{"well-formed git with real absolute dir", "git " + absDir, false, ""},
		// Confirmed live: `GOAUTH="off;netrc" go install golang.org/x/text@v0.14.0`
		// against the real, live public proxy Fatals in ~3ms (vs. 1.3s for a
		// successful fetch) with exactly this message, before any request.
		{"off combined with netrc", "off;netrc", true, "cannot be combined"},
		{"netrc combined with off", "netrc;off", true, "cannot be combined"},
		// Confirmed live: a stray leading/trailing/doubled semicolon — a
		// natural copy-paste or templating typo — produces this exact Fatal.
		{"trailing semicolon", "netrc;", true, "empty command"},
		{"leading semicolon", ";netrc", true, "empty command"},
		{"doubled semicolon", "netrc;;netrc", true, "empty command"},
		{"whitespace-only entry", "netrc; ;netrc", true, "empty command"},
		{"empty string", "", true, "empty command"},
		{"git with no dir argument", "git", true, "absolute path to the git working directory"},
		{"git with extra argument", "git /abs/dir extra", true, "absolute path to the git working directory"},
		{"git with relative dir", "git relative/dir", true, "dir is not absolute"},
		{"git with nonexistent dir", "git " + absDir + "/does-not-exist", true, "cannot stat"},
		{"git with dir that is a file, not a directory", "git " + absFile, true, "dir is not a directory"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := goAuthConfigError(c.raw)
			if (err != nil) != c.wantErr {
				t.Fatalf("goAuthConfigError(%q) = %v, want error: %v", c.raw, err, c.wantErr)
			}
			if c.wantErr && !strings.Contains(err.Error(), c.wantSubstr) {
				t.Errorf("goAuthConfigError(%q) = %v, want it to contain %q", c.raw, err, c.wantSubstr)
			}
		})
	}
}

// TestLocalGoAuthConfigError covers the go-env-reading wrapper the same way
// TestLocalGoproxyEmptyListError covers localGoproxyEmptyListError.
func TestLocalGoAuthConfigError(t *testing.T) {
	t.Setenv("GOAUTH", "off;netrc")
	if err := localGoAuthConfigError(); err == nil {
		t.Error("localGoAuthConfigError with GOAUTH=\"off;netrc\" = nil, want an error")
	}

	t.Setenv("GOAUTH", "netrc")
	if err := localGoAuthConfigError(); err != nil {
		t.Errorf("localGoAuthConfigError with the default GOAUTH = %v, want nil", err)
	}
}
