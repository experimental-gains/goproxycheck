package main

import (
	"fmt"
	"net/http"
	"strings"
)

type status string

const (
	statusReady                 status = "ready"
	statusNegativeCache         status = "negative-cache-suspected"
	statusNotYetIndexed         status = "not-yet-indexed"
	statusModuleUnknown         status = "module-unknown"
	statusSumdbLag              status = "sumdb-lag"
	statusNetworkError          status = "network-error"
	statusZipBuildError         status = "zip-build-error"
	statusModuleNegativeCache   status = "module-negative-cache-suspected"
	statusGoproxyOffLocally     status = "goproxy-off-locally"
	statusGoproxyDirectLocally  status = "goproxy-direct-locally"
	statusGoproxyCustomLocally  status = "goproxy-custom-locally"
	statusPrivateModuleLocally  status = "private-module-locally"
	statusBlocklistedMalicious  status = "blocklisted-malicious"
	statusRepoCheckInconclusive status = "repo-check-inconclusive"
	statusWrongImportPath       status = "wrong-import-path"
	statusRetracted             status = "retracted"
	statusProxyError            status = "proxy-error"
)

// isRepoCheckRateLimited reports whether a repo-reachability probe status
// code is GitHub rate-limiting or blocking the request itself, rather than
// answering "the repo doesn't exist." Unauthenticated GETs to github.com
// (not the api.github.com REST API, which has its own separate limits) can
// get a 403 from secondary rate limiting or a 429 under sustained load —
// both look exactly like a 404 through repoReachable's plain bool, but mean
// "we don't know" instead of "no."
func isRepoCheckRateLimited(statusCode int) bool {
	return statusCode == 403 || statusCode == 429
}

// blocklistMarker is the distinctive substring proxy.golang.org includes
// in the plain-text body of a 403 response when it has flagged a specific
// module as malicious and refuses to serve it — confirmed live against
// three real, independently documented malicious modules (github.com/
// shopsprint/decimal, github.com/boltdb-go/bolt, github.com/xinfeisoft/
// crypto, 2026-09). This is permanent proxy state, not the negative-cache
// or not-yet-indexed conditions the rest of this file exists to diagnose —
// without this check, a blocked module fell through to statusModuleUnknown
// (or statusModuleNegativeCache if the repo happened to still be
// reachable), telling the caller to check for a typo or wait for indexing
// lag when the real answer is "this was deliberately blocked, don't use it."
const blocklistMarker = "considers this module to be malicious"

func isBlocklistedMalicious(body string) bool {
	return strings.Contains(body, blocklistMarker)
}

// isZipBuildError reports whether a proxy error body is one of
// golang.org/x/mod/zip's "create zip[...]" errors — raised when the proxy
// can build a valid checkout of the tagged commit but can't turn it into a
// module zip (a case-insensitive filename collision, a disallowed file
// mode, a path outside the module, an oversized file, etc.). Confirmed
// against the real proxy: github.com/torvalds/linux's tags all fail this
// way, since the kernel tree has files that only differ by case
// (xt_MARK.h vs xt_mark.h). Unlike the negative-cache case, this is a
// permanent property of the tagged tree, not a poisoned cache entry — a
// new tag only fixes it if the underlying file collision is fixed too.
//
// Known limitation, confirmed live against github.com/torvalds/linux: the
// proxy's own response for the *same* module@version isn't deterministic
// across requests — repeated .info fetches were observed alternating
// between the real "create zip: ... case-insensitive file name
// collision: ..." body (detected here) and a plain "fetch timed out" body
// (which contains no "create zip" and so falls through to
// statusNegativeCache instead), likely because a large-repo zip build is
// re-attempted synchronously per request and sometimes exceeds the
// proxy's own internal timeout before finishing. There's no reliable way
// to tell that apart from an ordinary negative-cache 404 on a single
// probe; a caller who gets negative-cache-suspected here should be aware
// a retry might reveal the real (permanent) zip-build-error instead.
func isZipBuildError(body string) bool {
	return strings.Contains(body, "create zip")
}

