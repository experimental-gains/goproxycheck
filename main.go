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
	if localGoproxyOff() {
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
	} else {
		deadline := time.Now().Add(*timeout)
		for {
			r := ep.probe(module, version)
			d = diagnose(r)
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
			return parseModulePath(strings.TrimSpace(rest)), nil
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

// localGoproxyOff reports whether the local `go` command's effective
// GOPROXY disables module downloads outright. Reads it via `go env GOPROXY`
// rather than os.Getenv("GOPROXY") directly, so a value persisted with `go
// env -w GOPROXY=off` is picked up too, not just an explicit env var — `go
// env` is the authoritative source either way.
//
// GOPROXY may be a comma- or pipe-separated list of sources tried in order,
// but only the *first* entry matters here: "off" is a definitive stop with
// no fallback to later entries, confirmed live — GOPROXY=off,direct and
// GOPROXY=off|direct both fail immediately with "module lookup disabled by
// GOPROXY=off", while GOPROXY=direct,off succeeds via direct and never
// reaches the off entry at all.
func localGoproxyOff() bool {
	out, err := exec.Command("go", "env", "GOPROXY").Output()
	if err != nil {
		return false // best-effort: don't block the real check on this
	}
	proxy := strings.TrimSpace(string(out))
	if i := strings.IndexAny(proxy, ",|"); i >= 0 {
		proxy = proxy[:i]
	}
	return proxy == "off"
}

func gitDescribeTag() (string, error) {
	out, err := exec.Command("git", "describe", "--tags", "--exact-match", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git describe --tags --exact-match HEAD: %w (HEAD may not be tagged — pass module@version explicitly)", err)
	}
	return strings.TrimSpace(string(out)), nil
}
