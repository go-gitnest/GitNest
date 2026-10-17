// Package milestones owns a repository's milestones, an issue's milestone, and the progress counts a
// milestone list shows.
//
// It follows the shape `internal/issues`, `internal/comments`, and `internal/labels` established: a
// narrow model, one shared authorization decision per operation, and a record written in the same
// transaction as the change it describes.
//
// # The permission level, derived rather than chosen
//
// The catalogue's actor column is descriptive prose rather than the actor-kind enum, so the level had to
// be worked out — and **it is not the same as a label's**, which is why W2.4a's derivation was recorded
// as not transferring. Comparing the rows gives the answer:
//
//	| `repo.renamed`, `repo.visibility_changed`, `repo.archived`, `repo.default_branch_changed` | admin |
//	| `label.created` / `label.deleted` | admin |
//	| `milestone.closed` / `milestone.deleted` | user |
//	| `issue.created` / `issue.closed` / `issue.reopened` / `issue.deleted` | user |
//
// Every `admin` row is a **repository-configuration** change, and W1.10b gates each of them with
// `LevelAdmin`. The `user` rows are **content** — issues, comments, and milestones — so a milestone sits
// with the content group rather than the configuration group, and the level for changing a
// repository-scoped object in that group is **write**. That is the same level a label *attachment* needs
// and the same level an issue's deletion needs, and there is no author allowance at either, so none is
// invented here.
//
// **So: creating, editing, closing, reopening, and deleting a milestone, and attaching or detaching one,
// all require repository write.** Managing a repository's *labels* requires admin; managing its
// *milestones* requires write. The asymmetry is the catalogue's, and a test asserts both so neither can
// drift into the other.
//
// # The record shape
//
// `docs/reference/events.md` §4.12 defines exactly two milestone actions:
//
//	| `milestone.closed` / `milestone.deleted` | A milestone is closed or deleted | user | milestone | 2 |
//
// **There is no `milestone.created`, no `milestone.edited`, and no `milestone.opened`**, so creating,
// editing, and reopening write no ledger record. (The webhook payload catalogue at §5.2 lists a wider
// action set — `created`, `closed`, `opened`, `edited`, `deleted` — but that is a *delivery* contract that
// arrives in phase 4, not the ledger's, and the two are different lists.) Attaching an issue to a
// milestone writes nothing either, for the same reason a label attachment does not.
package milestones

import (
	"errors"
	"strings"
	"time"

	"github.com/go-gitnest/GitNest/internal/identifier"
)

// Title and description bounds, from `urls-and-identifiers.md` §6.5.
const (
	// MinTitleLength and MaxTitleLength bound a milestone's title.
	MinTitleLength = 1
	MaxTitleLength = 255

	// MaxDescriptionLength bounds a milestone's description.
	//
	// **No length is fixed for it in the documents.** `data-model.md` requires that text reaching the UI
	// be bounded, but §6.5 bounds the title only, so the value is chosen rather than derived: 65,536
	// characters, matching an issue body, because a milestone description is a comparable piece of prose
	// and a tighter bound would be arbitrary. This is the same reasoning, and the same number, that
	// `internal/comments` records for a comment.
	MaxDescriptionLength = 65536
)

// State is a milestone's lifecycle state.
//
// It is a string type with a named CHECK, per the data-model rule that an enumeration is text rather
// than a PostgreSQL enum type, so a new value is a migration rather than a type rewrite.
type State string

const (
	// StateOpen is a milestone that is accepting issues.
	StateOpen State = "open"
	// StateClosed is a milestone that is not. A closed milestone **rejects new attachments**, which is
	// W2.4's acceptance criterion, and it can be reopened.
	StateClosed State = "closed"
)

// Errors this package reports.
var (
	// ErrNotFound reports a milestone that does not exist in the repository, or one the caller may not
	// reach because the repository itself is invisible to them. The two are the same error: whether the
	// milestone is absent or merely unreadable is not disclosed.
	ErrNotFound = errors.New("the milestone does not exist")

	// ErrForbidden reports a caller who may read the repository but not change its milestones. It is
	// distinct from ErrNotFound because the caller can already see the repository, so refusing plainly
	// discloses nothing they did not know.
	ErrForbidden = errors.New("you may not change this repository's milestones")

	// ErrTitleTaken reports a title that differs from an existing milestone's only by ASCII case.
	ErrTitleTaken = errors.New("a milestone with that title already exists")

	// ErrInvalidTitle reports a title outside the documented grammar.
	ErrInvalidTitle = errors.New("a milestone title must be between 1 and 255 characters, without a leading or trailing space")

	// ErrInvalidDescription reports a description over the bound.
	ErrInvalidDescription = errors.New("a milestone description is too long")

	// ErrNoChange reports an update that would change nothing.
	ErrNoChange = errors.New("the update changes nothing")

	// ErrAlreadyInState reports a close of a closed milestone or a reopen of an open one.
	ErrAlreadyInState = errors.New("the milestone is already in that state")

	// ErrClosed reports an attempt to attach an issue to a closed milestone, which W2.4's acceptance
	// requires be rejected.
	ErrClosed = errors.New("a closed milestone cannot accept new issues")

	// ErrNotAttached reports a detachment of an issue that is not on the milestone.
	ErrNotAttached = errors.New("the issue is not on that milestone")

	// ErrInvalidDueDate reports a due date that is not a calendar date.
	ErrInvalidDueDate = errors.New("a due date must be a calendar date, as YYYY-MM-DD")

	// ErrInvalidState reports a state filter that is not a known state.
	ErrInvalidState = errors.New("the state filter must be open, closed, or all")
)

