package main

import (
	"net/url"
	"strings"
)

// escapePath implements the Go module proxy's case-encoding: each uppercase
// letter is replaced with an exclamation mark followed by its lowercase
// form, since proxy.golang.org module paths are case-sensitive but many
// filesystems and URLs are not. See https://go.dev/ref/mod#module-proxy.
func escapePath(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r - 'A' + 'a')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// urlPathEscape percent-encodes s (the output of escapePath, for a version
// or revision segment) for safe inclusion in an HTTP request URL string,
// mirroring cmd/go's own pathEscape (modfetch/proxy.go) exactly: "escapes
// things like ? and # (which really shouldn't appear anyway). It does not
// escape / to %2F: our REST API is designed so that / can be left as is."
//
// Needed because every proxy/sumdb URL this tool builds (proxy.go's probe())
// is a plain fmt.Sprintf'd string handed to (*http.Client).Get, which parses
// it with url.Parse before ever issuing the request. url.Parse treats an
// unescaped '#' as the start of a URL fragment — silently discarding it and
// everything after it, so the request actually sent is for a shorter,
// different path than the one this tool meant to check — and can fail
// outright on a malformed '%xy' escape sequence. Real cmd/go never hits
// either problem: it builds a url.URL struct directly with Path/RawPath
// already escaped via this identical algorithm (newProxyRepo/getBody in
// modfetch/proxy.go), so an unescaped special character never round-trips
// through url.Parse at all.
//
// '#' matters in practice because golang.org/x/mod/module.EscapeVersion —
// resolveTarget's own offline gate for a disallowed version string — allows
// it outright: EscapeVersion validates against fileNameOK, whose documented
// allowed-punctuation set explicitly includes '#' (unlike '?', '*', '<', '>',
// etc., which fileNameOK does reject, and which resolveTarget already turns
// away before ever reaching this file). And '#' is realistic input, not just
// a technicality: git itself accepts a tag or branch name containing it
// (confirmed live with `git check-ref-format --branch release#123`, a
// plausible way to fold an issue/PR number into a branch name), so a
// revision-identifier version query like "release#123" sails past every
// existing check in resolveTarget and reaches probe()'s URL construction
// completely unescaped.
//
// Confirmed live: handing http.Client.Get the string
// ".../example.com/foo/@v/v1.2.3#issue456.info" sends a request for only
// ".../example.com/foo/@v/v1.2.3" — url.Parse silently drops "#issue456.info"
// as a fragment before any request is made. If "v1.2.3" alone happens to
// already be a real, published version of the module, this tool reports a
// diagnosis for that unrelated version instead of the one actually named
// ("v1.2.3#issue456") — not merely a wasted poll or a confusing error, but a
// wrong answer about a different version entirely.
func urlPathEscape(s string) string {
	return strings.ReplaceAll(url.PathEscape(s), "%2F", "/")
}