// isProxyErrorStatus reports whether code is a "terminal error" response
// per the documented GOPROXY protocol (go.dev/ref/mod#goproxy-protocol):
// "Responses with status codes 4xx and 5xx are treated as errors. The
// error codes 404 (Not Found) and 410 (Gone) indicate that the requested
// module or version is not available on the proxy, but it may be found
// elsewhere." Any other non-2xx status (429, 500, 502, 503, a 403 that
// isn't the malicious-block marker checked separately, etc.) is a
// different thing entirely: per the same page, "If the proxy responds to
// a request with an error status other than 404 or 410, the go command
// will not fall back to later entries in the GOPROXY list" — with the
// default GOPROXY=proxy.golang.org,direct chain, that means `go install`
// fails outright with the raw error, not the graceful typo/negative-cache/
// wait-it-out outcomes this tool otherwise diagnoses. code==0 means no
// HTTP response was received at all (a transport error, already handled
// via the .err field before any of these checks run).
func isProxyErrorStatus(code int) bool {
	return code != 0 && code != http.StatusOK && code != http.StatusNotFound && code != http.StatusGone
}

func proxyErrorDiagnosis(host, endpoint string, code int) diagnosis {
	return diagnosis{statusProxyError, fmt.Sprintf(
		"%s's %s endpoint returned HTTP %d — not 200 (success) or 404/410 (not found). "+
			"Per the documented protocol (go.dev/ref/mod#goproxy-protocol), any other 4xx/5xx status is a terminal error, not evidence the module or version doesn't exist: with the default GOPROXY chain, the go command does NOT fall back or wait it out on a non-404/410 error, it fails outright with this same status. "+
			"This may be a transient issue on %s's side (retry), or a deliberate block unrelated to whether the module is real — it isn't the typo/negative-cache/indexing-lag situation the other diagnoses here describe.",
		host, endpoint, code, host)}
}

type diagnosis struct {
	status  status
	message string
}

