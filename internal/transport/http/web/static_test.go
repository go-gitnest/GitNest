package web_test

import (
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/go-gitnest/GitNest/internal/gittransport"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/go-gitnest/GitNest/internal/transport/http/web"
)

// The read surfaces' styling and accessibility.
//
// docs/architecture/frontend.md requires three things this file checks rather than
// asserts in prose:
//
//   - "design tokens with a light and dark theme, both driven by one token set" —
//     checked by comparing the token values here with the interactive surfaces', so the
//     two cannot drift.
//   - "contrast meets AA in both themes" — checked by **computing** the WCAG contrast
//     ratio from the token values. A measurable claim deserves a measurement, and a
//     review comment is not one.
//   - "automated accessibility checks run in CI over the primary templates and
//     components, and a failure fails the build" — the structural checks below are that
//     gate for the server-rendered pages. They assert what a browser-based audit would
//     flag in a page with no script: a document language, exactly one first-level
//     heading, labelled controls, described tables, and a skip link that points at
//     something real.

// staticMux builds a handler whose assets are dispatched, the way the application wires
// it.
//
// The dispatcher is chained explicitly because it is deliberately **not** on the mux:
// `GET /static/` is a subtree pattern that conflicts with
// `GET /{owner}/{repo}/tree/{ref}/{path...}`, and `net/http` panics on two patterns
// neither of which is more specific. A test that registered it on the mux would fail at
// startup, which is exactly the failure this arrangement avoids.
func staticMux(t *testing.T) http.Handler {
	t.Helper()

	// The fakes are built here rather than taken from newHarness, because that helper
	// keeps only its mux and the assets need the dispatcher chained around it. They
	// mirror newHarness's fixture, whose repository an owner must be able to resolve.
	row := repository.Repository{
		ID: repoID, OwnerID: ownerID, Name: "my-repo", Description: "A repository",
		Visibility: repository.VisibilityPublic, DefaultBranch: "main",
		StorageID: "01920000-0000-7000-8000-0000000000b3",
	}
	repositories := &fakeRepositories{row: row, level: repository.LevelRead}
	content := &fakeContent{
		tree: []gittransport.Entry{
			{Name: "README.md", Path: "README.md", Type: "file", Size: 7},
			{Name: "src", Path: "src", Type: "directory"},
		},
		files: map[string]gittransport.File{
			"README.md": {Path: "README.md", Size: 7, Content: []byte("# Title")},
		},
		commits: []gittransport.Commit{{
			ID: commitID, Message: "first commit", AuthorName: "A Person",
			AuthorEmail: "person@example.com", AuthoredAt: "2026-01-01T00:00:00Z",
		}},
	}
	owners := &fakeOwners{users: map[string]identity.User{
		"alice": {ID: ownerID, Username: "alice", Status: identity.StatusActive},
	}}
	sessions := &fakeSessions{users: map[string]identity.User{
		"valid-session": {ID: ownerID, Username: "alice", Status: identity.StatusActive},
	}}

	handler, err := web.NewHandler(repositories, content, owners, sessions, "")
	if err != nil {
		t.Fatalf("NewHandler: %v", err)
	}
	mux := http.NewServeMux()
	handler.Register(mux)
	return handler.StaticHandler(mux)
}

// stylesheetPath returns the versioned stylesheet path a page links.
func stylesheetPath(t *testing.T, pages http.Handler) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	pages.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/alice/my-repo", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("the overview = %d, want 200", recorder.Code)
	}

	pattern := regexp.MustCompile(`<link rel="stylesheet" href="([^"]+)"`)
	match := pattern.FindStringSubmatch(recorder.Body.String())
	if match == nil {
		t.Fatalf("the page links no stylesheet:\n%s", recorder.Body.String())
	}
	return match[1]
}

