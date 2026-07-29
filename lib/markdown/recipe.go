package markdown

import (
	"bytes"
	"fmt"
	"html/template"
	"regexp"
	"strings"

	"github.com/iammatthias/farfield/lib/recipe"
)

// A recipe entry carries its structure in a fenced ```recipe block of YAML.
// The block is the single source of truth for both views the renderer emits —
// the tabular grid and the traditional ingredients-and-steps layout — so the
// two can never drift apart.
//
// The block is lifted out before goldmark sees the body, for the same reason
// blob embeds are: goldmark would render it as a code block, and its contents
// must reach the recipe parser byte for byte.

// recipeFenceRe matches the opening fence of a recipe block. The info string
// is exactly "recipe" so an ordinary ```yaml block stays a code block.
var recipeFenceRe = regexp.MustCompile("^([ \t]*)(`{3,}|~{3,})[ \t]*recipe[ \t]*$")

func recipeTok(i int) string { return fmt.Sprintf("ffrecipeblock%dq", i) }

// extractRecipes replaces every fenced recipe block with a markdown-inert
// placeholder line and returns the rewritten body alongside each block's YAML.
func extractRecipes(body string) (string, []string) {
	if !strings.Contains(body, "recipe") {
		return body, nil
	}
	lines := strings.Split(body, "\n")
	var out []string
	var blocks []string
	for i := 0; i < len(lines); i++ {
		m := recipeFenceRe.FindStringSubmatch(lines[i])
		if m == nil {
			out = append(out, lines[i])
			continue
		}
		fence := m[2]
		var inner []string
		j := i + 1
		for ; j < len(lines); j++ {
			// CommonMark: a closing fence is the same character, at least as
			// long as the opening one, and nothing else on the line.
			t := strings.TrimSpace(lines[j])
			if len(t) >= len(fence) && strings.Trim(t, fence[:1]) == "" {
				break
			}
			inner = append(inner, lines[j])
		}
		if j >= len(lines) {
			// Unterminated fence — leave the source alone rather than eating
			// the rest of the entry.
			out = append(out, lines[i])
			continue
		}
		out = append(out, recipeTok(len(blocks)))
		blocks = append(blocks, strings.Join(inner, "\n"))
		i = j
	}
	return strings.Join(out, "\n"), blocks
}

// recipeRenderer wires the recipe package's prose fields to goldmark's inline
// rendering, so a step detail can carry emphasis, code, and links.
var recipeRenderer = &recipe.Renderer{Inline: inlineHTML}

// renderRecipe parses one block and renders it. A block that does not parse
// falls back to showing its source with the error above it — on the admin
// preview that is exactly the feedback the author needs, and it can never
// silently drop a recipe.
func renderRecipe(src string) string {
	rec, err := recipe.Parse(src)
	if err != nil {
		return `<div class="ff-recipe-error"><p>` + template.HTMLEscapeString(err.Error()) +
			`</p><pre><code>` + template.HTMLEscapeString(src) + `</code></pre></div>`
	}
	return string(recipeRenderer.Render(rec))
}

// inlineHTML renders a prose fragment as inline markdown, unwrapping the
// single paragraph goldmark puts around it.
func inlineHTML(s string) template.HTML {
	var buf bytes.Buffer
	if err := mdSoft.Convert([]byte(s), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(s))
	}
	out := strings.TrimSpace(buf.String())
	if strings.HasPrefix(out, "<p>") && strings.HasSuffix(out, "</p>") &&
		!strings.Contains(out[3:len(out)-4], "<p>") {
		out = out[3 : len(out)-4]
	}
	return template.HTML(out)
}
