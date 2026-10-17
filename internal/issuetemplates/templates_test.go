package issuetemplates

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// A repository's declared templates, read as untrusted content.
//
// The corpus below is the point of these tests as much as the happy path is: every template file is
// repository content, so each refusal T-H7 names — a duplicate key, an unknown key, unsupported syntax,
// an oversized file — has a case, and so does the one that is a security control rather than a
// correctness one, a contact link carrying an executable scheme.

// fakeSource is an in-memory repository tree.
type fakeSource struct {
	// files maps a full path to its content.
	files map[string][]byte
	// directories maps a path to its entry names.
	directories map[string][]Entry
	// listErr forces a listing failure, so the "never fails" contract can be asserted.
	listErr error
	// readErr forces a read failure for one path.
	readErr map[string]error
	// listed records the paths that were listed.
	listed []string
}

func newFakeSource() *fakeSource {
	return &fakeSource{files: map[string][]byte{}, directories: map[string][]Entry{}, readErr: map[string]error{}}
}

func (f *fakeSource) ListDirectory(_ context.Context, path string) ([]Entry, error) {
	f.listed = append(f.listed, path)
	if f.listErr != nil {
		return nil, f.listErr
	}
	entries, found := f.directories[path]
	if !found {
		return nil, errors.New("directory not found")
	}
	return entries, nil
}

func (f *fakeSource) ReadFile(_ context.Context, path string) ([]byte, error) {
	if err, forced := f.readErr[path]; forced {
		return nil, err
	}
	content, found := f.files[path]
	if !found {
		return nil, errors.New("not found")
	}
	return content, nil
}

// addFile makes a file appear in the template directory.
func (f *fakeSource) addFile(name string, content string) *fakeSource {
	f.files[Directory+"/"+name] = []byte(content)
	f.directories[Directory] = append(f.directories[Directory], Entry{Name: name})
	return f
}

// addDirectory makes a subdirectory appear in the template directory.
func (f *fakeSource) addDirectory(name string) *fakeSource {
	f.directories[Directory] = append(f.directories[Directory], Entry{Name: name, Directory: true})
	return f
}

// load runs the loader against the fake.
func load(t *testing.T, source *fakeSource) Catalogue {
	t.Helper()
	return Load(context.Background(), source)
}

// names returns the template names, for an assertion that reads as a list.
func names(catalogue Catalogue) []string {
	found := make([]string, 0, len(catalogue.Templates))
	for _, template := range catalogue.Templates {
		found = append(found, template.Name)
	}
	return found
}

// TestAMarkdownTemplateIsRead covers the classic format.
func TestAMarkdownTemplateIsRead(t *testing.T) {
	source := newFakeSource().addFile("bug_report.md", `---
name: Bug report
about: Create a report to help us improve
title: "[Bug]: "
labels: bug, triage
assignees: alice, bob
---

**Describe the bug**
A clear and concise description.
`)

	catalogue := load(t, source)
	if len(catalogue.Templates) != 1 {
		t.Fatalf("the catalogue holds %d templates, want 1; skipped=%+v", len(catalogue.Templates), catalogue.Skipped)
	}
	template := catalogue.Templates[0]
	if template.File != "bug_report.md" || template.Name != "Bug report" {
		t.Errorf("the template is %+v", template)
	}
	if template.Description != "Create a report to help us improve" {
		t.Errorf("the description is %q", template.Description)
	}
	if template.Title != "[Bug]: " {
		t.Errorf("the title is %q, want GitHub's bracketed prefix", template.Title)
	}
	// **The comma-separated spelling is read**, which is what a markdown template's front matter uses.
	if len(template.Labels) != 2 || template.Labels[0] != "bug" || template.Labels[1] != "triage" {
		t.Errorf("the labels are %v, want bug and triage", template.Labels)
	}
	if len(template.Assignees) != 2 || template.Assignees[0] != "alice" {
		t.Errorf("the assignees are %v", template.Assignees)
	}
	if template.Form {
		t.Error("a markdown template is reported as a form")
	}
	// The body is everything after the front matter, and the front matter is not part of it.
	if !strings.Contains(template.Body, "**Describe the bug**") {
		t.Errorf("the body is %q", template.Body)
	}
	if strings.Contains(template.Body, "name: Bug report") {
		t.Error("the front matter leaked into the body")
	}
}

