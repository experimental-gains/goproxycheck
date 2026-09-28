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
		{
			// Confirmed live (2026-09-27, `GOPROXY=direct go mod tidy -x`
			// against github.com/golang/protobuf@v1.5.0 with a fresh
			// GOMODCACHE): real cmd/go's parseGOVCS splits a rule on its
			// *first* colon only, so "github.com:hg:git" is pattern
			// "github.com", vcslist ["hg:git"] (a single VCS name that never
			// matches "git"), and every git fetch under github.com fails
			// with "GOVCS disallows using git for public
			// github.com/golang/protobuf; see 'go help vcs'". Splitting on
			// the *last* colon instead (the bug this rule catches) misreads
			// it as pattern "github.com:hg" (which never matches a real
			// module path) with vcslist "git", so the rule never matches and
			// execution wrongly falls through to "public:git|hg".
			"vcslist containing a colon is part of the pattern's first-colon split, not the last",
			"github.com/golang/protobuf", false,
			"github.com:hg:git,public:git|hg", false,
		},
		{
			// Confirmed live (2026-09-27, `GOPROXY=direct go get -x`
			// against github.com/golang/protobuf@v1.5.0 with a fresh
			// GOMODCACHE): real cmd/go trims whitespace around the colon
			// in each rule, so "public : off" behaves exactly like
			// "public:off" and blocks. This function used to compare the
			// untrimmed pattern "public " against the literal "public",
			// which never matched, so the rule was silently skipped.
			"whitespace around the colon is trimmed, same as real go",
			"github.com/golang/protobuf", false,
			"public : off,private:all", false,
		},
		{
			// Confirmed live the same way with `GOVCS="public: git | hg"`:
			// real cmd/go also trims each "|"-separated VCS name, so
			// " git " still matches "git".
			"whitespace around a pipe-separated VCS name is trimmed too",
			"github.com/golang/protobuf", false,
			"public: git | hg", true,
		},
		{
			// Confirmed live (2026-09-28, `GOPROXY=direct go mod download -x`
			// against github.com/golang/protobuf@v1.5.0 with a fresh
			// GOMODCACHE): real cmd/go's govcsConfig.allow doesn't treat
			// "all" as meaningful only when it's the *entire* vcslist string
			// — it splits vcslist the same way regardless of what's in it,
			// then permits the fetch the moment any individual item equals
			// the VCS name or "all". With GOVCS="public:hg|all", the direct
			// fetch succeeds (full git ls-remote/fetch/archive trace, no
			// "GOVCS disallows" error) because "all" is one of the two
			// alternatives, even though "hg" (the other one) doesn't itself
			// include git. This function used to only special-case vcslist
			// == "all" as an exact match against the whole string, so
			// "hg|all" fell through to a loop that only ever looked for a
			// literal "git" entry — never recognizing "all" as one of the
			// pipe-separated alternatives — and wrongly reported the fetch
			// as disallowed.
			"all as one alternative in a pipe-separated vcslist still allows git",
			"github.com/golang/protobuf", false,
			"public:hg|all", true,
		},
		{
			"all as one alternative works for a private module's rule too",
			"github.com/myorg/priv", true,
			"private:off|all", true,
		},
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

// TestLocalGovcsPrivate_UsesRepoRootNotFullModulePath is the fix for a real
// bug found via live-toolchain differential testing (2026-09-28): real
// cmd/go's checkGOVCS (internal/vcs/vcs.go) computes its private/public
// classification from `module.MatchPrefixPatterns(cfg.GOPRIVATE, root)`,
// where root is the VCS-resolved repository root — for github.com, always
// exactly the first two path segments (vcsPaths' `^(?P<root>github\.com/
// [\w.\-]+/[\w.\-]+)(/[\w.\-]+)*$`), discarding anything past that,
// including a major-version suffix like "/v2" that lives in a real
// subdirectory of the repo (as github.com/googleapis/gax-go/v2 actually
// does).
//
// Confirmed live: with GOPRIVATE="github.com/googleapis/gax-go/v2" (naming
// the exact module path) and GOVCS="public:off,private:git", `go mod
// download -x github.com/googleapis/gax-go/v2@v2.7.0` fails with "GOVCS
// disallows using git for public github.com/googleapis/gax-go" — real go
// classified it public (root "github.com/googleapis/gax-go" doesn't match
// the GOPRIVATE pattern) despite the pattern matching the full module path
// exactly. localGovcsPrivate used to match against module directly, so it
// reported private here — the opposite of what real go does.
func TestLocalGovcsPrivate_UsesRepoRootNotFullModulePath(t *testing.T) {
	t.Setenv("GOPRIVATE", "github.com/googleapis/gax-go/v2")
	if got := localGovcsPrivate("github.com/googleapis/gax-go/v2"); got {
		t.Errorf(`localGovcsPrivate("github.com/googleapis/gax-go/v2") with GOPRIVATE naming that exact path = %v, want false (real go classifies it public: the repo root "github.com/googleapis/gax-go" doesn't match)`, got)
	}
	// Sanity check the other direction: a GOPRIVATE pattern that does cover
	// the repo root (not just the full module path) must still count as
	// private, exactly like real go.
	t.Setenv("GOPRIVATE", "github.com/googleapis/gax-go")
	if got := localGovcsPrivate("github.com/googleapis/gax-go/v2"); !got {
		t.Errorf(`localGovcsPrivate("github.com/googleapis/gax-go/v2") with GOPRIVATE naming the repo root = %v, want true`, got)
	}
}

// TestGovcsConfigError covers govcsConfigError against the five malformed
// shapes confirmed live against a real toolchain (2026-09-27, GOPROXY=direct
// against a fresh GOMODCACHE so the direct-VCS/checkGOVCS path is actually
// exercised — see govcsConfigError's doc comment for the exact commands and
// error text `go mod download -x` produced for each): a plain, well-formed
// config (and the unset/empty case) must report no error, since a false
// positive here would make run() claim a fetch fails when it wouldn't.
func TestGovcsConfigError(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"unset/empty is well-formed", "", false},
		{"ordinary two-rule config is well-formed", "private:all,public:git|hg", false},
		{"a normal single pattern rule is well-formed", "github.com/myorg/*:git", false},
		{"empty entry from a double comma", "public:git|hg,,private:all", true},
		{"leading/trailing comma also produces an empty entry", ",public:git|hg", true},
		{"entry missing its colon", "badrule,public:git|hg", true},
		{"empty pattern", ":git", true},
		{"empty VCS list", "public:", true},
		{"relative pattern", "./foo:git", true},
		{"duplicate pattern later in the list is unreachable", "public:git,public:hg", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := govcsConfigError(c.raw)
			if (err != nil) != c.wantErr {
				t.Errorf("govcsConfigError(%q) = %v, want error: %v", c.raw, err, c.wantErr)
			}
		})
	}
}

// TestLocalGovcsConfigError covers the go-env-reading wrapper the same way
// TestLocalGovcsAllowsGit covers localGovcsAllowsGit.
func TestLocalGovcsConfigError(t *testing.T) {
	t.Setenv("GOVCS", "badrule,public:git|hg")
	if err := localGovcsConfigError(); err == nil {
		t.Error("localGovcsConfigError with GOVCS=badrule,public:git|hg = nil, want a malformed-entry error")
	}
	t.Setenv("GOVCS", "public:git|hg")
	if err := localGovcsConfigError(); err != nil {
		t.Errorf("localGovcsConfigError with GOVCS=public:git|hg = %v, want nil", err)
	}
}
