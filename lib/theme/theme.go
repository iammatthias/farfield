// Package theme exposes the shared farfield CSS, embedded into the binary at
// build time. Apps can serve CSS directly or write it into their own static
// directory. It depends only on the standard library.
package theme

import _ "embed"

// CSS is the shared farfield stylesheet — a small, dependency-free dark theme.
//
//go:embed theme.css
var CSS string

// EditorJS is the shared editor enhancement: it adds blob/series embedding to
// a markdown <textarea>. Dependency-free vanilla JavaScript; the apps with a
// markdown editor (content, feed) serve it at /static/editor.js.
//
//go:embed editor.js
var EditorJS string

// BandJS is the meta band behavior — the chip row that replaced the editor
// sidebar. Served by the apps whose edit pages carry a [data-band].
//
//go:embed band.js
var BandJS string
