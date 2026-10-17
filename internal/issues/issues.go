// Package issues owns the issue model, its lifecycle, and the rules for who may
// change it.
//
// It is the first collaboration domain, and it establishes the shape the others
// follow: a narrow model, one shared authorization decision per operation, and an
// audit record written in the same transaction as the change it describes.
//
// # Numbering
//
// An issue's number is per repository, shared with pull requests, and never reused —
// including after deletion. The mechanism and the reasoning are in
// `migrations/0007_issues.up.sql`; the short version is that a sequence cannot satisfy
// "no gaps from failed transactions" and a read-then-write cannot satisfy "two
// concurrent creates never share a number", so the counter is a locked column on the
// repository row allocated with an atomic `UPDATE ... RETURNING` inside the creating
// transaction.
//
// # History
//
// Four lifecycle events are recorded in the audit ledger — `issue.created`,
// `issue.closed`, `issue.reopened`, `issue.deleted` — and that ledger is the
// state-change history W2.2's acceptance requires. Nothing in this package keeps a
// second history, because two records of one change drift.
//
// # Authorization
//
// Every access question goes to `repository.Service`, the same shared decision the API,
// the browser, and the Git transports use. This package never inspects a level itself
// beyond choosing which level to require:
//
//   - reading an issue requires repository read
//   - opening one requires read **and** an authenticated account, because an issue needs
//     an author
//   - editing, closing, and reopening require repository write **or** being the author,
//     which is the "issue author rules" of W2.2's acceptance
//   - deleting requires repository write
//
// A refusal is a `403` rather than a not-found for a caller who may already read the
// issue, on the same reasoning W1.10b recorded for an archived repository: existence is
// not a secret from somebody who can see the thing.
package issues

import (
	"errors"
	"strings"
	"time"
)

// State is an issue's lifecycle state.
//
// It is a string type rather than a PostgreSQL enum, per the data-model rule that an
// enumeration is "text with a named CHECK constraint, never a PostgreSQL enum type, so a
// new value is a migration rather than a type rewrite".
type State string

const (
	// StateOpen is an issue that is receiving attention.
	StateOpen State = "open"
	// StateClosed is an issue that is not. A closed issue can be reopened, and every
	// transition is recorded.
	StateClosed State = "closed"
)

// String renders the state for a message.
func (s State) String() string { return string(s) }

// Content bounds, mirroring the CHECK constraints in the migration.
//
// They are re-stated in Go so a caller gets a named field rather than a driver error
// naming a constraint, and the two must agree: a test asserts the boundary cases are
// refused by both.
const (
	// MaxTitleLength bounds a title. A title reaches a list, a notification subject, and
	// an email header, so an unbounded one is a denial-of-service surface as much as a
	// layout problem.
	MaxTitleLength = 256
	// MaxBodyLength bounds a body. It is generous for a long report and small enough that
	// one row cannot dominate a page.
	MaxBodyLength = 65536

	// MaxBodyBytes is the largest a body can be **in bytes**, which is what a request reader needs
	// to bound itself with.
	//
	// The character limit above is what the domain enforces; a byte bound has to be looser, because
	// one character can occupy four bytes in UTF-8 and a bound that confused the two would refuse a
	// legitimate body of non-ASCII text. Four times the character limit is therefore the exact worst
	// case rather than an estimate.
	MaxBodyBytes = MaxBodyLength * 4

	// DefaultPageSize is the page a listing returns when a caller names no limit. It matches
	// the API's documented default so the two cannot disagree.
	DefaultPageSize = 30
	// MaxPageSize is the largest page a caller may ask for. A larger request is clamped
	// rather than refused, so a client asking for a thousand rows gets a full page instead of
	// an error — the same behaviour the repository list already has.
	MaxPageSize = 100
)

