package references

import (
	"context"
	"strings"
	"testing"
)

// Finding references in a text, and resolving them for one reader.
//
// The two halves are tested together because they are one behaviour from a reader's point of view: what
// a document looks like depends on what was found **and** on what the reader may see. The exclusions get
// their own cases, because a reference found inside a code sample is a visible defect — it changes text
// the author wrote into a link that should not be there.

// markers returns each reference's written form, for an assertion that reads as a list.
func markers(found []Reference) []string {
	rendered := make([]string, 0, len(found))
	for _, reference := range found {
		rendered = append(rendered, reference.String())
	}
	return rendered
}

// TestSameRepositoryReferencesAreFound covers the bare form.
func TestSameRepositoryReferencesAreFound(t *testing.T) {
	for _, test := range []struct {
		text string
		want []string
	}{
		{text: "see #1", want: []string{"#1"}},
		{text: "closes #42 and #7", want: []string{"#42", "#7"}},
		{text: "#1 at the start", want: []string{"#1"}},
		{text: "ends with #9", want: []string{"#9"}},
		{text: "(#3)", want: []string{"#3"}},
		{text: "**#4**", want: []string{"#4"}},
		{text: "a\n\nb #12 c", want: []string{"#12"}},
		{text: "#1\n#2\n#3", want: []string{"#1", "#2", "#3"}},
		// A large number is still a reference; the bound only refuses a pathological run of digits.
		{text: "see #999999", want: []string{"#999999"}},
		// A leading zero reads as the number a reader means, so `#007` and `#7` are one target rather
		// than two.
		{text: "see #007", want: []string{"#7"}},
	} {
		t.Run(test.text, func(t *testing.T) {
			got := markers(Extract(test.text))
			if len(got) != len(test.want) {
				t.Fatalf("found %v, want %v", got, test.want)
			}
			for index := range test.want {
				if got[index] != test.want[index] {
					t.Errorf("position %d is %q, want %q", index, got[index], test.want[index])
				}
			}
		})
	}
}

// TestCrossRepositoryReferencesAreFound covers the qualified form.
func TestCrossRepositoryReferencesAreFound(t *testing.T) {
	for _, test := range []struct {
		text      string
		want      []string
		wantOwner string
		wantRepo  string
	}{
		{text: "see alice/other#5", want: []string{"alice/other#5"}, wantOwner: "alice", wantRepo: "other"},
		{text: "fixes owner/repo-name#12", want: []string{"owner/repo-name#12"}, wantOwner: "owner", wantRepo: "repo-name"},
		{text: "(owner/repo#7)", want: []string{"owner/repo#7"}},
		{text: "**owner/repo#7**", want: []string{"owner/repo#7"}},
		{text: "a.b/c.d#1", want: []string{"a.b/c.d#1"}, wantOwner: "a.b", wantRepo: "c.d"},
		{text: "a_b/c_d#2", want: []string{"a_b/c_d#2"}},
	} {
		t.Run(test.text, func(t *testing.T) {
			found := Extract(test.text)
			got := markers(found)
			if len(got) != len(test.want) {
				t.Fatalf("found %v, want %v", got, test.want)
			}
			if got[0] != test.want[0] {
				t.Errorf("found %q, want %q", got[0], test.want[0])
			}
			if test.wantOwner != "" && (found[0].Owner != test.wantOwner || found[0].Repository != test.wantRepo) {
				t.Errorf("parsed owner=%q repo=%q, want %q and %q",
					found[0].Owner, found[0].Repository, test.wantOwner, test.wantRepo)
			}
			if found[0].SameRepository() {
				t.Error("a qualified reference reports itself as same-repository")
			}
		})
	}
}

// TestAReferenceInsideCodeIsNotFound is the exclusion that matters most.
//
// A reference inside code must not become a link, because substituting there either produces literal
// link syntax where the author wrote `#123` or — for the closing keywords phase 3 depends on — lets a
// keyword quoted in a sample act on the issue.
func TestAReferenceInsideCodeIsNotFound(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
	}{
		{name: "a fenced block, backticks", text: "before\n\n```\ncloses #12\nowner/repo#3\n```\n\nafter"},
		{name: "a fenced block, tildes", text: "before\n\n~~~\ncloses #12\n~~~\n\nafter"},
		{name: "a fenced block with an info string", text: "```markdown\ncloses #12\n```"},
		{name: "an inline code span", text: "write `closes #12` to close"},
		{name: "an inline span with double backticks", text: "write ``closes #12`` to close"},
		{name: "an inline span at the start", text: "`#1` then text"},
		{name: "an indented code block", text: "paragraph\n\n    closes #12\n    and #13"},
		{name: "indented code at the document start", text: "    #1 indented"},
		{name: "a tilde fence with a longer close", text: "~~~~\n#1\n~~~~"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if found := Extract(test.text); len(found) != 0 {
				t.Errorf("found %v inside code, want none", markers(found))
			}
		})
	}
}

