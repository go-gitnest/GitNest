// Package issuetemplates reads the issue templates a repository declares under
// `.github/ISSUE_TEMPLATE`, so the issue creation page can offer a picker instead of one blank form.
//
// # The two file formats, and why both are read
//
// GitHub's convention is that a repository may declare either or both:
//
//   - **A markdown template**, `bug_report.md`, whose optional YAML front matter carries `name`,
//     `about`, `title`, `labels`, and `assignees`, and whose body is the text the issue starts with.
//   - **An issue form**, `bug_report.yml`, whose top-level keys carry the same metadata plus
//     `description` and a `body` list of typed form elements.
//
// plus **`config.yml`**, which decides whether a blank issue may still be opened and where a reader is
// sent instead.
//
// # It never fails
//
// `Load` returns a catalogue that is **always safe to render**, and records every problem in
// `Skipped` rather than returning an error. That is deliberate: a malformed template is a repository
// author's mistake, and it must not stop everybody else from opening an issue. A missing directory is
// not even a problem — a repository with no templates is the common case, and it simply gets the blank
// form the page already had.
//
// # Untrusted input
//
// Template files are **repository content**, so they are untrusted. `docs/security/threat-model.md`
// T-H7 fixes the discipline for parsing repository-supplied structured text — "strict YAML decoding
// that rejects duplicate keys, unknown keys, and unsupported syntax" — and this package applies it:
//
//   - **duplicate keys are refused** by `yaml.v3`, which is why the library was chosen over a laxer
//     decoder: a second `name:` silently winning a race with the first is how a template ends up
//     showing something its author never wrote;
//   - **unknown keys are refused**, because a typo'd key is a template that silently does not do what
//     its author asked;
//   - **every file is size-bounded** and the directory's template count is capped, so a repository
//     cannot make issue creation expensive;
//   - **a contact link's URL is checked against the protocol allowlist** T-B1 fixes — `http`, `https`,
//     `mailto` — because it reaches an `href` on the page. Without that check, `config.yml` would be a
//     stored-XSS vector that no amount of contextual escaping would help with, since `javascript:` in
//     an `href` is a perfectly well-formed URL.
//
// Nothing here is trusted to be well-formed, and nothing here is rendered as markup: a template's name
// and description are displayed as text, and its body goes through W2.1's renderer like any other
// user-authored text.
package issuetemplates

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The directory and file names GitHub's convention fixes.
const (
	// Directory is where the templates live, relative to the repository root.
	Directory = ".github/ISSUE_TEMPLATE"
	// ConfigFile is the file that decides whether a blank issue may be opened and where a reader is
	// sent instead.
	ConfigFile = "config.yml"
)

// The bounds.
//
// A file bound and a count cap are what stop a repository making issue creation expensive; the field
// bounds keep a picker readable when a template author writes an essay in a name. **The field bound
// values are chosen rather than derived**: no surviving document fixes them, and they follow the same
// reasoning the label description's bound records.
const (
	// MaxFileBytes bounds one template file.
	MaxFileBytes = 64 << 10
	// MaxTemplates caps how many templates one repository may declare. Beyond it the remaining files
	// are recorded as skipped rather than read.
	MaxTemplates = 50
	// MaxNameLength and MaxDescriptionLength bound the two values a picker shows.
	MaxNameLength        = 200
	MaxDescriptionLength = 500
	// MaxContactLinks caps the alternatives offered when blank issues are disabled.
	MaxContactLinks = 10
	// MaxLabels and MaxAssignees bound the metadata a template may suggest.
	MaxLabels    = 20
	MaxAssignees = 10
)

// Source reads a repository's tree. It is implemented by the read surfaces' content reader, which
// binds a storage location and a revision, so this package never handles either.
type Source interface {
	// ListDirectory returns the entries directly inside a repository path.
	ListDirectory(ctx context.Context, path string) ([]Entry, error)
	// ReadFile returns a file's bytes, or an error when it does not exist.
	ReadFile(ctx context.Context, path string) ([]byte, error)
}

// Entry is one directory entry.
type Entry struct {
	Name string
	// Directory distinguishes a subdirectory from a file. A subdirectory inside the template directory
	// is not a template, and GitHub ignores it.
	Directory bool
}

// Config is `.github/ISSUE_TEMPLATE/config.yml`.
type Config struct {
	// BlankIssuesEnabled reports whether a reader may still open an issue with no template. **It
	// defaults to true**, which is GitHub's behaviour and the safe reading: a repository that says
	// nothing about it has not asked to close the blank form.
	BlankIssuesEnabled bool
	// ContactLinks are the destinations offered instead, when blank issues are disabled.
	ContactLinks []ContactLink
}

