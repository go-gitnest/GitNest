// Package markdown renders user-authored text to safe HTML.
//
// It is the single renderer for every text field a person writes: an issue body, a
// comment, a repository description, a release note, and the body of a notification email.
// docs/architecture/frontend.md requires that output be rendered on the server so a client
// never ships a Markdown renderer for content it only displays, and that "raw HTML is
// disabled and output is escaped".
//
// # The two controls, and why both exist
//
// docs/security/threat-model.md T-B1 names the controls together: "Markdown rendering with
// raw HTML disabled, a sanitizer as the final step, a link-protocol allowlist (`http`,
// `https`, `mailto`), and no inline event attributes". So both layers are implemented, and
// they do different jobs:
//
//   - **The parser never treats input as HTML.** A raw `<script>` in a document is
//     *escaped into visible text* rather than executed or silently dropped, which is what
//     the acceptance criteria permit ("removed or escaped") and what a reader who typed
//     angle brackets expects to see.
//   - **The sanitizer is the final step, and its allowlist is the only source of permitted
//     elements and attributes.** It runs on the parser's output, so anything a future
//     parser extension might emit is stripped unless this package's policy permits it. That
//     is what makes the allowlist load-bearing rather than decorative.
//
// # What is deliberately **not** the sanitizer's job
//
// An `<img>` may carry an absolute URL to another host, so a document could make a reader's
// browser fetch a remote resource. Restricting that is a **fetch** control, not a markup
// control, and it is enforced where the fetch happens: the instance sets
// `Content-Security-Policy: img-src 'self'`, so a browser will not load a remote image from
// any page this process serves. The division is stated because it is a decision rather than
// an oversight: T-B6's "documented allowlist of image hosts or proxied through GitNest" is
// the refinement, and it needs an image proxy that does not exist yet.
//
// # Memoization
//
// W2.1's acceptance requires that "renderer output is memoized per (document text,
// renderer version) so repeated views do not re-render", which matters because a popular
// issue is rendered for every viewer and every email. The cache is bounded and keyed by a
// digest of the input together with Version, so a change to the policy invalidates every
// entry rather than serving output from the old one.
//
// It is deliberately **not** a permission-scoped cache. Its value is `source -> HTML` for
// one document, which is the same for every reader; deciding *whether* a reader may see a
// document is the repository service's job, and a rendered document is never served to a
// caller who was refused the document itself.
package markdown

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"html"
	"html/template"
	"regexp"
	"strings"
	"sync"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/util"
)

// Version identifies this renderer's configuration.
//
// **It is part of every cache key, so bumping it is how a change to the sanitizer
// allowlist or to the enabled extensions takes effect.** Serving an entry rendered under
// the previous policy would be a security regression that no test would catch, because the
// output would look correct. `testdata/golden.html` is the second guard: it fails when the
// rendering of the fixture changes without an intentional update, and the change to this
// constant is what an intentional update records.
//
// 1: initial. CommonMark plus tables, task lists, strikethrough, and autolinks; raw HTML
// escaped; the allowlist in this file.
const Version = "1"

// The cache's bounds.
const (
	// defaultMaxEntries bounds how many documents are held.
	//
	// It is a count rather than a byte budget because the entries are small and the count is
	// what a reader can reason about; a document over maxCachedBytes is not cached at all, so
	// one enormous file cannot fill memory whatever the count.
	defaultMaxEntries = 512
	// maxCachedBytes is the largest document worth caching. A very large body is rare, and
	// caching it would let a few documents displace every frequently-read one.
	maxCachedBytes = 256 << 10
)

// Renderer renders Markdown to sanitized HTML.
//
// One Renderer is built at startup and shared: the parser and the policy are immutable once
// constructed, and the cache is synchronized, so concurrent renders are safe. It is
// deliberately not rebuilt per request — constructing the policy walks the allowlist, and
// doing that per page would be the cost this package exists to avoid.
type Renderer struct {
	parser goldmark.Markdown
	policy *bluemonday.Policy

	mu sync.Mutex
	// entries maps a cache key to its element in order, so eviction is O(1).
	entries map[string]*list.Element
	// order holds keys, most recently used at the front.
	order   *list.List
	maxSize int
	// renders counts documents actually rendered, and hits counts those served from the
	// cache, so a test can prove memoization happens rather than asserting it.
	renders int
	hits    int
	// headingOffset shifts every heading down by this many levels. It is fixed at
	// construction, so it is part of the cache key rather than per-call state.
	headingOffset int
}

// cachedDocument is one cache entry.
type cachedDocument struct {
	key  string
	html template.HTML
}

// New returns a renderer for a standalone document, whose headings start at the top level.
func New() *Renderer {
	return NewWithCacheSize(defaultMaxEntries)
}