// Milestone is one milestone.
type Milestone struct {
	ID           string
	RepositoryID string
	// Number is the repository-scoped number, its own sequence, never reused. It is what the route
	// addresses: `/{owner}/{repo}/milestone/{number}`.
	Number int64
	Title  string
	// Description is optional, so an empty string is a legitimate value.
	Description string
	// DueOn is the calendar date at midnight UTC, or nil when the milestone has no due date. It is a date
	// rather than an instant — the payload catalogue's `due_on` is a date — but the column is
	// `timestamptz` because `data-model.md` admits no temporal column outside that type.
	DueOn     *time.Time
	State     State
	ClosedAt  *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsOpen reports whether the milestone accepts issues.
func (m Milestone) IsOpen() bool { return m.State == StateOpen }

// Overdue reports whether a due date has passed, which a list shows so a reader does not have to compare
// dates themselves.
//
// **The comparison is between calendar dates, not instants.** A milestone due on the 14th is not overdue
// at one second past midnight on the 14th — it is overdue on the 15th — so both sides are truncated to the
// start of their UTC day. Comparing the stored midnight against the current instant instead reported every
// milestone overdue for the whole of its due date, which the boundary test caught.
//
// The truncation is to UTC because that is how the value is stored: a due date has no zone of its own,
// which is why `ParseDueDate` refuses a timestamp and why the column holds midnight UTC.
//
// The comparison is against `now`, which is passed in rather than read here, so a test can assert the
// boundary without depending on the wall clock.
func (m Milestone) Overdue(now time.Time) bool {
	if m.DueOn == nil || m.State != StateOpen {
		return false
	}
	// `Truncate` rounds down to a multiple of the duration since the zero time, and midnight UTC is a
	// multiple of a day from it — so this yields the start of the current UTC day.
	today := now.UTC().Truncate(24 * time.Hour)
	return m.DueOn.UTC().Before(today)
}

// Progress is a milestone's issue counts.
//
// **It is derived from a join rather than stored**, which is what makes the acceptance criterion
// "progress counts stay correct after an issue is closed, reopened, or relabeled" true by construction: a
// count that is computed cannot go stale. A stored counter would have to be updated by every operation
// that closes, reopens, attaches, or detaches an issue, and the one that forgot would make the number
// wrong in a way nothing would notice.
type Progress struct {
	// Total is how many issues carry the milestone.
	Total int
	// Closed is how many of those are closed.
	Closed int
}

// Open is how many attached issues are still open.
func (p Progress) Open() int {
	if p.Total < p.Closed {
		return 0
	}
	return p.Total - p.Closed
}

// PercentComplete is the rounded percentage of attached issues that are closed.
//
// A milestone with no attached issues is **0%**, not 100%: nothing has been done, and reporting a full
// bar for an empty milestone would be a claim about work that never happened.
func (p Progress) PercentComplete() int {
	if p.Total <= 0 {
		return 0
	}
	return (p.Closed * 100) / p.Total
}

// Fold returns the comparison form of a title.
//
// It is `identifier.Fold`, which is ASCII-only, because that is what `urls-and-identifiers.md` §6.1 fixes:
// names "are compared after Unicode-free ASCII case folding". `strings.ToLower` would fold `Ü` and `ü`
// together and make two milestones collide that the specification keeps apart.
func Fold(title string) string { return identifier.Fold(title) }

// ValidateTitle checks a title and returns it trimmed.
//
// §6.5 gives a title the same shape as a label's name minus the punctuation allowance: "1–255 characters",
// and the migration's CHECK requires the stored value to be unchanged by trimming. Trimming first is the
// reading that satisfies both, and control characters are refused because they are not printable and
// would make a page's layout depend on text a reader cannot see.
//
// The length is counted in **runes**, because the rule is about characters a reader sees.
func ValidateTitle(title string) (string, error) {
	trimmed := strings.TrimSpace(title)
	length := len([]rune(trimmed))
	if length < MinTitleLength || length > MaxTitleLength {
		return "", ErrInvalidTitle
	}
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return "", ErrInvalidTitle
		}
	}
	return trimmed, nil
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

// dueDateLayout is the form a due date is accepted and rendered in: a calendar date, no time, no zone.
const dueDateLayout = "2006-01-02"

// ParseDueDate reads a due date.
//
// It accepts the empty string as "no due date", which is how a form clears one without a second control.
//
// **It parses a calendar date rather than a timestamp**, because a due date is a date: a milestone due
// "today" is due today wherever the reader is, and interpreting it as an instant in the server's zone
// would move it for everybody else. The value is stored as midnight UTC — which is what GitHub's
// `due_on` returns — because `data-model.md` admits no temporal column outside `timestamptz`.
//
// A timestamp sent where a date is expected is refused rather than truncated, so a client that sends an
// instant is told the field's shape instead of having its value silently changed.
func ParseDueDate(value string) (*time.Time, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil, nil
	}
	parsed, err := time.Parse(dueDateLayout, trimmed)
	if err != nil {
		return nil, ErrInvalidDueDate
	}
	utc := parsed.UTC()
	return &utc, nil
}

// FormatDueDate renders a due date for a reader or a client.
func FormatDueDate(due *time.Time) string {
	if due == nil {
		return ""
	}
	return due.UTC().Format(dueDateLayout)
}

// ParseStateFilter reads a list filter.
//
// It accepts the empty string and `all` as "no filter", because a caller that omits the parameter and one
// that asks for everything mean the same thing.
func ParseStateFilter(value string) (State, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "all":
		return "", nil
	case "open":
		return StateOpen, nil
	case "closed":
		return StateClosed, nil
	default:
		return "", ErrInvalidState
	}
}