// ContactLink is one entry of `contact_links`.
type ContactLink struct {
	Name  string
	URL   string
	About string
}

// Template is one template a picker can offer.
type Template struct {
	// File is the file's name inside the template directory, which is what a `?template=` parameter
	// names. It is always one of the names this package discovered, never a value from a request.
	File string
	// Name is what the picker shows. It is required: a template without one is skipped, because a
	// picker entry with no label is not a choice a reader can make.
	Name string
	// Description is the template's `about` (for a markdown template) or `description` (for a form).
	Description string
	// Title is the default issue title, which may be empty.
	Title string
	// Labels and Assignees are the metadata the template suggests. They are **suggestions carried into
	// the form**, not applied by this package: whether the caller may use a label or assign an account
	// is the label and assignment services' decision, and this package deliberately does not make it.
	Labels    []string
	Assignees []string
	// Body is the text a markdown template starts an issue with. It is empty for a form, whose typed
	// fields are not assembled here — see `Form`.
	Body string
	// Form reports that this template is a YAML issue form. **Its metadata is offered but its `body`
	// elements are not rendered**, because a form is a typed interface of its own — inputs, textareas,
	// dropdowns, checkboxes, and their validations — rather than a block of text to prefill. Recording
	// the distinction rather than flattening a form into an empty body is what stops a reader being
	// handed a template that silently does nothing.
	Form bool
}

// Skipped is a file this package refused, and why.
//
// It exists so a refusal is **visible** rather than silent: a repository author whose template does not
// appear can be told which of their files was rejected and on what grounds, which is not something an
// error message on the issue page could convey.
type Skipped struct {
	File   string
	Reason string
}

// Catalogue is what a repository declares.
type Catalogue struct {
	Templates []Template
	Config    Config
	Skipped   []Skipped
}

// Empty reports whether the repository declares nothing a picker could show.
//
// A catalogue with no templates and blank issues enabled is the common case, and it means the creation
// page keeps the single blank form it had before this package existed.
func (c Catalogue) Empty() bool { return len(c.Templates) == 0 }

// TemplateNamed returns the template a `?template=` value names.
//
// **The value from a request is never used as a path.** It is matched against the names this package
// discovered, so a request cannot name a file outside the template directory however it escapes it —
// which is the reason the lookup exists rather than a caller reading the file directly.
func (c Catalogue) TemplateNamed(file string) (Template, bool) {
	wanted := strings.TrimSpace(file)
	for _, template := range c.Templates {
		if template.File == wanted {
			return template, true
		}
	}
	return Template{}, false
}

// Load reads a repository's catalogue.
//
// **It never fails.** Every problem is recorded in `Skipped`, and the returned catalogue is always safe
// to render — see the package comment for why a single bad template must not break issue creation.
func Load(ctx context.Context, source Source) Catalogue {
	catalogue := Catalogue{Config: Config{BlankIssuesEnabled: true}}

	entries, err := source.ListDirectory(ctx, Directory)
	if err != nil {
		// A missing directory is the common case and not a problem worth recording.
		if !isMissing(err) {
			catalogue.Skipped = append(catalogue.Skipped, Skipped{
				File: Directory, Reason: "the template directory could not be read: " + err.Error(),
			})
		}
		return catalogue
	}

	// Names are sorted so the picker's order is deterministic rather than the tree's order, which
	// would make a page's contents depend on how the repository was written.
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Directory {
			continue
		}
		names = append(names, entry.Name)
	}
	sort.Strings(names)

	for _, name := range names {
		if name == ConfigFile {
			// The configuration is read after the templates, so a problem with it cannot cost a
			// template its place in the picker.
			continue
		}
		extension := strings.ToLower(name[strings.LastIndex(name, ".")+1:])
		if !strings.Contains(name, ".") || (extension != "md" && extension != "yml" && extension != "yaml") {
			// A file that is neither format is not a template GitHub would show either, and silently
			// ignoring it is correct: a README in the directory is a normal thing to find.
			continue
		}
		if len(catalogue.Templates) >= MaxTemplates {
			catalogue.Skipped = append(catalogue.Skipped, Skipped{
				File:   name,
				Reason: fmt.Sprintf("this repository declares more than %d templates", MaxTemplates),
			})
			continue
		}

		content, readErr := readBounded(ctx, source, Directory+"/"+name)
		if readErr != nil {
			catalogue.Skipped = append(catalogue.Skipped, Skipped{File: name, Reason: readErr.Error()})
			continue
		}

		template, parseErr := parseTemplate(name, content)
		if parseErr != nil {
			catalogue.Skipped = append(catalogue.Skipped, Skipped{File: name, Reason: parseErr.Error()})
			continue
		}
		catalogue.Templates = append(catalogue.Templates, template)
	}

	config, configErr := loadConfig(ctx, source)
	if configErr != nil {
		catalogue.Skipped = append(catalogue.Skipped, Skipped{File: ConfigFile, Reason: configErr.Error()})
	} else {
		catalogue.Config = config
	}

	// The picker is ordered by the name a reader sees, with the file name breaking a tie so two
	// templates called the same thing still have one defined order.
	sort.SliceStable(catalogue.Templates, func(i, j int) bool {
		if catalogue.Templates[i].Name != catalogue.Templates[j].Name {
			return catalogue.Templates[i].Name < catalogue.Templates[j].Name
		}
		return catalogue.Templates[i].File < catalogue.Templates[j].File
	})
	return catalogue
}

