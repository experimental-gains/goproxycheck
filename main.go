// Command goproxycheck checks whether a Go module version is actually
// fetchable via the public module proxy and checksum database — the same
// two systems a plain `go install module@version` depends on — and
// diagnoses *why* when it isn't, instead of leaving you to guess whether to
// wait, retry, or re-tag.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, defaultEndpoints()))
}

func run(args []string, stdout, stderr io.Writer, ep endpoints) int {
	fs := flag.NewFlagSet("goproxycheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	wait := fs.Bool("wait", false, "poll until the version is ready (or --timeout elapses) instead of checking once")
	timeout := fs.Duration("timeout", 5*time.Minute, "max time to poll when --wait is set")
	interval := fs.Duration("interval", 15*time.Second, "how often to poll when --wait is set")
	jsonOut := fs.Bool("json", false, "print the diagnosis as JSON instead of text")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: goproxycheck [flags] [module@version]")
		_, _ = fmt.Fprintln(stderr, "  with no argument, reads the module path from ./go.mod and the version from `git describe --tags`")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	module, version, err := resolveTarget(fs.Args())
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "goproxycheck:", err)
		return 2
	}

	var d diagnosis
	if priv, pattern := localModulePrivate(module); priv {
		// Verified live: with GOPRIVATE (or GONOPROXY directly) set to a
		// pattern matching this module, `go install`/`go get`/`go mod
		// download` never contact proxy.golang.org at all for it — they
		// fetch directly from the VCS host instead (confirmed with `go mod
		// download -x`: a matching module shows a `git ls-remote` trace and
		// no proxy.golang.org request whatsoever, where the same module
		// without the match shows only proxy.golang.org GETs). Probing the
		// public proxy for a module like this would report false
		// module-unknown/not-yet-indexed verdicts — the proxy genuinely has
		// never heard of it, by design, regardless of whether the real `go
		// install` would succeed fine via direct VCS fetch right now.
		// Mirrors the existing GOPROXY=off short-circuit below: local
		// config makes the whole proxy probe moot, so skip it instead of
		// reporting on a system `go` itself won't consult.
		d = diagnosis{statusPrivateModuleLocally, fmt.Sprintf(
			"your local `GOPRIVATE`/`GONOPROXY` config matches %s via the pattern %q, so `go install`/`go get` will fetch it directly from its VCS host here, never through proxy.golang.org — "+
				"that's your machine's own config, not a proxy-availability problem, and this tool's proxy/sumdb checks don't apply to it. If you meant to check a *public* module instead, verify the module path doesn't accidentally match your GOPRIVATE pattern.",
			module, pattern)}
	} else if localGoproxyOff() {
		// Verified live: with GOPROXY=off (or its first comma/pipe-separated
		// entry), `go install`/`go mod download` refuse to fetch anything at
		// all — "module lookup disabled by GOPROXY=off" — regardless of what
		// proxy.golang.org itself has. Probing the public proxy in this case
		// would report a false "ready" (it did, before this check existed:
		// goproxycheck said `golang.org/x/mod@v0.30.0` was ready with
		// GOPROXY=off set, while the real `go install` in that exact
		// environment failed outright). Short-circuit instead of probing.
		d = diagnosis{statusGoproxyOffLocally, fmt.Sprintf(
			"your local `GOPROXY` is set to `off` (via env var or `go env -w`), so `go install`/`go get` will refuse to fetch %s@%s here at all — "+
				"that's your machine's own config, not a proxy-availability problem. Unset `GOPROXY` or point it at a real source "+
				"(e.g. `GOPROXY=https://proxy.golang.org,direct`) to install normally. This skips probing the proxy entirely: if %s@%s is "+
				"already sitting in your local module cache, `go install` can still succeed despite GOPROXY=off, since that bypasses the network fetch.",
			module, version, module, version)}
	} else if kind, custom := localGoproxyNonPublic(); kind == "direct" {
		// See localGoproxyNonPublic's doc comment. Same short-circuit shape
		// as the private-module/GOPROXY=off cases above: the public-proxy
		// probe below is moot when `go install` never talks to a proxy at
		// all for this fetch.
		d = diagnosis{statusGoproxyDirectLocally, fmt.Sprintf(
			"your local `GOPROXY` resolves to `direct` (via env var or `go env -w`), so `go install`/`go get` will fetch %s@%s straight from its VCS host here, never through proxy.golang.org — "+
				"that's your machine's own config, not a proxy-availability problem, and this tool's proxy/sumdb checks don't apply to it. A real failure here (auth, an unreachable host, a GOVCS restriction) would show up as its own error straight from `go`, not from this tool.",
			module, version)}
	} else if kind == "custom" {
		d = diagnosis{statusGoproxyCustomLocally, fmt.Sprintf(
			"your local `GOPROXY` is set to %q (via env var or `go env -w`), not the public proxy.golang.org — so `go install`/`go get` will fetch %s@%s from that proxy here, not the one this tool checks. "+
				"This tool only knows how to probe the public proxy.golang.org/sum.golang.org anonymously over plain HTTP; it has no way to know your custom proxy's auth or protocol quirks, so it can't tell you whether %[2]s@%[3]s is actually ready there. "+
				"If you're chasing a real failure, check your proxy's own logs/status instead of trusting this tool's result — or temporarily set GOPROXY=https://proxy.golang.org,direct to check against the public proxy specifically.",
			custom, module, version)}
	} else {
		deadline := time.Now().Add(*timeout)
		for {
			r := ep.probe(module, version)
			d = diagnose(r)
			if d.status == statusSumdbLag {
				// See localSumdbSkipped's doc comment: a local GOSUMDB=off
				// or a matching GONOSUMDB pattern means `go install` never
				// consults sum.golang.org for this module at all, so a
				// sumdb-lag verdict from the public sumdb doesn't reflect
				// what will actually happen here — the proxy already has
				// it, so it's ready right now.
				if skipped, reason := localSumdbSkipped(module); skipped {
					d = diagnosis{statusReady, fmt.Sprintf(
						"%s is live on proxy.golang.org. sum.golang.org doesn't have it yet, but your local %s means "+
							"`go install`/`go get` won't consult the checksum database for this module here at all, so that lag doesn't block you — a plain `go install` will work right now.",
						displayTarget(r), reason)}
				}
			}
			if !*wait || d.status == statusReady || d.status == statusModuleUnknown || d.status == statusBlocklistedMalicious || time.Now().After(deadline) {
				break
			}
			time.Sleep(*interval)
		}
	}

	if *jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]string{
			"module":  module,
			"version": version,
			"status":  string(d.status),
			"message": d.message,
		}); err != nil {
			_, _ = fmt.Fprintln(stderr, "goproxycheck:", err)
			return 2
		}
	} else if _, err := fmt.Fprintf(stdout, "%s@%s: %s\n%s\n", module, version, d.status, d.message); err != nil {
		_, _ = fmt.Fprintln(stderr, "goproxycheck:", err)
		return 2
	}

	if d.status == statusReady {
		return 0
	}
	return 1
}

