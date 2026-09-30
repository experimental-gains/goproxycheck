package main

import "testing"

func TestEscapePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"github.com/experimental-gains/modslop", "github.com/experimental-gains/modslop"},
		{"github.com/Azure/azure-sdk-for-go", "github.com/!azure/azure-sdk-for-go"},
		{"v1.2.3", "v1.2.3"},
		{"v0.1.0-BETA", "v0.1.0-!b!e!t!a"},
		// Pins the `r <= 'Z'` boundary (found LIVED by mutation
		// testing, run #127: no existing case has a literal 'Z').
		{"github.com/foo/Zebra", "github.com/foo/!zebra"},
	}
	for _, c := range cases {
		if got := escapePath(c.in); got != c.want {
			t.Errorf("escapePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestUrlPathEscape pins urlPathEscape's behavior against the real cmd/go
// pathEscape algorithm it mirrors (modfetch/proxy.go): percent-encode
// everything url.PathEscape would, except restore a literal '/' (module
// paths use it as a real path separator, not data to escape).
func TestUrlPathEscape(t *testing.T) {
	cases := []struct{ in, want string }{
		{"v1.2.3", "v1.2.3"},
		// The bug this exists to fix: '#' is a real, unescaped byte
		// golang.org/x/mod/module.EscapeVersion allows in a version string
		// (see urlPathEscape's doc comment) — left unescaped, it would make
		// url.Parse treat everything from here on as a URL fragment.
		{"v1.2.3#issue456", "v1.2.3%23issue456"},
		// A literal '%' not part of a valid escape sequence would otherwise
		// make url.Parse fail outright with "invalid URL escape".
		{"v1.2.3%zz", "v1.2.3%25zz"},
		// '/' must survive unescaped (a real module path separator), even
		// though url.PathEscape alone would encode it to %2F.
		{"github.com/foo/!zebra", "github.com/foo/%21zebra"},
	}
	for _, c := range cases {
		if got := urlPathEscape(c.in); got != c.want {
			t.Errorf("urlPathEscape(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
