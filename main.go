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
	} else if precedingCustom, anyErrorFallback, fallbackOK := publicProxyFallback(); kind == "custom" && !fallbackOK {
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
			if !*wait || d.status == statusReady || d.status == statusModuleUnknown || d.status == statusBlocklistedMalicious || d.status == statusWrongImportPath || d.status == statusRetracted || time.Now().After(deadline) {
				break
			}
			time.Sleep(*interval)
		}
		if kind == "custom" && fallbackOK {
			// See publicProxyFallback's doc comment: the chain reaches the
			// public proxy after one or more custom entries, so the probe
			// above is real and relevant, but only conditionally — prefix
			// the diagnosis with that caveat instead of either silently
			// omitting it (misleadingly presenting the result as
			// unconditional) or bailing out entirely (the old behavior,
			// which threw away a real, checkable answer).
			d.message = fallbackCaveat(precedingCustom, anyErrorFallback) + " " + d.message
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
	mod, err := moduleDirective(string(data))
	if err != nil {
		return "", fmt.Errorf("%s %w", path, err)
	}
	return mod, nil
}

// moduleDirective extracts the module path from a go.mod-format body's
// `module` directive line. Shared by moduleFromGoMod (reading the local
// ./go.mod) and canonicalModuleNote in diagnose.go (reading the go.mod the
// proxy serves for a resolved version) — same file format, same parsing
// rules, so one implementation covers both instead of drifting apart.
func moduleDirective(data string) (string, error) {
	for _, line := range strings.Split(data, "\n") {
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
				return "", fmt.Errorf("has a 'module' directive with no path: %q", line)
			}
			return mod, nil
		}
	}
	return "", fmt.Errorf("has no 'module' directive")
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
//
// Delegates to parseGoproxyChain (below) rather than re-splitting the raw
// string itself: a naive "split on the first ,/|" used to treat a
// leading/interior empty entry (e.g. `GOPROXY="$UNSET_VAR,off"`) as the
// first entry being "", never reaching "off" at all — verified live that
// real `go` skips blank entries instead, so `GOPROXY=",off"` disables
// lookups exactly like `GOPROXY=off` does, and this tool used to
// misreport such a config as fully working.
func firstGoproxyEntry() string {
	out, err := exec.Command("go", "env", "GOPROXY").Output()
	if err != nil {
		return "" // best-effort: don't block the real check on this
	}
	entries, _ := parseGoproxyChain(strings.TrimSpace(string(out)))
	if len(entries) == 0 {
		return ""
	}
	return entries[0]
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

// parseGoproxyChain replicates cmd/go's own GOPROXY-list walk (proxyList in
// cmd/go/internal/modfetch/proxy.go, verified directly against that source)
// far enough to recover entry positions and separators: "," and "|" both
// separate entries ("|" meaning "fall back to the next entry on *any*
// error," not just 404/410), and both "off" and "direct" terminate the walk
// outright — real go's own comment says it ignores every entry after either
// "for forward-compatibility," so an entry placed after one is never
// actually reachable. seps[i] is the separator that precedes entries[i+1].
func parseGoproxyChain(raw string) (entries []string, seps []byte) {
	pending := raw
	for pending != "" {
		var url string
		var trailingSep byte
		if i := strings.IndexAny(pending, ",|"); i >= 0 {
			url, trailingSep, pending = pending[:i], pending[i], pending[i+1:]
		} else {
			url, pending = pending, ""
		}
		url = strings.TrimSpace(url)
		if url == "" {
			continue
		}
		entries = append(entries, url)
		if url == "off" || url == "direct" {
			break
		}
		if pending != "" {
			seps = append(seps, trailingSep)
		}
	}
	return entries, seps
}

// publicProxyFallback reports whether the local `go` command's effective
// GOPROXY reaches the public proxy.golang.org this tool actually probes as
// a *later* entry in the chain, rather than the first — the officially
// documented "https://corp.example.com,https://proxy.golang.org" pattern
// (go.dev/ref/mod#goproxy-protocol's own worked example): a company proxy
// for private modules, falling back to the public proxy for everything
// else. Before this existed, localGoproxyNonPublic only ever inspected the
// chain's first entry (see firstGoproxyEntry's doc comment), so this exact
// documented and common pattern made the tool claim total ignorance ("it
// has no way to know... can't tell you whether it's ready there") even
// though the public-proxy probe is still exactly the right thing to check
// — confirmed live: with a stand-in proxy that 404s on everything as the
// first entry and real proxy.golang.org as the second, `go mod download -x`
// falls straight through (404 on the first entry, per go's own documented
// rule) and succeeds via the public entry, for an ordinary public module.
//
// Returns ok=false when the public proxy isn't reachable via the chain at
// all (not present, or only present after an "off"/"direct" that already
// terminates the walk first) — that case keeps the existing "custom,
// can't check" behavior. precedingCustom lists the entries tried before
// reaching the public proxy (for the caveat message); anyErrorFallback is
// true when the separator immediately before the public entry is "|"
// (fallback on any error) rather than "," (fallback only on 404/410).
func publicProxyFallback() (precedingCustom []string, anyErrorFallback, ok bool) {
	out, err := exec.Command("go", "env", "GOPROXY").Output()
	if err != nil {
		return nil, false, false
	}
	entries, seps := parseGoproxyChain(strings.TrimSpace(string(out)))
	for i, url := range entries {
		if url != defaultProxyBase && url != defaultProxyBase+"/" {
			continue
		}
		if i == 0 {
			return nil, false, false // first entry is already public; not this case
		}
		return entries[:i], seps[i-1] == '|', true
	}
	return nil, false, false
}

// fallbackCaveat builds the explanatory prefix used when the public proxy
// is only reachable in the local GOPROXY chain after one or more custom
// entries (see publicProxyFallback) — it describes exactly what condition
// has to hold for the probe result that follows to actually apply.
func fallbackCaveat(precedingCustom []string, anyErrorFallback bool) string {
	trigger := "reports the module not found there (404/410)"
	if anyErrorFallback {
		trigger = "fails for any reason (a `|` separator precedes proxy.golang.org in your chain, so any error triggers fallback there, not just 404/410)"
	}
	return fmt.Sprintf(
		"Note: your local `GOPROXY` tries %s before proxy.golang.org, in that order — the documented go.dev/ref/mod#goproxy-protocol fallback-chain pattern (e.g. a company proxy for private modules, falling back to the public proxy for everything else). "+
			"This tool can only check proxy.golang.org directly, not your own %s, so the result below only applies once every entry ahead of it %s.",
		strings.Join(precedingCustom, ", "), strings.Join(precedingCustom, ", "), trigger)
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

// sumdbName extracts the checksum-database name a raw GOSUMDB value
// resolves to, mirroring the parsing cmd/go itself does in
// modfetch/sumdb.go's dbDial before it ever opens a connection: $GOSUMDB is
// "off", a bare name, or "name[+key] [url]" (see `go help goproxy`) — the
// optional key/url fields say *how* to reach/verify the database, not
// *which* one it is, so only the first field (before the first space, then
// before the first "+") matters for identifying it. "sum.golang.google.cn"
// is a documented special case cmd/go rewrites internally to "sum.golang.org
// https://sum.golang.google.cn" (a China-reachable mirror of the *same*
// public tree, not a different database) before this same field-splitting
// — confirmed against modfetch/sumdb.go's dbDial source — so it resolves to
// "sum.golang.org" here too, matching real behavior instead of being
// mistaken for an unrelated custom database.
func sumdbName(gosumdb string) string {
	if gosumdb == "" || gosumdb == "sum.golang.google.cn" {
		return "sum.golang.org"
	}
	name, _, _ := strings.Cut(gosumdb, " ")
	name, _, _ = strings.Cut(name, "+")
	return name
}

// localSumdbSkipped reports whether the local `go` command's effective
// config means it will never consult the public sum.golang.org for module
// at all — GOSUMDB is explicitly set to "off" (globally disables checksum
// database verification, a real setting used by CI/corporate environments
// that trust their proxy or run fully offline), GOSUMDB names a custom
// checksum database instead of the public one (a real, documented setting
// — e.g. an org running its own sumdb to avoid leaking module paths/
// versions to Google — see sumdbName), or GONOSUMDB (which defaults to
// GOPRIVATE's value when unset, confirmed live the same way GONOPROXY does)
// has a pattern matching module.
//
// Confirmed live with `go mod download -x` in an isolated GOMODCACHE: with
// GOSUMDB=off, no sum.golang.org (or proxy.golang.org/sumdb/...) request
// appears in the trace at all, where the default config clearly shows both.
// With a custom GOSUMDB (a valid verifier key naming a different database,
// plus an explicit URL — confirmed with a local HTTP server logging every
// request), every sumdb request went to the custom server; none went to
// sum.golang.org at all, so a sumdb-lag verdict from the public database
// is exactly as meaningless here as under GOSUMDB=off — it's naming a
// different database, not an absent one, but the public one's state still
// can't block a real `go install` in this environment. With GONOSUMDB
// matching a module and GOPRIVATE left unset, the module is still fetched
// normally through proxy.golang.org (confirmed live: the .info/.zip
// fetches go through proxy.golang.org as usual) — only the sumdb lookup is
// skipped. That's different from localModulePrivate (which mirrors
// GONOPROXY and already short-circuits the whole proxy probe earlier in
// run()): a GONOSUMDB-only match still needs the normal proxy-reachability
// check, it just means a sumdb-lag verdict from this tool wouldn't
// actually block a real `go install` in this environment, since
// sum.golang.org's state is irrelevant once the local config has already
// decided not to consult it. Without this, goproxycheck told a
// GOSUMDB=off (or custom-GOSUMDB) user to "retry shortly" for a module
// that was already installable right now.
func localSumdbSkipped(module string) (skipped bool, reason string) {
	if out, err := exec.Command("go", "env", "GOSUMDB").Output(); err == nil {
		gosumdb := strings.TrimSpace(string(out))
		if gosumdb == "off" {
			return true, "GOSUMDB=off"
		}
		if name := sumdbName(gosumdb); name != "sum.golang.org" {
			return true, fmt.Sprintf("custom GOSUMDB %q", name)
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