// TestAReferenceAfterAFenceIsFoundAgain is the property the first version of the extractor broke.
//
// The fence-tracking state was assigned to variables that an `if` statement shadowed, so the opening
// character was never remembered and **a fence never closed** — everything after the first ``` in a
// document was treated as code, and no reference following it was ever found. This asserts the closing
// actually closes, which a test that only checked "nothing inside a fence" could not have caught.
func TestAReferenceAfterAFenceIsFoundAgain(t *testing.T) {
	text := "a #1\n\n```\ncode #99\n```\n\nb #2\n\n~~~\nmore #98\n~~~\n\nc #3\n"
	got := markers(Extract(text))
	want := []string{"#1", "#2", "#3"}
	if len(got) != len(want) {
		t.Fatalf("found %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("position %d is %q, want %q", index, got[index], want[index])
		}
	}
}

// TestAFenceWithAMatchingCloseWorks covers the closing rule.
//
// The closing run must be the same character and at least as long, which is why the state is tracked
// rather than a single delimiter searched for.
func TestAFenceWithAMatchingCloseWorks(t *testing.T) {
	// A longer closing run closes a shorter opening one.
	if found := Extract("```\n#1\n`````\n#2"); len(found) != 1 || found[0].String() != "#2" {
		t.Errorf("a longer closing run did not close: %v", markers(found))
	}
	// A shorter closing run does not, so the fence stays open.
	if found := Extract("`````\n#1\n```\n#2"); len(found) != 0 {
		t.Errorf("a shorter closing run closed the fence: %v", markers(found))
	}
	// A closing run of the other character does not close a backtick fence.
	if found := Extract("```\n#1\n~~~\n#2"); len(found) != 0 {
		t.Errorf("a tilde run closed a backtick fence: %v", markers(found))
	}
	// A closing run carrying text is not a closing run.
	if found := Extract("```\n#1\n``` still code\n#2"); len(found) != 0 {
		t.Errorf("a closing run with text closed the fence: %v", markers(found))
	}
}

// TestShapesThatAreNotReferences covers what must not be read as one.
func TestShapesThatAreNotReferences(t *testing.T) {
	for _, test := range []struct {
		name string
		text string
	}{
		{name: "a bare hash", text: "a # b"},
		{name: "a heading", text: "# A heading"},
		{name: "a heading with a digit", text: "## 2 is a heading, not a reference"},
		{name: "a url fragment", text: "https://example.com/page#section"},
		{name: "a colour in text", text: "the colour #ff0000"},
		{name: "a hex colour in a style-like run", text: "background: #abcdef"},
		{name: "a word before the hash", text: "abc#1 is not a reference"},
		{name: "an underscore before the hash", text: "snake_case#1 is not a reference"},
		{name: "zero", text: "see #0 for nothing"},
		{name: "a hash with no digits", text: "see #abc"},
		{name: "a hash with a trailing word", text: "see #1abc"},
		{name: "a path with several segments", text: "a/b/c#1 is not owner/repo"},
		{name: "a slash with no owner", text: "/repo#1"},
		{name: "a url with a scheme", text: "https://github.com/owner/repo#5"},
		{name: "an empty line", text: ""},
		{name: "a huge number", text: "see #12345678901234567890"},
	} {
		t.Run(test.name, func(t *testing.T) {
			found := Extract(test.text)
			// `## 2` and `a/b/c#1` legitimately contain a trailing `#2` and `#1`; what must not happen is
			// a reference being read where the document names no issue. So the assertion is on the
			// *qualified* form for the path case and on nothing for the rest.
			for _, reference := range found {
				if test.name == "a path with several segments" || test.name == "a url with a scheme" {
					if !reference.SameRepository() {
						t.Errorf("read %q as a qualified reference", reference.String())
					}
					continue
				}
				if test.name == "a heading with a digit" {
					continue
				}
				t.Errorf("read %q as a reference", reference.String())
			}
		})
	}
}

