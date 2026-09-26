package main

import "testing"

// TestGovcsAllowsGit covers govcsAllowsGit's rule-matching against cases
// confirmed live against a real toolchain (GOPROXY=direct go mod download
// -x github.com/golang/protobuf@v1.5.0, comparing a real "GOVCS disallows
// using git for ..." failure against a successful direct-fetch trace — see
// govcs.go's doc comment for the exact commands run).
func TestGovcsAllowsGit(t *testing.T) {
	cases := []struct {
		name    string
		module  string
		private bool
		raw     string
		want    bool
	}{
		{"unset GOVCS, public module, default allows git", "github.com/golang/protobuf", false, "", true},
		{"unset GOVCS, private module, default allows anything", "github.com/myorg/priv", true, "", true},
		{"public:hg only, git not in list, blocks", "github.com/golang/protobuf", false, "public:hg", false},
		{"public:git|hg, git in list, allows", "github.com/golang/protobuf", false, "public:git|hg", true},
		{"public:off blocks a public module", "github.com/golang/protobuf", false, "public:off", false},
		{"private:off blocks a private module", "github.com/myorg/priv", true, "private:off", false},
		{"private:all allows a private module", "github.com/myorg/priv", true, "private:all", true},
		{
			"specific pattern before public/private wins even though it's more specific",
			"github.com/golang/protobuf", false,
			"github.com:off,public:git|hg", false,
		},
		{
			"public/private rule before a specific pattern still wins (order, not specificity)",
			"github.com/golang/protobuf", false,
			"public:off,github.com:git", false,
		},
		{
			"non-matching specific pattern falls through to public/private",
			"github.com/golang/protobuf", false,
			"evil.com:off,public:git|hg", true,
		},
		{"malformed rule (no colon) is skipped, falls through to default", "github.com/golang/protobuf", false, "garbage", true},
		{"blank entries in the list are skipped", "github.com/golang/protobuf", false, ",public:git|hg,", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := govcsAllowsGit(c.module, c.private, c.raw); got != c.want {
				t.Errorf("govcsAllowsGit(%q, %v, %q) = %v, want %v", c.module, c.private, c.raw, got, c.want)
			}
		})
	}
}

// TestLocalGovcsAllowsGit covers the go-env-reading wrapper the same way
// TestLocalGoproxyOff covers firstGoproxyEntry: t.Setenv is safe since
// exec.Command inherits the process environment and `go env` picks up an
// explicit env var over any persisted `go env -w` value.
func TestLocalGovcsAllowsGit(t *testing.T) {
	t.Setenv("GOVCS", "public:hg")
	if got := localGovcsAllowsGit("github.com/golang/protobuf", false); got {
		t.Errorf("localGovcsAllowsGit with GOVCS=public:hg = %v, want false", got)
	}
	t.Setenv("GOVCS", "public:git|hg")
	if got := localGovcsAllowsGit("github.com/golang/protobuf", false); !got {
		t.Errorf("localGovcsAllowsGit with GOVCS=public:git|hg = %v, want true", got)
	}
}
