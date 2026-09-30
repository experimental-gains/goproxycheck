package main

import (
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

// retraction reports whether checkVersion is covered by a `retract`
// directive in modBody (the go.mod the proxy serves for the resolved
// version — see proxy.go's modFile fetch, already made unconditionally
// once r.versionInfo.ok for the canonicalModulePath check, so this reuses
// it instead of any new request), returning the maintainer's rationale
// comment when one was given.
//
// A retracted version is not a proxy-availability problem at all:
// proxy.golang.org and sum.golang.org keep serving it exactly like any
// other tagged version. Confirmed live, 2026-09: github.com/mattn/
// go-sqlite3's go.mod (at its latest tag, v1.14.52) carries
//
//	retract (
//		[v2.0.0+incompatible, v2.0.7+incompatible] // Accidental; no major changes or features.
//	)
//
// yet @v/v2.0.3+incompatible.info, sum.golang.org/lookup for that same
// version, and `go mod download github.com/mattn/go-sqlite3@v2.0.3+incompatible`
// all succeed outright — the download trace shows no warning anywhere,
// because retraction is advisory only (only `go list -m -u` surfaces it;
// `go install`/`go get`/`go mod download` never consult it at all). So
// without this check, a version the maintainer explicitly published a
// retraction for — the same "don't use this" signal this tool already
// treats as a hard stop for a proxy-level block (see
// isBlocklistedMalicious) — reports statusReady exactly like a healthy
// one, on the one signal (the go.mod this tool already fetches) that
// would have caught it.
// A go.mod can carry more than one `retract` directive whose ranges both
// cover the same version — not just the single shared-comment-group case
// TestDiagnose_RetractedGroupedCommentNotAttributedToThisVersion already
// covers (one `retract (...)` block, one leading comment attributed to only
// the first entry in the group), but genuinely separate `retract` statements
// written anywhere in the file. Real cmd/go's own CheckRetractions
// (modload/modfile.go) walks *every* retract entry unconditionally — it
// never stops at the first match — ORing `isRetracted` across all of them
// and collecting every matching entry's non-empty Rationale, in file order,
// into a slice; ModuleRetractedError.Error() (and modinfo.ModulePublic.Retracted,
// what `go list -m -u -retracted` actually prints) then uses Rationale[0],
// the first non-empty one collected — not necessarily the rationale on the
// first *matching* entry, if that one happens to carry none.
//
// Before this loop matched real go's, this function returned on the first
// matching entry outright, rationale or not, so a go.mod like:
//
//	retract v1.2.3
//	retract [v1.0.0, v2.0.0] // big incident, avoid this whole range
//
// — where the first, commentless entry covers v1.2.3 and a second, broader,
// explained entry also covers it — reported the bare "retracted by module
// author" for v1.2.3, silently dropping the real explanation. Confirmed live
// (2026-09-30) against a from-scratch module served by a local fake proxy:
// `go list -m -u -retracted -f '{{.Retracted}}'` for this exact go.mod
// prints `[big incident, avoid this whole range]` for v1.2.3, never the bare
// "retracted by module author" this function used to produce.
func retraction(modBody, checkVersion string) (rationale string, retracted bool) {
	mf, err := modfile.Parse("go.mod", []byte(modBody), nil)
	if err != nil || mf == nil {
		return "", false
	}
	for _, r := range mf.Retract {
		if r.Low == "" || r.High == "" {
			continue
		}
		// semver.Compare ignores build metadata (the "+incompatible" suffix)
		// for ordering purposes, matching real semantic-versioning precedence
		// rules — confirmed against the go-sqlite3 case above, where the
		// checked version and both interval bounds all carry it.
		if semver.Compare(checkVersion, r.Low) >= 0 && semver.Compare(checkVersion, r.High) <= 0 {
			retracted = true
			if rationale == "" && r.Rationale != "" {
				rationale = r.Rationale
			}
		}
	}
	return rationale, retracted
}