// TestPositionsAreAbsoluteAndExclusive covers the offsets substitution depends on.
func TestPositionsAreAbsoluteAndExclusive(t *testing.T) {
	text := "line one\nline two #7 end\nline three owner/repo#8 end\n"
	found := Extract(text)
	if len(found) != 2 {
		t.Fatalf("found %v, want 2", markers(found))
	}
	for _, reference := range found {
		if text[reference.Start:reference.End] != reference.String() {
			t.Errorf("the span [%d:%d] is %q, want %q",
				reference.Start, reference.End, text[reference.Start:reference.End], reference.String())
		}
	}
}

// TestPunctuationAfterANumberStillYieldsTheReference is a defect the probe found and the corpus missed.
//
// A reference's trailing check rejects a digit run that continues into a **word**, which is what makes
// `#1abc` an identifier rather than a reference. The first version used the *name* alphabet for that
// check, which includes `-` and `.` — so `see #1-2` lost the `#1` entirely, because a dash followed it.
// A dash or a dot ends a number in ordinary prose; a letter or an underscore continues one.
func TestPunctuationAfterANumberStillYieldsTheReference(t *testing.T) {
	for _, test := range []struct {
		text string
		want []string
	}{
		{text: "see #1-2", want: []string{"#1"}},
		{text: "see #1.", want: []string{"#1"}},
		{text: "see #1, and #2", want: []string{"#1", "#2"}},
		{text: "see #1)", want: []string{"#1"}},
		{text: "see #1/x", want: []string{"#1"}},
		{text: "see #1: a note", want: []string{"#1"}},
		// And the two forms that must still be refused, because a word continues the run.
		{text: "see #1abc", want: nil},
		{text: "see #1_2", want: nil},
		{text: "abc#1", want: nil},
	} {
		t.Run(test.text, func(t *testing.T) {
			got := markers(Extract(test.text))
			if len(got) != len(test.want) {
				t.Fatalf("found %v, want %v", got, test.want)
			}
			for index := range test.want {
				if got[index] != test.want[index] {
					t.Errorf("position %d is %q, want %q", index, got[index], test.want[index])
				}
			}
		})
	}
}

// TestTwoReferencesWithNoSeparatorAreBothFound covers the adjacency case.
//
// `#1#2` is two references: the `1` before the second `#` is the **first reference's own last digit**,
// not a word the second is glued to. An earlier version refused the second, because the prefix check
// could not tell a word from the tail of a reference already found.
func TestTwoReferencesWithNoSeparatorAreBothFound(t *testing.T) {
	got := markers(Extract("#1#2"))
	want := []string{"#1", "#2"}
	if len(got) != len(want) {
		t.Fatalf("found %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("position %d is %q, want %q", index, got[index], want[index])
		}
	}
}

// TestIndentedCodeContinuesAcrossLines covers the state the exclusion needs.
//
// Only the **first** line of an indented block follows a blank line; the rest follow each other, so a
// check on the preceding line alone skips one line and scans the rest as prose. The defect it produced
// was a reference found inside a code block, which is visible to a reader as a link appearing in code.
func TestIndentedCodeContinuesAcrossLines(t *testing.T) {
	text := "paragraph\n\n    closes #12\n    and also #13\n    and #14\n\nback to prose #15\n"
	got := markers(Extract(text))
	if len(got) != 1 || got[0] != "#15" {
		t.Errorf("found %v, want only #15 — every indented line is code", got)
	}
}

// TestDuplicatesAreFound covers a text that names one target more than once.
//
// Both occurrences are found, because the substitution has to link both; **the store is what collapses
// them to one recorded relationship**, which is a different question from what the text says.
func TestDuplicatesAreFound(t *testing.T) {
	found := Extract("see #5 and later #5 again, plus #5")
	if len(found) != 3 {
		t.Fatalf("found %v, want three occurrences", markers(found))
	}
	// Their positions differ, which is what lets each be substituted independently.
	if found[0].Start == found[1].Start || found[1].Start == found[2].Start {
		t.Error("two occurrences share a position")
	}
}

// fakeResolver answers from a set of visible targets.
//
// It is the seam the access decision arrives through, so a test can assert **what a reader sees** without
// a database — which is the property §7's rule is about.
type fakeResolver struct {
	// visible maps a written reference to its href.
	visible map[string]string
	// asked records every resolution request, so a test can assert that an invisible target was still
	// asked about rather than skipped.
	asked []string
}

func (f *fakeResolver) Resolve(_ context.Context, _ Actor, owner, repository string, number int64) (Target, bool) {
	written := "#" + Number(number)
	if owner != "" {
		written = owner + "/" + repository + written
	}
	f.asked = append(f.asked, written)
	href, ok := f.visible[written]
	if !ok {
		return Target{}, false
	}
	return Target{Href: href}, true
}

