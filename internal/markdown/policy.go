package markdown

import (
	"regexp"

	"github.com/microcosm-cc/bluemonday"
)

// checkboxType is the only value the `type` attribute of a task-list input may carry.
//
// It is anchored and case-insensitive rather than a substring test, so `checkbox onmouseover`
// or a trailing payload cannot ride along inside an allowed value. The pattern is a grant
// with a boundary, which is the form every constrained attribute here takes.
var checkboxType = regexp.MustCompile(`(?i)^checkbox$`)

// sanitizerPolicy is the allowlist.
//
// docs/security/threat-model.md T-B1 states its role: "the sanitizer allowlist is the only
// source of permitted elements and attributes". Nothing else in this package decides what
// may appear in the output — the parser produces elements, and this decides which survive —
// which is what makes adding a parser extension safe: a new element is stripped until it is
// named here, deliberately.
//
// # Built as an allowlist, from nothing
//
// bluemonday's `NewPolicy` starts from an empty set, so every element and attribute below is
// an explicit grant. `AllowStandardURLs` and `AllowStandardAttributes` are deliberately
// **not** used: they grant a set this package would then have to audit, and a grant nobody
// wrote down is one nobody reviews. The one exception is documented where it appears.
//
// # What the parser can emit, and nothing more
//
// Every element permitted here is one goldmark produces for the extensions this package
// enables. There is no element that exists only because a document asked for it, because raw
// HTML is escaped before the sanitizer runs. The allowlist is therefore the second of two
// independent barriers rather than the only one, which is what T-B1's "a sanitizer as the
// final step" asks for.
func sanitizerPolicy() *bluemonday.Policy {
	policy := bluemonday.NewPolicy()

	// Block structure.
	policy.AllowElements(
		"p", "br", "hr", "blockquote", "pre", "code",
		"h1", "h2", "h3", "h4", "h5", "h6",
		"ul", "ol", "li",
	)

	// Inline emphasis. `del` is what the strikethrough extension emits.
	policy.AllowElements("em", "strong", "del", "sup", "sub")

	// Tables, from the table extension.
	policy.AllowElements("table", "thead", "tbody", "tfoot", "tr", "th", "td")

	// Links. `href` and `title` are the only attributes a link may carry, and `title` is the
	// attribute context T-B2 names — the sanitizer encodes it, and a test asserts an injected
	// quote cannot escape it.
	policy.AllowAttrs("href", "title").OnElements("a")

	// Images. `src` and `alt`; see the package comment for why the host is not constrained
	// here.
	policy.AllowAttrs("src", "alt", "title").OnElements("img")

	// Task-list checkboxes, which the task-list extension emits as `<input type="checkbox"
	// disabled>`.
	//
	// **Allowing `input` is the one grant worth scrutiny, and it is safe for a reason that
	// does not depend on the sanitizer.** The three attributes below are the only ones
	// permitted — no `name`, no `value`, no `form` — so a checkbox cannot contribute a field
	// to any submission. And there is no path to an *enabled* input: the parser escapes raw
	// HTML, so a document cannot write its own `<input>`, and the extension always emits
	// `disabled`. A disabled checkbox with no name does nothing, needs no script, and is what
	// a task list looks like.
	policy.AllowAttrs("type").Matching(checkboxType).OnElements("input")
	policy.AllowAttrs("checked", "disabled").OnElements("input")

	// # URL schemes
	//
	// The protocol allowlist T-B1 names, and nothing else. A scheme absent from this list has
	// its attribute removed rather than its value cleaned, so `javascript:alert(1)` becomes a
	// link with no `href` — inert text, which is what a reader should see.
	//
	// `data:` is absent deliberately: it is the other scheme T-B1 lists, and a `data:` URL in
	// an `href` or a `src` is a way to carry an executable payload past a content check.
	policy.AllowURLSchemes("http", "https", "mailto")

	// A relative link or a fragment carries no scheme and is therefore always same-origin,
	// which is both useful — a document may link to another page or an anchor on this one —
	// and incapable of executing script.
	policy.AllowRelativeURLs(true)

	// A URL that cannot be parsed is refused rather than passed through. Without this, a
	// malformed value whose scheme the parser cannot identify could survive on the theory
	// that no scheme means relative.
	policy.RequireParseableURLs(true)

	// A link out of the instance is user-generated content, so it is marked as such:
	// `rel="nofollow"` tells a crawler not to treat it as an endorsement, and
	// `rel="noreferrer"` keeps the reader's address off the next origin.
	//
	// It applies to a *fully qualified* link only. A relative link stays inside the instance,
	// where the destination is our own page and a crawler's treatment of it is ours to
	// decide rather than a document's.
	policy.RequireNoFollowOnFullyQualifiedLinks(true)
	policy.RequireNoReferrerOnFullyQualifiedLinks(true)

	// **Comments are not allowed.** bluemonday strips them unless `AllowComments` is called,
	// which is the behaviour wanted: T-B1's test obligations name "HTML comments" among the
	// inputs to remove, and a comment is a place a conditional-comment payload has
	// historically hidden.
	//
	// # Attributes that are not granted, and stay that way
	//
	// No `style`, at any element. No `class`, so a document cannot borrow the instance's
	// markup. No `id`, so it cannot collide with one the page defines. No `on*` handler of any
	// kind, which is the attribute control T-B1 names last. No `target`, so a link cannot
	// reach a new browsing context. Each of those is absent rather than filtered, because an
	// allowlist that has to enumerate what it refuses is not an allowlist.
	return policy
}
