package mdrender

import (
	"bytes"
	"sync"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// Render turns markdown into HTML that is safe to insert into the admin origin,
// and it is the only place in this binary that does (D35).
//
// # Why this is a package and not three lines at the call site
//
// The two inputs are a Hugging Face model card and a GitHub release changelog.
// Both are markdown written by strangers, both routinely contain RAW HTML — HF
// cards carry `<img>` badges, `<details>` blocks and, on a repository anyone can
// create, a `<script>` — and both are rendered into the origin that holds the
// admin session cookie. Rendering either one unsanitized is a stored-XSS hole
// with an admin session behind it, so DESIGN section 14 calls bluemonday
// "non-negotiable" and section 1 puts the pipeline in one package. A caller
// cannot get half of it.
//
// # The two layers, and why both
//
// goldmark is configured WITHOUT `html.WithUnsafe`, so raw HTML in the source is
// not passed through to begin with. bluemonday then sanitizes the result anyway.
// That is deliberate belt and braces: the first layer is a renderer option one
// line of refactoring could flip, and the second is a policy that describes what
// may reach a browser regardless of how the HTML was produced.
//
// The policy is bluemonday's UGC policy, which is exactly the case this is —
// user-generated content from an untrusted author. On top of it:
//
//   - only `http`, `https` and `mailto` URLs survive, so `javascript:` and
//     `data:` links cannot;
//   - links get `rel="nofollow noopener noreferrer"` and open in a new tab,
//     because a card's links go to the wider internet and `window.opener` on a
//     page holding an admin session is not something to hand out;
//   - `id` and `class` are dropped rather than allowed, so a card cannot style
//     itself into looking like part of the application, and cannot collide with
//     the app's own ids.
//
// Rendering is pure and has no I/O: nothing here fetches an image, resolves a
// relative link or contacts the origin the markdown came from.

// policy is built once. bluemonday policies are safe for concurrent use, and
// building one per render would be the most expensive part of a 300 KB card.
var policy = sync.OnceValue(func() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowURLSchemes("http", "https", "mailto")
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	// A relative link in a model card resolves against OUR origin, not against
	// huggingface.co, so it points at a page of this application that has
	// nothing to do with the card. Requiring a parseable absolute URL turns
	// those into plain text instead of a link to somewhere confusing.
	p.RequireParseableURLs(true)
	p.AllowRelativeURLs(false)
	return p
})

// md is the renderer. GFM is enabled because model cards are written for
// GitHub-flavored renderers: tables above all — a quantization table rendered as
// a wall of pipes is the single most common way a card becomes unreadable.
var md = sync.OnceValue(func() goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		// Heading ids are deliberately NOT auto-generated: they would be
		// attacker-chosen ids inside the admin document, and the sanitizer
		// strips `id` attributes anyway, so generating them would only cost
		// time. goldmark generates none by default, which is why there is no
		// option here to turn them off.
	)
})

// MaxSourceBytes bounds one document. A model card is a README; the largest ones
// on the Hub are a few hundred kilobytes, and a multi-megabyte one is either a
// mistake or an attempt to make the daemon spend its memory on a stranger's
// file. The excess is dropped and the render proceeds, because a truncated card
// is a better answer than a page that will not load.
const MaxSourceBytes = 1 << 20

// Render renders and sanitizes one markdown document.
//
// It never returns an error: a document goldmark cannot parse is not a
// condition a user can act on, and the honest answer is the sanitized text
// rather than a failed page. What it does guarantee is that the result contains
// no script, no event handler and no non-http(s) URL.
func Render(source string) string {
	if len(source) > MaxSourceBytes {
		source = source[:MaxSourceBytes]
	}
	var buf bytes.Buffer
	if err := md().Convert([]byte(source), &buf); err != nil {
		// Fall back to the escaped source. Convert fails only on a writer error
		// or an internal panic, neither of which makes the TEXT unsafe once the
		// policy has been through it.
		return policy().Sanitize(source)
	}
	return string(policy().SanitizeBytes(buf.Bytes()))
}
