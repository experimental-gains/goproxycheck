package main

import "strings"

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