// diagnose turns raw probe results into an actionable verdict. The
// negative-cache case is the one this tool exists for: proxy.golang.org
// caches a failed per-version fetch (e.g. one made while the repo was still
// private) separately from @latest/@v/list, so those can look completely
// healthy while the specific version you just tagged 404s indefinitely. See
// https://github.com/experimental-gains/modslop's v0.1.0/v0.1.1 history for
// a real example this tool was built from.
func diagnose(r report) diagnosis {
	// A transport error (err set) is not the same as a clean non-2xx response
	// (ok false, err nil) — the latter is meaningful proxy state, the former
	// is "we don't actually know." Checking all four probes, not just
	// latest/list, matters: an errored versionInfo or sum request used to
	// fall through with ok=false and an empty body, which read exactly like
	// a real 404 and got misdiagnosed as negative-cache-suspected or
	// sumdb-lag — the tool's core diagnoses — on a plain network blip.
	if err := firstErr(r.latest.err, r.list.err, r.versionInfo.err, r.sum.err); err != nil {
		return diagnosis{statusNetworkError, fmt.Sprintf("request to proxy.golang.org or sum.golang.org failed: %v", err)}
	}

	if isBlocklistedMalicious(r.latest.body) || isBlocklistedMalicious(r.list.body) {
		return diagnosis{statusBlocklistedMalicious, fmt.Sprintf(
			"proxy.golang.org has explicitly flagged %s as malicious and refuses to serve it. "+
				"This is a permanent security block, not a caching or indexing problem — do not use this module, and don't expect --wait or a new tag to change the outcome.",
			r.module)}
	}

	if !r.moduleKnown() {
		// Checked before any of the typo/negative-cache/rate-limited
		// verdicts below: those all assume @latest and @v/list gave a
		// clean, meaningful "not found" (404/410) or a transport error
		// (already ruled out above). A genuine HTTP error status from the
		// proxy itself (429, 500, 502, 503, an unrelated 403, ...) is
		// neither — see isProxyErrorStatus's doc comment.
		if isProxyErrorStatus(r.latest.statusCode) {
			return proxyErrorDiagnosis("proxy.golang.org", "@latest", r.latest.statusCode)
		}
		if isProxyErrorStatus(r.list.statusCode) {
			return proxyErrorDiagnosis("proxy.golang.org", "@v/list", r.list.statusCode)
		}
		if r.repoReachable != nil && *r.repoReachable {
			if r.repoCheckedNestedPath {
				repoRoot := githubRepoRoot(r.module)
				return diagnosis{statusModuleNegativeCache, fmt.Sprintf(
					"proxy.golang.org has no listing for %s (@latest and @v/list both 404). The repo root https://%s is reachable and public right now, but %s has extra path segments past the repo root, which GitHub's plain URLs 404 on whether or not they're real (this is also true for the common major-version-on-a-branch layout, e.g. a module published as .../v9 straight from the repo root with no /v9 directory) — so this check only confirms the repo exists, not that %s itself is a real module path. "+
						"If the module path is right, this is very likely the same negative-cache mechanism this tool detects at the per-version level (see the negative-cache-suspected status), just poisoning the whole module instead of one version — usually because the proxy tried to fetch it once while the repo was still private; there's no known trick that reliably clears it and no documented SLA (see https://github.com/golang/go/issues/67958). "+
						"But double-check the module path itself first (typo, moved import path, or a nested subdirectory that was never created) — that failure mode looks identical from here and waiting won't fix it.",
					r.module, repoRoot, r.module, r.module)}
			}
			return diagnosis{statusModuleNegativeCache, fmt.Sprintf(
				"proxy.golang.org has no listing for %s (@latest and @v/list both 404), but https://%s is reachable and public right now. "+
					"This is very likely the same negative-cache mechanism this tool detects at the per-version level (see the negative-cache-suspected status), just poisoning the whole module instead of one version — usually because the proxy tried to fetch it once while the repo was still private. "+
					"Unlike the per-version case, there's no known trick that reliably clears it (cutting a new tag doesn't help here, since @latest itself is what's cached negative) and no documented SLA — see https://github.com/golang/go/issues/67958 for another report of the same thing. "+
					"GOPROXY=direct works around it for your own local build but does not fix what other users or CI see from the shared proxy — and only if your GOVCS setting allows a direct fetch for this module (the default does; a custom GOVCS restriction can still block it with its own 'GOVCS disallows' error). Waiting is the only broadly-effective known fix.",
				r.module, r.module)}
		}
		if r.repoReachable != nil && isRepoCheckRateLimited(r.repoCheckStatusCode) {
			repoRoot := githubRepoRoot(r.module)
			return diagnosis{statusRepoCheckInconclusive, fmt.Sprintf(
				"proxy.golang.org has never heard of %s (both @latest and @v/list failed), and checking whether https://%s is reachable got HTTP %d instead of a clear answer. "+
					"That status means GitHub itself rate-limited or blocked this tool's unauthenticated check — not that the repo doesn't exist. This is a known way for the check to be inconclusive when run frequently in CI (e.g. via the shipped GitHub Action). "+
					"Check https://%s in a browser, or retry this check in a few minutes; don't treat this the same as a confirmed module-unknown.",
				r.module, repoRoot, r.repoCheckStatusCode, repoRoot)}
		}
		return diagnosis{statusModuleUnknown, "proxy.golang.org has never heard of this module (both @latest and @v/list failed). " +
			"Check: is the repo public? does the module path in go.mod exactly match the repo (case matters)? " +
			"is it covered by a GOPRIVATE/GONOSUMDB pattern that's intentionally excluding it from the public proxy?"}
	}

	// Checked ahead of the ready/sumdb-lag verdicts below (both require
	// r.versionInfo.ok, same as this): a canonical-path mismatch makes `go
	// install` fail outright regardless of sumdb state, so it isn't a
	// milder "ready, but note this" case — it's a different failure this
	// tool would otherwise miss entirely. See canonicalModulePath's doc
	// comment.
	if canonical, mismatched := canonicalModulePath(r); mismatched {
		return diagnosis{statusWrongImportPath, fmt.Sprintf(
			"%s resolves through proxy.golang.org and sum.golang.org under this import path, but the go.mod at this version declares its module path as %q, not %q — a plain `go install`/`go get` will fail outright with \"module declares its path as: %s\n\tbut was required as: %s\". "+
				"This isn't a proxy-availability problem `--wait` or a retry can fix: use %s instead of %s.",
			displayTarget(r), canonical, r.module, canonical, r.module, canonical, r.module)}
	}

	// Checked ahead of ready/sumdb-lag for the same reason as
	// canonicalModulePath above: retraction doesn't affect proxy or sumdb
	// availability at all (see retraction's doc comment — `go install`
	// succeeds outright on a retracted version), so it isn't a milder
	// "ready, but note this" footnote — it's the maintainer's own explicit
	// "don't use this version" signal, which matters regardless of whether
	// sum.golang.org has caught up yet.
	if r.latestModFile.ok {
		if rationale, retracted := retraction(r.latestModFile.body, r.checkVersion()); retracted {
			explain := "no rationale was given in the retract directive"
			if rationale != "" {
				explain = fmt.Sprintf("rationale given: %q", rationale)
			}
			return diagnosis{statusRetracted, fmt.Sprintf(
				"%s resolves fine through proxy.golang.org and sum.golang.org — a plain `go install` will succeed — but this exact version is covered by a `retract` directive in the module's own go.mod (%s). "+
					"Retraction is advisory only: `go install`/`go get`/`go mod download` don't consult it and will fetch this version anyway; only `go list -m -u` surfaces it. This isn't a proxy-availability problem `--wait` or a retry can fix — it's the maintainer telling you not to use this version. Use a different version instead.",
				displayTarget(r), explain)}
		}
	}

	if r.versionInfo.ok && r.sum.ok {
		return diagnosis{statusReady, fmt.Sprintf("%s is live on both proxy.golang.org and sum.golang.org — a plain `go install` will work.", displayTarget(r))}
	}

	if r.versionInfo.ok && !r.sum.ok {
		if isProxyErrorStatus(r.sum.statusCode) {
			return proxyErrorDiagnosis("sum.golang.org", "/lookup", r.sum.statusCode)
		}
		return diagnosis{statusSumdbLag, "the module proxy has this version, but sum.golang.org doesn't yet. " +
			"sumdb usually catches up within a minute or two of the proxy; this is normal lag, not the negative-cache bug. Retry shortly."}
	}

	if isZipBuildError(r.versionInfo.body) {
		return diagnosis{statusZipBuildError, fmt.Sprintf(
			"%s: the proxy can't build a module zip from this tag: %s. "+
				"This is a permanent property of the tagged tree (a bad file name, an oversized file, a case-insensitive filename collision, or similar), not the negative-cache bug — cutting a new tag won't help unless it also fixes the underlying file problem.",
			displayTarget(r), firstLine(r.versionInfo.body))}
	}

	// The blocklist check above only sees @latest/@v/list, which catches a
	// module blocked outright. The Go security team can also block a single
	// malicious version while leaving the rest of the module (and @latest,
	// if it doesn't resolve to the bad version) untouched — a realistic
	// shape for a supply-chain compromise where only one published version
	// is bad. Without this, that 403 fell through to statusZipBuildError
	// (isZipBuildError doesn't match this body) or statusNegativeCache (if
	// the version is still listed in @v/list), both of which tell the user
	// to cut a new tag or wait — actively wrong for a version that was
	// deliberately and permanently blocked.
	if isBlocklistedMalicious(r.versionInfo.body) {
		return diagnosis{statusBlocklistedMalicious, fmt.Sprintf(
			"proxy.golang.org has explicitly flagged %s as malicious and refuses to serve it. "+
				"This is a permanent security block, not a caching or indexing problem — do not use this version, and don't expect --wait or a new tag pointing at the same code to change the outcome. "+
				"(@latest and @v/list are otherwise healthy, so this block is scoped to this specific version, not the whole module — an older or newer version may still be safe to use.)",
			displayTarget(r))}
	}

	// Checked after the zip-build and malicious-block checks above (both
	// look at r.versionInfo.body, still meaningful even on a non-200
	// status): a genuine proxy error status here (429, 500, ...) is
	// neither of those and isn't the negative-cache/not-yet-indexed
	// situation the fallback below assumes either.
	if isProxyErrorStatus(r.versionInfo.statusCode) {
		return proxyErrorDiagnosis("proxy.golang.org", fmt.Sprintf("@v/%s.info", escapePath(r.checkVersion())), r.versionInfo.statusCode)
	}

	// versionInfo failed but the module itself is known. Distinguish "never
	// published" from "published but poisoned/not-yet-indexed" using @v/list.
	// Compare against checkVersion(), not the raw r.version: for a "latest"
	// query, r.version is the literal string "latest", which never appears
	// in @v/list — the resolved concrete version is what was actually
	// probed and would show up there.
	checkVersion := r.checkVersion()
	for _, v := range r.listedVersions() {
		if v == checkVersion {
			return diagnosis{statusNegativeCache, fmt.Sprintf(
				"%s is in @v/list (so it was tagged and the proxy has seen the module) but @v/%s.info still 404s. "+
					"This is the per-version negative-cache pattern: the proxy tried to fetch this exact version once — often while the repo was still private — and cached that failure separately from @latest/@v/list. "+
					"It has been observed not to clear on its own within 30+ minutes. Fix: cut a new patch tag (no code change needed) rather than waiting; a version that was never fetched while private has nothing poisoned to clear.",
				checkVersion, escapePath(checkVersion))}
		}
	}

	return diagnosis{statusNotYetIndexed, fmt.Sprintf(
		"%s is not in @v/list yet, so the proxy likely hasn't picked up this tag at all (rather than the negative-cache bug, which requires the version to already be listed). "+
			"If you just pushed the tag, this is ordinary indexing lag — retry in a minute, or use --wait.", displayTarget(r))}
}

