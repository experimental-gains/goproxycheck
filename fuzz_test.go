package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
)

// FuzzModuleFromGoMod checks moduleFromGoMod's module-line parsing against
// golang.org/x/mod/modfile — the Go team's own canonical go.mod parser —
// as an oracle, using Go's native fuzzer to explore the module-line syntax
// space instead of a hand-written or corpus-derived set of cases. This
// function's parsing logic has already had three real bugs found against
// it by hand (trailing "//" comments, quoted paths, tab separators) — a
// fuzzer searches the same syntax space exhaustively instead of one case
// at a time.
func FuzzModuleFromGoMod(f *testing.F) {
	seeds := []string{
		"github.com/foo/bar",
		"github.com/foo/bar // comment",
		`"github.com/foo/bar"`,
		"github.com/foo/bar\t// tab before comment",
		`"github.com/foo/bar" // trailing comment after quote`,
		"github.com/foo/bar//no-space-comment",
		`"foo\"bar"`,
		"foo bar",
		``,
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, rest string) {
		// Keep the fuzzed text confined to the module line: a literal
		// newline would make it multi-line input, no longer an
		// apples-to-apples comparison of how one line gets parsed.
		if strings.ContainsAny(rest, "\n\r") {
			return
		}

		content := "module " + rest + "\n\ngo 1.24\n"

		mf, err := modfile.ParseLax("go.mod", []byte(content), nil)
		if err != nil || mf.Module == nil {
			return // not valid go.mod syntax per the canonical parser; no oracle to check against
		}
		want := mf.Module.Mod.Path
		if module.CheckPath(want) != nil {
			// Confirmed live: `go list -m`/`go build` refuse to load a
			// go.mod whose module path fails this check (e.g. "malformed
			// module path ...: double slash"), so no real, working repo
			// can ever produce this input — not a case worth matching.
			return
		}

		dir := t.TempDir()
		path := filepath.Join(dir, "go.mod")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := moduleFromGoMod(path)
		if err != nil {
			t.Fatalf("moduleFromGoMod errored on %q but golang.org/x/mod/modfile parsed it as module %q: %v", content, want, err)
		}
		if got != want {
			t.Errorf("moduleFromGoMod(%q) = %q, want %q (per golang.org/x/mod/modfile)", content, got, want)
		}
	})
}

// FuzzEscapePath checks escapePath against golang.org/x/mod/module.EscapePath
// — the same case-encoding scheme this package's doc comment cites as its
// source — restricted to inputs module.CheckPath accepts as a real module
// path, since that's the only shape escapePath is ever called with in probe().
func FuzzEscapePath(f *testing.F) {
	seeds := []string{
		"github.com/experimental-gains/modslop",
		"github.com/Azure/azure-sdk-for-go",
		"golang.org/x/mod",
		"gopkg.in/yaml.v2",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, path string) {
		if module.CheckPath(path) != nil {
			return
		}
		want, err := module.EscapePath(path)
		if err != nil {
			t.Fatalf("module.CheckPath(%q) passed but module.EscapePath errored: %v", path, err)
		}
		if got := escapePath(path); got != want {
			t.Errorf("escapePath(%q) = %q, want %q (per golang.org/x/mod/module.EscapePath)", path, got, want)
		}
	})
}
