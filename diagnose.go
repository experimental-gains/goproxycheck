package main

import (
	"fmt"
	"strings"
)

type status string

const (
	statusReady                status = "ready"
	statusNegativeCache        status = "negative-cache-suspected"
	statusNotYetIndexed        status = "not-yet-indexed"
	statusModuleUnknown        status = "module-unknown"
	statusSumdbLag             status = "sumdb-lag"
	statusNetworkError         status = "network-error"
	statusZipBuildError        status = "zip-build-error"
	statusModuleNegativeCache  status = "module-negative-cache-suspected"
	statusGoproxyOffLocally    status = "goproxy-off-locally"
	statusBlocklistedMalicious status = "blocklisted-malicious"
)

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
		return diagnosis{statusModuleUnknown, "proxy.golang.org has never heard of this module (both @latest and @v/list failed). " +
			"Check: is the repo public? does the module path in go.mod exactly match the repo (case matters)? " +
			"is it covered by a GOPRIVATE/GONOSUMDB pattern that's intentionally excluding it from the public proxy?"}
	}

	if r.versionInfo.ok && r.sum.ok {
		return diagnosis{statusReady, fmt.Sprintf("%s@%s is live on both proxy.golang.org and sum.golang.org — a plain `go install` will work.", r.module, r.version)}
	}

	if r.versionInfo.ok && !r.sum.ok {
		return diagnosis{statusSumdbLag, "the module proxy has this version, but sum.golang.org doesn't yet. " +
			"sumdb usually catches up within a minute or two of the proxy; this is normal lag, not the negative-cache bug. Retry shortly."}
	}

	if isZipBuildError(r.versionInfo.body) {
		return diagnosis{statusZipBuildError, fmt.Sprintf(
			"%s@%s: the proxy can't build a module zip from this tag: %s. "+
				"This is a permanent property of the tagged tree (a bad file name, an oversized file, a case-insensitive filename collision, or similar), not the negative-cache bug — cutting a new tag won't help unless it also fixes the underlying file problem.",
			r.module, r.version, firstLine(r.versionInfo.body))}
	}

	// versionInfo failed but the module itself is known. Distinguish "never
	// published" from "published but poisoned/not-yet-indexed" using @v/list.
	for _, v := range r.listedVersions() {
		if v == r.version {
			return diagnosis{statusNegativeCache, fmt.Sprintf(
				"%s is in @v/list (so it was tagged and the proxy has seen the module) but @v/%s.info still 404s. "+
					"This is the per-version negative-cache pattern: the proxy tried to fetch this exact version once — often while the repo was still private — and cached that failure separately from @latest/@v/list. "+
					"It has been observed not to clear on its own within 30+ minutes. Fix: cut a new patch tag (no code change needed) rather than waiting; a version that was never fetched while private has nothing poisoned to clear.",
				r.version, escapePath(r.version))}
		}
	}

	return diagnosis{statusNotYetIndexed, fmt.Sprintf(
		"%s is not in @v/list yet, so the proxy likely hasn't picked up this tag at all (rather than the negative-cache bug, which requires the version to already be listed). "+
			"If you just pushed the tag, this is ordinary indexing lag — retry in a minute, or use --wait.", r.version)}
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