// TestVisibleReferencesBecomeLinks covers the accessible half of §7's rule.
func TestVisibleReferencesBecomeLinks(t *testing.T) {
	text := "closes #1 and alice/other#2"
	resolver := &fakeResolver{visible: map[string]string{
		"#1":            "/alice/this/issues/1",
		"alice/other#2": "/alice/other/issues/2",
	}}

	got := Substitute(context.Background(), resolver, Actor{AccountID: "someone"}, text, Extract(text))
	if !strings.Contains(got, "[#1](/alice/this/issues/1)") {
		t.Errorf("the same-repository reference is not a link: %q", got)
	}
	if !strings.Contains(got, "[alice/other#2](/alice/other/issues/2)") {
		t.Errorf("the cross-repository reference is not a link: %q", got)
	}
	// The surrounding text is untouched.
	if !strings.HasPrefix(got, "closes ") {
		t.Errorf("the text before the reference changed: %q", got)
	}
}

// TestInvisibleReferencesStayPlainText is §7's rule, and the reason resolution is per reader.
//
// "renders as plain text when access is absent" — so the same text produces **different output** for two
// readers, and an invisible reference is left exactly as written rather than removed or replaced by a
// placeholder.
func TestInvisibleReferencesStayPlainText(t *testing.T) {
	text := "closes #1 and alice/secret#2"
	// A resolver that answers only for the first, which is what a reader without access to `alice/secret`
	// produces.
	resolver := &fakeResolver{visible: map[string]string{"#1": "/alice/this/issues/1"}}

	got := Substitute(context.Background(), resolver, Anonymous(), text, Extract(text))
	if !strings.Contains(got, "[#1](/alice/this/issues/1)") {
		t.Errorf("the visible reference is not a link: %q", got)
	}
	if !strings.Contains(got, "alice/secret#2") {
		t.Errorf("the invisible reference's text was lost: %q", got)
	}
	if strings.Contains(got, "](/alice/secret") {
		t.Errorf("an invisible reference was linked: %q", got)
	}
	// It is still asked about: the resolver is what decides, so the extractor must not pre-filter.
	if len(resolver.asked) != 2 {
		t.Errorf("the resolver was asked about %v, want both references", resolver.asked)
	}
}

// TestSubstitutionPreservesEverythingElse covers the byte-for-byte property.
func TestSubstitutionPreservesEverythingElse(t *testing.T) {
	text := "**bold** #1 `code #99` tail\n\n- a list\n- with #2\n\n> quote #3\n"
	resolver := &fakeResolver{visible: map[string]string{
		"#1": "/r/issues/1", "#2": "/r/issues/2", "#3": "/r/issues/3",
	}}

	got := Substitute(context.Background(), resolver, Anonymous(), text, Extract(text))
	// Everything that was not a reference is unchanged, which is the property that lets this run before
	// the Markdown renderer without disturbing it.
	for _, want := range []string{"**bold**", "`code #99`", "- a list", "> quote"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q was lost: %q", want, got)
		}
	}
	// The code span still holds its own un-linked text.
	if !strings.Contains(got, "`code #99`") {
		t.Error("the code span's contents changed")
	}
}

// TestSubstitutionHandlesAdjacentReferences covers references that touch.
//
// Running the substitution back to front is what makes this work: a forward loop would shift the second
// reference's offsets the moment the first grew.
func TestSubstitutionHandlesAdjacentReferences(t *testing.T) {
	text := "#1#2 #3"
	resolver := &fakeResolver{visible: map[string]string{
		"#1": "/r/issues/1", "#2": "/r/issues/2", "#3": "/r/issues/3",
	}}
	// `#1#2` is two references with nothing between them.
	got := Substitute(context.Background(), resolver, Anonymous(), text, Extract(text))
	if !strings.Contains(got, "[#1](/r/issues/1)") || !strings.Contains(got, "[#2](/r/issues/2)") {
		t.Errorf("adjacent references were not both substituted: %q", got)
	}
	if !strings.Contains(got, "[#3](/r/issues/3)") {
		t.Errorf("the third reference was lost: %q", got)
	}
}

// TestSubstitutionWithNoReferencesIsIdentity keeps the common case free.
func TestSubstitutionWithNoReferencesIsIdentity(t *testing.T) {
	text := "# A document\n\nWith no references at all.\n"
	got := Substitute(context.Background(), &fakeResolver{}, Anonymous(), text, Extract(text))
	if got != text {
		t.Errorf("a text with no references was changed:\n got %q\nwant %q", got, text)
	}
}

