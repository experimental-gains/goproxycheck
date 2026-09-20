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
- uses: experimental-gains/goproxycheck@v0.1.1
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

## License

MIT