// canonicalModulePath reports whether the go.mod the proxy serves for the
// resolved version declares a different module path than the one that was
// checked, returning that canonical path when it does.
//
// This is invisible to every other check in this file: @latest, @v/list,
// @v/<version>.info, and sum.golang.org/lookup all key off the import path
// given to them and the VCS origin it resolves to, not the module
// directive inside go.mod — so a module that moved its canonical path
// (the real-world case this was built from: github.com/grpc/grpc-go
// renamed to google.golang.org/grpc, but the proxy still resolves
// @latest/@v/list/@v/<version>.info identically under the old path)
// reports fully "ready" under either path with no other signal that one
// of them is wrong. Confirmed live (2026-09-24): `go install
// github.com/grpc/grpc-go@v1.84.0` fails outright with "module declares
// its path as: google.golang.org/grpc\n\tbut was required as:
// github.com/grpc/grpc-go" — a real, tool-breaking gap this closes.
// Reported by an external user, https://github.com/experimental-gains/
// goproxycheck/issues/2.
//
// Only meaningful once r.versionInfo.ok is true — see probe()'s modFile
// fetch, which is gated on the same condition.
func canonicalModulePath(r report) (canonical string, mismatched bool) {
	if !r.modFile.ok {
		return "", false
	}
	canonical, err := moduleDirective(r.modFile.body)
	if err != nil || canonical == "" || canonical == r.module {
		return "", false
	}
	return canonical, true
}

// displayTarget renders module@version for a diagnosis message, adding the
// resolved concrete version when the argument was a "latest" query — so the
// message says what was actually checked, not just the literal input.
func displayTarget(r report) string {
	if r.resolvedVersion != "" {
		return fmt.Sprintf("%s@%s (resolved to %s)", r.module, r.version, r.resolvedVersion)
	}
	return fmt.Sprintf("%s@%s", r.module, r.version)
}

// githubRepoRoot returns the github.com/owner/repo prefix of a module path
// that matches githubRepoPattern (only called when it already has). Used to
// report exactly what repoReachable actually checked, which is the repo
// root and not necessarily the full module path.
func githubRepoRoot(module string) string {
	return githubRepoPattern.FindString(module)
}

// firstLine returns the first line of a (possibly multi-line) proxy error
// body, trimmed of surrounding whitespace. Zip-build errors can list one
// line per offending file, which is useful in full but too long to inline
// in a one-line diagnosis message.
func firstLine(body string) string {
	body = strings.TrimSpace(body)
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		return body[:i]
	}
	return body
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
