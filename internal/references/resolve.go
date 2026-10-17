package references

import (
	"context"
	"strconv"
	"strings"
)

// Resolution is what one reference resolves to for one reader.
type Resolution struct {
	// Reference is the span that was found.
	Reference Reference
	// Href is the URL to link to, or empty when the reader may not see the target.
	//
	// **Empty is the documented outcome for absent access**, not an error: `urls-and-identifiers.md` §7
	// requires that a reference "renders as plain text when access is absent", so a resolver that
	// returned an error would force every caller to catch it and decide the same thing.
	Href string
	// Title is the link's text, which is the reference as it was written — `#12` or `owner/repo#12`.
	//
	// It is the written form rather than the target's title because resolving a title would mean reading
	// the target's row for every reference on every page, and because a link whose text silently became
	// an issue's subject is harder to scan than the number the author wrote.
	Title string
}

// Visible reports whether the reader may see the target.
func (r Resolution) Visible() bool { return r.Href != "" }

// Target is a resolved target, and whether the reader may see it.
type Target struct {
	// Href is where the reference links, built from the target's own `owner/repo` and number so a
	// same-repository reference links by the resolved name rather than by what was typed.
	Href string
}

// Resolver answers whether a reader may see a referenced target.
//
// It is the single seam this package needs from the rest of the application, and **it is where the
// access decision is made** — this package never inspects a permission itself. The implementation calls
// the same `repository.Service` the API, the browser, and the Git transports call, which is what stops a
// reference from becoming a second way to read a repository.
type Resolver interface {
	// Resolve returns the target and whether the actor may read it. A target that does not exist and one
	// the actor may not see are **both** returned as not-visible, because §7's rule is about what a
	// reader may see rather than about what exists.
	Resolve(ctx context.Context, actor Actor, owner, repository string, number int64) (Target, bool)
}

// Actor is who is reading.
//
// It is defined here rather than taken from the repository package so that a caller with no repository
// access at all — an anonymous reader of a public page — is expressible without this package importing
// the permission model.
type Actor struct {
	// AccountID is empty for an anonymous reader.
	AccountID string
	// IsAdmin reports an instance administrator.
	IsAdmin bool
}

// Anonymous is a reader with no credential.
func Anonymous() Actor { return Actor{} }

// Substitute replaces every visible reference in a text with a Markdown link, and every invisible one
// with no change at all.
//
// # Why substitution rather than rendering
//
// W2.1's renderer turns a document into HTML and caches the result per text. A reference's visibility is
// per **reader**, so putting the link into the HTML would make the cache entry reader-dependent and
// either leak one reader's links to another or defeat the cache entirely.
//
// Substituting into the **source** before rendering keeps W2.1's pipeline intact and its cache keyed on
// the substituted text: the parser and sanitizer still see Markdown, and a document whose references all
// resolve the same way for two readers produces the same substituted text and therefore one cache entry.
// See the package comment for why this is the design rather than a compromise.
//
// # What it deliberately does not do
//
// It does not escape the substituted link's title or URL, because **both are constructed, never copied**:
// the URL is built from a resolved `owner/repo` and an integer, and the title is the reference's own
// written form, which the parser produced from a restricted grammar. A caller that later adds a
// user-controlled value to either must escape it, and this comment is the warning.
func Substitute(ctx context.Context, resolver Resolver, actor Actor, text string, references []Reference) string {
	if resolver == nil || len(references) == 0 {
		return text
	}

	// Substitution walks **forwards with a cursor**, reading from the original text and writing into a
	// fresh builder. That is what makes the offsets safe: a forward loop that edited in place would shift
	// every later reference the moment an earlier one grew, which is the problem the recorded positions
	// exist to avoid — and building a new string sidesteps it entirely.
	//
	// **An earlier version walked backwards and appended, which produced the whole document in reverse.**
	// The offsets were right and the order was wrong, so it was visibly broken rather than subtly — which
	// is the one mercy of getting this wrong.
	var builder strings.Builder
	builder.Grow(len(text))

	cursor := 0
	for _, reference := range references {
		if reference.Start < cursor || reference.Start < 0 || reference.End > len(text) {
			// A reference that overlaps one already written, or that lies outside the text, is skipped
			// rather than allowed to corrupt the output.
			continue
		}

		target, visible := resolver.Resolve(ctx, actor, reference.Owner, reference.Repository, reference.Number)

		// Everything between the previous reference and this one is copied byte for byte.
		builder.WriteString(text[cursor:reference.Start])

		if visible {
			builder.WriteString("[" + escapeLinkText(reference.String()) + "](" +
				escapeDestination(target.Href) + ")")
		} else {
			// **An invisible reference keeps its own text.** §7 requires that it render as plain text,
			// so nothing is removed and no placeholder is inserted.
			builder.WriteString(reference.String())
		}
		cursor = reference.End
	}
	builder.WriteString(text[cursor:])

	return builder.String()
}

// Resolve returns what each reference resolves to for one reader, in the order given.
//
// It exists for the API and for a caller that needs the resolutions rather than the substituted text —
// the timeline, for instance, records which references a text made, and it wants the visible ones.
func Resolve(ctx context.Context, resolver Resolver, actor Actor, references []Reference) []Resolution {
	if resolver == nil {
		return nil
	}
	resolved := make([]Resolution, 0, len(references))
	for _, reference := range references {
		target, visible := resolver.Resolve(ctx, actor, reference.Owner, reference.Repository, reference.Number)
		resolution := Resolution{Reference: reference, Title: reference.String()}
		if visible {
			resolution.Href = target.Href
		}
		resolved = append(resolved, resolution)
	}
	return resolved
}

// escapeLinkText escapes the characters that would end a Markdown link's text.
//
// A reference's written form is produced by `Reference.String` from a restricted grammar, so it contains
// only name characters, `/`, and `#` — none of which can end a link. The function exists so that a
// future change to that grammar cannot silently introduce one, and it is asserted by a test that feeds
// it the characters a link cannot carry.
func escapeLinkText(text string) string {
	if !strings.ContainsAny(text, "[]\\") {
		return text
	}
	replacer := strings.NewReplacer(`\`, `\\`, "[", `\[`, "]", `\]`)
	return replacer.Replace(text)
}

// escapeDestination escapes the characters that would end a Markdown link's destination.
//
// The destination is built from a resolved `owner/repo` and an integer by the resolver, so it is already
// safe; this is the guard that keeps that a property of the code rather than of the resolver's current
// behaviour.
func escapeDestination(destination string) string {
	if !strings.ContainsAny(destination, " ()<>") {
		return destination
	}
	replacer := strings.NewReplacer(
		`\`, `\\`, "(", `\(`, ")", `\)`, " ", `%20`, "<", `%3C`, ">", `%3E`,
	)
	return replacer.Replace(destination)
}

// Number renders a target number, so a caller building a path does not use `strconv` directly and
// disagree with `Reference.String` about the format.
func Number(number int64) string { return strconv.FormatInt(number, 10) }