// readBounded reads a file, refusing one over the bound.
//
// The size is checked **before** the content is used rather than after, and the check is on what was
// read rather than on what the tree said, because a tree entry's size is a claim by the repository
// rather than a measurement.
func readBounded(ctx context.Context, source Source, path string) ([]byte, error) {
	content, err := source.ReadFile(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("the file could not be read: %w", err)
	}
	if len(content) > MaxFileBytes {
		return nil, fmt.Errorf("the file is larger than %d bytes", MaxFileBytes)
	}
	return content, nil
}

// markdownFrontMatter is the YAML a markdown template may open with.
//
// The field set is GitHub's, and it is decoded strictly: an unknown key is an author's typo, and a
// template that silently ignores one does not do what its author asked.
type markdownFrontMatter struct {
	Name      string     `yaml:"name"`
	About     string     `yaml:"about"`
	Title     string     `yaml:"title"`
	Labels    stringList `yaml:"labels"`
	Assignees stringList `yaml:"assignees"`
}

// formFile is a YAML issue form.
//
// `Body` is kept as raw nodes because this package does not interpret a form's typed elements — see
// `Template.Form`. Declaring it keeps strict decoding honest: the key is expected and simply not
// interpreted here, rather than unknown and refused.
type formFile struct {
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	Title       string      `yaml:"title"`
	Labels      stringList  `yaml:"labels"`
	Assignees   stringList  `yaml:"assignees"`
	Body        []yaml.Node `yaml:"body"`
}

// configFile is `config.yml`.
type configFile struct {
	BlankIssuesEnabled *bool         `yaml:"blank_issues_enabled"`
	ContactLinks       []contactLink `yaml:"contact_links"`
}

// contactLink is one `contact_links` entry.
type contactLink struct {
	Name  string `yaml:"name"`
	URL   string `yaml:"url"`
	About string `yaml:"about"`
}

// stringList reads either a YAML sequence or a comma-separated string.
//
// **Both spellings are accepted because GitHub's own formats use both**: a markdown template's front
// matter writes `labels: bug, triage` while a form writes a sequence. Accepting one and refusing the
// other would make an author's correct-by-GitHub's-docs file silently invalid here, and the cost of
// accepting both is three lines rather than a rule nobody can remember.
type stringList []string

// UnmarshalYAML implements yaml.Unmarshaler.
func (l *stringList) UnmarshalYAML(node *yaml.Node) error {
	// An alias refers to an anchor declared elsewhere in the document; dereference it before the kind
	// check, so `labels: *somewhere` behaves like the node it names.
	if node.Kind == yaml.AliasNode {
		node = node.Alias
	}
	switch node.Kind {
	case yaml.SequenceNode:
		var items []string
		if err := node.Decode(&items); err != nil {
			return err
		}
		*l = items
		return nil
	case yaml.ScalarNode:
		var scalar string
		if err := node.Decode(&scalar); err != nil {
			return err
		}
		*l = splitComma(scalar)
		return nil
	default:
		// A mapping where a list belongs is refused rather than ignored, on T-H7's "rejects
		// unsupported syntax".
		return errors.New("expected a list or a comma-separated string")
	}
}

// splitComma splits a comma-separated value, trimming and dropping empties.
func splitComma(value string) []string {
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			items = append(items, trimmed)
		}
	}
	return items
}

