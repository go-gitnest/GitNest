// Package references finds the issue references in a piece of text and resolves them for one reader.
//
// # What a reference is
//
// `urls-and-identifiers.md` §7 fixes the two forms and — more importantly — what each one may render as:
//
//	| Display | `#123` in rendered Markdown resolves within the current repository only;
//	  `owner/repo#123` requires read access to the target at render time and renders as plain text
//	  when access is absent |
//
// So `#123` means "an issue in this repository" and `owner/repo#123` means "an issue somewhere else",
// and the second form's visibility is decided **per reader, at render time**.
//
// # Why that rule makes resolution a per-reader operation
//
// The sentence "renders as plain text when access is absent" is the whole reason this package is not a
// pure function of its input. The same comment must render as a link for a collaborator and as plain
// text for a stranger, so the rendered HTML **cannot be shared between readers that see different
// targets**.
//
// The design that follows from that is in `Substitute`: the *expensive* part — deciding which spans of
// the source are references — is **parsed once per distinct text**, and only the cheap part — one access
// check per reference — runs per reader. The substituted source is then what W2.1's renderer caches, and
// its cache key is the substituted text rather than the original. A document with no references
// therefore still has exactly one cache entry shared by everybody, and a document with references has
// one entry per distinct *set of access outcomes* — a handful, not one per reader.
//
// # What is deliberately not a reference
//
// A reference-looking run of digits inside code is **not** a reference, and getting that wrong is a
// visible defect in two ways: substituting inside a fence produces literal `[#123](/…/123)` text where
// the author wrote `#123`, and — for the closing keywords phase 3 depends on — a keyword quoted in a
// code sample must not act. So the extractor skips fenced blocks, indented code, and inline code spans,
// and `references_test.go` asserts each.
package references

import (
	"strconv"
	"strings"
)

// Reference is one issue reference found in a text.
type Reference struct {
	// Start and End are byte offsets into the source, end-exclusive, so a caller can substitute a span
	// without re-finding it. **They are positions rather than a matched string** because substitution
	// has to preserve everything around the reference byte for byte.
	Start int
	End   int
	// Owner and Repository are the target's `owner/repo`, and are **empty** for a same-repository
	// reference — `#123` — which the resolver interprets against the repository the text lives in.
	Owner      string
	Repository string
	// Number is the target's issue or pull-request number, always at least 1.
	Number int64

	// TargetRepositoryID is the repository the target lives in, which the parser could not know: `#12`
	// names a repository only in the context of the text it was written in. It is filled in by the caller
	// that resolves the reference against its repository, and it is what a record is written against.
	TargetRepositoryID string
	// SelfTarget reports that this reference resolves to the very issue it was written on.
	//
	// **It is decided here rather than by a database constraint, and the migration records why**: a `CHECK`
	// cannot join, and the comparison is between the source's own `(repository, number)` and the target
	// pair. GitHub treats a self-reference as ordinary text, so it is refused rather than recorded.
	SelfTarget bool
}

// SameRepository reports whether the reference is the bare `#123` form.
func (r Reference) SameRepository() bool {
	return r.Owner == "" && r.Repository == ""
}

// String renders the reference as it was written.
//
// It is derived from the parsed parts rather than stored, so a `Reference` cannot disagree with itself
// about what it names.
func (r Reference) String() string {
	if r.SameRepository() {
		return "#" + strconv.FormatInt(r.Number, 10)
	}
	return r.Owner + "/" + r.Repository + "#" + strconv.FormatInt(r.Number, 10)
}

// Marker is the text to search for when a caller wants to locate a reference in the source.
//
// It is the same as `String`, and it exists as a separate name because the two are different facts: one
// is how the reference reads, the other is what a search is anchored on.
func (r Reference) Marker() string { return r.String() }

// maxNumberDigits bounds the digits of a number, so a pathological run of digits is rejected rather than
// parsed into an overflow.
//
// A repository's numbers grow by one per issue, so nineteen digits is beyond any instance's lifetime;
// the bound exists to make a hostile input cheap to refuse rather than to express a real limit.
const maxNumberDigits = 19

