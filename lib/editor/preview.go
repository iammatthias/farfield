package editor

// PreviewSample is the document the preview opens on: every kind of block and
// span the editor styles.
const PreviewSample = `# Field notes

This is **farfield's editor**, drawn entirely by hand-written WebAssembly — the text, the caret, the selection, every pixel. *Italic*, **bold**, ` + "`inline code`" + `, ~~struck~~, and a [link](https://farfield.systems) (Cmd-click to open it).

## Try it

- Type anywhere; the Markdown styles itself as you go
- Press **Enter** at the end of this item to continue the list
- Select words and press Cmd-B, Cmd-I, Cmd-E or Cmd-K
  - Tab nests, Shift-Tab un-nests
1. Numbered lists count up
2. …when you press Enter

> A blockquote keeps its bar as it wraps across lines, and Enter carries the quote along.

` + "```" + `go
func main() {
	fmt.Println("code keeps its indentation")
}
` + "```" + `

---

Double-click selects a word, triple-click a line; Cmd-Z undoes by word → arrows come from the mono face. Smart quotes “like these”, accents — é, ñ, ü — and dashes all come from the brand faces, Newsreader and IBM Plex Mono.
`