// TestAMarkdownTemplateWithoutFrontMatterIsSkipped covers the required name.
func TestAMarkdownTemplateWithoutFrontMatterIsSkipped(t *testing.T) {
	source := newFakeSource().addFile("notes.md", "Just some markdown with no front matter.\n")

	catalogue := load(t, source)
	if len(catalogue.Templates) != 0 {
		t.Fatalf("a template with no name was offered: %+v", catalogue.Templates)
	}
	// **The refusal is recorded**, so a repository author can be told which file was rejected and why.
	if len(catalogue.Skipped) != 1 {
		t.Fatalf("the refusal was not recorded: %+v", catalogue.Skipped)
	}
	if catalogue.Skipped[0].File != "notes.md" || !strings.Contains(catalogue.Skipped[0].Reason, "no name") {
		t.Errorf("the reason is %q", catalogue.Skipped[0].Reason)
	}
}

// TestAPlainFilenameIsNotATemplate keeps a repository's other files out of the picker.
func TestAPlainFilenameIsNotATemplate(t *testing.T) {
	source := newFakeSource().
		addFile("README", "a note to a reader").
		addFile("contributing.txt", "not a template").
		addFile("subdir", "ignored").
		addDirectory("nested")

	catalogue := load(t, source)
	// A file with no extension, a `.txt`, and a subdirectory are all ignored **without** being recorded
	// as refusals: none of them is a template, so there is nothing to tell an author about.
	if len(catalogue.Templates) != 0 {
		t.Errorf("non-templates were offered: %v", names(catalogue))
	}
	if len(catalogue.Skipped) != 0 {
		t.Errorf("non-templates were reported as refusals: %+v", catalogue.Skipped)
	}
}

// TestAYAMLFormIsRead covers the modern format, including its list-shaped metadata.
func TestAYAMLFormIsRead(t *testing.T) {
	source := newFakeSource().addFile("feature.yml", `
name: Feature request
description: Suggest an idea for this project
title: "[Feature]: "
labels:
  - enhancement
  - triage
assignees:
  - alice
body:
  - type: markdown
    attributes:
      value: Thanks for taking the time to fill out this request.
  - type: textarea
    id: problem
    attributes:
      label: Is your feature request related to a problem?
    validations:
      required: true
`)

	catalogue := load(t, source)
	if len(catalogue.Templates) != 1 {
		t.Fatalf("the catalogue holds %d templates, want 1; skipped=%+v", len(catalogue.Templates), catalogue.Skipped)
	}
	template := catalogue.Templates[0]
	if template.Name != "Feature request" || template.Description != "Suggest an idea for this project" {
		t.Errorf("the template is %+v", template)
	}
	// **The sequence spelling is read**, which is what a YAML form uses.
	if len(template.Labels) != 2 || template.Labels[0] != "enhancement" {
		t.Errorf("the labels are %v", template.Labels)
	}
	// **A form is marked as one, and carries no body.** Its typed elements are not flattened into text,
	// which is what stops a reader being handed a template that silently does nothing.
	if !template.Form {
		t.Error("a YAML form is not marked as a form")
	}
	if template.Body != "" {
		t.Errorf("a form carried a body: %q", template.Body)
	}
}

// TestAFormAcceptsACommaSeparatedListToo covers the leniency that both spellings are read.
//
// GitHub documents a sequence for a form and a comma-separated string for a markdown template, but the
// two are not enforced by the format, so a file that uses the other spelling is read rather than
// refused.
func TestAFormAcceptsACommaSeparatedListToo(t *testing.T) {
	source := newFakeSource().addFile("form.yml", "name: A form\nlabels: bug, triage\n")

	catalogue := load(t, source)
	if len(catalogue.Templates) != 1 {
		t.Fatalf("the template was refused: %+v", catalogue.Skipped)
	}
	if len(catalogue.Templates[0].Labels) != 2 {
		t.Errorf("the labels are %v, want both", catalogue.Templates[0].Labels)
	}
}