// TestSubstitutionWithNoResolverIsIdentity keeps a nil dependency from being a crash.
func TestSubstitutionWithNoResolverIsIdentity(t *testing.T) {
	text := "closes #1"
	if got := Substitute(context.Background(), nil, Anonymous(), text, Extract(text)); got != text {
		t.Errorf("a nil resolver changed the text: %q", got)
	}
}

// TestResolveReportsEachTarget covers the resolutions a timeline needs.
func TestResolveReportsEachTarget(t *testing.T) {
	text := "closes #1 and alice/other#2"
	resolver := &fakeResolver{visible: map[string]string{"#1": "/alice/this/issues/1"}}

	resolved := Resolve(context.Background(), resolver, Anonymous(), Extract(text))
	if len(resolved) != 2 {
		t.Fatalf("resolved %d references, want 2", len(resolved))
	}
	// The first is visible and carries an href; the second is not, and its title is still what was
	// written — which is what a timeline shows.
	if !resolved[0].Visible() || resolved[0].Href != "/alice/this/issues/1" {
		t.Errorf("the first resolution is %+v", resolved[0])
	}
	if resolved[1].Visible() {
		t.Errorf("the second resolution is visible: %+v", resolved[1])
	}
	if resolved[1].Title != "alice/other#2" {
		t.Errorf("the second resolution's title is %q", resolved[1].Title)
	}
}

// TestTheLinkTitleCannotBreakOutOfItsContext covers the two escape helpers.
//
// A reference's written form comes from a restricted grammar, so these characters cannot appear today —
// and the helpers exist so that a later change to that grammar cannot silently introduce a way out of
// the link. The test feeds them directly, because the guard is about what they do rather than about
// whether they are currently reachable.
func TestTheLinkTitleCannotBreakOutOfItsContext(t *testing.T) {
	for _, test := range []struct {
		name string
		in   string
		want string
	}{
		{name: "a closing bracket", in: "a]b", want: `a\]b`},
		{name: "an opening bracket", in: "a[b", want: `a\[b`},
		{name: "a backslash", in: `a\b`, want: `a\\b`},
		{name: "an ordinary reference", in: "alice/other#12", want: "alice/other#12"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := escapeLinkText(test.in); got != test.want {
				t.Errorf("escapeLinkText(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}

	for _, test := range []struct {
		name string
		in   string
		want string
	}{
		{name: "a closing parenthesis", in: "a)b", want: `a\)b`},
		{name: "an opening parenthesis", in: "a(b", want: `a\(b`},
		{name: "a space", in: "a b", want: "a%20b"},
		{name: "an ordinary href", in: "/alice/repo/issues/1", want: "/alice/repo/issues/1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := escapeDestination(test.in); got != test.want {
				t.Errorf("escapeDestination(%q) = %q, want %q", test.in, got, test.want)
			}
		})
	}
}

// TestReferenceStringRoundTripsWithExtraction asserts the two directions agree.
func TestReferenceStringRoundTripsWithExtraction(t *testing.T) {
	for _, text := range []string{
		"#1", "alice/other#1", "a.b/c-d#999", "before #7 after", "x owner/repo#12 y",
	} {
		found := Extract(text)
		if len(found) == 0 {
			t.Errorf("%q found nothing", text)
			continue
		}
		// Re-extracting a reference's own written form finds the same thing, which is what makes
		// `String` a faithful rendering rather than a display convenience.
		again := Extract(found[0].String())
		if len(again) != 1 || again[0].Number != found[0].Number ||
			again[0].Owner != found[0].Owner || again[0].Repository != found[0].Repository {
			t.Errorf("%q round-tripped as %v", text, markers(again))
		}
	}
}

// TestAnEmptyActorOnlySeesWhatTheResolverAllows keeps this package from making an access decision.
//
// The anonymous actor is passed through unchanged, so **the resolver is the only thing that decides** —
// which is the architectural rule that a reference must not become a second way to read a repository.
func TestAnEmptyActorOnlySeesWhatTheResolverAllows(t *testing.T) {
	resolver := &fakeResolver{}
	// A resolver that answers nothing means nothing is visible, for any actor.
	for _, actor := range []Actor{Anonymous(), {AccountID: "alice"}, {AccountID: "admin", IsAdmin: true}} {
		got := Substitute(context.Background(), resolver, actor, "closes #1", Extract("closes #1"))
		if got != "closes #1" {
			t.Errorf("an actor saw a link the resolver denied: %q", got)
		}
	}
	if len(resolver.asked) != 3 {
		t.Errorf("the resolver was asked %d times, want once per actor", len(resolver.asked))
	}
}