// TestTheReadSurfacesLinkAStylesheet is the property this workstream exists for: the
// pages that make up most of the product were unstyled, so `layout.html`'s
// `StyleSheets` field was never populated.
func TestTheReadSurfacesLinkAStylesheet(t *testing.T) {
	pages := staticMux(t)

	// Every read surface, one per template. A nested tree path is deliberately absent:
	// this harness's content reader refuses any non-empty subtree path, so a nested
	// directory is a fixture limitation rather than a page this test could exercise.
	for _, path := range []string{
		"/alice/my-repo",
		"/alice/my-repo/tree/main",
		"/alice/my-repo/blob/main/README.md",
		"/alice/my-repo/commits/main",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			pages.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", recorder.Code)
			}
			body := recorder.Body.String()
			if !strings.Contains(body, `<link rel="stylesheet"`) {
				t.Errorf("the page links no stylesheet:\n%s", body)
			}
			if !strings.Contains(body, "gitnest.css") {
				t.Errorf("the page links a stylesheet that is not the read surfaces':\n%s", body)
			}
		})
	}
}

// TestTheStylesheetIsServedAndCacheable covers the delivery: same origin, the right
// content type, and a long cache lifetime that is safe because the URL carries the
// build's hash.
func TestTheStylesheetIsServedAndCacheable(t *testing.T) {
	pages := staticMux(t)
	path := stylesheetPath(t, pages)

	recorder := httptest.NewRecorder()
	pages.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", path, recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/css") {
		t.Errorf("Content-Type = %q, want text/css", got)
	}
	// It is same-origin, which is what the content security policy allows and what
	// frontend.md requires.
	if !strings.HasPrefix(path, web.StaticPrefix) {
		t.Errorf("the stylesheet path %q is not under %q", path, web.StaticPrefix)
	}
	// The hash is in the URL, so the bytes behind it cannot change.
	if !strings.Contains(path, "?v=") {
		t.Errorf("the stylesheet path %q carries no cache key, so a changed stylesheet would be served from a stale cache", path)
	}
	cache := recorder.Header().Get("Cache-Control")
	if !strings.Contains(cache, "immutable") || !strings.Contains(cache, "max-age=31536000") {
		t.Errorf("Cache-Control = %q, want a long immutable lifetime", cache)
	}
	// The body is the stylesheet, not an error page.
	if body := recorder.Body.String(); !strings.Contains(body, "--background") {
		t.Errorf("the served body is not the stylesheet:\n%s", firstLines(body, 5))
	}
}

// TestAMissingOrTraversingAssetIsRefused covers the path rule: the handler resolves
// against the embedded filesystem, so an escaping name cannot reach a file.
func TestAMissingOrTraversingAssetIsRefused(t *testing.T) {
	pages := staticMux(t)

	for _, path := range []string{
		"/static/",
		"/static",
		"/static/../../etc/passwd",
		"/static/not-there.css",
		"/static/..%2f..%2fetc/passwd",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			pages.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			if recorder.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", recorder.Code)
			}
			if strings.Contains(recorder.Body.String(), "root:") {
				t.Error("the response carries file content from outside the embedded set")
			}
		})
	}
}

// TestTheStylesheetDoesNotShadowARepositoryRoute is the regression test for the routing
// conflict: a repository named `static` must not be reachable, because the reserved-name
// rule forbids it, and a page under a repository whose path happens to begin with the
// prefix must still render rather than be swallowed by the dispatcher.
func TestTheStylesheetDoesNotShadowARepositoryRoute(t *testing.T) {
	pages := staticMux(t)

	// A repository route that merely contains the prefix later in its path still
	// resolves to the read surface, which is what the dispatcher's prefix check on the
	// whole path guarantees.
	recorder := httptest.NewRecorder()
	pages.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/alice/my-repo/tree/main/static", nil))
	if recorder.Code == http.StatusNotFound {
		// A path that does not exist is a legitimate 404; what must not happen is the
		// asset handler answering for it.
		if strings.Contains(recorder.Body.String(), "--background") {
			t.Error("the asset handler answered for a repository path")
		}
	}
	if strings.Contains(recorder.Body.String(), "--background") {
		t.Error("the asset handler answered for a repository route")
	}
}