// parseTemplate reads one template file.
func parseTemplate(name string, content []byte) (Template, error) {
	if strings.EqualFold(extension(name), "md") {
		return parseMarkdownTemplate(name, content)
	}
	return parseFormTemplate(name, content)
}

// parseMarkdownTemplate reads a markdown template, with or without front matter.
func parseMarkdownTemplate(name string, content []byte) (Template, error) {
	front, body, found := splitFrontMatter(string(content))

	template := Template{File: name, Body: body}
	if found {
		var decoded markdownFrontMatter
		if err := decodeStrict([]byte(front), &decoded); err != nil {
			return Template{}, err
		}
		template.Name = strings.TrimSpace(decoded.Name)
		template.Description = strings.TrimSpace(decoded.About)
		template.Title = decoded.Title

		// **The two formats are held to the same rule, and the first version was not.** A form refused an
		// oversized list while a markdown template silently truncated one — so the same mistake was an
		// error in one file and a quiet surprise in the other. Both now refuse, because a template whose
		// metadata does not fit is not the template its author wrote.
		labels, err := boundListErr(decoded.Labels, MaxLabels, "labels")
		if err != nil {
			return Template{}, err
		}
		assignees, err := boundListErr(decoded.Assignees, MaxAssignees, "assignees")
		if err != nil {
			return Template{}, err
		}
		template.Labels, template.Assignees = labels, assignees
	}

	if err := template.validate(); err != nil {
		return Template{}, err
	}
	return template, nil
}

// parseFormTemplate reads a YAML issue form.
func parseFormTemplate(name string, content []byte) (Template, error) {
	var decoded formFile
	if err := decodeStrict(content, &decoded); err != nil {
		return Template{}, err
	}

	template := Template{
		File:        name,
		Name:        strings.TrimSpace(decoded.Name),
		Description: strings.TrimSpace(decoded.Description),
		Title:       decoded.Title,
		Form:        true,
	}
	labels, err := boundListErr(decoded.Labels, MaxLabels, "labels")
	if err != nil {
		return Template{}, err
	}
	assignees, err := boundListErr(decoded.Assignees, MaxAssignees, "assignees")
	if err != nil {
		return Template{}, err
	}
	template.Labels, template.Assignees = labels, assignees

	if err := template.validate(); err != nil {
		return Template{}, err
	}
	return template, nil
}

// validate checks the parts every template must have.
//
// **A name is required**, for both formats: a picker entry with no label is not a choice a reader can
// make, and GitHub requires the key too. The bound is a chosen value, recorded as such.
func (t Template) validate() error {
	if t.Name == "" {
		return errors.New("the template has no name, so a picker could not label it")
	}
	if len([]rune(t.Name)) > MaxNameLength {
		return fmt.Errorf("the template's name is longer than %d characters", MaxNameLength)
	}
	if len([]rune(t.Description)) > MaxDescriptionLength {
		return fmt.Errorf("the template's description is longer than %d characters", MaxDescriptionLength)
	}
	return nil
}

// loadConfig reads `config.yml`.
func loadConfig(ctx context.Context, source Source) (Config, error) {
	config := Config{BlankIssuesEnabled: true}

	content, err := readBounded(ctx, source, Directory+"/"+ConfigFile)
	if err != nil {
		if isMissing(err) {
			// No configuration is the common case: blank issues stay enabled.
			return config, nil
		}
		return Config{}, err
	}

	var decoded configFile
	if err := decodeStrict(content, &decoded); err != nil {
		// **A malformed configuration keeps blank issues enabled rather than disabling them.** The
		// safe direction is the permissive one: a file that cannot be understood must not be read as
		// having asked to close the only way a reader can still file a report.
		return Config{}, err
	}
	if decoded.BlankIssuesEnabled != nil {
		config.BlankIssuesEnabled = *decoded.BlankIssuesEnabled
	}

	if len(decoded.ContactLinks) > MaxContactLinks {
		return Config{}, fmt.Errorf("the configuration declares more than %d contact links", MaxContactLinks)
	}
	for index, link := range decoded.ContactLinks {
		name := strings.TrimSpace(link.Name)
		if name == "" {
			return Config{}, fmt.Errorf("contact link %d has no name", index+1)
		}
		if len([]rune(name)) > MaxNameLength {
			return Config{}, fmt.Errorf("contact link %d's name is too long", index+1)
		}
		if err := validateContactURL(link.URL); err != nil {
			return Config{}, fmt.Errorf("contact link %d: %w", index+1, err)
		}
		config.ContactLinks = append(config.ContactLinks, ContactLink{
			Name: name, URL: strings.TrimSpace(link.URL), About: strings.TrimSpace(link.About),
		})
	}
	return config, nil
}