// Extract returns the references in a text, in the order they appear.
//
// It is a **pure function of the text** — no access decision, no repository — which is what lets the
// result be reused across readers. See the package comment.
func Extract(text string) []Reference {
	var found []Reference

	// The scanner walks lines so it can track fenced blocks and indented code, which are the two
	// exclusions that span a line boundary.
	offset := 0
	inFence := false
	var fenceChar byte
	fenceLength := 0
	previousBlank := true
	// **Indented code continues across lines, so it needs its own state.** Relying on the
	// preceding-blank rule alone skipped only the *first* line of a block: the second indented line
	// follows a non-blank line, so it was scanned as prose and its references were found inside code —
	// which is exactly the defect the exclusion exists to prevent.
	inIndentedCode := false

	for offset <= len(text) {
		line, next := cutLine(text, offset)
		lineStart := offset
		offset = next

		trimmed := strings.TrimLeft(line, " \t")
		indent := line[:len(line)-len(trimmed)]

		// A fence opens or closes on a line whose first non-space characters are three or more of the
		// same backtick or tilde. The closing run must be at least as long as the opening one and must
		// carry nothing else, which is CommonMark's rule.
		//
		// **The marker's state is assigned to the outer variables, not declared inside the `if`.** An
		// `if marker, length, ok := fenceMarker(...); ok` form shadows both and leaves the caller's copies
		// at their zero values — so the opening character was never remembered, every close was compared
		// against a NUL byte, and **a fence never closed**: everything after the first ``` in a document
		// was treated as code, and no reference following it was ever found.
		marker, length, isFence := fenceMarker(trimmed, inFence, fenceChar, fenceLength)
		if isFence {
			inFence = !inFence
			fenceChar, fenceLength = marker, length
			previousBlank = false
			continue
		}
		if inFence {
			previousBlank = false
			continue
		}

		// Indented code: four spaces or a tab, following a blank line or the document's start, **or
		// continuing an indented block already open**. The preceding-blank condition is what keeps a list
		// item's indented continuation from being mistaken for code — CommonMark's own rule, and the
		// reason a reference inside a nested bullet still resolves.
		if isIndentedCode(indent, previousBlank || inIndentedCode) {
			inIndentedCode = true
			previousBlank = false
			continue
		}
		// A line that is not indented ends any indented block, which is what stops the state from
		// swallowing the rest of the document.
		inIndentedCode = false
		previousBlank = strings.TrimSpace(line) == ""

		// Inline code spans are excluded, and they are found within the line rather than by a regular
		// expression: a span runs from a backtick run to the next run of the same length, so the scan has
		// to count backticks.
		found = append(found, scanLine(line, lineStart, codeSpans(line))...)
	}
	return found
}

// codeSpans returns the byte ranges of a line that are inside inline code, as offsets into the line.
//
// A span opens with a run of N backticks and closes with the next run of exactly N — CommonMark's rule,
// and the reason “ `#1` “ is code while “ “#1“ “ is also code.
func codeSpans(line string) [][2]int {
	var spans [][2]int
	index := 0
	for index < len(line) {
		if line[index] != '`' {
			index++
			continue
		}
		openStart := index
		for index < len(line) && line[index] == '`' {
			index++
		}
		run := index - openStart
		// The closing run must be exactly as long, which is why the scan counts rather than searching
		// for a single backtick.
		closing := -1
		scan := index
		for scan < len(line) {
			if line[scan] != '`' {
				scan++
				continue
			}
			runStart := scan
			for scan < len(line) && line[scan] == '`' {
				scan++
			}
			if scan-runStart == run {
				closing = runStart
				break
			}
		}
		if closing < 0 {
			// An unterminated run is not a code span, so the rest of the line is ordinary text.
			break
		}
		spans = append(spans, [2]int{openStart, scan})
		index = scan
	}
	return spans
}