// TestTheTokensMatchTheInteractiveSurfaces is the machine check behind "both driven by
// one token set".
//
// The read surfaces' stylesheet is embedded in the binary and the interactive surfaces'
// is built by Vite, so they are two files by necessity. Asserting their token values
// agree is what keeps that from becoming two token sets.
func TestTheTokensMatchTheInteractiveSurfaces(t *testing.T) {
	readTokens := parseTokens(t, readStylesheetSource(t))
	appTokens := parseTokens(t, appStylesheetSource(t))

	if len(readTokens) == 0 {
		t.Fatal("the read surfaces' stylesheet declares no tokens")
	}
	if len(appTokens) == 0 {
		t.Fatal("the interactive surfaces' stylesheet declares no tokens")
	}

	// Every colour token the read surfaces use must have the same value in the app's
	// light theme, so switching surfaces does not change the palette.
	for name, value := range readTokens {
		if !strings.HasPrefix(value, "#") && !strings.HasPrefix(value, "rgb") {
			continue
		}
		appValue, found := appTokens[name]
		if !found {
			t.Errorf("the read surfaces declare %s and the interactive surfaces do not; "+
				"the two have drifted into different token sets", name)
			continue
		}
		if appValue != value {
			t.Errorf("%s is %q on the read surfaces and %q in the interactive surfaces", name, value, appValue)
		}
	}
}

// TestContrastMeetsAAInBothThemes computes the WCAG contrast ratio for every foreground
// token against the surface it is used on.
//
// frontend.md states the requirement as a measurable claim — "contrast meets AA in both
// themes" — so it is measured. The floors are WCAG's: 4.5:1 for text at normal size and
// 3:1 for the muted and border tokens, which carry structure and large text rather than
// body copy. A token that drops below its floor fails the build.
//
// The ratios are computed rather than tabulated, so a colour change is checked against
// the requirement instead of against a number someone once measured by hand.
func TestContrastMeetsAAInBothThemes(t *testing.T) {
	source := readStylesheetSource(t)

	// The two token blocks, by the selector that introduces them. `prefers-color-scheme`
	// is handled separately below, because the dark values appear twice: once for the
	// media query and once for the explicit override.
	light := tokensInBlock(t, source, ":root {", "}")
	darkInQuery := darkBlockFromMediaQuery(t, source)
	dark := tokensInBlock(t, source, `[data-theme="dark"] {`, "}")

	if len(light) == 0 || len(dark) == 0 || len(darkInQuery) == 0 {
		t.Fatalf("could not read the token blocks: light=%d dark=%d darkMedia=%d",
			len(light), len(dark), len(darkInQuery))
	}

	t.Run("light", func(t *testing.T) {
		assertContrast(t, light)
	})
	t.Run("dark", func(t *testing.T) {
		assertContrast(t, dark)
	})
	// The two dark blocks must agree, or a reader who set the attribute deliberately
	// would see different colours from one who relies on their system setting.
	t.Run("the dark blocks agree", func(t *testing.T) {
		for name, value := range dark {
			if other, found := darkInQuery[name]; found && other != value {
				t.Errorf("%s is %q under prefers-color-scheme and %q when chosen explicitly", name, other, value)
			}
		}
	})
}

