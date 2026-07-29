package markdown

import (
	"context"
	"strings"
	"testing"
)

const recipeBody = "Notes before the recipe.\n\n" +
	"```recipe\n" +
	"yield: 2 drinks\n" +
	"ingredients:\n" +
	"  - {item: Bourbon, amount: 1½ oz}\n" +
	"  - {item: Lime juice, amount: ½ oz}\n" +
	"steps:\n" +
	"  - {in: [bourbon, lime-juice], do: shake, detail: Shake hard with **ice**.}\n" +
	"  - {do: strain}\n" +
	"```\n\n" +
	"Prose after the recipe.\n"

func TestRenderRecipeBlock(t *testing.T) {
	var r Renderer
	html := string(r.Render(context.Background(), recipeBody))

	for _, want := range []string{
		"<p>Notes before the recipe.</p>",
		`class="ff-recipe-grid"`,
		`class="ff-recipe-detail"`,
		"<p>Prose after the recipe.</p>",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in:\n%s", want, html)
		}
	}
	// The block is rendered, not left as source.
	if strings.Contains(html, "ingredients:") {
		t.Error("recipe YAML leaked into the output as text")
	}
	// Step details keep their inline markdown.
	if !strings.Contains(html, "<strong>ice</strong>") {
		t.Error("step detail lost its inline markdown")
	}
}

func TestRecipeBlockSurvivesTheDocumentEditor(t *testing.T) {
	// The editor round-trips a recipe block as editable source; rendering it
	// there would make it uneditable and a save would destroy it.
	var r Renderer
	html := string(r.RenderEditable(context.Background(), recipeBody))
	if !strings.Contains(html, `data-lang="recipe"`) {
		t.Errorf("recipe block should stay a code block in the editor:\n%s", html)
	}
	if !strings.Contains(html, "ingredients:") {
		t.Error("recipe source should be editable verbatim")
	}
}

func TestOtherFencedBlocksAreUntouched(t *testing.T) {
	var r Renderer
	body := "```yaml\nyield: not a recipe\n```\n"
	html := string(r.Render(context.Background(), body))
	if strings.Contains(html, "ff-recipe") {
		t.Error("a plain yaml block must not be treated as a recipe")
	}
	if !strings.Contains(html, "yield: not a recipe") {
		t.Error("code block content should survive")
	}
}

func TestBadRecipeShowsItsError(t *testing.T) {
	var r Renderer
	body := "```recipe\ningredients: [{item: flour}]\nsteps: [{in: [sugar], do: mix}]\n```\n"
	html := string(r.Render(context.Background(), body))
	if !strings.Contains(html, "ff-recipe-error") {
		t.Errorf("a broken recipe should surface its error:\n%s", html)
	}
	if !strings.Contains(html, "unknown id") {
		t.Error("the error should name the problem")
	}
}

func TestUnterminatedFenceIsLeftAlone(t *testing.T) {
	var r Renderer
	body := "before\n\n```recipe\nyield: 1\n"
	html := string(r.Render(context.Background(), body))
	if !strings.Contains(html, "before") {
		t.Errorf("an unterminated fence must not eat the entry:\n%s", html)
	}
}
