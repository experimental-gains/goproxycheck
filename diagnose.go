package main

import "fmt"

type status string

const (
	statusReady         status = "ready"
	statusNegativeCache status = "negative-cache-suspected"
	statusNotYetIndexed status = "not-yet-indexed"
	statusModuleUnknown status = "module-unknown"
	statusSumdbLag      status = "sumdb-lag"
	statusNetworkError  status = "network-error"
)

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
	if r.latest.err != nil || r.list.err != nil {
		return diagnosis{statusNetworkError, fmt.Sprintf("request to proxy.golang.org failed: %v", firstErr(r.latest.err, r.list.err))}
	}

	if !r.moduleKnown() {
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

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