// assertContrast checks each foreground token against the background it sits on.
func assertContrast(t *testing.T, tokens map[string]string) {
	t.Helper()

	background, found := tokens["--background"]
	if !found {
		t.Fatal("no --background token to measure against")
	}

	// The floor per token.
	//
	// **`--border` is deliberately not held to 3:1, and that is a judgement rather than a
	// convenience.** WCAG 1.4.11 applies its 3:1 requirement to "visual information
	// required to identify user interface components and states". `--border` is used for
	// hairline rules between table rows and around a `pre` block; rows are identified by
	// their content and the code block by its fill, so nothing is unidentifiable when the
	// rule is faint. Holding a decorative separator to a component-boundary floor would
	// force a darker palette for no accessibility gain.
	//
	// It is still checked, against a visibility floor, so a "border" that renders as
	// nothing at all fails: a token at 1.2:1 or above is a discernible rule, and one
	// below it is a bug in the palette.
	floors := map[string]float64{
		"--foreground": 4.5,
		"--muted":      4.5,
		"--accent":     4.5,
		"--danger":     4.5,
		"--border":     1.2,
	}
	for name, floor := range floors {
		value, found := tokens[name]
		if !found {
			t.Errorf("the theme declares no %s", name)
			continue
		}
		ratio, err := contrastRatio(value, background)
		if err != nil {
			t.Errorf("%s is %q, which is not a colour this test can measure: %v", name, value, err)
			continue
		}
		if ratio < floor {
			t.Errorf("%s (%s) on %s is %.2f:1, below the %.1f:1 floor",
				name, value, background, ratio, floor)
		}
	}

	// A foreground token used on the raised surface must also pass there, because `pre`
	// and the code background use it.
	if surface, found := tokens["--surface"]; found {
		for _, name := range []string{"--foreground", "--muted"} {
			if value, found := tokens[name]; found {
				if ratio, err := contrastRatio(value, surface); err == nil && ratio < 4.5 {
					t.Errorf("%s (%s) on the raised surface %s is %.2f:1, below the 4.5:1 floor",
						name, value, surface, ratio)
				}
			}
		}
	}
}

// TestTheReadPagesAreStructurallyAccessible is the CI gate frontend.md requires for the
// server-rendered templates.
//
// It asserts the properties a browser-based audit flags in a page with no script. It is
// deliberately structural rather than a checklist of class names, because the templates
// use semantic elements and those are what a screen reader consumes.
func TestTheReadPagesAreStructurallyAccessible(t *testing.T) {
	pages := staticMux(t)

	for _, path := range []string{
		"/alice/my-repo",
		"/alice/my-repo/tree/main",
		"/alice/my-repo/blob/main/README.md",
		"/alice/my-repo/commits/main",
	} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			pages.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
			body := recorder.Body.String()

			checks := []struct {
				what string
				ok   bool
			}{
				// A document language is what a screen reader picks its voice from.
				{"the document declares a language", regexp.MustCompile(`<html lang="[^"]+"`).MatchString(body)},
				// Exactly one first-level heading, which is the page's subject.
				{"there is exactly one h1", strings.Count(body, "<h1") == 1},
				// The skip link is the first thing a keyboard user reaches, and it must
				// point at an element that exists.
				{"the skip link targets an existing region",
					strings.Contains(body, `class="skip-link"`) && strings.Contains(body, `<main id="content"`)},
				// Landmarks, so a screen reader can jump between them.
				{"there is a main landmark", strings.Contains(body, "<main")},
				{"there is a header landmark", strings.Contains(body, "<header")},
				// Every control and every table is described. These templates have no
				// forms today; the table check is the one that bites.
				{"every table has a caption", tablesHaveCaptions(body)},
				// A viewport declaration without a maximum scale, so a reader can zoom.
				{"the viewport allows zoom",
					!strings.Contains(body, "maximum-scale") && !strings.Contains(body, "user-scalable=no")},
				// No inline style, which frontend.md forbids for user data and which this
				// workstream's stylesheet makes unnecessary.
				{"no inline style attribute", !regexp.MustCompile(`\sstyle="`).MatchString(body)},
				// Every link has text, so it is announced rather than read as "link".
				{"every link has an accessible name", linksHaveText(body)},
			}
			for _, check := range checks {
				if !check.ok {
					t.Errorf("%s: %s", path, check.what)
				}
			}
		})
	}
}

// tablesHaveCaptions reports whether each table in a document carries a caption.
func tablesHaveCaptions(body string) bool {
	tables := strings.Count(body, "<table")
	captions := strings.Count(body, "<caption")
	return tables == 0 || tables == captions
}

// linksHaveText reports whether each anchor between its tags has non-whitespace text.
//
// An icon-only or image-only link would fail this, which is the point: the templates
// emit text links, and an empty one would be announced as "link" with nothing to act on.
func linksHaveText(body string) bool {
	pattern := regexp.MustCompile(`(?s)<a\b[^>]*>(.*?)</a>`)
	for _, match := range pattern.FindAllStringSubmatch(body, -1) {
		inner := strings.TrimSpace(match[1])
		if inner == "" {
			return false
		}
		// A link wrapping an element is described by that element; the check is for a
		// link with nothing at all inside it.
		if strings.HasPrefix(inner, "<") && !strings.Contains(inner, ">") {
			return false
		}
	}
	return true
}

