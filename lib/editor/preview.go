package editor

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"html/template"
)

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

var previewTmpl = template.Must(template.New("preview").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Farfield Editor</title>
<style>
:root {
	--surface: #f3e5d1; --panel: #eee0cc; --ink: #0e222d; --accent: #0d3560;
	--hairline: rgba(14, 34, 45, 0.16); --accent-line: rgba(13, 53, 96, 0.32);
	--r-m: 6px; color-scheme: light;
	--font-sans: ui-sans-serif, system-ui, -apple-system, "Helvetica Neue", sans-serif;
	--font-mono: ui-monospace, "SF Mono", Menlo, monospace;
}
@media (prefers-color-scheme: dark) {
	:root:not([data-theme="light"]) {
		--surface: #0e222d; --panel: #132f3d; --ink: #f3e5d1; --accent: #e59f67;
		--hairline: rgba(243, 229, 209, 0.14); --accent-line: rgba(229, 159, 103, 0.36); color-scheme: dark;
	}
}
:root[data-theme="dark"] {
	--surface: #0e222d; --panel: #132f3d; --ink: #f3e5d1; --accent: #e59f67;
	--hairline: rgba(243, 229, 209, 0.14); --accent-line: rgba(229, 159, 103, 0.36); color-scheme: dark;
}
* { box-sizing: border-box; }
body { margin: 0; background: var(--panel); color: var(--ink); font: 14px/1.4 var(--font-sans); }
main { max-width: 64rem; margin: 0 auto; padding: 16px; }
header { display: flex; align-items: baseline; gap: 12px; flex-wrap: wrap; margin: 4px 0 12px; }
header h1 { font-size: 15px; font-weight: 600; margin: 0; letter-spacing: 0.01em; }
header .meta { font: 11px var(--font-mono); text-transform: uppercase; letter-spacing: 0.06em; opacity: 0.6; }
header .spacer { flex: 1; }
.bar { display: flex; flex-wrap: wrap; gap: 4px; margin-bottom: 8px; }
.bar button {
	font: 12px var(--font-mono); color: var(--ink); background: var(--surface);
	border: 1px solid var(--hairline); border-radius: 3px; padding: 5px 8px; cursor: pointer; min-height: 30px;
}
.bar button:hover { border-color: var(--accent-line); }
.bar .sep { width: 1px; background: var(--hairline); margin: 0 4px; }
#editor { height: min(74vh, 56rem); }
footer { margin-top: 8px; font: 11px var(--font-mono); opacity: 0.6; display: flex; justify-content: space-between; gap: 12px; flex-wrap: wrap; }
{{.CSS}}
</style>
</head>
<body>
<main>
	<header>
		<h1>Farfield editor</h1>
		<span class="meta">hand-written wasm · no DOM rendering</span>
		<span class="spacer"></span>
		<button type="button" id="themebtn" class="meta" style="background:none;border:0;color:inherit;cursor:pointer">theme</button>
	</header>
	<div class="bar" id="bar">
		<button data-cmd="bold" title="⌘B"><b>B</b></button>
		<button data-cmd="italic" title="⌘I"><i>I</i></button>
		<button data-cmd="strike" title="⌘⇧X"><s>S</s></button>
		<button data-cmd="code" title="⌘E">code</button>
		<button data-cmd="link" title="⌘K">link</button>
		<span class="sep"></span>
		<button data-cmd="h1" title="⌘⌥1">H1</button>
		<button data-cmd="h2" title="⌘⌥2">H2</button>
		<button data-cmd="h3" title="⌘⌥3">H3</button>
		<span class="sep"></span>
		<button data-cmd="quote" title="⌘⇧9">quote</button>
		<button data-cmd="bullets" title="⌘⇧8">• list</button>
		<button data-cmd="numbers" title="⌘⇧7">1. list</button>
		<button data-cmd="codeblock" title="⌘⌥C">block</button>
		<button data-cmd="rule">rule</button>
		<span class="sep"></span>
		<button data-cmd="undo" title="⌘Z">undo</button>
		<button data-cmd="redo" title="⌘⇧Z">redo</button>
	</div>
	<div id="editor"></div>
	<footer><span id="words"></span><span>⌘S logs the document to the console · nothing here saves anywhere</span></footer>
</main>
<script>{{.JS}}</script>
<script>
(function () {
	function b64(s) { var bin = atob(s), u = new Uint8Array(bin.length); for (var i = 0; i < bin.length; i++) u[i] = bin.charCodeAt(i); return u.buffer; }
	var inline = {{.Inline}};
	var wasm = inline ? b64({{.Wasm}}) : {{.WasmURL}};
	var fonts = inline ? {{.Fonts}}.map(b64) : {{.FontURLs}};
	var words = document.getElementById("words");
	FarfieldEditor.mount(document.getElementById("editor"), {
		wasm: wasm, fonts: fonts, value: {{.Sample}}, placeholder: "Write something…",
		onChange: function () { if (ed) words.textContent = ed.words() + " words"; },
		onSave: function () { console.log(ed.value); words.textContent = ed.words() + " words · logged"; },
	}).then(function (e) {
		window.ed = ed = e;
		words.textContent = e.words() + " words";
		e.focus();
	}).catch(function (err) { document.getElementById("editor").textContent = "editor failed to load: " + err; });
	var ed = null;
	document.getElementById("bar").addEventListener("mousedown", function (e) { e.preventDefault(); });
	document.getElementById("bar").addEventListener("click", function (e) {
		var b = e.target.closest("button[data-cmd]");
		if (b && ed) ed.command(b.dataset.cmd);
	});
	document.getElementById("themebtn").onclick = function () {
		var r = document.documentElement;
		var dark = r.dataset.theme ? r.dataset.theme === "dark" : matchMedia("(prefers-color-scheme: dark)").matches;
		r.dataset.theme = dark ? "light" : "dark";
	};
})();
</script>
</body>
</html>
`))

// PreviewHTML renders the standalone preview page. inline embeds the module
// and fonts as base64 so the page works as a single file; otherwise it loads
// them from prefix (as served by Handler).
func PreviewHTML(inline bool, prefix string) ([]byte, error) {
	bin, err := Wasm()
	if err != nil {
		return nil, err
	}
	data := map[string]any{
		"CSS":      template.CSS(HostCSS),
		"JS":       template.JS(HostJS),
		"Sample":   PreviewSample,
		"Inline":   inline,
		"WasmURL":  prefix + "editor.wasm?v=" + Version(),
		"FontURLs": FontURLs(prefix),
	}
	if inline {
		data["Wasm"] = base64.StdEncoding.EncodeToString(bin)
		var fonts []string
		for _, n := range FontSlots {
			b, err := Font(n)
			if err != nil {
				return nil, err
			}
			fonts = append(fonts, base64.StdEncoding.EncodeToString(b))
		}
		j, _ := json.Marshal(fonts)
		data["Fonts"] = template.JS(j)
	} else {
		data["Wasm"] = ""
		data["Fonts"] = template.JS("[]")
	}
	var buf bytes.Buffer
	if err := previewTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