// TestDuplicateKeysAreRefused is T-H7's first refusal.
func TestDuplicateKeysAreRefused(t *testing.T) {
	source := newFakeSource().addFile("dup.yml", "name: First\nname: Second\n")

	catalogue := load(t, source)
	if len(catalogue.Templates) != 0 {
		t.Fatalf("a template with a duplicate key was offered as %q", catalogue.Templates[0].Name)
	}
	if len(catalogue.Skipped) != 1 || !strings.Contains(catalogue.Skipped[0].Reason, "already defined") {
		t.Errorf("the refusal reason is %+v", catalogue.Skipped)
	}
}

// TestUnknownKeysAreRefused is T-H7's second refusal.
func TestUnknownKeysAreRefused(t *testing.T) {
	for _, content := range []string{
		"name: A template\nnames: a typo\n",
		"name: A template\nassignee: alice\n",
		"name: A template\nbodyy: []\n",
	} {
		source := newFakeSource().addFile("typo.yml", content)
		catalogue := load(t, source)
		if len(catalogue.Templates) != 0 {
			t.Errorf("a template with an unknown key was accepted: %q", content)
		}
		if len(catalogue.Skipped) != 1 || !strings.Contains(catalogue.Skipped[0].Reason, "not found") {
			t.Errorf("the refusal reason for %q is %+v", content, catalogue.Skipped)
		}
	}
}

// TestUnsupportedSyntaxIsRefused is T-H7's third refusal.
func TestUnsupportedSyntaxIsRefused(t *testing.T) {
	for _, test := range []struct {
		name    string
		content string
	}{
		{name: "a list where a mapping belongs", content: "- one\n- two\n"},
		{name: "an unterminated quote", content: "name: \"unterminated\n"},
		{name: "a mapping where a list belongs", content: "name: A\nlabels:\n  bug: true\n"},
		{name: "a tab for indentation", content: "name: A\n\tlabels: bug\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := newFakeSource().addFile("bad.yml", test.content)
			catalogue := load(t, source)
			if len(catalogue.Templates) != 0 {
				t.Errorf("unsupported syntax was accepted: %+v", catalogue.Templates)
			}
			if len(catalogue.Skipped) != 1 {
				t.Errorf("the refusal was not recorded: %+v", catalogue.Skipped)
			}
		})
	}
}

// TestAnOversizedFileIsRefused covers the bound that stops a repository making a page expensive.
func TestAnOversizedFileIsRefused(t *testing.T) {
	source := newFakeSource().addFile("huge.md", "---\nname: Huge\n---\n"+strings.Repeat("x", MaxFileBytes))

	catalogue := load(t, source)
	if len(catalogue.Templates) != 0 {
		t.Error("an oversized template was accepted")
	}
	if len(catalogue.Skipped) != 1 || !strings.Contains(catalogue.Skipped[0].Reason, "larger than") {
		t.Errorf("the refusal reason is %+v", catalogue.Skipped)
	}
}

// TestTheTemplateCountIsCapped covers the second bound.
func TestTheTemplateCountIsCapped(t *testing.T) {
	source := newFakeSource()
	for index := 0; index < MaxTemplates+5; index++ {
		// Names differ, so each is a distinct template rather than a duplicate.
		source.addFile("template-"+strings.Repeat("a", index%5)+"-"+string(rune('a'+index%26))+string(rune('a'+index/26))+".yml",
			"name: Template "+string(rune('A'+index%26))+string(rune('a'+index/26))+"\n")
	}

	catalogue := load(t, source)
	if len(catalogue.Templates) > MaxTemplates {
		t.Errorf("the catalogue holds %d templates, want at most %d", len(catalogue.Templates), MaxTemplates)
	}
	// **The files beyond the cap are recorded**, so the cap is visible rather than looking like files
	// that vanished.
	if len(catalogue.Skipped) == 0 {
		t.Error("the cap was applied without recording it")
	}
}

