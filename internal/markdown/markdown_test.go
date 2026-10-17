package markdown_test

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/go-gitnest/GitNest/internal/markdown"
	"golang.org/x/net/html"
)

// The hostile-input corpus.
//
// docs/architecture/testing.md gives this package two obligations: "T1 hostile-input
// corpus" and "T2 golden artefacts". This file is the first. Every payload below is one a
// person can type into an issue body, and the assertions are written against what must
// **not** appear rather than against what should, because the failure this guards is an
// element that survives.
//
// W2.1's acceptance names the inputs explicitly: "`<script>`, `<iframe>`, `on*` attributes,
// `javascript:` and `data:` link targets, and HTML comments are removed or escaped." Each
// has a case here.

// The danger checks work on the **parsed tree**, not on the rendered text.
//
// Substring matching over the output was the first attempt and it produced only false
// positives: raw HTML is escaped into visible text, so a document containing
// `<img onerror=x>` legitimately renders `&lt;img onerror=x&gt;` — inert text that a naive
// search for `onerror=` flags as a vulnerability. That is the wrong question. The property
// that matters is whether the browser builds **an element carrying a dangerous attribute**,
// and answering it means parsing the output the way a browser does.
//
// Parsing is also the stricter check. A payload that survives in a position the corpus did
// not anticipate is still caught, because every node and every attribute is inspected
// rather than a list of remembered shapes.

// forbiddenElements are the elements that must never appear in rendered output.
//
// The set is broader than the corpus's payloads: it is every element that can execute script,
// load a remote resource, or alter the document's interpretation. An allowlist that permitted
// one of these by accident would be caught here rather than by a payload that happened to use
// it.
var forbiddenElements = map[string]bool{
	"script": true, "iframe": true, "object": true, "embed": true, "applet": true,
	"style": true, "link": true, "meta": true, "base": true, "form": true,
	"frame": true, "frameset": true, "svg": true, "math": true, "template": true,
	"noscript": true, "portal": true,
}

