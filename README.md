# goproxycheck

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
- uses: experimental-gains/goproxycheck@v0.1.8
  with:
    args: --wait --timeout 10m github.com/you/yourmodule@v1.2.3
```

Handy right after a release step, to block the rest of a pipeline
(e.g. an announcement or downstream build) until the tag is actually
fetchable.

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

## License

MIT
