package editor

import (
	"embed"
)

// The brand faces as static TrueType, which the editor's own rasterizer reads
// (the theme's WOFF2 copies are compressed containers it cannot open):
// Newsreader at its 16pt text optical size for the document, IBM Plex Mono
// for code. Both SIL Open Font License; the licences sit beside the files.
//
//go:embed fonts/*.ttf fonts/*.txt
var fontFS embed.FS

// FontSlots lists the files for font slots 0–6, in the order the editor
// expects them: serif regular, semibold, italic, semibold italic, mono, mono
// semibold, serif medium (H2 headings).
var FontSlots = []string{
	"Newsreader16pt-Regular.ttf",
	"Newsreader16pt-SemiBold.ttf",
	"Newsreader16pt-Italic.ttf",
	"Newsreader16pt-SemiBoldItalic.ttf",
	"IBMPlexMono-Regular.ttf",
	"IBMPlexMono-SemiBold.ttf",
	"Newsreader16pt-Medium.ttf",
}

// Font returns one embedded font file by name.
func Font(name string) ([]byte, error) { return fontFS.ReadFile("fonts/" + name) }