// resolveTarget parses "module@version" from args, or falls back to reading
// the module path from ./go.mod and the version from the most recent git
// tag — the shape of "just tagged a release, is it live yet?" this tool is
// meant for.
func resolveTarget(args []string) (module, version string, err error) {
	if len(args) > 1 {
		return "", "", fmt.Errorf("expected at most one argument (module@version), got %d", len(args))
	}
	if len(args) == 1 {
		parts := strings.SplitN(args[0], "@", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("argument must be in module@version form, got %q", args[0])
		}
		return parts[0], parts[1], nil
	}

	module, err = moduleFromGoMod("go.mod")
	if err != nil {
		return "", "", fmt.Errorf("no argument given and couldn't read module path from ./go.mod: %w", err)
	}
	version, err = gitDescribeTag()
	if err != nil {
		return "", "", fmt.Errorf("no argument given and couldn't determine a git tag for HEAD: %w", err)
	}
	return module, version, nil
}

func moduleFromGoMod(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module"); ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
			mod := parseModulePath(strings.TrimSpace(rest))
			if mod == "" {
				// e.g. a "module" line whose entire value is a "//"
				// comment (found via mutation testing, run #125: the
				// comment-stripping boundary at index 0 is exercised,
				// but nothing checked what it produces). Silently
				// probing an empty module path would send a malformed
				// request to the proxy instead of a clear error.
				return "", fmt.Errorf("%s has a 'module' directive with no path: %q", path, line)
			}
			return mod, nil
		}
	}
	return "", fmt.Errorf("no 'module' directive found in %s", path)
}