// allowedContactSchemes is T-B1's protocol allowlist.
//
// `docs/security/threat-model.md` T-B1 names it as "a link-protocol allowlist (`http`, `https`,
// `mailto`)", and a contact link reaches an `href`. The check is here rather than left to the
// template's escaping because **every one of these values is a syntactically valid URL**: contextual
// escaping cannot tell `javascript:alert(1)` from `https://example.com`, so a scheme check is the only
// control that works — and without it `config.yml` would be a stored-XSS vector.
var allowedContactSchemes = map[string]bool{
	"http":   true,
	"https":  true,
	"mailto": true,
}

// validateContactURL checks a contact link against the protocol allowlist.
func validateContactURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return errors.New("the link has no URL")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return errors.New("the link's URL could not be parsed")
	}
	// A URL with no scheme is refused: the field's purpose is to send a reader somewhere else, and a
	// bare path would point at this instance's own routes instead.
	if parsed.Scheme == "" {
		return errors.New("the link's URL needs a scheme")
	}
	if !allowedContactSchemes[strings.ToLower(parsed.Scheme)] {
		return fmt.Errorf("the link's scheme %q is not one of http, https, or mailto", parsed.Scheme)
	}
	return nil
}

// splitFrontMatter returns the YAML front matter and the body of a markdown document.
//
// The delimiter is a `---` line at the very start and the next `---` line, which is GitHub's. A file
// with no front matter has an empty front and the whole content as its body, which is a legitimate
// template — it simply has no name, and is skipped for that reason rather than this one.
func splitFrontMatter(content string) (front string, body string, found bool) {
	const delimiter = "---"

	rest := content
	firstLine, remainder := cutLine(rest)
	if strings.TrimSpace(firstLine) != delimiter {
		return "", content, false
	}
	rest = remainder

	for {
		line, next := cutLine(rest)
		if strings.TrimSpace(line) == delimiter {
			return front, next, true
		}
		if line == "" && next == "" {
			// The document ends without a closing delimiter, so there was no front matter after all.
			return "", content, false
		}
		front += line + "\n"
		rest = next
	}
}

// cutLine splits off the first line, returning the line and the remainder.
//
// It handles both line endings, because a repository may be written with either and the delimiter
// check must not depend on which.
func cutLine(text string) (line string, rest string) {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return strings.TrimSuffix(text[:index], "\r"), text[index+1:]
	}
	return strings.TrimSuffix(text, "\r"), ""
}

// extension returns a file's extension without the dot.
func extension(name string) string {
	index := strings.LastIndex(name, ".")
	if index < 0 {
		return ""
	}
	return name[index+1:]
}

// decodeStrict decodes YAML, refusing duplicate keys and unknown fields.
//
// T-H7's discipline in one function: `yaml.v3` already refuses a duplicate key, and `KnownFields` adds
// the refusal of an unknown one. Both are errors rather than warnings because a template that is not
// understood must not be offered as though it were.
func decodeStrict(content []byte, target any) error {
	decoder := yaml.NewDecoder(strings.NewReader(string(content)))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		// `io.EOF` here means an empty document, which is not a parse failure: it is an empty file, and
		// the caller's own validation reports what is missing from it.
		if errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("the YAML could not be read: %w", err)
	}
	return nil
}

// boundListErr trims a list and caps it.
func boundListErr(items []string, limit int, field string) ([]string, error) {
	trimmed := make([]string, 0, len(items))
	for _, item := range items {
		if value := strings.TrimSpace(item); value != "" {
			trimmed = append(trimmed, value)
		}
	}
	if len(trimmed) > limit {
		return nil, fmt.Errorf("the template suggests more than %d %s", limit, field)
	}
	return trimmed, nil
}

// isMissing reports whether an error means the path is not there.
//
// The content reader reports an absent file or directory with its own error, so the check is on the
// message rather than on a sentinel this package cannot import. It is deliberately narrow: it must not
// swallow a genuine read failure, which the caller wants recorded.
func isMissing(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "not found") ||
		strings.Contains(message, "does not exist") ||
		strings.Contains(message, "no such") ||
		strings.Contains(message, "bad file") ||
		strings.Contains(message, "directory not found")
}
