// Package labels owns a repository's labels and their attachment to issues.
//
// It is the third collaboration domain, and it follows the shape `internal/issues` and
// `internal/comments` established: a narrow model, one shared authorization decision per operation,
// and a record written in the same transaction as the change it describes.
//
// # Two levels, and how each was derived rather than chosen
//
// The catalogue's "Actor" column is descriptive prose rather than the actor-kind enum, so the level
// each operation needs had to be worked out. Comparing the rows that say `admin` with the rows that
// say `user` gives the answer, and W1.10b's code is the confirmation:
//
//	| `repo.renamed`, `repo.visibility_changed`, `repo.archived`, `repo.default_branch_changed`,
//	  `repo.issues_toggled` | admin |
//	| `label.created` / `label.deleted` | admin |
//	| `repo.created`, `repo.forked` | user |
//
// Every repository-configuration event marked `admin` is gated by `LevelAdmin` in
// `internal/repository`, and the two marked `user` are the ones that *create* a repository — where the
// actor becomes its admin rather than needing to be one. So:
//
//   - **managing a label requires repository admin.** A label set is repository configuration rather
//     than content, which is what puts it with the renamed/visibility/archived group.
//   - **attaching and detaching a label on an issue requires repository write.** That is changing an
//     issue's metadata rather than the repository's configuration, and it is the level that already
//     guards an issue's own fields.
//
// **No author allowance is given here, and its absence is deliberate.** W2.2's acceptance explicitly
// requires "write permission **or issue author rules**" for editing, closing, and reopening an issue;
// W2.4's acceptance says nothing of the kind, and the catalogue's `issues` action list does not
// distinguish who applies a label. So this package does not invent an allowance the documents do not
// describe — a repository writer may label any issue, which is what GitHub's triage-and-above rule
// amounts to in a three-level model.
//
// # The colour grammar
//
// `docs/security/threat-model.md` T-B2 requires that "user-controlled colors and identifiers are
// validated against a grammar, never interpolated raw", and `urls-and-identifiers.md` §6.5 fixes the
// grammar: "Six hexadecimal digits, no `#` in storage, rendered with `#`". `NormalizeColor` is the
// only way a colour enters this package, and it produces **lowercase** digits — so a stored value is
// drawn from a sixteen-character alphabet and cannot carry a quote, a semicolon, a parenthesis, or a
// control character into whatever context the read surfaces place it in.
package labels

import (
	"errors"
	"strings"
	"time"

	"github.com/go-gitnest/GitNest/internal/identifier"
)

// Name and colour bounds and rules, from `urls-and-identifiers.md` §6.5.
const (
	// MinNameLength and MaxNameLength bound a label's displayed name.
	MinNameLength = 1
	MaxNameLength = 50

	// MaxDescriptionLength bounds a label's description.
	//
	// **No length is fixed for a label description in the documents.** `data-model.md` requires that
	// "text length is bounded by a CHECK for values that reach the UI, so a hostile client cannot store
	// a megabyte in a title", but §6.5 bounds the name and the colour only. The value is therefore
	// chosen rather than derived: 100 characters, which is GitHub's limit, so a client migrating from
	// it does not have to shorten a description that already fit.
	MaxDescriptionLength = 100

	// colorLength is how many hexadecimal digits a colour has.
	colorLength = 6
)

// Errors this package reports.
var (
	// ErrNotFound reports a label that does not exist in the repository, or one the caller may not
	// reach because the repository itself is invisible to them. The two are the same error: whether the
	// label is absent or merely unreadable is not disclosed.
	ErrNotFound = errors.New("the label does not exist")

	// ErrForbidden reports a caller who may read the repository but not change its labels. It is
	// distinct from ErrNotFound because the caller can already see the repository, so refusing plainly
	// discloses nothing they did not know.
	ErrForbidden = errors.New("you may not change this repository's labels")

	// ErrNameTaken reports a name that differs from an existing label only by ASCII case. It is the
	// conflict the acceptance names, and it is enforced by a unique constraint on the folded form
	// rather than by a check that could race.
	ErrNameTaken = errors.New("a label with that name already exists")

	// ErrInvalidName reports a name outside the documented grammar.
	ErrInvalidName = errors.New("a label name must be between 1 and 50 characters, without a leading or trailing space")

	// ErrInvalidColor reports a colour outside the documented grammar.
	ErrInvalidColor = errors.New("a label color must be six hexadecimal digits")

	// ErrInvalidDescription reports a description over the bound.
	ErrInvalidDescription = errors.New("a label description must be at most 100 characters")

	// ErrNoChange reports an update that would change nothing, so no write happens for a change that
	// did not occur.
	ErrNoChange = errors.New("the update changes nothing")

	// ErrNotAttached reports a detachment of a label the issue does not carry. It is a not-found rather
	// than a silent success, so a client that lost track of the state is told.
	ErrNotAttached = errors.New("the issue does not carry that label")
)