// Errors this package reports.
var (
	// ErrNotFound reports an issue that does not exist in the repository, or one the
	// caller may not reach because the repository itself is invisible to them. The two
	// are deliberately the same error: whether the issue is absent or merely unreadable
	// is not disclosed.
	ErrNotFound = errors.New("the issue does not exist")

	// ErrForbidden reports a caller who may read the issue but not change it. It is
	// distinct from ErrNotFound because the caller can already see the issue, so
	// refusing plainly discloses nothing they did not know.
	ErrForbidden = errors.New("you may not change this issue")

	// ErrConflict reports an edit against a version that is no longer current. The
	// data-model concurrency rule requires "optimistic concurrency with a version column
	// or `updated_at` comparison; a conflict is a visible error, not a silent overwrite".
	ErrConflict = errors.New("the issue changed since you read it")

	// ErrInvalidTitle reports a title that is absent or out of bounds.
	ErrInvalidTitle = errors.New("the title must be between 1 and 256 characters")

	// ErrInvalidBody reports a body over the bound.
	ErrInvalidBody = errors.New("the body must be at most 65536 characters")

	// ErrNoChange reports an edit that would change nothing, so no record is written for
	// a change that did not happen.
	ErrNoChange = errors.New("the edit changes nothing")

	// ErrAlreadyInState reports a close of a closed issue or a reopen of an open one.
	ErrAlreadyInState = errors.New("the issue is already in that state")

	// ErrAnonymousAuthor reports an attempt to open an issue with no account. An issue
	// needs an author, so the operation requires a credential where reading does not.
	ErrAnonymousAuthor = errors.New("opening an issue requires a signed-in account")

	// ErrInvalidState reports a state filter that is not a known state.
	ErrInvalidState = errors.New("the state filter must be open, closed, or all")
)

// Issue is one issue.
//
// It carries the author's and the closer's **names** as well as their identifiers. That is a
// deliberate denormalization rather than a view leaking into the model: a list page shows
// thirty issues at once, and resolving thirty author names afterwards is thirty queries, so the
// read projection joins the two names it needs. They are display values — nothing in this
// package decides anything from them, and every authorization question uses the identifiers.
type Issue struct {
	ID           string
	RepositoryID string
	// Number is the repository-scoped number, shared with pull requests and never reused.
	Number int64
	Title  string
	Body   string
	// AuthorID is empty when the author's account has been deleted. The issue survives its
	// author, and the ledger still names who acted.
	AuthorID string
	// AuthorName is the author's current username, empty when there is no author because the
	// account was deleted.
	AuthorName string
	State      State
	// ClosedAt and ClosedBy describe the close that is in force, so a reopened-then-closed
	// issue reports the later close. Every transition is in the ledger.
	ClosedAt *time.Time
	ClosedBy string
	// ClosedByName is the closer's current username, on the same terms as AuthorName.
	ClosedByName string
	// CreatedAt never changes; UpdatedAt is set by the database on every write.
	CreatedAt time.Time
	UpdatedAt time.Time
	// Version is what an edit names to prove it read the current row.
	Version int
}

// IsOpen reports whether the issue is open.
func (i Issue) IsOpen() bool { return i.State == StateOpen }

// AuthoredBy reports whether an account is the issue's author.
//
// An empty account is never the author: an issue whose author was deleted has no author
// to make the author-rules allowance for.
func (i Issue) AuthoredBy(accountID string) bool {
	return accountID != "" && i.AuthorID != "" && i.AuthorID == accountID
}

// ValidateTitle checks a title and returns it trimmed.
//
// Trimming before the length check is deliberate: a title of spaces is not a title, and a
// title whose only fault is trailing whitespace should not be refused.
func ValidateTitle(title string) (string, error) {
	trimmed := strings.TrimSpace(title)
	if trimmed == "" || len([]rune(trimmed)) > MaxTitleLength {
		return "", ErrInvalidTitle
	}
	return trimmed, nil
}

// ValidateBody checks a body and returns it.
//
// A body may be empty: an issue whose whole content is its title is a legitimate report.
// It is not trimmed, because whitespace inside a body is content — a code block's
// indentation, for one — and removing a trailing newline changes what Markdown renders.
func ValidateBody(body string) (string, error) {
	if len([]rune(body)) > MaxBodyLength {
		return "", ErrInvalidBody
	}
	return body, nil
}

// ParseStateFilter reads a list filter.
//
// It accepts the empty string and `all` as "no filter", because a caller that omits the
// parameter and one that asks for everything mean the same thing, and both are common.
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
