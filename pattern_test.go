package main

import "testing"

func TestMatchesPrefixPattern(t *testing.T) {
	cases := []struct {
		pattern, module string
		want            bool
	}{
		{"github.com/myorg/*", "github.com/myorg/foo", true},
		{"github.com/myorg/*", "github.com/myorg/foo/bar", true},
		{"github.com/myorg/*", "github.com/otherorg/foo", false},
		{"github.com/myorg", "github.com/myorg/foo", true}, // prefix match, fewer segments
		{"github.com/myorg", "github.com/myorgtypo/foo", false},
		{"*", "anything.com/x/y", true},
		{"*.corp.example.com", "eng.corp.example.com/tools", true},
		{"*.corp.example.com", "example.com/tools", false},
		{"github.com/myorg/", "github.com/myorg/foo", true}, // trailing slash ignored
		{"", "github.com/myorg/foo", false},
		{`*\/0`, "0.0/0", true}, // backslash-escaped "/" glob syntax, see comment on matchesPrefixPattern
	}
	for _, c := range cases {
		got := matchesPrefixPattern(c.pattern, c.module)
		if got != c.want {
			t.Errorf("matchesPrefixPattern(%q, %q) = %v, want %v", c.pattern, c.module, got, c.want)
		}
	}
}

func TestMatchesAnyPattern(t *testing.T) {
	patterns := []string{"github.com/myorg/*", "example.com/other"}
	if !matchesAnyPattern("github.com/myorg/foo", patterns) {
		t.Error("expected match on first pattern")
	}
	if !matchesAnyPattern("example.com/other/sub", patterns) {
		t.Error("expected match on second pattern (prefix)")
	}
	if matchesAnyPattern("github.com/unrelated/foo", patterns) {
		t.Error("expected no match")
	}
	if matchesAnyPattern("github.com/myorg/foo", nil) {
		t.Error("expected no match against empty pattern list")
	}
}

// TestSplitPatterns checks that splitPatterns only splits on comma and
// drops genuinely empty entries, without trimming surrounding whitespace
// from the patterns it keeps — real `go`'s own
// golang.org/x/mod/module.MatchPrefixPatterns never trims a glob either
// (confirmed against that source), so a leading/trailing space around a
// pattern is part of the glob, not noise to clean up. See splitPatterns'
// doc comment: a version of this test that expected trimming used to lock
// in a real divergence from `go`'s behavior (a space-separated
// GOPRIVATE/GONOPROXY list silently failed to match the intended module).
func TestSplitPatterns(t *testing.T) {
	got := splitPatterns(" github.com/myorg/*, example.com/other ,,")
	want := []string{" github.com/myorg/*", " example.com/other "}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
	if splitPatterns("") != nil {
		t.Error("expected nil for empty input")
	}
}

// TestSplitPatterns_LeadingSpaceBreaksMatch is the regression case for the
// real bug found via live differential testing against golang.org/x/mod
// and `go mod download -x`: a pattern with a leading space (as produced by
// a comma-separated list written with ", " for readability, e.g.
// GOPRIVATE="nomatch/*, golang.org/x/text") must NOT match the unspaced
// module path, exactly like real `go` — confirmed live that
// `go mod download -x` fetches golang.org/x/text through proxy.golang.org
// under that exact GOPRIVATE value, rather than going direct to VCS.
func TestSplitPatterns_LeadingSpaceBreaksMatch(t *testing.T) {
	patterns := splitPatterns("nomatch/*, golang.org/x/text")
	if matchesAnyPattern("golang.org/x/text", patterns) {
		t.Errorf("matchesAnyPattern matched %q against patterns %v, but real `go` does not match a glob with a leading space against an unspaced module path", "golang.org/x/text", patterns)
	}
}