// TestAnExecutableContactSchemeIsRefused is the security control, not a correctness one.
//
// A contact link reaches an `href`, and every one of these values is a **syntactically valid URL** — so
// contextual escaping cannot help and a scheme allowlist is the only control that works. Without it,
// `config.yml` would be a stored-XSS vector.
func TestAnExecutableContactSchemeIsRefused(t *testing.T) {
	refused := []string{
		"javascript:alert(1)",
		"JavaScript:alert(1)",
		"  javascript:alert(1)  ",
		"JAVASCRIPT:alert(1)",
		"vbscript:msgbox(1)",
		"data:text/html,<script>alert(1)</script>",
		"data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==",
		"file:///etc/passwd",
		"blob:https://example.com/abc",
		"/contact",
		"mailto",
		"",
	}
	for _, raw := range refused {
		t.Run(raw, func(t *testing.T) {
			source := newFakeSource().addFile("config.yml",
				"blank_issues_enabled: false\ncontact_links:\n  - name: Chat\n    url: "+quoteYAML(raw)+"\n    about: Ask a question\n")

			catalogue := load(t, source)
			// The configuration is refused as a whole, and the refusal is recorded.
			if len(catalogue.Skipped) != 1 || catalogue.Skipped[0].File != ConfigFile {
				t.Fatalf("the configuration was accepted: %+v", catalogue)
			}
			// **Blank issues stay enabled**, because a configuration that cannot be understood must not
			// be read as having asked to close the only way a reader can file a report.
			if !catalogue.Config.BlankIssuesEnabled {
				t.Error("a refused configuration disabled blank issues")
			}
			if len(catalogue.Config.ContactLinks) != 0 {
				t.Errorf("a refused configuration produced contact links: %+v", catalogue.Config.ContactLinks)
			}
		})
	}

	// The three allowed schemes are accepted, which is what makes the refusals meaningful.
	for _, allowed := range []string{
		"https://example.com/help",
		"http://example.com/help",
		"mailto:help@example.com",
	} {
		source := newFakeSource().addFile("config.yml",
			"blank_issues_enabled: false\ncontact_links:\n  - name: Help\n    url: "+quoteYAML(allowed)+"\n    about: Ask\n")
		catalogue := load(t, source)
		if len(catalogue.Skipped) != 0 {
			t.Errorf("%q was refused: %+v", allowed, catalogue.Skipped)
			continue
		}
		if len(catalogue.Config.ContactLinks) != 1 || catalogue.Config.ContactLinks[0].URL != allowed {
			t.Errorf("%q produced %+v", allowed, catalogue.Config.ContactLinks)
		}
	}
}

// TestTheConfigurationIsRead covers the two things config.yml decides.
func TestTheConfigurationIsRead(t *testing.T) {
	source := newFakeSource().addFile("config.yml", `
blank_issues_enabled: false
contact_links:
  - name: Community discussions
    url: https://example.com/discussions
    about: Please ask and answer questions here.
  - name: Documentation
    url: https://example.com/docs
    about: Read the guide first.
`)
	catalogue := load(t, source)

	if catalogue.Config.BlankIssuesEnabled {
		t.Error("blank issues are enabled, but the configuration disabled them")
	}
	if len(catalogue.Config.ContactLinks) != 2 {
		t.Fatalf("the configuration declares %d contact links, want 2", len(catalogue.Config.ContactLinks))
	}
	if catalogue.Config.ContactLinks[0].Name != "Community discussions" {
		t.Errorf("the first link is %+v", catalogue.Config.ContactLinks[0])
	}
	if catalogue.Config.ContactLinks[0].About == "" {
		t.Error("the link's about text was dropped")
	}
}

// TestBlankIssuesDefaultToEnabled is the direction that matters.
func TestBlankIssuesDefaultToEnabled(t *testing.T) {
	// No configuration file at all.
	catalogue := load(t, newFakeSource().addFile("bug.yml", "name: Bug\n"))
	if !catalogue.Config.BlankIssuesEnabled {
		t.Error("blank issues are disabled without a configuration saying so")
	}

	// A configuration that names every other key.
	catalogue = load(t, newFakeSource().addFile("config.yml",
		"contact_links:\n  - name: Help\n    url: https://example.com\n    about: Ask\n"))
	if !catalogue.Config.BlankIssuesEnabled {
		t.Error("blank issues are disabled by a configuration that never mentions them")
	}

	// And the explicit value is honoured, both ways.
	catalogue = load(t, newFakeSource().addFile("config.yml", "blank_issues_enabled: true\n"))
	if !catalogue.Config.BlankIssuesEnabled {
		t.Error("an explicit true was not honoured")
	}
}