// parseModulePath cleans up the raw text after "module " on a go.mod module
// line: strips a trailing "//" line comment (valid go.mod syntax — `go list
// -m` ignores it, but a naive TrimSpace would fold it straight into the
// module path and send goproxycheck probing a bogus URL) and unquotes the
// path if it's written as a quoted Go string literal (also valid go.mod
// syntax, just rarer).
func parseModulePath(s string) string {
	if i := strings.Index(s, "//"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if unquoted, err := strconv.Unquote(s); err == nil {
		return unquoted
	}
	return s
}

// firstGoproxyEntry returns the first comma/pipe-separated entry of the
// local `go` command's effective GOPROXY. Reads it via `go env GOPROXY`
// rather than os.Getenv("GOPROXY") directly, so a value persisted with `go
// env -w GOPROXY=...` is picked up too, not just an explicit env var — `go
// env` is the authoritative source either way.
//
// GOPROXY may be a comma- or pipe-separated list of sources tried in order,
// but only the *first* entry matters for both callers below: confirmed live
// that "off" is a definitive stop with no fallback to later entries
// (GOPROXY=off,direct and GOPROXY=off|direct both fail immediately with
// "module lookup disabled by GOPROXY=off", while GOPROXY=direct,off
// succeeds via direct and never reaches the off entry at all), and a
// comma-separated list only falls through to a later entry when the
// earlier one 404s/410s (a pipe-separated list falls through on any
// error) — either way, the first entry is what `go install` tries first
// and is what determines whether this tool's proxy.golang.org probe below
// even applies.
func firstGoproxyEntry() string {
	out, err := exec.Command("go", "env", "GOPROXY").Output()
	if err != nil {
		return "" // best-effort: don't block the real check on this
	}
	proxy := strings.TrimSpace(string(out))
	if i := strings.IndexAny(proxy, ",|"); i >= 0 {
		proxy = proxy[:i]
	}
	return proxy
}

// localGoproxyOff reports whether the local `go` command's effective
// GOPROXY disables module downloads outright (its first entry is "off").
func localGoproxyOff() bool {
	return firstGoproxyEntry() == "off"
}

// localGoproxyNonPublic reports whether the local `go` command's effective
// GOPROXY resolves to something other than the public proxy.golang.org this
// tool actually probes — either "direct" (skip the proxy protocol
// entirely and fetch straight from the VCS host) or a custom proxy URL (a
// private mirror: Athens, Artifactory, and regional mirrors like
// goproxy.cn are all in real, common use). Returns ("", "") when the first
// entry is the public proxy (the default, "https://proxy.golang.org,direct")
// or empty/unreadable, in which case the normal probe below applies as-is.
//
// Confirmed live: with GOPROXY pointed at a working custom proxy serving a
// module the public proxy has never heard of, `go mod download` (the same
// operation `go install`/`go get` depend on) succeeds in that environment,
// while probing proxy.golang.org unconditionally — what this tool did
// before this check existed — reported a false "module-unknown" telling
// the user to check for a typo, when nothing was wrong at all; it was just
// asking the wrong proxy. Mirrors the localGoproxyOff/localModulePrivate
// short-circuits above: local config makes the public-proxy probe not
// reflect what `go install` will actually do, so this reports that
// honestly instead of guessing at a private proxy's own auth/protocol
// (which this tool has no way to know).
func localGoproxyNonPublic() (kind, value string) {
	switch first := firstGoproxyEntry(); first {
	case "", "off":
		return "", "" // "" (unreadable): fall through to the normal probe; "off" is handled separately above
	case defaultProxyBase, defaultProxyBase + "/":
		return "", ""
	case "direct":
		return "direct", ""
	default:
		return "custom", first
	}
}

// localModulePrivate reports whether module matches the local `go`
// command's effective GONOPROXY pattern list, returning the specific
// pattern that matched. GONOPROXY defaults to GOPRIVATE's value when not
// set explicitly (confirmed live: `GOPRIVATE=x go env GONOPROXY` prints
// x, with no GONOPROXY set at all) — `go env GONOPROXY` reports that
// already-resolved effective value either way, so reading it alone covers
// both GOPRIVATE and an explicit GONOPROXY override without needing to
// check both separately.
func localModulePrivate(module string) (bool, string) {
	out, err := exec.Command("go", "env", "GONOPROXY").Output()
	if err != nil {
		return false, "" // best-effort: don't block the real check on this
	}
	patterns := splitPatterns(strings.TrimSpace(string(out)))
	for _, p := range patterns {
		if matchesPrefixPattern(p, module) {
			return true, p
		}
	}
	return false, ""
}

// localSumdbSkipped reports whether the local `go` command's effective
// config means it will never consult sum.golang.org for module at all —
// either GOSUMDB is explicitly set to "off" (globally disables checksum
// database verification, a real setting used by CI/corporate environments
// that trust their proxy or run fully offline), or GONOSUMDB (which
// defaults to GOPRIVATE's value when unset, confirmed live the same way
// GONOPROXY does) has a pattern matching module.
//
// Confirmed live with `go mod download -x` in an isolated GOMODCACHE: with
// GOSUMDB=off, no sum.golang.org (or proxy.golang.org/sumdb/...) request
// appears in the trace at all, where the default config clearly shows both.
// With GONOSUMDB matching a module and GOPRIVATE left unset, the module is
// still fetched normally through proxy.golang.org (confirmed live: the
// .info/.zip fetches go through proxy.golang.org as usual) — only the sumdb
// lookup is skipped. That's different from localModulePrivate (which
// mirrors GONOPROXY and already short-circuits the whole proxy probe
// earlier in run()): a GONOSUMDB-only match still needs the normal
// proxy-reachability check, it just means a sumdb-lag verdict from this
// tool wouldn't actually block a real `go install` in this environment,
// since sum.golang.org's state is irrelevant once the local config has
// already decided not to consult it. Without this, goproxycheck told a
// GOSUMDB=off user to "retry shortly" for a module that was already
// installable right now.
func localSumdbSkipped(module string) (skipped bool, reason string) {
	if out, err := exec.Command("go", "env", "GOSUMDB").Output(); err == nil {
		if strings.TrimSpace(string(out)) == "off" {
			return true, "GOSUMDB=off"
		}
	}
	out, err := exec.Command("go", "env", "GONOSUMDB").Output()
	if err != nil {
		return false, "" // best-effort: don't block the real check on this
	}
	patterns := splitPatterns(strings.TrimSpace(string(out)))
	for _, p := range patterns {
		if matchesPrefixPattern(p, module) {
			return true, fmt.Sprintf("GONOSUMDB pattern %q", p)
		}
	}
	return false, ""
}

func gitDescribeTag() (string, error) {
	out, err := exec.Command("git", "describe", "--tags", "--exact-match", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git describe --tags --exact-match HEAD: %w (HEAD may not be tagged — pass module@version explicitly)", err)
	}
	return strings.TrimSpace(string(out)), nil
}
