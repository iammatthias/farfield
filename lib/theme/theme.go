// Package theme exposes the shared farfield CSS, embedded into the binary at
// build time. Apps can serve CSS directly or write it into their own static
// directory. It depends only on the standard library.
package theme

import _ "embed"

// CSS is the shared farfield stylesheet — a small, dependency-free dark theme.
//
//go:embed theme.css
var CSS string