// scanLine finds references in one line, skipping the given inline-code spans.
//
// `base` is the line's byte offset in the whole text, so a returned reference's positions are absolute.
func scanLine(line string, base int, code [][2]int) []Reference {
	var found []Reference
	// previousEnd is where the last reference found on this line ended, which is what lets `#1#2` be read
	// as two references: the `1` before the second `#` belongs to the first, so it is not a word the
	// second is glued to.
	previousEnd := -1

	for index := 0; index < len(line); index++ {
		if inSpan(code, index) {
			// Skip to the end of the span rather than testing each byte.
			index = spanEnd(code, index) - 1
			continue
		}
		if line[index] != '#' {
			continue
		}
		// A `#` must be followed by at least one digit.
		if !isDigit(line[index+1:]) {
			continue
		}

		// **The character before the `#` is judged after the qualified form is tried, not before.** An
		// earlier version refused any `#` preceded by a word byte, which rejected **every**
		// `owner/repo#123` reference — the byte before the `#` there is the repository name's last
		// letter, so `alice/other#5` looked like `abc#1`. The two cases are told apart by whether a
		// name/repo pair precedes the `#`, so that question is answered first.
		owner, repository, nameStart := readOwnerRepo(line, index, code)

		if owner == "" && index > 0 && index != previousEnd && isWordByte(line[index-1]) {
			// A bare `#` glued to a word is text rather than a reference: `abc#1`. The `index !=
			// previousEnd` case is a second reference written with no separator, where the preceding byte
			// is the previous reference's last digit rather than a word the `#` belongs to.
			continue
		}

		number, digits := readNumber(line[index+1:])
		if digits == 0 {
			continue
		}
		// **A digit run followed by a word character is not a reference.** `#1abc` names nothing, and
		// reading it as `#1` would link an identifier the author wrote — the same class of mistake as
		// linking inside code.
		//
		// **The check is `isWordByte`, not `isNameByte`, and the difference is a real defect.** A name
		// byte includes `-` and `.`, so rejecting those lost the reference in `see #1-2`: the `#1` was
		// refused because a dash followed it. A dash or a dot ends a number in ordinary prose, while a
		// letter or underscore continues an identifier, so only the latter glues.
		if index+1+digits < len(line) && isWordByte(line[index+1+digits]) {
			continue
		}

		start := base + index
		reference := Reference{Start: start, End: start + 1 + digits, Number: number}
		if owner != "" {
			reference.Owner, reference.Repository = owner, repository
			reference.Start = base + nameStart
		}
		found = append(found, reference)
		previousEnd = index + 1 + digits
	}
	return found
}

// readOwnerRepo reads an `owner/repo` ending at the `#`, if the bytes before it form one.
//
// `hashIndex` is the `#`'s position in the line. The returned `start` is where the owner name begins, so
// the caller can extend the reference's span to cover the whole form.
func readOwnerRepo(line string, hashIndex int, code [][2]int) (owner, repository string, start int) {
	end := hashIndex
	if end == 0 {
		return "", "", 0
	}

	// The repository name runs backwards until the `/`.
	slash := -1
	for index := end - 1; index >= 0; index-- {
		if inSpan(code, index) {
			return "", "", 0
		}
		if line[index] == '/' {
			slash = index
			break
		}
		if !isNameByte(line[index]) {
			return "", "", 0
		}
	}
	if slash <= 0 {
		return "", "", 0
	}

	// The owner name runs backwards from the `/` until a byte that cannot be one.
	nameStart := slash
	for index := slash - 1; index >= 0; index-- {
		if inSpan(code, index) {
			return "", "", 0
		}
		if !isNameByte(line[index]) {
			break
		}
		nameStart = index
	}
	if nameStart == slash {
		// A `/` with nothing before it is a path, not an owner.
		return "", "", 0
	}
	// The character before the owner must not be one that would make this part of a longer word or path,
	// so `/a/b/c#1` is not read as owner `b`, repository `c`.
	if nameStart > 0 && (isNameByte(line[nameStart-1]) || line[nameStart-1] == '/') {
		return "", "", 0
	}

	owner = line[nameStart:slash]
	repository = line[slash+1 : end]
	// Both parts must be plausible names, which is what stops a URL like `https://x/y#1` from being read
	// as a reference to owner `x`, repository `y`.
	if !plausibleName(owner) || !plausibleName(repository) {
		return "", "", 0
	}
	return owner, repository, nameStart
}

// plausibleName reports whether a name could be an account or repository name.
//
// The grammar is deliberately looser than `repository.ValidateName`'s, because this decides what to
// *link*: a name that does not match is a name somebody wrote that will simply not resolve, which the
// resolver reports as absent. The check here exists only to reject obviously-not-a-name sequences like a
// scheme's `//`.
func plausibleName(name string) bool {
	if name == "" || len(name) > 100 {
		return false
	}
	for index := 0; index < len(name); index++ {
		character := name[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-' || character == '.' || character == '_':
		default:
			return false
		}
	}
	return true
}

