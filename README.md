# goproxycheck

[![Go Reference](https://pkg.go.dev/badge/github.com/experimental-gains/goproxycheck.svg)](https://pkg.go.dev/github.com/experimental-gains/goproxycheck)
[![License: MIT](https://img.shields.io/github/license/experimental-gains/goproxycheck)](LICENSE)
[![Latest release](https://img.shields.io/github/v/tag/experimental-gains/goproxycheck)](https://github.com/experimental-gains/goproxycheck/releases)

Check whether a Go module version is actually fetchable via the public
module proxy and checksum database — and get a real diagnosis when it
isn't, instead of guessing whether to wait, retry, or re-tag.

## Why this exists

`go install module@version` depends on two services outside your
control: `proxy.golang.org` (serves the code) and `sum.golang.org`
(verifies it). Both can say no in ways that look identical from the
outside but need completely different fixes:

- **Ordinary indexing lag** — you just pushed the tag; give it a minute.
- **A per-version negative cache** — `proxy.golang.org` tried to fetch
  this *exact* version once, often because the repo was still private
  at the time, and cached that failure separately from `@latest` and
  `@v/list`. It can stay poisoned for 30+ minutes and the fix isn't
  "wait longer," it's cutting a new tag — a version that was never
  fetched while private has nothing to clear.
- **The module proxy genuinely doesn't know the module** — wrong path
  case, repo still private, or intentionally excluded via `GOPRIVATE`.
- **A module-level negative cache** — the same poisoning as above, but
  hitting `@latest`/`@v/list` themselves instead of one version,
  usually because the proxy tried the *whole module* once while the
  repo was still private. It looks exactly like "the proxy has never
  heard of this," except the repo is actually live and public right
  now — and unlike the per-version case, there's no known trick (a new
  tag doesn't help; `@latest` itself is what's cached) or documented
  SLA to clear it. For `github.com`-hosted modules, `goproxycheck`
  checks the repo's live reachability directly so it can tell the two
  apart instead of pointing you at a typo that isn't there.
- **sumdb lag** — the proxy has it but the checksum database hasn't
  caught up yet, usually seconds behind.

These look the same from `go install`'s one-line error. `goproxycheck`
makes the four separate underlying requests and tells you which one
you're actually in, based on a real incident (see
[`modslop`'s v0.1.0 → v0.1.1 history](https://github.com/experimental-gains/modslop)
for the case this tool was extracted from).

The only existing tool in this space,
[`go-proxy-pull-action`](https://github.com/andrewslotin/go-proxy-pull-action),
warms the cache but only runs as a GitHub Action and doesn't diagnose
*why* a version isn't ready. `goproxycheck` is a plain binary — usable
from any CI system, a release script, or your own terminal.

## If you're searching for one of these errors

Verified against the real proxy, right now, so this matches what you're
actually seeing:

- `invalid version: unknown revision vX.Y.Z` — the version you just
  tagged isn't showing up yet. Could be ordinary indexing lag (wait a
  minute), could be the negative-cache poisoning described above (wait
  won't fix it — you need a new tag). `goproxycheck` tells you which.
- `module lookup disabled by GOPROXY=off` — not a proxy-availability
  problem at all, your own `GOPROXY` is set to `off` (checked via `go env
  GOPROXY`, so this also catches a value persisted with `go env -w`, not
  just an explicit env var). `goproxycheck` checks this *before* probing
  the proxy and reports it as a local config issue instead of a false
  "ready" — confirmed live that without this check it would otherwise say
  a module@version is ready to install while the real `go install` in
  that same environment fails outright.
- You're checking a module that's covered by your own `GOPRIVATE` (or
  `GONOPROXY`) config — confirmed live with `go mod download -x` that a
  matching module is fetched directly from its VCS host and never touches
  `proxy.golang.org` at all, so a proxy/sumdb check is meaningless for it
  either way. `goproxycheck` checks this *before* probing (via `go env
  GONOPROXY`, which already resolves the `GOPRIVATE` fallback) and reports
  it as a `private-module-locally` diagnosis instead of a false
  `module-unknown` — the public proxy genuinely has never heard of it, by
  design, regardless of whether `go install` works fine right now.
- Your local `GOPROXY` is set to `direct`, or to a private/custom proxy
  (an Athens or Artifactory mirror, a regional mirror like `goproxy.cn`,
  etc.) instead of the public `proxy.golang.org` — `goproxycheck` only
  ever probes the public proxy, so its result doesn't reflect what
  `go install`/`go get` will actually do here. Checked via `go env
  GOPROXY` (its first comma/pipe-separated entry, matching how `go`
  itself only tries later entries on a failure) *before* probing, and
  reported as a `goproxy-direct-locally`/`goproxy-custom-locally`
  diagnosis instead of a false verdict — confirmed live with a
  hand-built custom proxy serving a module the public proxy has never
  heard of: `go mod download` succeeds in that environment while probing
  `proxy.golang.org` unconditionally (what this tool did before this
  check existed) reported a false `module-unknown`, telling you to check
  for a typo when nothing was wrong at all.
- `git ls-remote -q origin ... exit status 128` / `could not read
  Username for 'https://github.com'` — this one bypasses the module
  proxy protocol entirely (Go fell back to a direct VCS fetch). Usually
  the path is wrong, the repo's still private, or it never existed —
  but if you just made a previously-private `github.com` repo public
  and are still seeing this, it can also be the module-level negative
  cache above, which looks identical from this error alone.
  `goproxycheck` checks the repo's live reachability to tell the two
  apart instead of sending you looking for a typo that isn't there.
- `create zip: ... case-insensitive file name collision` — the proxy
  built a checkout of your tag but couldn't turn it into a module zip.
  This is a permanent property of the tagged tree (two files that only
  differ by case, an oversized file, a disallowed path), not the
  negative-cache bug — a new tag won't fix it unless the underlying
  file problem is fixed too. `goproxycheck` reports this separately
  instead of telling you to just retag.
- A `403` with `... considers this module to be malicious ...` in the
  body — `proxy.golang.org` has explicitly blocklisted this exact
  module path and permanently refuses to serve it. This is not a
  typo or a private-repo issue; the proxy operator has already
  identified the module as malicious. `goproxycheck` reports this as
  a distinct `blocklisted-malicious` diagnosis instead of folding it
  into generic "module unknown" guidance (which would otherwise send
  you looking for a typo that isn't there). Verified live against real,
  publicly reported malicious Go modules, including a
  `shopspring/decimal` typosquat and a `boltdb/bolt` typosquat carrying
  an RCE backdoor.
- `module declares its path as: X` / `but was required as: Y` — the
  import path you checked resolves fine through `proxy.golang.org` and
  `sum.golang.org` (so nothing else in this list applies), but the go.mod
  at that version declares a *different* canonical module path — usually
  because the module moved (a real example: `github.com/grpc/grpc-go`
  still resolves `@latest`/`@v/list`/`@v/<version>.info` identically to
  its current canonical path, `google.golang.org/grpc`, since the proxy
  resolves those by VCS origin discovery, not by checking the module
  directive). `goproxycheck` fetches the actual go.mod for the resolved
  version and reports this as a distinct `wrong-import-path` diagnosis
  (not `ready`) — without this check, every other signal this tool has
  says the module is fine, and it would report `ready` on exactly the
  import path a plain `go install` fails on. Reported by an external
  user, [issue #2](https://github.com/experimental-gains/goproxycheck/issues/2).
- You're checking a version the module's own maintainer retracted via a
  `retract` directive — retraction is advisory only, so `proxy.golang.org`,
  `sum.golang.org`, and a plain `go install`/`go get`/`go mod download` all
  keep working on a retracted version exactly like a healthy one; only `go
  list -m -u` surfaces it. Verified live against a real retraction:
  `github.com/mattn/go-sqlite3`'s go.mod (at its latest tag) retracts
  `[v2.0.0+incompatible, v2.0.7+incompatible]` ("Accidental; no major
  changes or features."), and every version in that range still resolves
  and installs cleanly. `goproxycheck` reports this as a distinct
  `retracted` diagnosis (not `ready`) instead of missing the one signal —
  the maintainer's own go.mod — that says "don't use this."

## Install

```bash
go install github.com/experimental-gains/goproxycheck@latest
```

Or via Homebrew:

```bash
brew install experimental-gains/tap/goproxycheck
```

## Usage

```bash
# check one version explicitly
goproxycheck github.com/you/yourmodule@v1.2.3

# same "latest" query `go install` accepts — resolved to the real version first
goproxycheck github.com/you/yourmodule@latest

# or, from inside the module's repo right after tagging:
# reads the module path from ./go.mod and the version from `git describe --tags`
goproxycheck

# block a release script until it's actually live (or give up after 10m)
goproxycheck --wait --timeout 10m github.com/you/yourmodule@v1.2.3

# machine-readable
goproxycheck --json github.com/you/yourmodule@v1.2.3
```

Exit code is `0` when the version is confirmed live on both the proxy
and sumdb, `1` otherwise (including timeouts under `--wait`), `2` on
argument errors.

## Use as a GitHub Action

```yaml
- uses: experimental-gains/goproxycheck@v0.1.25
  with:
    args: --wait --timeout 10m github.com/you/yourmodule@v1.2.3
```

Handy right after a release step, to block the rest of a pipeline
(e.g. an announcement or downstream build) until the tag is actually
fetchable.

## Use as a Claude Code / Copilot CLI plugin

goproxycheck also ships as a skill in the
[`supplychain-guard`](https://github.com/experimental-gains/claude-plugins)
plugin, so an agent diagnoses an ambiguous `go get`/`go build`
proxy failure instead of guessing whether to wait, retry, or re-tag:

```
claude plugin marketplace add experimental-gains/claude-plugins
claude plugin install supplychain-guard@experimental-gains-plugins
```

Works the same way with GitHub Copilot CLI (`copilot plugin marketplace add
experimental-gains/claude-plugins`, same install command with `copilot`).

## What it doesn't do

- Doesn't publish, tag, or push anything — read-only checks against
  the public proxy/sumdb.
- Doesn't verify zip contents against the checksum, only that the
  checksum lookup itself succeeds.
- `--wait`'s polling interval is fixed, not exponential backoff — fine
  for the minutes-scale waits this is meant for, not for hammering the
  proxy over hours.
- The module-level negative-cache check only fires for `github.com`-
  hosted module paths. Other hosts and vanity import paths (custom
  domains with a `go-import` redirect) fall back to the plain
  module-unknown diagnosis — there's no reliable way to know how many
  path segments form the repo root without following VCS discovery.
- The `github.com` reachability check is an unauthenticated scrape
  request, which GitHub can rate-limit (403) or throttle (429)
  independently of whether the repo actually exists — most likely if
  this runs frequently in CI. When that happens you get
  `repo-check-inconclusive` instead of a real answer; retry later or
  check the repo in a browser.

## Support

This project is free and open source. If it's useful to you, tips are
welcome via [Liberapay](https://liberapay.com/experimental-gains/) or
this ETH address (self-custody, no KYC, no obligation):
`0x87053a1898994043e7476800cB5d4BDB423eADD7`

## License

MIT