// TestAMalformedConfigurationKeepsBlankIssuesEnabled covers the safe direction under a bad file.
func TestAMalformedConfigurationKeepsBlankIssuesEnabled(t *testing.T) {
	source := newFakeSource().addFile("config.yml", "blank_issues_enabled: [not, a, bool]\n")

	catalogue := load(t, source)
	if !catalogue.Config.BlankIssuesEnabled {
		t.Error("a malformed configuration disabled blank issues")
	}
	if len(catalogue.Skipped) != 1 {
		t.Errorf("the refusal was not recorded: %+v", catalogue.Skipped)
	}
}

// TestOneBadTemplateDoesNotBreakTheOthers is the "never fails" contract.
//
// A malformed template is a repository author's mistake and must not stop everybody else from opening
// an issue — so the good templates still appear and the bad one is recorded.
func TestOneBadTemplateDoesNotBreakTheOthers(t *testing.T) {
	source := newFakeSource().
		addFile("good-one.yml", "name: Good one\n").
		addFile("broken.yml", "name: Broken\nname: Duplicated\n").
		addFile("good-two.md", "---\nname: Good two\n---\n\nbody\n")

	catalogue := load(t, source)
	if len(catalogue.Templates) != 2 {
		t.Fatalf("the catalogue holds %d templates, want the two good ones: %v", len(catalogue.Templates), names(catalogue))
	}
	if len(catalogue.Skipped) != 1 || catalogue.Skipped[0].File != "broken.yml" {
		t.Errorf("the skipped list is %+v", catalogue.Skipped)
	}
}

// TestAMissingDirectoryIsNotAProblem covers the common case.
func TestAMissingDirectoryIsNotAProblem(t *testing.T) {
	source := newFakeSource() // no directories at all

	catalogue := load(t, source)
	if !catalogue.Empty() {
		t.Error("a repository with no template directory produced templates")
	}
	// **A missing directory is not recorded as a refusal**, because there is nothing an author got wrong.
	if len(catalogue.Skipped) != 0 {
		t.Errorf("a missing directory was recorded as a refusal: %+v", catalogue.Skipped)
	}
	if !catalogue.Config.BlankIssuesEnabled {
		t.Error("blank issues are disabled without a configuration")
	}
}

// TestAReadFailureIsRecordedButDoesNotFail covers the other half of "never fails".
func TestAReadFailureIsRecordedButDoesNotFail(t *testing.T) {
	source := newFakeSource().addFile("present.yml", "name: Present\n")
	source.files[Directory+"/missing.yml"] = []byte("name: Missing\n")
	source.directories[Directory] = append(source.directories[Directory], Entry{Name: "missing.yml"})
	source.readErr[Directory+"/missing.yml"] = errors.New("the object could not be read")

	catalogue := load(t, source)
	if len(catalogue.Templates) != 1 || catalogue.Templates[0].Name != "Present" {
		t.Errorf("the readable template is missing: %v", names(catalogue))
	}
	if len(catalogue.Skipped) != 1 || catalogue.Skipped[0].File != "missing.yml" {
		t.Errorf("the read failure was not recorded: %+v", catalogue.Skipped)
	}
}

// TestAListingFailureIsRecordedButDoesNotFail covers a tree that cannot be read at all.
func TestAListingFailureIsRecordedButDoesNotFail(t *testing.T) {
	source := newFakeSource()
	source.listErr = errors.New("the tree could not be read")

	catalogue := load(t, source)
	if !catalogue.Empty() {
		t.Error("a failed listing produced templates")
	}
	// A failure that is not "missing" **is** recorded, because it is something an operator would want
	// to know about, unlike a repository that simply has no templates.
	if len(catalogue.Skipped) != 1 {
		t.Errorf("the listing failure was not recorded: %+v", catalogue.Skipped)
	}
}

