package main

import "testing"

func TestEscapePath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"github.com/experimental-gains/modslop", "github.com/experimental-gains/modslop"},
		{"github.com/Azure/azure-sdk-for-go", "github.com/!azure/azure-sdk-for-go"},
		{"v1.2.3", "v1.2.3"},
		{"v0.1.0-BETA", "v0.1.0-!b!e!t!a"},
	}
	for _, c := range cases {
		if got := escapePath(c.in); got != c.want {
			t.Errorf("escapePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
