package main

import "golang.org/x/mod/modfile"

// deprecation reports whether modBody's `module` directive carries a
// deprecation notice (a comment paragraph starting "Deprecated:" immediately
// attached to the module directive, per go.dev/ref/mod#go-mod-file-module),
// returning the message with the "Deprecated:" prefix already stripped.
//
// modfile.Parse already extracts this into mf.Module.Deprecated — no custom
// parsing needed, same as this file's sibling retraction() reuses the same
// package for retract directives.
//
// Like retraction, this is a whole-module, not-just-this-version signal, so
// callers should read it from r.latestModFile (the highest tag in the
// current major-version line), not the checked version's own go.mod.
// Confirmed live (2026-09-26): github.com/golang/protobuf's go.mod carries
//
//	// Deprecated: Use the "google.golang.org/protobuf" module instead.
//	module github.com/golang/protobuf
//
// only as of its latest tag (v1.5.4) — v1.3.0's own go.mod has no such
// comment at all — yet `go get github.com/golang/protobuf@v1.3.0` still
// prints "go: module github.com/golang/protobuf is deprecated: ..." despite
// checking out that old, comment-free go.mod. Reading the checked version's
// own modFile instead of latestModFile would miss the notice for every
// version published before the maintainer added it, exactly the same gap
// probe()'s normalizedMajor-scoped latestModFile fetch already exists to
// close for retract directives (see report.latestModFile's doc comment).
func deprecation(modBody string) (message string, deprecated bool) {
	mf, err := modfile.Parse("go.mod", []byte(modBody), nil)
	if err != nil || mf == nil || mf.Module == nil || mf.Module.Deprecated == "" {
		return "", false
	}
	return mf.Module.Deprecated, true
}