// TestThePickerOrderIsDeterministic covers the ordering.
func TestThePickerOrderIsDeterministic(t *testing.T) {
	// Added out of order, and with two templates sharing a name so the tie-break is exercised.
	source := newFakeSource().
		addFile("z.yml", "name: Zebra\n").
		addFile("a.yml", "name: apple\n").
		addFile("m.yml", "name: Mango\n").
		addFile("tie-b.yml", "name: Mango\n")

	catalogue := load(t, source)
	got := names(catalogue)
	// **The sort is by UTF-8 bytes, not case-insensitively**, because that is what `sort.Strings` does
	// and what every other ordered list in this project does. So an uppercase name sorts before a
	// lowercase one, and the expectation says so rather than the code being changed to match a
	// preference: a case-insensitive picker order would be a second collation rule to keep in step with
	// the label and milestone folds, which are deliberately ASCII-only for a different purpose.
	want := []string{"Mango", "Mango", "Zebra", "apple"}
	if len(got) != len(want) {
		t.Fatalf("the order is %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("position %d is %q, want %q", index, got[index], want[index])
		}
	}
	// The tie is broken by file name, so the order is total rather than whatever the tree returned.
	if catalogue.Templates[0].File != "m.yml" || catalogue.Templates[1].File != "tie-b.yml" {
		t.Errorf("the tie-break is %q then %q", catalogue.Templates[0].File, catalogue.Templates[1].File)
	}
}

// TestTemplateNamedRefusesAnythingNotDiscovered is the path-traversal defence.
//
// **A request's `?template=` value is never used as a path.** It is matched against the names this
// package discovered, so no escaping, no relative segment, and no absolute path can reach a file
// outside the template directory — which is the reason the lookup exists rather than a caller reading
// the file the parameter names.
func TestTemplateNamedRefusesAnythingNotDiscovered(t *testing.T) {
	source := newFakeSource().addFile("bug_report.md", "---\nname: Bug report\n---\n\nbody\n")
	catalogue := load(t, source)

	// The discovered one resolves.
	if found, ok := catalogue.TemplateNamed("bug_report.md"); !ok || found.Name != "Bug report" {
		t.Errorf("the discovered template did not resolve: %+v %v", found, ok)
	}

	for _, hostile := range []string{
		"../../../etc/passwd",
		"../../.git/config",
		"/etc/passwd",
		"..",
		".",
		"./bug_report.md",
		"bug_report.md/../config.yml",
		"config.yml",
		"",
		"  ",
		"BUG_REPORT.MD",
		"bug_report.md\x00.yml",
	} {
		t.Run(hostile, func(t *testing.T) {
			if _, ok := catalogue.TemplateNamed(hostile); ok {
				t.Errorf("%q resolved to a template", hostile)
			}
		})
	}
}

// TestTheDirectoryListedIsTheDocumentedOne keeps the path the package reads fixed.
func TestTheDirectoryListedIsTheDocumentedOne(t *testing.T) {
	source := newFakeSource()
	load(t, source)

	if len(source.listed) != 1 || source.listed[0] != Directory {
		t.Errorf("the loader listed %v, want the documented directory %q", source.listed, Directory)
	}
	if Directory != ".github/ISSUE_TEMPLATE" {
		t.Errorf("the directory is %q, which is not GitHub's", Directory)
	}
}

// TestTheBodyKeepsMarkdownIntact covers what a template's body is for.
func TestTheBodyKeepsMarkdownIntact(t *testing.T) {
	body := "# A heading\n\n- a list item\n\n```go\nvar x = 1\n```\n\n<not-a-tag>\n"
	source := newFakeSource().addFile("t.md", "---\nname: T\n---\n"+body)

	catalogue := load(t, source)
	if len(catalogue.Templates) != 1 {
		t.Fatalf("the template was refused: %+v", catalogue.Skipped)
	}
	// **The body is passed through untouched**, including raw HTML: escaping and sanitizing is W2.1's
	// renderer's job, and doing it here would mean a template's body was escaped twice.
	if catalogue.Templates[0].Body != body {
		t.Errorf("the body was altered:\n got %q\nwant %q", catalogue.Templates[0].Body, body)
	}
}