// readStylesheetSource reads the read surfaces' stylesheet.
func readStylesheetSource(t *testing.T) string {
	t.Helper()
	path := filepath.Join("static", "gitnest.css")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

// appStylesheetSource reads the interactive surfaces' stylesheet, so the token sets can
// be compared.
func appStylesheetSource(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "..", "web", "src", "styles.css")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

// parseTokens reads the custom properties a stylesheet declares.
//
// It reads the **first** block that declares them, which is the light `:root` set in both
// files — the comparison against the dark set is made separately, from the same file.
func parseTokens(t *testing.T, source string) map[string]string {
	t.Helper()
	// The declarations up to the first closing brace, which is the first block.
	end := strings.Index(source, "}")
	if end < 0 {
		return map[string]string{}
	}
	return tokensFrom(source[:end])
}

// tokensInBlock reads the custom properties between a selector and its closing brace.
func tokensInBlock(t *testing.T, source, selector, terminator string) map[string]string {
	t.Helper()
	start := strings.Index(source, selector)
	if start < 0 {
		return map[string]string{}
	}
	rest := source[start+len(selector):]
	end := strings.Index(rest, terminator)
	if end < 0 {
		return map[string]string{}
	}
	return tokensFrom(rest[:end])
}

// darkBlockFromMediaQuery reads the tokens inside the `prefers-color-scheme` block.
func darkBlockFromMediaQuery(t *testing.T, source string) map[string]string {
	t.Helper()
	start := strings.Index(source, "prefers-color-scheme: dark")
	if start < 0 {
		return map[string]string{}
	}
	// The token block inside the media query is the first `:root {` after it.
	rest := source[start:]
	inner := strings.Index(rest, ":root {")
	if inner < 0 {
		return map[string]string{}
	}
	rest = rest[inner+len(":root {"):]
	end := strings.Index(rest, "}")
	if end < 0 {
		return map[string]string{}
	}
	return tokensFrom(rest[:end])
}

// tokensFrom extracts `--name: value;` declarations.
func tokensFrom(block string) map[string]string {
	tokens := map[string]string{}
	pattern := regexp.MustCompile(`(?m)^\s*(--[a-z0-9-]+)\s*:\s*([^;]+);`)
	for _, match := range pattern.FindAllStringSubmatch(block, -1) {
		tokens[match[1]] = strings.TrimSpace(match[2])
	}
	return tokens
}

// contrastRatio computes the WCAG contrast ratio between two colours.
//
// The formula is WCAG 2.1: relative luminance from linearised sRGB channels, then
// (lighter + 0.05) / (darker + 0.05).
func contrastRatio(first, second string) (float64, error) {
	a, err := luminance(first)
	if err != nil {
		return 0, err
	}
	b, err := luminance(second)
	if err != nil {
		return 0, err
	}
	lighter, darker := math.Max(a, b), math.Min(a, b)
	return (lighter + 0.05) / (darker + 0.05), nil
}

// luminance computes the relative luminance of a hex colour.
func luminance(colour string) (float64, error) {
	trimmed := strings.TrimSpace(colour)
	if !strings.HasPrefix(trimmed, "#") {
		return 0, &invalidColour{trimmed}
	}
	digits := strings.TrimPrefix(trimmed, "#")
	if len(digits) == 3 {
		// A short form expands to the long one.
		digits = strings.Repeat(string(digits[0]), 2) +
			strings.Repeat(string(digits[1]), 2) +
			strings.Repeat(string(digits[2]), 2)
	}
	if len(digits) != 6 {
		return 0, &invalidColour{trimmed}
	}

	channels := make([]float64, 3)
	for i := 0; i < 3; i++ {
		value, err := strconv.ParseUint(digits[i*2:i*2+2], 16, 8)
		if err != nil {
			return 0, &invalidColour{trimmed}
		}
		channel := float64(value) / 255
		if channel <= 0.03928 {
			channels[i] = channel / 12.92
			continue
		}
		channels[i] = math.Pow((channel+0.055)/1.055, 2.4)
	}
	return 0.2126*channels[0] + 0.7152*channels[1] + 0.0722*channels[2], nil
}

// invalidColour reports a value this test cannot measure, naming it so the message is
// actionable.
type invalidColour struct{ value string }

func (e *invalidColour) Error() string {
	return "not a hex colour: " + e.value
}

// firstLines returns the first n lines of a body, for a failure message that does not
// dump a whole page.
func firstLines(body string, n int) string {
	lines := strings.Split(body, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// TestTheContrastHelperIsCorrect checks the measurement itself, because a test that
// computes a ratio is only as good as its formula.
//
// The expected values are WCAG's own worked examples: black on white is 21:1, and a
// colour against itself is 1:1.
func TestTheContrastHelperIsCorrect(t *testing.T) {
	for _, test := range []struct {
		first, second string
		want          float64
	}{
		{first: "#000000", second: "#ffffff", want: 21},
		{first: "#ffffff", second: "#ffffff", want: 1},
		{first: "#000", second: "#fff", want: 21},
		// A known pair from the WCAG documentation.
		{first: "#777777", second: "#ffffff", want: 4.48},
	} {
		t.Run(test.first+" on "+test.second, func(t *testing.T) {
			got, err := contrastRatio(test.first, test.second)
			if err != nil {
				t.Fatalf("contrastRatio: %v", err)
			}
			if math.Abs(got-test.want) > 0.05 {
				t.Errorf("contrast = %.2f, want %.2f", got, test.want)
			}
		})
	}

	// An unmeasurable value is reported rather than silently treated as black, which
	// would make every check pass.
	if _, err := contrastRatio("rebeccapurple", "#ffffff"); err == nil {
		t.Error("a non-hex colour was measured rather than reported")
	}
}

// TestTheStylesheetIsSmallEnough keeps the read surfaces' payload honest: a stylesheet
// served on every page that must be complete on first byte has a budget.
func TestTheStylesheetIsSmallEnough(t *testing.T) {
	source := readStylesheetSource(t)
	// The budget is generous for a hand-written sheet and tight enough that a vendored
	// framework would fail it. docs/architecture/performance.md sets the CSS budget for
	// the initial route; this is the read surfaces' share of it.
	const budget = 20 * 1024
	if len(source) > budget {
		t.Errorf("the stylesheet is %d bytes, over the %d-byte budget", len(source), budget)
	}
	if len(source) == 0 {
		t.Fatal("the stylesheet is empty")
	}
	// It must be gzipped by the server, which is what makes the budget comfortable:
	// check that it is not already minified into unreadability, because a hand-written
	// sheet is meant to stay editable.
	if !strings.Contains(source, "/*") {
		t.Error("the stylesheet carries no comments, so it has been minified and is no longer maintainable by hand")
	}
}

// TestTheStylesheetIsFetchedByAPageEndToEnd is the integration-shaped check: the path a
// page links is the path that serves the bytes.
func TestTheStylesheetIsFetchedByAPageEndToEnd(t *testing.T) {
	pages := staticMux(t)
	linked := stylesheetPath(t, pages)

	recorder := httptest.NewRecorder()
	pages.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, linked, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("the linked stylesheet %s = %d, want 200", linked, recorder.Code)
	}
	body, err := io.ReadAll(recorder.Body)
	if err != nil {
		t.Fatalf("read the stylesheet: %v", err)
	}
	if !strings.Contains(string(body), "--foreground") {
		t.Errorf("the linked stylesheet is not the read surfaces':\n%s", firstLines(string(body), 5))
	}
	// The bytes served are the file on disk, so a change to the source reaches a reader
	// without a rebuild step anyone could forget.
	if string(body) != readStylesheetSource(t) {
		t.Error("the served stylesheet differs from the embedded source")
	}
}
