package main

import (
	"bytes"
	"html/template"
	"strings"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
)

// farfieldStyle is a chroma style built from the farfield palette: warm
// paper surface, black ink with opacity-derived grays (pre-composited over
// the surface, since chroma needs concrete colors), and the NASA-red accent
// reserved for literals and errors. Keywords differentiate by weight, not
// hue — the instrument-panel look.
var farfieldStyle = chroma.MustNewStyle("farfield", chroma.StyleEntries{
	chroma.Background:            "bg:#fafaf7 #0a0a0a",
	chroma.Error:                 "#d93a00",
	chroma.LineHighlight:         "bg:#f7ebe3",
	chroma.LineNumbers:           "#b2b2b0",
	chroma.LineNumbersTable:      "#b2b2b0",
	chroma.Comment:               "italic #82827f",
	chroma.CommentPreproc:        "#82827f",
	chroma.Keyword:               "bold #0a0a0a",
	chroma.KeywordType:           "bold #0a0a0a",
	chroma.KeywordConstant:       "bold #0a0a0a",
	chroma.NameFunction:          "#0a0a0a",
	chroma.NameClass:             "bold #0a0a0a",
	chroma.NameTag:               "bold #0a0a0a",
	chroma.NameBuiltin:           "#0a0a0a",
	chroma.NameAttribute:         "#52524f",
	chroma.NameDecorator:         "#82827f",
	chroma.LiteralString:         "#52524f",
	chroma.LiteralStringEscape:   "#d93a00",
	chroma.LiteralStringInterpol: "#d93a00",
	chroma.LiteralNumber:         "#d93a00",
	chroma.Operator:              "#0a0a0a",
	chroma.Punctuation:           "#52524f",
	chroma.GenericDeleted:        "#d93a00",
	chroma.GenericInserted:       "#52524f",
	chroma.GenericEmph:           "italic",
	chroma.GenericStrong:         "bold",
	chroma.GenericSubheading:     "#82827f",
})

// htmlFormatter renders with classes (CSS embedded once per page, not inline
// styles), a line-number gutter, and linkable L<n> anchors for #L10 deep
// links. Format is stateless, so a single formatter serves all requests.
var htmlFormatter = chromahtml.New(
	chromahtml.WithClasses(true),
	chromahtml.WithLineNumbers(true),
	chromahtml.WithLinkableLineNumbers(true, "L"),
	chromahtml.TabWidth(4),
)

// highlightHTML renders a body as highlighted HTML. Unknown or blank langs
// fall back to the plaintext lexer — plain mono, but still with the gutter
// and line anchors.
func highlightHTML(body, lang string) (template.HTML, error) {
	lexer := lexers.Get(lang)
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)
	it, err := lexer.Tokenise(nil, body)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := htmlFormatter.Format(&buf, farfieldStyle, it); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

// highlightCSS returns the stylesheet for the chroma classes, embedded into
// the view page so reading a paste needs no client JS and no extra fetch.
func highlightCSS() template.CSS {
	var buf strings.Builder
	_ = htmlFormatter.WriteCSS(&buf, farfieldStyle)
	return template.CSS(buf.String())
}

// knownLang reports whether chroma has a real lexer for lang.
func knownLang(lang string) bool {
	return lang != "" && lexers.Get(lang) != nil
}