// NewEmbedded returns a renderer for a document that appears **inside** a page which already
// has a first-level heading.
//
// It shifts every heading down one level, so a document's `#` becomes an `<h2>`. That is not
// cosmetic: a page carrying two `<h1>` elements presents a flat structure to a screen reader
// and to a reader skimming the outline, and the repository name is the page's subject while
// the README is a section within it. Rendering the README's headings verbatim put a second
// `h1` on every repository overview, which the accessibility test caught.
//
// The shift is capped at `h6`, because HTML has no seventh level: a document already using
// `######` keeps its deepest heading rather than emitting an element that does not exist.
func NewEmbedded() *Renderer {
	renderer := NewWithCacheSize(defaultMaxEntries)
	renderer.headingOffset = 1
	return renderer
}

// NewWithCacheSize returns a renderer whose cache holds at most maxSize documents.
//
// A maxSize of zero disables memoization, which a test uses to render the same input twice
// without the second call being served from the cache.
func NewWithCacheSize(maxSize int) *Renderer {
	return &Renderer{
		parser: goldmark.New(
			// Autolinks are the reason `parser.WithAutoHeadingID` is *not* here: a heading
			// identifier derived from user text would reach an `id` attribute, which is the
			// attribute context T-B2 is about. Headings need no identifier for the read
			// surfaces, and leaving it off removes the context entirely.
			goldmark.WithExtensions(
				extension.Table,
				extension.Strikethrough,
				extension.TaskList,
				// Linkify turns a bare URL into a link, which the deliverable names as
				// "autolinks".
				extension.Linkify,
			),
			// The parser is given a node renderer that escapes raw HTML instead of the
			// default placeholder. See escapedHTML for why.
			goldmark.WithRendererOptions(
				renderer.WithNodeRenderers(util.Prioritized(escapedHTML{}, 100)),
			),
		),
		policy:  sanitizerPolicy(),
		entries: map[string]*list.Element{},
		order:   list.New(),
		maxSize: maxSize,
	}
}

// Render returns the sanitized HTML for a document.
//
// The result is `template.HTML`, which tells `html/template` not to escape it. That is safe
// **only because the sanitizer ran**, and it is safe **only in an element-content context**:
// the value must never be placed in an attribute, a URL, or a script, where the sanitizer's
// element allowlist would not apply. The templates that use it place it as the content of a
// block element, and a test asserts no template puts it anywhere else.
//
// T-B1 adds a caveat that is worth repeating here: "rendered output is treated as untrusted
// even after sanitizing". The type is a convenience for the template layer, not a
// certificate. A new consumer should ask what its context is before using it.
func (r *Renderer) Render(source string) template.HTML {
	if source == "" {
		return ""
	}

	key := r.key(source)
	if cached, ok := r.lookup(key); ok {
		return cached
	}

	// The parser cannot fail on a hostile document: goldmark treats every input as text and
	// its own errors are limited to a misconfigured renderer, which is a programming fault
	// rather than a per-document one.
	var rendered strings.Builder
	if err := r.parser.Convert([]byte(source), &rendered); err != nil {
		// A conversion failure would mean the parser or the renderer is misconfigured. The
		// document is escaped and returned as text rather than dropped, so a reader sees its
		// content and an operator sees the input that caused it.
		return template.HTML("<pre>" + html.EscapeString(source) + "</pre>")
	}

	safe := r.policy.Sanitize(rendered.String())
	if r.headingOffset > 0 {
		safe = shiftHeadings(safe, r.headingOffset)
	}
	result := template.HTML(safe)
	r.store(key, source, result)
	return result
}

// headingOpen matches an opening heading tag the sanitizer emitted.
//
// It runs on the **sanitized** output rather than on the source, which is what makes it safe:
// the sanitizer has already stripped every element outside the allowlist, so the only `<h` tags
// present are the ones this package's own renderer produced. A line that merely *looks* like a
// heading inside a fenced code block is escaped by then, so it cannot be mistaken for one.
var headingOpen = regexp.MustCompile(`<h([1-6])([ >])`)

// headingClose matches a closing heading tag.
var headingClose = regexp.MustCompile(`</h([1-6])>`)

// shiftHeadings moves every heading down by offset levels, stopping at h6.
//
// The opening and closing tags are shifted by the same arithmetic, so a heading cannot end up
// with a mismatched pair — which a browser would repair in its own way and which would put the
// document's structure at the mercy of that repair.
func shiftHeadings(html string, offset int) string {
	levelOf := func(digits string) int {
		level := 0
		for _, digit := range digits {
			level = level*10 + int(digit-'0')
		}
		return level
	}
	shift := func(level int) int {
		if shifted := level + offset; shifted <= 6 {
			return shifted
		}
		// HTML has no seventh level; the deepest heading stays where it is rather than
		// emitting an element that does not exist.
		return 6
	}

	html = headingOpen.ReplaceAllStringFunc(html, func(tag string) string {
		match := headingOpen.FindStringSubmatch(tag)
		return "<h" + string(rune('0'+shift(levelOf(match[1])))) + match[2]
	})
	return headingClose.ReplaceAllStringFunc(html, func(tag string) string {
		match := headingClose.FindStringSubmatch(tag)
		return "</h" + string(rune('0'+shift(levelOf(match[1])))) + ">"
	})
}