// Label is one label.
type Label struct {
	ID           string
	RepositoryID string
	// Name is what a reader sees, and keeps the case chosen at creation.
	Name string
	// Color is six lowercase hexadecimal digits with no `#`. The displayed form adds the `#`, which is
	// what §6.5 means by "rendered with `#`".
	Color       string
	Description string
	// CreatedAt and UpdatedAt are the database's, so the ordering of two changes is one clock's answer
	// rather than two applications'. The transport formats them; a domain type carries instants.
	CreatedAt time.Time
	UpdatedAt time.Time
}

// DisplayColor is the colour as a reader sees it.
//
// The `#` is added here rather than stored, so the stored value is always the bare digits the CHECK
// admits — there is no path by which a `#` or anything else reaches the column.
func (l Label) DisplayColor() string { return "#" + l.Color }

// Fold returns the comparison form of a name.
//
// It is `identifier.Fold`, which is ASCII-only, because that is what `urls-and-identifiers.md` §6.1
// fixes: names "are compared after Unicode-free ASCII case folding". Using `strings.ToLower` would be
// wrong — it is Unicode-aware, so it would fold `Ü` and `ü` together and make two labels collide that
// the specification keeps apart, and it would depend on the runtime's Unicode tables rather than on the
// stored column's bytes.
func Fold(name string) string { return identifier.Fold(name) }

// ValidateName checks a label name and returns it trimmed.
//
// §6.5 permits "any printable characters **except a leading or trailing space**". Trimming first is the
// reading that satisfies both halves: a name that is only spaces trims to nothing and is refused as too
// short, and a stored name cannot begin or end with a space — which the migration's CHECK enforces
// independently, so the rule is a property of the table rather than of this one code path.
//
// The length is counted in **runes**, because the rule is about characters a reader sees and the
// permitted set includes non-ASCII ones. Control characters are refused: they are not printable, and a
// name containing one would make a page's layout depend on text a reader cannot see.
func ValidateName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	length := len([]rune(trimmed))
	if length < MinNameLength || length > MaxNameLength {
		return "", ErrInvalidName
	}
	for _, r := range trimmed {
		// The printable range excludes the C0 and C1 control blocks and the delete character. A space
		// is allowed in the interior.
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return "", ErrInvalidName
		}
	}
	return trimmed, nil
}

// NormalizeColor validates a colour and returns its stored form.
//
// A leading `#` is accepted because that is how a colour is written everywhere a person sees one —
// §6.5 says the stored form has no `#` and the rendered form has one — and the returned value is
// lowercase, so the CHECK's lowercase-only grammar is satisfied by construction rather than by the
// input already happening to be lowercase.
//
// It is the single entry point for a colour, which is what makes T-B2's "validated against a grammar,
// never interpolated raw" true: nothing else in the package accepts one.
func NormalizeColor(color string) (string, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(color), "#")
	if len(trimmed) != colorLength {
		return "", ErrInvalidColor
	}
	normalized := make([]byte, colorLength)
	for index := 0; index < colorLength; index++ {
		digit := trimmed[index]
		switch {
		case digit >= '0' && digit <= '9':
			normalized[index] = digit
		case digit >= 'a' && digit <= 'f':
			normalized[index] = digit
		case digit >= 'A' && digit <= 'F':
			// Folded to lowercase, so the stored alphabet is exactly sixteen characters.
			normalized[index] = digit + ('a' - 'A')
		default:
			return "", ErrInvalidColor
		}
	}
	return string(normalized), nil
}

// ValidateDescription bounds a description.
//
// A description may be empty: it is optional metadata rather than the thing itself.
func ValidateDescription(description string) (string, error) {
	if len([]rune(description)) > MaxDescriptionLength {
		return "", ErrInvalidDescription
	}
	if strings.ContainsRune(description, 0) {
		return "", ErrInvalidDescription
	}
	return description, nil
}