// readNumber reads the run of digits at the start of a string, returning its value and length.
//
// It refuses a run longer than any instance could produce rather than overflowing, and it refuses a
// leading zero beyond a single `0` so that `#007` is read as `7` — which is what a reader means by it,
// and which keeps two spellings of one number from becoming two references.
func readNumber(text string) (int64, int) {
	digits := 0
	for digits < len(text) && text[digits] >= '0' && text[digits] <= '9' {
		digits++
	}
	if digits == 0 {
		return 0, 0
	}
	// **A run longer than the bound is refused rather than truncated.** An earlier version stopped
	// counting at the bound, so `#12345678901234567890` was read as `#1234567890123456789` — inventing a
	// reference to a number the author never wrote, which is worse than refusing the input.
	if digits > maxNumberDigits {
		return 0, 0
	}
	value, err := strconv.ParseInt(text[:digits], 10, 64)
	if err != nil || value < 1 {
		// `#0` is not a number any issue has, and neither is a run that overflowed.
		return 0, 0
	}
	return value, digits
}

// isDigit reports whether a string starts with a digit.
func isDigit(text string) bool {
	return len(text) > 0 && text[0] >= '0' && text[0] <= '9'
}

// isWordByte reports whether a byte can be part of a word, which is what a `#` must not be preceded by.
func isWordByte(character byte) bool {
	switch {
	case character >= 'a' && character <= 'z',
		character >= 'A' && character <= 'Z',
		character >= '0' && character <= '9',
		character == '_':
		return true
	}
	return false
}

// isNameByte reports whether a byte can be part of an `owner` or `repo` name.
func isNameByte(character byte) bool {
	switch {
	case character >= 'a' && character <= 'z',
		character >= 'A' && character <= 'Z',
		character >= '0' && character <= '9',
		character == '-' || character == '.' || character == '_':
		return true
	}
	return false
}

// inSpan reports whether an offset is inside one of the line's code spans.
func inSpan(spans [][2]int, offset int) bool {
	for _, span := range spans {
		if offset >= span[0] && offset < span[1] {
			return true
		}
	}
	return false
}

// spanEnd returns the end of the span containing an offset, or the offset itself.
func spanEnd(spans [][2]int, offset int) int {
	for _, span := range spans {
		if offset >= span[0] && offset < span[1] {
			return span[1]
		}
	}
	return offset
}

// cutLine splits off one line, returning the line without its terminator and the next offset.
//
// Both line endings are handled, because a repository may be written with either and a reference must be
// found whichever it is.
func cutLine(text string, offset int) (line string, next int) {
	if offset >= len(text) {
		return "", offset + 1
	}
	if index := strings.IndexByte(text[offset:], '\n'); index >= 0 {
		end := offset + index
		return strings.TrimSuffix(text[offset:end], "\r"), end + 1
	}
	return strings.TrimSuffix(text[offset:], "\r"), len(text) + 1
}

// fenceMarker reports whether a line opens or closes a fenced code block.
//
// `inFence` says whether one is already open, so a closing marker is recognised as a close rather than a
// new open, and `openChar`/`openLength` are the opening run's, because CommonMark requires the closing
// run to be the same character and at least as long.
func fenceMarker(line string, inFence bool, openChar byte, openLength int) (char byte, length int, isFence bool) {
	if line == "" {
		return openChar, openLength, false
	}
	character := line[0]
	if character != '`' && character != '~' {
		return openChar, openLength, false
	}
	run := 0
	for run < len(line) && line[run] == character {
		run++
	}
	if run < 3 {
		return openChar, openLength, false
	}
	if inFence {
		// A closing fence must be the same character, at least as long, and carry nothing else.
		if character != openChar || run < openLength {
			return openChar, openLength, false
		}
		if strings.TrimSpace(line[run:]) != "" {
			return openChar, openLength, false
		}
		return character, run, true
	}
	// An opening fence may carry an info string, so anything after the run is allowed.
	return character, run, true
}

// isIndentedCode reports whether a line begins an indented code block.
//
// `previousBlank` is whether the line before it was blank — or whether this is the document's first line,
// which counts as blank. Requiring it is CommonMark's rule, and the reason a list item's indented
// continuation is prose rather than code: without it, every reference inside a nested bullet would be
// skipped.
func isIndentedCode(indent string, previousBlank bool) bool {
	if indent == "" {
		return false
	}
	// A tab is one indentation step, so it counts on its own.
	if strings.Contains(indent, "\t") {
		return previousBlank
	}
	return len(indent) >= 4 && previousBlank
}