// TestACRLFDocumentIsRead covers a repository written on Windows.
func TestACRLFDocumentIsRead(t *testing.T) {
	source := newFakeSource().addFile("windows.md", "---\r\nname: Windows\r\nabout: A CRLF file\r\n---\r\n\r\nThe body.\r\n")

	catalogue := load(t, source)
	if len(catalogue.Templates) != 1 {
		t.Fatalf("a CRLF document was refused: %+v", catalogue.Skipped)
	}
	if catalogue.Templates[0].Name != "Windows" {
		t.Errorf("the name is %q, want it to survive the carriage returns", catalogue.Templates[0].Name)
	}
	if !strings.Contains(catalogue.Templates[0].Body, "The body.") {
		t.Errorf("the body is %q", catalogue.Templates[0].Body)
	}
}

// TestAFieldOverTheBoundIsRefused covers the two display bounds.
func TestAFieldOverTheBoundIsRefused(t *testing.T) {
	long := strings.Repeat("n", MaxNameLength+1)
	source := newFakeSource().addFile("long.yml", "name: "+long+"\n")

	catalogue := load(t, source)
	if len(catalogue.Templates) != 0 {
		t.Error("a template with an over-long name was accepted")
	}
	if len(catalogue.Skipped) != 1 || !strings.Contains(catalogue.Skipped[0].Reason, "longer than") {
		t.Errorf("the refusal reason is %+v", catalogue.Skipped)
	}
}

// TestAnOversizedMetadataListIsRefused covers the metadata caps.
//
// **The payload is built one over the limit deliberately, and the first version of this test was not.**
// It sliced `labels[1:]`, which dropped one element and produced exactly `MaxLabels` — so it demanded a
// refusal for a file that was inside the bound, and a working cap looked broken. A boundary test has to
// say which side of the boundary it is on, which is why the count is asserted before it is used.
func TestAnOversizedMetadataListIsRefused(t *testing.T) {
	over := make([]string, 0, MaxLabels+1)
	for index := 0; index <= MaxLabels; index++ {
		over = append(over, "label-"+string(rune('a'+index)))
	}
	if len(over) != MaxLabels+1 {
		t.Fatalf("the payload holds %d labels, want %d", len(over), MaxLabels+1)
	}
	source := newFakeSource().addFile("many.yml", "name: Many\nlabels:\n  - "+strings.Join(over, "\n  - ")+"\n")

	catalogue := load(t, source)
	if len(catalogue.Templates) != 0 {
		t.Errorf("a template suggesting %d labels was accepted, and the cap is %d", len(over), MaxLabels)
	}
	if len(catalogue.Skipped) != 1 || !strings.Contains(catalogue.Skipped[0].Reason, "labels") {
		t.Errorf("the refusal reason is %+v", catalogue.Skipped)
	}

	// **And the limit itself is accepted**, which is what makes the refusal above a boundary rather than
	// a rule that refuses anything large.
	atLimit := make([]string, 0, MaxLabels)
	for index := 0; index < MaxLabels; index++ {
		atLimit = append(atLimit, "label-"+string(rune('a'+index)))
	}
	source = newFakeSource().addFile("at-limit.yml", "name: At limit\nlabels:\n  - "+strings.Join(atLimit, "\n  - ")+"\n")
	catalogue = load(t, source)
	if len(catalogue.Templates) != 1 {
		t.Errorf("a template at the %d-label limit was refused: %+v", MaxLabels, catalogue.Skipped)
	} else if len(catalogue.Templates[0].Labels) != MaxLabels {
		t.Errorf("the accepted template has %d labels, want %d", len(catalogue.Templates[0].Labels), MaxLabels)
	}
}

// quoteYAML renders a value as a YAML double-quoted scalar, so a corpus case can carry any character.
func quoteYAML(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\t", `\t`)
	return `"` + replacer.Replace(value) + `"`
}
