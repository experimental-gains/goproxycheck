package main

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"golang.org/x/mod/modfile"
)

type status string

const (
	statusReady                   status = "ready"
	statusNegativeCache           status = "negative-cache-suspected"
	statusNotYetIndexed           status = "not-yet-indexed"
	statusModuleUnknown           status = "module-unknown"
	statusSumdbLag                status = "sumdb-lag"
	statusNetworkError            status = "network-error"
	statusZipBuildError           status = "zip-build-error"
	statusModuleNegativeCache     status = "module-negative-cache-suspected"
	statusGoproxyOffLocally       status = "goproxy-off-locally"
	statusGoproxyEmptyLocally     status = "goproxy-empty-locally"
	statusGoproxyDirectLocally    status = "goproxy-direct-locally"
	statusGoproxyCustomLocally    status = "goproxy-custom-locally"
	statusPrivateModuleLocally    status = "private-module-locally"
	statusBlocklistedMalicious    status = "blocklisted-malicious"
	statusRepoCheckInconclusive   status = "repo-check-inconclusive"
	statusWrongImportPath         status = "wrong-import-path"
	statusRetracted               status = "retracted"
	statusDeprecated              status = "deprecated"
	statusProxyError              status = "proxy-error"
	statusMajorVersionMismatch    status = "major-version-mismatch"
	statusGovcsDisallowedLocally  status = "govcs-disallowed-locally"
	statusGovcsMalformedLocally   status = "govcs-malformed-locally"
	statusGosumdbMalformedLocally status = "gosumdb-malformed-locally"
	statusGosumdbRequiredLocally  status = "gosumdb-required-locally"
	statusUnknownRevision         status = "unknown-revision"
	statusInvalidPseudoVersion    status = "invalid-pseudo-version"
	statusNoMatchingVersion       status = "no-matching-version"
	statusGoModUnparseable        status = "go-mod-unparseable"
)

// isRepoCheckInconclusive reports whether a repo-reachability probe status
// code means the check itself failed to get a trustworthy answer, rather
// than confirming "the repo doesn't exist." Unauthenticated GETs to
// github.com (not the api.github.com REST API, which has its own separate
// limits) can get a 403 from secondary rate limiting or a 429 under
// sustained load (the original case this covered) — but github.com is also
// a real service with real outages: its own status page documents 5xx
// incidents (500/502/503/504) that are unrelated to whether any given repo
// exists, and this tool's plain HTTP GET can also fail at the transport
// level entirely (DNS failure, timeout, connection refused — no HTTP
// response at all, surfaced as statusCode 0 by probeResult.get's err
// branch, since a genuine HTTP round trip always sets a real status code).
// All three cases look exactly like a 404 through repoReachable's plain
// bool, but mean "we don't know" instead of "no" — the same gap the
// original 403/429 fix closed, just for the other ways an HTTP check can
// fail to produce a real answer.
func isRepoCheckInconclusive(statusCode int) bool {
	return statusCode == 403 || statusCode == 429 || statusCode == 0 || (statusCode >= 500 && statusCode < 600)
}

