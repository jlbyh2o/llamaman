package mdrender

import (
	"strings"
	"testing"
)

// D35's whole claim: a model card is attacker-controlled markdown containing raw
// HTML, and what reaches the origin holding the admin session cookie carries no
// script, no event handler and no non-http(s) URL.
func TestRenderSanitizes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		source   string
		mustNot  []string
		mustHave []string
	}{
		{
			name:    "a script tag in raw HTML",
			source:  "# Card\n\n<script>fetch('/api/v1/auth/sessions')</script>\n",
			mustNot: []string{"<script", "fetch("},
		},
		{
			name:    "an inline event handler",
			source:  `<img src="https://example.test/a.png" onerror="alert(1)">`,
			mustNot: []string{"onerror", "alert(1)"},
		},
		{
			name:    "a javascript: link",
			source:  "[click me](javascript:alert(1))",
			mustNot: []string{"javascript:"},
		},
		{
			name:    "a data: image",
			source:  `<img src="data:text/html;base64,PHNjcmlwdD4=">`,
			mustNot: []string{"data:text/html"},
		},
		{
			name:    "an iframe",
			source:  `<iframe src="https://example.test/"></iframe>`,
			mustNot: []string{"<iframe"},
		},
		{
			name:    "a style attribute impersonating the application chrome",
			source:  `<div style="position:fixed;top:0">Sign in</div>`,
			mustNot: []string{"position:fixed"},
		},
		{
			name:    "an id that could collide with the application's own",
			source:  `<h2 id="app-root">Card</h2>`,
			mustNot: []string{`id="app-root"`},
		},
		{
			name:     "ordinary markdown survives",
			source:   "# Qwen3 8B\n\nA **great** model. See [the paper](https://example.test/p).\n",
			mustHave: []string{"<h1", "<strong>great</strong>", `href="https://example.test/p"`},
		},
		{
			name:     "a GFM table survives, because a quant table is the point",
			source:   "| Quant | Size |\n|---|---|\n| Q4_K_M | 4.9 GB |\n",
			mustHave: []string{"<table>", "<td>Q4_K_M</td>"},
		},
		{
			name:     "a fenced code block survives",
			source:   "```\nllama-server --model x.gguf\n```\n",
			mustHave: []string{"<pre>", "llama-server --model x.gguf"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := Render(tc.source)
			for _, bad := range tc.mustNot {
				if strings.Contains(got, bad) {
					t.Errorf("the rendered HTML still contains %q:\n%s", bad, got)
				}
			}
			for _, want := range tc.mustHave {
				if !strings.Contains(got, want) {
					t.Errorf("the rendered HTML is missing %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestRenderAddsLinkHardening: a card's links go to the wider internet from a
// page that holds an admin session, so `window.opener` is not handed out.
func TestRenderAddsLinkHardening(t *testing.T) {
	t.Parallel()

	got := Render("[hub](https://huggingface.co/bartowski)")
	for _, want := range []string{`rel="`, "nofollow", "noreferrer", `target="_blank"`} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendered link is missing %q:\n%s", want, got)
		}
	}
}

func TestRenderBoundsTheSource(t *testing.T) {
	t.Parallel()

	// A card larger than the cap renders truncated rather than failing the page.
	got := Render(strings.Repeat("word ", MaxSourceBytes))
	if got == "" {
		t.Fatal("an oversized card rendered to nothing")
	}
	if len(got) > 2*MaxSourceBytes {
		t.Errorf("the rendered output is %d bytes from a capped %d-byte source",
			len(got), MaxSourceBytes)
	}
}