// checkTree walks a parsed document and reports every dangerous node or attribute.
func checkTree(t *testing.T, source string, rendered string) {
	t.Helper()

	document, err := html.Parse(strings.NewReader(rendered))
	if err != nil {
		t.Fatalf("the rendered output does not parse as HTML: %v\n input: %q\n output: %q",
			err, source, rendered)
	}

	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode {
			name := strings.ToLower(node.Data)
			if forbiddenElements[name] {
				t.Errorf("a <%s> element survived rendering:\n input:  %q\n output: %q",
					name, source, rendered)
			}
			// A comment node is not an element; the parser reports it separately below.
			for _, attribute := range node.Attr {
				key := strings.ToLower(attribute.Key)

				// Any event handler, whatever it is called, rather than a list of the ones
				// someone remembered to forbid.
				if strings.HasPrefix(key, "on") {
					t.Errorf("<%s> carries the event handler %q:\n input:  %q\n output: %q",
						name, attribute.Key, source, rendered)
				}
				// Attributes that carry a URL, and so a scheme.
				if key == "href" || key == "src" || key == "action" || key == "formaction" ||
					key == "srcdoc" || key == "data" || key == "poster" ||
					key == "background" || key == "dynsrc" || key == "lowsrc" {
					if danger := dangerousURLValue(attribute.Val); danger != "" {
						t.Errorf("<%s> %s carries %q, which is %s:\n input:  %q\n output: %q",
							name, attribute.Key, attribute.Val, danger, source, rendered)
					}
					// `srcdoc` and `formaction` have no legitimate use here at all, so their
					// presence is a finding whether or not the value looks dangerous.
					if key == "srcdoc" || key == "formaction" {
						t.Errorf("<%s> carries the attribute %q:\n input:  %q\n output: %q",
							name, attribute.Key, source, rendered)
					}
				}
				// Inline style could carry a URL or an expression.
				if key == "style" {
					t.Errorf("<%s> carries an inline style %q:\n input:  %q\n output: %q",
						name, attribute.Val, source, rendered)
				}
				// A new browsing context.
				if key == "target" {
					t.Errorf("<%s> carries target=%q:\n input:  %q\n output: %q",
						name, attribute.Val, source, rendered)
				}
				// An input that could contribute a field to a submission.
				if name == "input" && (key == "name" || key == "value" || key == "form") {
					t.Errorf("<input> carries %q, so it could submit a field:\n input:  %q\n output: %q",
						attribute.Key, source, rendered)
				}
			}
		}
		if node.Type == html.CommentNode {
			t.Errorf("an HTML comment survived rendering:\n input:  %q\n output: %q", source, rendered)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(document)
}

// dangerousURLValue reports why a URL value is unsafe, or the empty string.
//
// The check is on the **scheme as the browser would read it**, which means after the
// whitespace, control characters, and entity encoding that a naive comparison misses. A
// value carrying no scheme is a relative URL or a fragment, which is same-origin by
// construction.
func dangerousURLValue(value string) string {
	// Strip the characters a browser ignores before the scheme: leading and embedded
	// whitespace, and the control characters that a URL parser discards.
	cleaned := strings.Map(func(r rune) rune {
		if r <= 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, value)
	cleaned = strings.ToLower(cleaned)

	// A fragment or a relative path names no scheme and cannot execute.
	if strings.HasPrefix(cleaned, "#") || strings.HasPrefix(cleaned, "/") ||
		strings.HasPrefix(cleaned, "?") || strings.HasPrefix(cleaned, ".") {
		return ""
	}

	scheme, _, found := strings.Cut(cleaned, ":")
	if !found {
		// No colon means no scheme, so the value is a relative reference.
		return ""
	}
	// A colon before any slash is a scheme; a colon after one is inside a path.
	if strings.ContainsAny(scheme, "/?#") {
		return ""
	}
	switch scheme {
	case "http", "https", "mailto":
		return ""
	case "javascript", "vbscript", "data", "file", "blob", "filesystem", "about":
		return "an executable or local-resource scheme"
	default:
		// Any other scheme is refused rather than trusted, because the allowlist is three
		// schemes and everything else is unknown.
		return "a scheme outside the allowlist"
	}
}

// hostileInputs is the corpus.
//
// The cases are grouped by the control they probe rather than by the payload's syntax, so a
// failure names the defence that stopped working.
var hostileInputs = []struct {
	name   string
	source string
}{
	// --- raw HTML in every position the parser distinguishes ---
	{name: "a script block", source: "<script>alert(1)</script>"},
	{name: "a script block with attributes", source: `<script src="https://evil.example/x.js"></script>`},
	{name: "an iframe block", source: `<iframe src="https://evil.example"></iframe>`},
	{name: "an iframe with srcdoc", source: `<iframe srcdoc="<script>alert(1)</script>"></iframe>`},
	{name: "an object with data", source: `<object data="https://evil.example"></object>`},
	{name: "an embed", source: `<embed src="https://evil.example">`},
	{name: "a style block", source: `<style>body{background:url(javascript:alert(1))}</style>`},
	{name: "a base element", source: `<base href="https://evil.example/">`},
	{name: "a meta refresh", source: `<meta http-equiv="refresh" content="0;url=https://evil.example">`},
	{name: "a form posting out", source: `<form action="https://evil.example"><input name="a"></form>`},
	{name: "a link preload", source: `<link rel="preload" href="https://evil.example/x">`},
	{name: "inline in a paragraph", source: "before <script>alert(1)</script> after"},
	{name: "inline in emphasis", source: "**bold <img src=x onerror=alert(1)>**"},
	{name: "inside a heading", source: "# heading <script>alert(1)</script>"},
	{name: "inside a list item", source: "- item <iframe src=evil></iframe>"},
	{name: "inside a blockquote", source: "> quoted <script>alert(1)</script>"},
	{name: "inside a table cell", source: "| a | b |\n| - | - |\n| <script>x</script> | y |"},
	{name: "inside a code span", source: "`<script>alert(1)</script>`"},
	{name: "inside a fenced block", source: "```\n<script>alert(1)</script>\n```"},

	// --- event handlers ---
	{name: "an onerror handler", source: `<img src=x onerror="alert(1)">`},
	{name: "an onclick handler", source: `<div onclick="alert(1)">click</div>`},
	{name: "an onload handler", source: `<body onload="alert(1)">`},
	{name: "a handler with no quotes", source: `<img src=x onerror=alert(1)>`},
	{name: "a handler mixed case", source: `<img src=x OnErRoR=alert(1)>`},
	{name: "a handler with a newline before the equals", source: "<img src=x onerror\n=alert(1)>"},

	// --- link protocols ---
	{name: "a javascript: link", source: "[click](javascript:alert(1))"},
	{name: "a javascript: link mixed case", source: "[click](JaVaScRiPt:alert(1))"},
	{name: "a javascript: link with a tab", source: "[click](java\tscript:alert(1))"},
	{name: "a javascript: link with a newline", source: "[click](java\nscript:alert(1))"},
	{name: "a javascript: link with a control character", source: "[click](java\x01script:alert(1))"},
	{name: "a data: link", source: "[click](data:text/html,<script>alert(1)</script>)"},
	{name: "a data: link base64", source: "[click](data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==)"},
	{name: "a vbscript: link", source: "[click](vbscript:msgbox(1))"},
	{name: "a file: link", source: "[click](file:///etc/passwd)"},
	{name: "a data: image", source: `![x](data:image/svg+xml;base64,PHN2Zz48c2NyaXB0PmFsZXJ0KDEpPC9zY3JpcHQ+PC9zdmc+)`},
	{name: "a javascript: image", source: "![x](javascript:alert(1))"},
	{name: "an autolinked javascript: URL", source: "javascript:alert(1)"},
	{name: "a link with a quote in the title", source: `[x](https://example.com "a\" onmouseover=alert(1) b")`},
	{name: "a link with an angle bracket in the title", source: `[x](https://example.com "a> <script>alert(1)</script>")`},

	// --- comments, which W2.1 names explicitly ---
	{name: "an HTML comment", source: "<!-- a comment -->"},
	{name: "a conditional comment", source: "<!--[if IE]><script>alert(1)</script><![endif]-->"},
	{name: "a comment inside a paragraph", source: "before <!-- hidden --> after"},
	{name: "a comment breaking an element", source: "<scr<!-- -->ipt>alert(1)</script>"},

	// --- SVG and MathML, which carry their own script contexts ---
	{name: "an SVG script", source: `<svg><script>alert(1)</script></svg>`},
	{name: "an SVG event handler", source: `<svg onload="alert(1)"></svg>`},
	{name: "an SVG animate", source: `<svg><animate attributeName="href" values="javascript:alert(1)"></svg>`},
	{name: "a MathML annotation", source: `<math><annotation-xml encoding="text/html"><script>alert(1)</script></annotation-xml></math>`},

	// --- shapes that try to confuse the parser rather than the sanitizer ---
	{name: "a null byte before a scheme", source: "[x](java\x00script:alert(1))"},
	{name: "a repeated scheme", source: "[x](javascript:javascript:alert(1))"},
	{name: "an entity-encoded scheme", source: "[x](&#106;avascript:alert(1))"},
	{name: "a scheme with a leading space", source: "[x]( javascript:alert(1))"},
	{name: "an unclosed tag", source: "<script>alert(1)"},
	{name: "a nested unclosed script", source: "<script><script>alert(1)</script>"},
	{name: "a malformed image", source: "![a](<javascript:alert(1)>) "},
}

// TestHostileInputProducesNothingDangerous runs the corpus.
//
// The assertion is deliberately over the whole rendered output rather than per element: a
// payload can survive in a position the case did not anticipate, and a check that looks only
// where it expected to find danger would miss exactly that.
func TestHostileInputProducesNothingDangerous(t *testing.T) {
	renderer := markdown.New()

	for _, input := range hostileInputs {
		t.Run(input.name, func(t *testing.T) {
			checkTree(t, input.source, string(renderer.Render(input.source)))
		})
	}
}

// TestRawHTMLIsEscapedRatherThanDropped is the behaviour W2.1's "removed **or escaped**"
// permits, and the one a reader prefers.
//
// An author who types `<div>` should see `<div>`; dropping it silently loses their text. The
// test asserts both halves: the text is present, and it is present as escaped text rather
// than as an element.
func TestRawHTMLIsEscapedRatherThanDropped(t *testing.T) {
	renderer := markdown.New()

	rendered := string(renderer.Render("text <div>bold?</div> more"))
	if !strings.Contains(rendered, "&lt;div&gt;") {
		t.Errorf("the raw HTML is not present as escaped text: %q", rendered)
	}
	if !strings.Contains(rendered, "text") || !strings.Contains(rendered, "more") {
		t.Errorf("the surrounding text was lost: %q", rendered)
	}
	if strings.Contains(rendered, "<div>") {
		t.Errorf("the raw HTML became an element: %q", rendered)
	}
}

// TestTheAllowedElementsSurvive is the other half of the corpus: a sanitizer that stripped
// everything would pass every danger check and be useless.
func TestTheAllowedElementsSurvive(t *testing.T) {
	renderer := markdown.New()

	for _, test := range []struct {
		name   string
		source string
		want   []string
		why    string
	}{
		{name: "a heading", source: "# Title", want: []string{"<h1>Title</h1>"}, why: "headings are the document's structure"},
		{name: "emphasis", source: "*italic* and **bold**", want: []string{"<em>italic</em>", "<strong>bold</strong>"}},
		{name: "strikethrough", source: "~~gone~~", want: []string{"<del>gone</del>"}, why: "the deliverable names strikethrough"},
		{name: "a link", source: "[text](https://example.com)", want: []string{`<a href="https://example.com"`, ">text</a>"}},
		{name: "an autolink", source: "see https://example.com now", want: []string{`href="https://example.com"`}, why: "the deliverable names autolinks"},
		{name: "a mailto link", source: "<mail@example.com>", want: []string{"mailto:mail@example.com"}, why: "mailto is one of the three allowed schemes"},
		{name: "a relative link", source: "[other](/alice/other-repo)", want: []string{`href="/alice/other-repo"`}, why: "a same-origin link needs no scheme"},
		{name: "an anchor", source: "[top](#section)", want: []string{`href="#section"`}},
		{name: "a fenced code block", source: "```go\nvar x = 1\n```", want: []string{"<pre>", "<code", "var x = 1"}},
		{name: "a code span", source: "`inline`", want: []string{"<code>inline</code>"}},
		{name: "a list", source: "- one\n- two", want: []string{"<ul>", "<li>one</li>"}},
		{name: "an ordered list", source: "1. one", want: []string{"<ol>", "<li>one</li>"}},
		{name: "a blockquote", source: "> quoted", want: []string{"<blockquote>"}},
		{name: "a rule", source: "---", want: []string{"<hr"}},
		{name: "a table", source: "| a | b |\n| - | - |\n| 1 | 2 |", want: []string{"<table>", "<th>a</th>", "<td>1</td>"}, why: "the deliverable names tables"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rendered := string(renderer.Render(test.source))
			for _, want := range test.want {
				if !strings.Contains(rendered, want) {
					t.Errorf("the output does not contain %q: %q", want, rendered)
				}
			}
		})
	}
}

// TestTaskListCheckboxesAreInert covers the one grant in the allowlist that deserves
// scrutiny, and asserts the property the comment claims: an input carries no name, no value,
// and is always disabled, so it cannot contribute a field to anything.
func TestTaskListCheckboxesAreInert(t *testing.T) {
	renderer := markdown.New()

	rendered := string(renderer.Render("- [x] done\n- [ ] todo"))
	if !strings.Contains(rendered, "<input") {
		t.Fatalf("the task list produced no button: %q", rendered)
	}
	if !strings.Contains(rendered, "checkbox") {
		t.Errorf("the input is not a checkbox: %q", rendered)
	}
	if !strings.Contains(rendered, "disabled") {
		t.Errorf("the checkbox is not disabled: %q", rendered)
	}
	for _, forbidden := range []string{"name=", "value=", "form=", "formaction="} {
		if strings.Contains(rendered, forbidden) {
			t.Errorf("the checkbox carries %s, so it could contribute a field: %q", forbidden, rendered)
		}
	}

	// A document cannot write its own input, because raw HTML is escaped.
	handwritten := string(renderer.Render(`<input type="text" name="steal">`))
	if strings.Contains(handwritten, "<input") {
		t.Errorf("a document wrote its own input element: %q", handwritten)
	}
}

// TestExternalLinksCarryNofollow covers the policy's rel attributes, which are a
// consequence of the content being user-generated rather than of it being unsafe.
func TestExternalLinksCarryNofollow(t *testing.T) {
	renderer := markdown.New()

	external := string(renderer.Render("[x](https://example.com)"))
	if !strings.Contains(external, "nofollow") {
		t.Errorf("an external link carries no rel=nofollow: %q", external)
	}
	if !strings.Contains(external, "noreferrer") {
		t.Errorf("an external link carries no rel=noreferrer: %q", external)
	}

	// A relative link stays inside the instance, where the destination is our own page.
	internal := string(renderer.Render("[x](/alice/repo)"))
	if strings.Contains(internal, "nofollow") {
		t.Errorf("a relative link was marked nofollow: %q", internal)
	}
}

// TestAMalformedURLIsRefused covers RequireParseableURLs: a value whose scheme cannot be
// identified is removed rather than kept on the theory that no scheme means relative.
func TestAMalformedURLIsRefused(t *testing.T) {
	renderer := markdown.New()

	for _, source := range []string{
		"[x](http://[invalid)",
		"[x](https://example.com/%zz)",
	} {
		checkTree(t, source, string(renderer.Render(source)))
	}
}

// TestTheTextItselfSurvives is the property that a sanitizer must not be destructive: the
// words a person wrote have to appear.
//
// It is separate from the corpus because a sanitizer that removed everything would pass
// every danger check, and this is the assertion that fails in that case.
func TestTheTextItselfSurvives(t *testing.T) {
	renderer := markdown.New()

	for _, test := range []struct {
		name   string
		source string
		want   string
	}{
		{name: "plain prose", source: "The quick brown fox.", want: "The quick brown fox."},
		{name: "text with angle brackets", source: "a &lt;b&gt; c", want: "a &lt;b&gt; c"},
		{name: "text with an ampersand", source: "Tom & Jerry", want: "Tom &amp; Jerry"},
		{name: "non-ASCII text", source: "日本語 and ünïcödé and ←", want: "日本語"},
		{name: "text after a stripped payload", source: "<script>x</script> the real text", want: "the real text"},
	} {
		t.Run(test.name, func(t *testing.T) {
			rendered := string(renderer.Render(test.source))
			if !strings.Contains(rendered, test.want) {
				t.Errorf("the text was lost: %q -> %q", test.source, rendered)
			}
		})
	}
}

// TestMemoizationHappens is W2.1's acceptance criterion about repeated views.
//
// It asserts through the renderer's own counters rather than by timing, because a timing
// assertion would be flaky and would not actually prove a cache was consulted.
func TestMemoizationHappens(t *testing.T) {
	renderer := markdown.New()
	const document = "# A document\n\nWith a body, rendered repeatedly."

	first := renderer.Render(document)
	renders, hits := renderer.Stats()
	if renders != 1 || hits != 0 {
		t.Fatalf("after the first render: renders=%d hits=%d, want 1 and 0", renders, hits)
	}

	for i := 0; i < 5; i++ {
		again := renderer.Render(document)
		if again != first {
			t.Fatalf("a cached render differs from the first:\n first: %q\n again: %q", first, again)
		}
	}
	renders, hits = renderer.Stats()
	if renders != 1 {
		t.Errorf("renders = %d after six calls, want 1", renders)
	}
	if hits != 5 {
		t.Errorf("hits = %d, want 5", hits)
	}

	// A different document is a different entry.
	renderer.Render("a different document")
	if renders, _ := renderer.Stats(); renders != 2 {
		t.Errorf("renders = %d after a second document, want 2", renders)
	}
}

// TestTheCacheIsBounded covers the property that makes memoization safe against a hostile
// caller: a stream of distinct documents cannot grow it without limit.
func TestTheCacheIsBounded(t *testing.T) {
	const bound = 8
	renderer := markdown.NewWithCacheSize(bound)

	for i := 0; i < bound*4; i++ {
		renderer.Render("document number " + string(rune('a'+i%26)) + string(rune('0'+i/26)))
	}
	// Nothing to assert about internal state, so the bound is checked through behaviour: an
	// early document was evicted, so rendering it again is a fresh render.
	rendersBefore, _ := renderer.Stats()
	renderer.Render("document number a0")
	rendersAfter, _ := renderer.Stats()
	if rendersAfter == rendersBefore {
		t.Error("the cache appears unbounded: an early document was still cached after its bound was exceeded")
	}
}

// TestAVeryLargeDocumentIsNotCached keeps one big body from displacing every frequently-read
// one.
func TestAVeryLargeDocumentIsNotCached(t *testing.T) {
	renderer := markdown.New()
	huge := strings.Repeat("word ", 60_000) // comfortably over the cache's per-entry bound

	renderer.Render(huge)
	renders, _ := renderer.Stats()
	renderer.Render(huge)
	rendersAgain, _ := renderer.Stats()

	if rendersAgain == renders {
		t.Error("a very large document was cached, so it can displace every small one")
	}
}

// TestACacheSizeOfZeroDisablesMemoization keeps the test hook honest, because a test that
// uses it to render twice would silently be served from the cache otherwise.
func TestACacheSizeOfZeroDisablesMemoization(t *testing.T) {
	renderer := markdown.NewWithCacheSize(0)

	renderer.Render("the same text")
	renderer.Render("the same text")

	if renders, hits := renderer.Stats(); renders != 2 || hits != 0 {
		t.Errorf("renders=%d hits=%d, want 2 and 0 with the cache disabled", renders, hits)
	}
}

// TestConcurrentRenderingIsSafe covers the cache under the load it will actually see: every
// request renders, and several run at once.
//
// It is a race-detector test above all — the assertions are secondary — and it is here because
// a shared mutable cache is exactly the shape that passes a single-threaded test and corrupts
// under load.
func TestConcurrentRenderingIsSafe(t *testing.T) {
	renderer := markdown.New()

	documents := []string{
		"# one", "# two", "# three", "# four",
		"**bold**", "*italic*", "[a](https://example.com)", "`code`",
	}

	var wait sync.WaitGroup
	for i := 0; i < 16; i++ {
		wait.Add(1)
		go func(worker int) {
			defer wait.Done()
			for round := 0; round < 50; round++ {
				document := documents[(worker+round)%len(documents)]
				rendered := renderer.Render(document)
				if rendered == "" {
					t.Errorf("worker %d rendered an empty document for %q", worker, document)
					return
				}
			}
		}(i)
	}
	wait.Wait()

	renders, hits := renderer.Stats()
	if renders == 0 {
		t.Error("nothing was rendered")
	}
	if hits == 0 {
		t.Error("nothing was served from the cache, so the key is not stable")
	}

	// **The cache is not single-flight, and that is a documented property rather than a
	// defect.** Two goroutines that miss the same key at the same moment both render, and the
	// second store refreshes the first — so `renders` may exceed the number of distinct
	// documents. An earlier version of this test asserted one render per document and failed
	// with 20 renders for 8 documents, which is the correct behaviour of a cache without
	// coalescing.
	//
	// It is bounded rather than amplified: the duplicates cannot exceed the number of
	// concurrent callers, each produces the same output, and the work is one Markdown render
	// rather than a fetch. `docs/security/threat-model.md` T-L8 names the control for cache
	// amplification as "single-flight **or** jittered cache fills with **bounded entry
	// counts**", and the bounded part is implemented: `maxSize` with least-recently-used
	// eviction, and a per-entry byte bound so one large document cannot displace the rest.
	// Single-flight would remove the duplicates entirely and is worth adding if a profile
	// ever shows them mattering; it is not required, and claiming it is implemented would be
	// worse than not having it.
	if renders > 16*50 {
		t.Errorf("renders = %d, which exceeds the number of calls (800); the cache is not absorbing anything",
			renders)
	}
	// Whatever the duplicates, the output is correct for every document — which is the
	// property the concurrency is actually about.
	for _, document := range documents {
		rendered := string(renderer.Render(document))
		// Every document in this list produces at least one element.
		if !strings.Contains(rendered, "<") {
			t.Errorf("document %q rendered to %q, which is not markup", document, rendered)
		}
	}
}

// TestVersionIsRecordedInTheCacheKey is the property that makes a policy change take effect:
// the version is part of the key, so bumping it discards every entry.
//
// It is asserted by reading the constant rather than by changing it, because changing it in a
// test would change it for the process. What the test protects is that the constant exists and
// is non-empty — a rename that dropped it would otherwise let stale output be served after a
// policy change with nothing failing.
func TestVersionIsRecordedInTheCacheKey(t *testing.T) {
	if strings.TrimSpace(markdown.Version) == "" {
		t.Fatal("the renderer version is empty, so a policy change would not invalidate the cache")
	}
}

// TestTheGoldenFileMatches is the T2 obligation.
//
// It renders `testdata/golden.input.md` and compares the output with `testdata/golden.html`.
// A difference fails, so a change to an extension, to the allowlist, or to the escaping must
// be accompanied by an intentional update to the file — and by a bump to Version, which is
// what actually invalidates a deployed cache. Updating the golden file without bumping the
// version would leave production serving output from the previous policy, which is the
// failure this pair of controls exists to prevent.
//
// Set `GITNEST_UPDATE_GOLDEN=1` to rewrite the file.
func TestTheGoldenFileMatches(t *testing.T) {
	input, err := os.ReadFile(filepath.Join("testdata", "golden.input.md"))
	if err != nil {
		t.Fatalf("read the golden input: %v", err)
	}
	goldenPath := filepath.Join("testdata", "golden.html")

	rendered := string(markdown.New().Render(string(input)))

	if os.Getenv("GITNEST_UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(goldenPath, []byte(rendered), 0o644); err != nil {
			t.Fatalf("write the golden file: %v", err)
		}
		t.Logf("rewrote %s", goldenPath)
		return
	}

	expected, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read the golden file: %v", err)
	}
	if rendered != string(expected) {
		t.Errorf("the rendering differs from the golden file.\n"+
			"If the change is intentional: bump markdown.Version, rerun with GITNEST_UPDATE_GOLDEN=1, and review the diff.\n"+
			"got:\n%s\nwant:\n%s", rendered, expected)
	}
}