// repoCheckInconclusiveDetail describes why a repo-reachability probe
// didn't produce a trustworthy answer, given its raw HTTP status code (0
// for a transport error, per isRepoCheckInconclusive's doc comment) and,
// only meaningful when statusCode is 0, the transport error itself.
func repoCheckInconclusiveDetail(statusCode int, err error) (detail, explanation string) {
	switch {
	case statusCode == 0:
		return fmt.Sprintf("the request itself failed (%v) instead of getting any HTTP response", err),
			"That's a network-level failure reaching github.com, not an answer from GitHub at all."
	case statusCode == 403 || statusCode == 429:
		return fmt.Sprintf("got HTTP %d", statusCode),
			"That status means GitHub itself rate-limited or blocked this tool's unauthenticated check — not that the repo doesn't exist."
	default:
		return fmt.Sprintf("got HTTP %d", statusCode),
			"That status means GitHub's own server had an error serving this request — not that the repo doesn't exist."
	}
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

// majorVersionMismatchMarker is the distinctive substring proxy.golang.org
// includes in the plain-text body of a 404 response when the requested
// version has a go.mod file present, but that go.mod's module path doesn't
// carry the major-version suffix required for the requested version — the
// semantic import versioning rule (go.dev/ref/mod#major-version-suffix):
// "If a module has a major version of 2 or higher, ... the module path must
// have a corresponding /vN suffix". Confirmed live (2026-09-26) against
// three real, independently affected public repos, all still exhibiting the
// bug today (old tags predating a later /vN-suffix fix, still reachable by
// an explicit version query): @v/v2.16.0.info for github.com/osrg/gobgp,
// @v/v2.14.2.info for github.com/mislav/hub, and @v/v2.10.0.info for
// github.com/git-lfs/git-lfs each 404 with this exact wording (differing
// only in the module path and suggested suffix) — even though @latest and
// @v/list are both healthy for all three (moduleKnown() is true, so this
// isn't module-unknown either). `go get github.com/osrg/gobgp@v2.16.0` in a
// real module reproduces the identical message verbatim and fails outright
// (exit 1) — not the "wait and retry" situation a bare 404 for a listed
// module usually means.
//
// Without this check, this 404 fell through to the not-yet-indexed/
// negative-cache fallback at the bottom of diagnose (the requested version
// never appears in @v/list either, since the proxy excludes major-version-
// mismatched tags from it, so it landed on statusNotYetIndexed specifically)
// — "ordinary indexing lag, retry in a minute, or use --wait", which is
// actively wrong: this is a permanent property of the go.mod committed at
// that tag, exactly like isZipBuildError right above. No amount of waiting
// or retrying fixes it; only a new tag with the corrected module path does
// (or, for a pre-existing tag, nothing — it can never resolve as requested).
const majorVersionMismatchMarker = "so module path must match major version"

// majorVersionMismatchSuggestionPattern extracts the parenthesized canonical
// path proxy.golang.org's error body suggests, e.g. from `...major version
// ("github.com/osrg/gobgp/v2")` this captures `github.com/osrg/gobgp/v2`.
var majorVersionMismatchSuggestionPattern = regexp.MustCompile(`major version \("([^"]+)"\)`)

// majorVersionMismatchSuggestion returns the corrected module path
// proxy.golang.org's own error body names, or "" if the body doesn't match
// the expected quoted-suggestion shape (defensive: callers fall back to
// generic advice instead of interpolating a blank path into a message).
func majorVersionMismatchSuggestion(body string) string {
	m := majorVersionMismatchSuggestionPattern.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// modPathWrongMajorMarker is the distinctive substring proxy.golang.org
// includes in the plain-text body of a 404 response when the requested
// version's go.mod, if any, declares a DIFFERENT major-version path than
// the one actually requested — the mirror image of
// majorVersionMismatchMarker right above (a go.mod committed with NO
// major-version suffix at all, for a version that requires one): this is a
// go.mod that does carry a suffix, just not the one that was asked for.
//
// Confirmed live (2026-09-30), a common real-world shape: a module that
// renamed itself and bumped its major-version path (e.g. a repo moving
// from an unsuffixed/lower-major import path to a new /vN one) leaves its
// OLD tags — still reachable by an explicit version query — with their
// own go.mod still naming the prior path. Querying
// github.com/redis/go-redis/v9@v8.0.0 (v8.0.0 is a real, currently-tagged
// go-redis release, from before the repo's v9 rename) 404s with `invalid
// version: go.mod has non-.../v9 module path "github.com/go-redis/redis/v8"
// (and .../v9/go.mod does not exist) at revision v8.0.0` — and a real `go
// get github.com/redis/go-redis/v9@v8.0.0` fails outright with this exact
// message right now, under the real, unmodified default GOPROXY chain
// (proxy.golang.org then direct both agree, since this is evaluated from
// the tag's own committed go.mod content, not a proxy-side cache — so
// unlike the negative-cache/not-yet-indexed cases, a direct-VCS fallback
// doesn't help either). v8.0.0 can never retroactively declare a /v9 path:
// only an actual v9.x.y tag (at the /v9 path) resolves.
//
// Without this check, this 404 matched none of this file's other markers —
// isUnknownRevision requires "unknown revision", majorVersionMismatchMarker
// requires "so module path must match major version" (a completely
// different phrase for the opposite direction) — and fell through to the
// generic not-yet-indexed fallback ("ordinary indexing lag ... retry in a
// minute, or use --wait"), the same doomed-poll shape
// majorVersionMismatchMarker already exists to prevent for the other
// direction.
const modPathWrongMajorMarker = "has non-"

// isModPathWrongMajor reports whether body is the modPathWrongMajorMarker
// shape. Requiring "module path" alongside the marker (rather than the bare
// substring alone) keeps this from ever colliding with
// majorVersionMismatchMarker's own body, which also happens to contain
// "module path" but never "has non-" — the two markers are checked as
// alternatives, in either order, with no shared body ever matching both.
func isModPathWrongMajor(body string) bool {
	return strings.Contains(body, modPathWrongMajorMarker) && strings.Contains(body, "module path")
}

// unknownRevisionMarker is the distinctive substring proxy.golang.org
// includes in the plain-text body of a 404 response when the requested
// version query — a branch name, a raw or embedded commit hash, or a
// malformed/fabricated pseudo-version — doesn't correspond to any revision
// that actually exists in the module's repository. Confirmed live
// (2026-09-27) against two independent proxy backends: a fabricated
// pseudo-version and a nonexistent branch name against golang.org/x/tools
// (Google's go.googlesource.com-backed proxy) both 404 with "invalid
// version: unknown revision <name>", and the identical wording (minus the
// git-ls-remote preamble a cold/never-fetched module gets) appears for a
// bogus tag or branch against a live, already-indexed GitHub-hosted module
// (github.com/experimental-gains/agent-bootstrap-log) — `go get` fails
// outright with this exact message in both cases, immediately, not after
// any delay. This is a permanent property of the query itself (the named
// revision does not exist), not the not-yet-indexed/negative-cache timing
// conditions the fallback at the bottom of diagnose assumes: verified live
// that a real, freshly-pushed tag on an already-public GitHub repo resolves
// correctly on the very first probe after the push reaches GitHub (no
// "unknown revision" transient state observed), so this marker doesn't
// collide with genuine indexing lag — see also isZipBuildError and
// majorVersionMismatchMarker right above, the other two "permanent, not a
// timing problem" 404 bodies this file already special-cases.
//
// Without this check, a bogus revision query fell through to the
// not-yet-indexed fallback ("ordinary indexing lag — retry in a minute, or
// use --wait"), which is actively wrong for a revision that will never
// exist, and under --wait polled the full --timeout for an answer that was
// already final on the first probe — the same shape of waste already fixed
// for statusZipBuildError and statusMajorVersionMismatch.
const unknownRevisionMarker = "invalid version: unknown revision"

func isUnknownRevision(body string) bool {
	return strings.Contains(body, unknownRevisionMarker)
}

// invalidPseudoVersionMarker is the distinctive prefix proxy.golang.org
// includes in the plain-text body of a 404 response when the requested
// version has the syntactic shape of a pseudo-version
// (vX.Y.Z-yyyymmddhhmmss-abcdef123456, go.dev/ref/mod#pseudo-versions) but
// fails a check specific to that shape, rather than naming a revision that
// doesn't exist at all (isUnknownRevision, right above, handles that
// separate case). Confirmed live (2026-09-28) with two distinct variants,
// each reproduced against two independent proxy backends (a
// go.googlesource.com-backed module and a GitHub-hosted one):
//
//   - A real, existing commit hash paired with a fabricated/mistyped
//     timestamp segment: querying
//     golang.org/x/mod/@v/v0.0.0-20200101000000-d0a27b2d4a48.info (the real
//     v0.41.0 commit hash, wrong date) 404s with "invalid pseudo-version:
//     does not match version-control timestamp (expected
//     20260824205642)"; github.com/golang/protobuf/@v/
//     v0.0.0-20200101000000-75de7c059e36.info (the real v1.5.4 commit hash,
//     same wrong date) 404s with the identical wording, just a different
//     expected timestamp.
//   - A base-version segment that doesn't correspond to any real preceding
//     tag: golang.org/x/mod/@v/v0.41.5-0.20260824205642-d0a27b2d4a48.info
//     (correct timestamp and commit hash, but v0.41.5's implied preceding
//     tag v0.41.4 was never tagged) 404s with "invalid pseudo-version:
//     preceding tag (v0.41.4) not found".
//
// `go get` fails outright with this exact message in both cases, immediately
// — confirmed live for the first variant: `go get
// golang.org/x/mod@v0.0.0-20200101000000-d0a27b2d4a48` in a fresh module
// exits 1 with "invalid pseudo-version: does not match version-control
// timestamp (expected 20260824205642)" verbatim. Like unknownRevisionMarker,
// this is a permanent property of the version string itself (its encoded
// timestamp or base tag can never retroactively become correct — the real
// commit's timestamp is fixed forever, and a tag that was never cut can't be
// waited for at this exact version string), not the not-yet-indexed/
// negative-cache timing conditions the fallback at the bottom of diagnose
// assumes. A realistic way this happens in practice: a script or CI step
// hand-constructs a pseudo-version from a commit's author date instead of
// its commit date (git tracks both separately and `go` always uses the
// commit date), or derives the "preceding tag" segment from the wrong
// major/minor/patch line.
//
// Without this check, a malformed pseudo-version like this fell through to
// the not-yet-indexed fallback ("ordinary indexing lag — retry in a minute,
// or use --wait"), which is actively wrong: no amount of waiting or
// retrying fixes a timestamp or base-tag that was wrong the moment it was
// written, and under --wait this polled the full --timeout for an answer
// that was already final on the first probe — the same shape of waste
// already fixed for statusUnknownRevision, statusZipBuildError, and
// statusMajorVersionMismatch.
const invalidPseudoVersionMarker = "invalid pseudo-version:"

func isInvalidPseudoVersion(body string) bool {
	return strings.Contains(body, invalidPseudoVersionMarker)
}

// noMatchingVersionsMarker is the distinctive substring proxy.golang.org
// includes in the plain-text body of a 404 response when a version query it
// resolves server-side (a partial/prefix version like "v1.2" or "v1", or a
// branch/revision identifier — see probe()'s doc comment on why those are
// sent to @v/<query>.info directly rather than resolved client-side the way
// comparisonQueryNoMatch/resolveComparisonQuery handle "<v1.2.3"-shaped
// queries) has zero published versions satisfying it. This is the same
// underlying "no matching versions" failure statusNoMatchingVersion already
// exists to report for a comparison query — proxy.golang.org just surfaces
// it through a different endpoint for a prefix/revision query, since this
// tool never resolves those against @v/list itself.
//
// Confirmed live (2026-09-30) two ways, both against the real
// proxy.golang.org: an ordinary nonexistent-prefix query (github.com/pkg/
// errors's real @v/list tops out at v0.9.1; querying @v/v0.99.info 404s with
// `not found: module github.com/pkg/errors: no matching versions for query
// "v0.99"`, and a real `go list -m github.com/pkg/errors@v0.99` fails
// immediately with the identical message, entirely independent of any
// caching/indexing state), and a retraction-driven case (github.com/
// jayconrod/retract's real v1.0.0 and v1.0.1 — the only two versions the
// prefix "v1.0" could match — are both retracted, so @v/v1.0.info 404s the
// same way, and a real `go list -m github.com/jayconrod/retract@v1.0` fails
// with the identical message: proxy.golang.org's server-side resolution for
// this endpoint applies the same retraction-aware algorithm cmd/go's own
// client-side resolution does, not a separate, potentially-diverging one).
//
// Before this check, either shape 404'd with a body matching none of this
// file's other markers, so it fell through all the way to the generic
// not-yet-indexed fallback ("ordinary indexing lag ... retry in a minute, or
// use --wait") — wrong in the same way already fixed for a comparison query
// with no matching version: the query can't resolve no matter how long it's
// retried unless the underlying set of published/retracted versions actually
// changes, which --wait's polling loop cannot cause or detect.
const noMatchingVersionsMarker = "no matching versions for query"

func isNoMatchingVersionsQuery(body string) bool {
	return strings.Contains(body, noMatchingVersionsMarker)
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
		if r.repoReachable != nil && isRepoCheckInconclusive(r.repoCheckStatusCode) {
			repoRoot := githubRepoRoot(r.module)
			detail, explanation := repoCheckInconclusiveDetail(r.repoCheckStatusCode, r.repoCheckErr)
			return diagnosis{statusRepoCheckInconclusive, fmt.Sprintf(
				"proxy.golang.org has never heard of %s (both @latest and @v/list failed), and checking whether https://%s is reachable %s instead of a clear answer. "+
					"%s This is a known way for the check to be inconclusive when run frequently in CI (e.g. via the shipped GitHub Action). "+
					"Check https://%s in a browser, or retry this check in a few minutes; don't treat this the same as a confirmed module-unknown.",
				r.module, repoRoot, detail, explanation, repoRoot)}
		}
		return diagnosis{statusModuleUnknown, "proxy.golang.org has never heard of this module (both @latest and @v/list failed). " +
			"Check: is the repo public? does the module path in go.mod exactly match the repo (case matters)? " +
			"is it covered by a GOPRIVATE/GONOSUMDB pattern that's intentionally excluding it from the public proxy?"}
	}

	// Checked ahead of every other "module known" diagnosis below: a
	// comparison query (comparisonQuery/resolveComparisonQuery in proxy.go)
	// for which zero published versions satisfy the bound is a permanent,
	// offline-provable failure, not the not-yet-indexed/negative-cache timing
	// conditions the fallback at the bottom of this function assumes. Real
	// cmd/go's own query resolution (modload/query.go) fails immediately with
	// `no matching versions for query %q` the moment @v/list comes back with
	// nothing satisfying the bound — confirmed live (2026-09-29): `go get -x
	// golang.org/x/mod@<v0.0.1` issues only @v/list requests (golang.org/x/mod's
	// lowest published version is well above v0.0.1), then fails with that
	// exact message, never requesting any "<v0.0.1"-shaped path at all.
	//
	// Before this check, probe() fell back to sending the literal,
	// never-satisfiable comparison string to the proxy as if it were an
	// ordinary version (the same doomed-request shape
	// TestProbe_ComparisonQueryResolvesViaList already exists to prevent for
	// the *resolvable* comparison-query case) — proxy.golang.org 404s that
	// with a "bad request: invalid escaped version ... invalid char" body
	// matching none of this file's markers, so it fell through all the way to
	// the generic not-yet-indexed fallback ("ordinary indexing lag ... retry
	// in a minute, or use --wait") for a query that could never resolve no
	// matter how long it's retried — the same shape of waste already fixed
	// for "patch", whitespace, and the other permanent failures this file
	// special-cases.
	if r.comparisonQueryNoMatch {
		return diagnosis{statusNoMatchingVersion, fmt.Sprintf(
			"%s@%s: no published version of %s satisfies this comparison query — proxy.golang.org's @v/list has nothing in range. "+
				"A real `go get`/`go install` fails immediately and permanently with `no matching versions for query %q`, without ever contacting the proxy for this literal string. "+
				"This isn't the negative-cache bug or ordinary indexing lag (both require the version to already exist) — --wait and a retry can't fix it. Check the comparison bound, or run `go list -m -versions %s` to see what's actually published.",
			r.module, r.version, r.module, r.version, r.module)}
	}

	// Checked ahead of every "module known" diagnosis below, specifically for
	// a "latest" query: r.moduleKnown() above can be satisfied purely by
	// @v/list succeeding even when @latest itself failed (see moduleKnown's
	// doc comment) — and probe() has no dedicated way to resolve a "latest"
	// query once @latest itself is unusable, since there is no @v/latest.info
	// endpoint: querying it literally always 404s with "invalid version"
	// regardless of module health (see probe()'s "Confirmed live" comment on
	// exactly that). So when @latest fails with a genuine proxy-error status
	// (a 429, 500, ... — a transport error is already ruled out above, and a
	// clean 404/410 is the ordinary "module has no @latest" case handled
	// below), r.versionInfo ends up probed against that literal, meaningless
	// "latest" string instead of any real version, and used to fall through
	// silently to the generic not-yet-indexed fallback further down —
	// "MODULE@latest is not in @v/list yet ... retry in a minute, or use
	// --wait" — even when @v/list plainly listed real tagged versions right
	// there in the same probe.
	//
	// That's not just misleadingly worded, it's the wrong diagnosis: per the
	// documented protocol (see isProxyErrorStatus/proxyErrorDiagnosis above),
	// a real `go install module@latest` does NOT wait out or retry a non-404/
	// 410 error on @latest, it fails outright with that exact error right
	// now. Confirmed directly against cmd/go's own source
	// (modfetch/proxy.go's proxyRepo.Latest): it only falls back to deriving
	// a version from @v/list when @latest fails with a 404/410
	// (fs.ErrNotExist-equivalent, per web/api.go's Response.Err mapping) —
	// any other error status is returned to the caller immediately,
	// unconditionally, with no @v/list fallback at all. Reproduced live with
	// a fake proxy returning HTTP 500 on @latest while @v/list serves two
	// perfectly ordinary tagged versions: before this check, goproxycheck
	// reported statusNotYetIndexed and, under --wait, polled the doomed
	// literal "@v/latest.info" 404 for the full --timeout instead of
	// surfacing the real, actionable @latest error immediately.
	// "upgrade" belongs alongside "latest" here for the same reason it's
	// folded into the same branch in probe(): per cmd/go's own
	// modload/query.go, "upgrade" resolves via the identical Latest lookup as
	// "latest" whenever there's no existing requirement to move up from —
	// always true for a bare goproxycheck module@version argument (see
	// probe()'s "upgrade" comment) — so a genuine @latest proxy error fails a
	// real `go get module@upgrade` immediately and unconditionally too, not
	// just `go get module@latest`.
	if (r.version == "latest" || r.version == "upgrade") && isProxyErrorStatus(r.latest.statusCode) {
		return proxyErrorDiagnosis("proxy.golang.org", "@latest", r.latest.statusCode)
	}

	// Checked ahead of canonicalModulePath/ready/sumdb-lag below (all three
	// require r.versionInfo.ok, same as this): proxy.golang.org and
	// sum.golang.org serve a version's @v/<version>.info, @v/<version>.mod,
	// and sumdb lookup from the raw bytes committed at that tag, with no
	// go.mod syntax validation at all — confirmed live (2026-10-02) by
	// publishing a real, public GitHub module whose go.mod opens with a "/*
	// ... */" block comment (a realistic mistake: a license header
	// copy-pasted from a .go file, where that comment style is normal, into
	// go.mod, where it categorically is not — go.mod's lexer only ever
	// recognizes "//" line comments, confirmed directly against
	// golang.org/x/mod/modfile/read.go). @v/list, @v/<version>.info,
	// @v/<version>.mod, and sum.golang.org/lookup all answered 200 for it —
	// proxy.golang.org happily indexed and served the broken file verbatim
	// — while a real `go get` against the same tag (GOPROXY=direct, bypassing
	// any proxy-side caching) failed immediately and permanently with `go:
	// MODULE@VERSION requires MODULE@VERSION: parsing go.mod: go.mod:1: mod
	// files must use // comments (not /* */ comments)`, entirely offline
	// once the bytes were in hand, before ever consulting sum.golang.org.
	// Before this check, goproxycheck's own canonicalModulePath,
	// retraction, and deprecation checks already parsed this exact go.mod
	// body (modFile/latestModFile) for their own narrower purposes and
	// silently treated a parse failure as "nothing to report" — so this
	// permanent, unconditional failure fell all the way through to a false
	// statusReady ("a plain `go install` will work"), the opposite of
	// reality. golang.org/x/mod/modfile.Parse is the exact parser cmd/go
	// itself uses, so a failure here reproduces a real `go install`/`go
	// get`/`go build` Fatal, not just a stylistic lint.
	if r.versionInfo.ok && r.modFile.ok {
		if _, err := modfile.Parse("go.mod", []byte(r.modFile.body), nil); err != nil {
			return diagnosis{statusGoModUnparseable, fmt.Sprintf(
				"%s: the go.mod committed at this tag doesn't parse: %v. "+
					"proxy.golang.org and sum.golang.org don't validate go.mod syntax before serving/indexing a version, so this can still report as present on both — but a real `go install`/`go get`/`go build` fails outright with this exact error the moment it tries to use this go.mod, entirely offline, regardless of proxy or sumdb state. "+
					"This isn't the negative-cache bug or ordinary indexing lag: it's a permanent property of the file committed at this tag, so --wait and a retry can't fix it. Fix the go.mod (a common cause: a \"/* */\" block comment — go.mod only allows \"//\" line comments) and cut a new tag.",
				displayTarget(r), err)}
		}
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
	//
	// Gated on r.versionInfo.ok (unlike r.latestModFile.ok alone, which only
	// reflects whether the *module* has a @latest, not whether the specific
	// checked *version* was ever published): a retract range is written in
	// version-number order, not against the set of versions that actually
	// exist, so a version that was never tagged can still fall inside it —
	// confirmed live (2026-09-26) against this file's own go-sqlite3 example,
	// whose retract range is [v2.0.0+incompatible, v2.0.7+incompatible] but
	// only v2.0.0-v2.0.3+incompatible were ever published (@v/list). Querying
	// v2.0.5+incompatible — never tagged, confirmed 404 on both
	// @v/v2.0.5+incompatible.info and a real `go install` ("unknown revision
	// v2.0.5") — was misdiagnosed as statusRetracted with a message claiming
	// it "resolves fine through proxy.golang.org and sum.golang.org" before
	// this gate, the exact opposite of what a real `go install` does.
	if r.versionInfo.ok && r.latestModFile.ok {
		if rationale, retracted := retraction(r.latestModFile.body, r.checkVersion()); retracted {
			// "retracted by module author" — not "no rationale was given"
			// (this tool's wording before this fix) — matching the real go
			// command's own phrasing exactly (cmd/go/internal/modload/
			// modfile.go's ModuleRetractedError.Error(): msg := "retracted by
			// module author"; only appended ": "+rationale when one was
			// actually attributed to *this* entry). The distinction matters
			// because an empty rationale here doesn't mean the go.mod gave no
			// explanation at all — golang.org/x/mod/modfile.Parse (the same
			// parser retraction() uses) only attributes a retract block's
			// leading "//" comment to the first version immediately following
			// it, not to every version in a multi-version group sharing that
			// comment. Confirmed live, 2026-09, against the real, current
			// github.com/klauspost/compress go.mod, which groups
			// v1.14.3/v1.14.2/v1.14.1 under one shared comment
			// ("https://github.com/klauspost/compress/pull/503"):
			// mf.Retract[i].Rationale is "" for v1.14.2 and v1.14.1 even
			// though the go.mod plainly explains the whole group. `go list -m
			// -u -retracted -f '{{.Retracted}}'` and `go get` against the real
			// proxy (go1.25.1) both confirm real go never makes the false "no
			// rationale" claim either — they print "retracted by module
			// author" for v1.14.2, the same fixed fallback string used
			// whether or not a sibling entry in the same go.mod happens to
			// carry a comment. See modslop's identical fix for the same
			// underlying x/mod/modfile behavior (check.go's
			// evaluateModuleStatus).
			explain := "retracted by module author"
			if rationale != "" {
				explain = fmt.Sprintf("retracted by module author: %q", rationale)
			}
			// r.versionInfo.ok only confirms the proxy has this version; a
			// still-inflight sum.golang.org (r.sum.ok false) means it hasn't
			// finished replicating yet, so the "will succeed" claim below
			// must stay honestly scoped to that (same principle as the
			// sumdb-lag diagnosis below, which this check runs ahead of).
			resolves := "resolves fine through proxy.golang.org and sum.golang.org — a plain `go install` will succeed"
			if !r.sum.ok {
				resolves = "resolves through proxy.golang.org, but sum.golang.org hasn't caught up yet (ordinary indexing lag, not the negative-cache bug — see sumdb-lag)"
			}
			return diagnosis{statusRetracted, fmt.Sprintf(
				"%s %s — but this exact version is covered by a `retract` directive in the module's own go.mod (%s). "+
					"Retraction is advisory only: `go install`/`go get`/`go mod download` don't consult it and will fetch this version anyway; only `go list -m -u` surfaces it. This isn't a proxy-availability problem `--wait` or a retry can fix — it's the maintainer telling you not to use this version. Use a different version instead.",
				displayTarget(r), resolves, explain)}
		}
	}

	// Checked ahead of ready/sumdb-lag for the same reason as retraction
	// above, and using the same r.latestModFile (not r.modFile): deprecation
	// is a whole-module notice attached to the `module` directive, not a
	// per-version property, so it doesn't affect proxy/sumdb availability
	// either — `go install`/`go get` fetch a deprecated module exactly like
	// any other, they just also print a warning first. Confirmed live
	// (2026-09-26): `go get github.com/golang/protobuf@v1.3.0` prints "go:
	// module github.com/golang/protobuf is deprecated: Use the
	// \"google.golang.org/protobuf\" module instead." even though v1.3.0's
	// own go.mod predates the deprecation comment entirely — see
	// deprecation's doc comment for why r.latestModFile (not r.modFile) is
	// the right source, mirroring probe()'s normalizedMajor-scoped fetch.
	//
	// Checked after retraction rather than before: retraction is the more
	// specific, more urgent signal (this exact version, not just the module
	// in general), so it takes priority on the rare version that's somehow
	// both. Every other version of a deprecated module still gets this
	// check, unaffected by whether that particular version happens to also
	// be retracted.
	if r.versionInfo.ok && r.latestModFile.ok {
		if message, deprecated := deprecation(r.latestModFile.body); deprecated {
			// Same honest scoping as the retraction diagnosis above: don't
			// claim sum.golang.org is caught up when r.sum.ok says otherwise.
			resolves := "resolves fine through proxy.golang.org and sum.golang.org — a plain `go install` will succeed"
			if !r.sum.ok {
				resolves = "resolves through proxy.golang.org, but sum.golang.org hasn't caught up yet (ordinary indexing lag, not the negative-cache bug — see sumdb-lag)"
			}
			return diagnosis{statusDeprecated, fmt.Sprintf(
				"%s %s, but will also print a warning first: the module's own go.mod deprecates it (%q). "+
					"Like retraction, this is advisory only: `go install`/`go get`/`go mod download` still fetch it. Unlike retraction, this applies to every version of the module, not just this one — check the deprecation message for what to use instead.",
				displayTarget(r), resolves, message)}
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

	// Checked alongside isZipBuildError above for the same reason: a 404 on
	// r.versionInfo whose body carries this specific proxy-generated message
	// is a permanent, structural failure, not the negative-cache/not-yet-
	// indexed situation the fallback further down assumes — see
	// majorVersionMismatchMarker's doc comment for the live verification.
	if strings.Contains(r.versionInfo.body, majorVersionMismatchMarker) {
		advice := "add the matching /vN suffix to the module path and cut a new tag"
		if suggestion := majorVersionMismatchSuggestion(r.versionInfo.body); suggestion != "" {
			advice = fmt.Sprintf("use %s instead of %s (if that's a real, existing version there — otherwise the module needs a new tag with that corrected path)", suggestion, r.module)
		}
		return diagnosis{statusMajorVersionMismatch, fmt.Sprintf(
			"%s: this version has a go.mod file, but its module path doesn't carry the major-version suffix Go's semantic import versioning rule requires (go.dev/ref/mod#major-version-suffix) — proxy.golang.org refuses to serve it: %s. "+
				"This isn't the negative-cache bug or ordinary indexing lag: it's a permanent property of the go.mod committed at this tag, so --wait and a retry can't fix it. %s.",
			displayTarget(r), firstLine(r.versionInfo.body), advice)}
	}

	// Checked alongside majorVersionMismatchMarker right above for the same
	// reason, just for the mirror-image mismatch direction — see
	// modPathWrongMajorMarker's doc comment for the live verification
	// against a real, currently-tagged github.com/redis/go-redis version.
	if isModPathWrongMajor(r.versionInfo.body) {
		return diagnosis{statusMajorVersionMismatch, fmt.Sprintf(
			"%s: this version exists, but the go.mod committed at this tag declares a different major-version path than the one requested — proxy.golang.org refuses to serve it: %s. "+
				"This isn't the negative-cache bug or ordinary indexing lag, and a direct-VCS fallback won't help either: it's a permanent property of the go.mod committed at this exact tag (most likely an old tag from before the module renamed/bumped its major-version path), so --wait and a retry can't fix it. Run `go list -m -versions %s` to see what's actually published at this path, or use the module path/version the go.mod at this tag actually declares.",
			displayTarget(r), firstLine(r.versionInfo.body), r.module)}
	}

	// Checked alongside isZipBuildError and majorVersionMismatchMarker above
	// for the same reason: a 404 whose body carries this specific
	// proxy-generated message means the requested version/branch/commit
	// query doesn't name any revision that exists at all — see
	// unknownRevisionMarker's doc comment for the live verification against
	// both a fabricated pseudo-version and a nonexistent branch name, on two
	// independent proxy backends. This can't be the not-yet-indexed/
	// negative-cache situation the fallback further down assumes: those both
	// require the version to be a real revision the proxy just hasn't
	// caught up on yet, not one that never existed in the first place.
	if isUnknownRevision(r.versionInfo.body) {
		return diagnosis{statusUnknownRevision, fmt.Sprintf(
			"%s: proxy.golang.org says this isn't a revision that exists in the module's repository at all: %s. "+
				"This isn't the negative-cache bug or ordinary indexing lag (both require the version to already be a real, existing tag/branch/commit that the proxy just hasn't picked up yet) — it's a permanent invalid-version error, and a real `go install`/`go get` fails outright with this same message right now. --wait and a retry can't fix it. "+
				"Check for a typo in the version, tag, or commit hash, or confirm that revision actually exists in the repository.",
			displayTarget(r), firstLine(r.versionInfo.body))}
	}

	// Checked alongside isUnknownRevision above for the same reason: a 404
	// whose body carries this specific proxy-generated prefix means the
	// pseudo-version string names a real revision but has a malformed
	// timestamp or base-tag segment that can never become correct — see
	// invalidPseudoVersionMarker's doc comment for the live verification
	// across two distinct variants and two independent proxy backends. This
	// is a different, more specific failure than isUnknownRevision: the
	// named commit does exist, it's the pseudo-version encoding around it
	// that's wrong, so it needs its own advice rather than "confirm that
	// revision actually exists."
	if isInvalidPseudoVersion(r.versionInfo.body) {
		return diagnosis{statusInvalidPseudoVersion, fmt.Sprintf(
			"%s: proxy.golang.org rejects this as a malformed pseudo-version: %s. "+
				"This isn't the negative-cache bug or ordinary indexing lag — the named commit/tag isn't in question, but the pseudo-version's own encoded timestamp or base-tag segment doesn't match reality, which can never become correct no matter how long you wait or retry, and a real `go install`/`go get` fails outright with this same message right now. "+
				"Check how this pseudo-version was constructed: `go mod download %s@<commit-hash-or-branch>` (or `go list -m %s@<commit>`) has the proxy compute the correct canonical pseudo-version for you, rather than hand-building the yyyymmddhhmmss-hash segment.",
			displayTarget(r), firstLine(r.versionInfo.body), r.module, r.module)}
	}

	// Checked alongside isUnknownRevision/isInvalidPseudoVersion above for the
	// same reason: a 404 whose body carries this specific proxy-generated
	// message means the query itself (resolved server-side by
	// proxy.golang.org, not client-side by this tool — see
	// noMatchingVersionsMarker's doc comment) has nothing that satisfies it,
	// the same terminal condition comparisonQueryNoMatch/statusNoMatchingVersion
	// already exists to report for a "<"/">"-style comparison query, just
	// reached through a different endpoint for a prefix or revision-shaped
	// query this tool delegates to the proxy instead of resolving itself.
	if isNoMatchingVersionsQuery(r.versionInfo.body) {
		return diagnosis{statusNoMatchingVersion, fmt.Sprintf(
			"%s: proxy.golang.org says no published version satisfies this query: %s. "+
				"This isn't the negative-cache bug or ordinary indexing lag — a real `go get`/`go install` for the identical query fails immediately and the same way right now, regardless of proxy caching or indexing state. "+
				"Check for a typo, or run `go list -m -versions %s` to see what's actually published (a version can still be retracted and excluded from every query's own resolution — see the retracted diagnosis — while still being installable if named explicitly and literally).",
			displayTarget(r), firstLine(r.versionInfo.body), r.module)}
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

	// The fallback below distinguishes not-yet-indexed from negative-cache
	// by searching r.listedVersions(), which only ever returns data when
	// r.list.ok is a clean 200 — a genuine proxy-error status here (429,
	// 500, 502, 503, ...) reads exactly like an empty/absent list (the same
	// zero-information shape as a clean-but-empty 200), silently telling the
	// caller "not in @v/list yet ... ordinary indexing lag" when the truth
	// is "@v/list itself errored, so whether this version is listed is
	// unknown" — it could just as easily be the negative-cache case this
	// tool exists to catch. Every other endpoint already gets this same
	// proxy-error check (see the !moduleKnown() branch above for
	// r.latest/r.list when both fail, and r.versionInfo/r.sum below/above)
	// — the one gap was @v/list's own status when r.latest.ok alone already
	// satisfies moduleKnown(), so the !moduleKnown() branch's list check
	// above is never reached. Reproduced with a fake proxy: @latest 200,
	// @v/list 503, @v/<version>.info a clean 404 — pre-fix this reported
	// statusNotYetIndexed ("retry in a minute, or use --wait") with no
	// mention that @v/list had errored at all.
	if isProxyErrorStatus(r.list.statusCode) {
		return proxyErrorDiagnosis("proxy.golang.org", "@v/list", r.list.statusCode)
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