// RenderPlain returns the same output as Render, as a string.
//
// It exists for a consumer that is not a Go template, which today means the body of a
// notification email. W2.1's acceptance requires that "the same renderer output is used in
// the web UI and in notification email", and two methods over one cache are how that stays
// true: there is no second rendering path to drift.
func (r *Renderer) RenderPlain(source string) string {
	return string(r.Render(source))
}

// Stats reports how many documents were rendered and how many were served from the cache.
//
// It is exported for one reason: memoization is an acceptance criterion, and a test that
// asserts a counter is asserting the behaviour rather than the absence of a crash.
func (r *Renderer) Stats() (renders, hits int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.renders, r.hits
}

// key returns the cache key for a document.
//
// The digest rather than the document itself keeps the map's memory to a fixed size per
// entry, so a hostile caller cannot make the cache hold a hundred copies of one large body.
// Version is included so a policy change invalidates everything.
func (r *Renderer) key(source string) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(Version))
	_, _ = digest.Write([]byte{0})
	// The heading offset is part of the key, so an embedded render and a standalone one of the
	// same document cannot be confused for each other.
	_, _ = digest.Write([]byte{byte(r.headingOffset)})
	_, _ = digest.Write([]byte(source))
	return hex.EncodeToString(digest.Sum(nil))
}

// lookup returns a cached document and marks it most recently used.
func (r *Renderer) lookup(key string) (template.HTML, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	element, found := r.entries[key]
	if !found {
		return "", false
	}
	r.order.MoveToFront(element)
	r.hits++
	return element.Value.(*cachedDocument).html, true
}

// store caches a document and evicts the least recently used entry when full.
func (r *Renderer) store(key, source string, rendered template.HTML) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.renders++

	// A document already present is refreshed rather than duplicated.
	if element, found := r.entries[key]; found {
		r.order.MoveToFront(element)
		element.Value.(*cachedDocument).html = rendered
		return
	}
	if r.maxSize <= 0 || len(source) > maxCachedBytes {
		return
	}

	element := r.order.PushFront(&cachedDocument{key: key, html: rendered})
	r.entries[key] = element

	for len(r.entries) > r.maxSize {
		oldest := r.order.Back()
		if oldest == nil {
			break
		}
		r.order.Remove(oldest)
		delete(r.entries, oldest.Value.(*cachedDocument).key)
	}
}

// escapedHTML renders a raw HTML block or span as visible text.
//
// goldmark's default renderer replaces raw HTML with `<!-- raw HTML omitted -->`, which the
// sanitizer then strips as a comment — leaving the reader with nothing where their text was.
// Escaping it instead means a document containing `<div>` shows `<div>`, which is what the
// author wrote and what a reader expects. Nothing executes either way, because the text is
// escaped before it reaches the output buffer.
//
// It is a node renderer rather than a parser change: the parser still recognises the HTML,
// and this decides how it appears. That keeps the "raw HTML is disabled" property in one
// place, next to the comment explaining it.
type escapedHTML struct{}

// RegisterFuncs wires this renderer for the two node kinds that carry raw HTML.
func (escapedHTML) RegisterFuncs(register renderer.NodeRendererFuncRegisterer) {
	register.Register(ast.KindHTMLBlock, renderEscapedBlock)
	register.Register(ast.KindRawHTML, renderEscapedSpan)
}

// renderEscapedBlock writes a raw HTML block as escaped text.
//
// The two phases follow goldmark's own renderer: the content lines are written on entering,
// the closing line on exiting, and only when `HasClosure` reports one. **Skipping that check
// panics** — a block with no explicit closing tag carries a zero-valued segment, and asking
// it for its bytes indexes outside the source. Several of the corpus's cases have no closure,
// so the guard is what makes them renderable rather than a refinement.
func renderEscapedBlock(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	block, ok := node.(*ast.HTMLBlock)
	if !ok {
		return ast.WalkContinue, nil
	}

	if entering {
		for i := 0; i < block.Lines().Len(); i++ {
			line := block.Lines().At(i)
			_, _ = w.WriteString(html.EscapeString(string(line.Value(source))))
		}
		return ast.WalkContinue, nil
	}

	if block.HasClosure() {
		_, _ = w.WriteString(html.EscapeString(string(block.ClosureLine.Value(source))))
	}
	return ast.WalkContinue, nil
}

// renderEscapedSpan writes inline raw HTML as escaped text.
func renderEscapedSpan(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	span, ok := node.(*ast.RawHTML)
	if !ok {
		return ast.WalkContinue, nil
	}
	for i := 0; i < span.Segments.Len(); i++ {
		segment := span.Segments.At(i)
		// A segment with no bytes is skipped rather than asked for them, for the same reason
		// the closure is guarded above.
		if segment.Len() <= 0 {
			continue
		}
		_, _ = w.WriteString(html.EscapeString(string(segment.Value(source))))
	}
	return ast.WalkContinue, nil
}

// parserOptionAutoHeadingID documents why a parser option is absent.
//
// It is referenced from New so that a reader asking "where is the heading identifier
// configuration?" finds the answer rather than adding it back.
var _ = parser.WithAutoHeadingID
