// Package comments owns issue comments, their edit history, and the rules for who may change
// them.
//
// It is the second collaboration domain, and it follows the shape `internal/issues` established:
// a narrow model, one shared authorization decision per operation, and a record written in the
// same transaction as the change it describes.
//
// # Two records, and why an edit writes only one of them
//
// A comment's life produces two kinds of record, and this package keeps them apart deliberately:
//
//   - **An edit appends a revision** — the previous body, attributed and ordered — and writes no
//     audit record. `docs/reference/events.md` §4.12 defines no `comment.edited`, and that is
//     consistent rather than an omission: the ledger is the security record and a revision is
//     domain data the UI shows as "edited".
//   - **A deletion writes `comment.deleted`** — the one comment action the catalogue defines — and
//     removes the revisions. Deleting is the consequential act, so it is the audited one.
//
// W2.3's acceptance is what ties them together: "editing a comment records the previous body and
// does not rewrite history visible in the UI", and "deletion is soft or audited and never leaves a
// dangling timeline entry". A soft delete leaves the row so the conversation keeps its shape, and
// clearing the body **and** the revisions is what makes "deleted" mean the words are gone rather
// than merely hidden.
//
// # Authorization
//
// Every question goes to `repository.Service`, the same shared decision the API, the browser, and
// the Git transports use. This package only chooses which level to require:
//
//   - reading a comment requires repository read
//   - writing one requires read **and** an authenticated account, because a comment needs an author
//   - editing and deleting require repository write **or** being the comment's author, which is the
//     catalogue's "deleted by its author or a moderator"
package comments

import (
	"errors"
	"strings"
	"time"
)

// Content bounds, mirroring the CHECK constraints in the migration.
//
// They match the issue body's bounds, so a reader is never told two different limits for one kind
// of text. They are re-stated in Go so a caller gets a named field rather than a driver error
// naming a constraint, and the two must agree: a test asserts the boundary cases are refused by
// both.
const (
	// MaxBodyLength bounds a comment.
	MaxBodyLength = 65536
	// MaxBodyBytes is the largest a body can be **in bytes**, which is what a request reader needs
	// to bound itself with. Four times the character limit is the exact worst case for UTF-8, not an
	// estimate.
	MaxBodyBytes = MaxBodyLength * 4
)

// Pagination bounds, matching the issue list's so the two surfaces behave alike.
const (
	// DefaultPageSize is the page a listing returns when a caller names no limit.
	DefaultPageSize = 30
	// MaxPageSize is the largest page a caller may ask for.
	MaxPageSize = 100
)

// Errors this package reports.
var (
	// ErrNotFound reports a comment that does not exist on the issue, or one the caller may not
	// reach because the repository itself is invisible to them. The two are the same error: whether
	// the comment is absent or merely unreadable is not disclosed.
	ErrNotFound = errors.New("the comment does not exist")

	// ErrForbidden reports a caller who may read the comment but not change it. It is distinct from
	// ErrNotFound because the caller can already see the comment, so refusing plainly discloses
	// nothing they did not know.
	ErrForbidden = errors.New("you may not change this comment")

	// ErrConflict reports an edit against a version that is no longer current, per the data-model
	// rule that a conflict is "a visible error, not a silent overwrite".
	ErrConflict = errors.New("the comment changed since you read it")

	// ErrEmptyBody reports a comment with nothing in it. A comment is its text, so an empty one is
	// not a comment — unlike an issue, whose title can carry the whole report.
	ErrEmptyBody = errors.New("a comment needs a body")

	// ErrBodyTooLong reports a body over the bound.
	ErrBodyTooLong = errors.New("the comment is too long")

	// ErrNoChange reports an edit that would change nothing, so no revision is appended for an edit
	// that did not happen.
	ErrNoChange = errors.New("the edit changes nothing")

	// ErrDeleted reports an attempt to edit a comment that has been deleted. Restoring a deleted
	// comment is not an operation this workstream offers: the text is gone, so there is nothing to
	// restore.
	ErrDeleted = errors.New("the comment was deleted")

	// ErrAnonymousAuthor reports an attempt to write a comment with no account. A comment needs an
	// author, so writing requires a credential where reading does not.
	ErrAnonymousAuthor = errors.New("writing a comment requires a signed-in account")
)

// Comment is one comment.
//
// It carries the author's **name** as well as the identifier, on the same reasoning `issues.Issue`
// records: a page shows a conversation of many comments at once, so the read projection joins the
// name it needs rather than resolving one per row. The name is a display value and nothing here
// decides anything from it.
type Comment struct {
	ID      string
	IssueID string
	// Number is the comment's position in its issue, counted from 1 in creation order.
	//
	// **It is derived rather than allocated, and the derivation is exact.** The ordering is
	// `(created_at, id)`, which is total, and a comment is never hard-deleted — the deletion is soft,
	// so the row keeps its place. So the rank in that order is stable for the life of the
	// conversation, and a number allocated from a counter would be a second scheme to keep in step
	// with an order that already exists.
	//
	// It is what a URL and a fragment anchor carry: `urls-and-identifiers.md` L29 permits "names,
	// slugs, numbers, refs, and content digests" in a URL and forbids a stable internal UUID, and
	// L159's `{id}` list does not include comments — so a comment is addressed by a number.
	Number int
	// AuthorID is empty when the author's account has been deleted. The comment survives its author
	// and shows none rather than disappearing.
	AuthorID   string
	AuthorName string
	// Body is empty when the comment is deleted, because the delete clears it. `Deleted` says which
	// case it is, so an empty body is never mistaken for a deletion.
	Body string
	// Deleted reports a soft deletion: the row remains so the conversation keeps its shape.
	Deleted   bool
	DeletedBy string
	// CreatedAt and EditedAt are set by the database; `edited_at` is nil until the first edit, and
	// stays set afterwards.
	CreatedAt time.Time
	EditedAt  *time.Time
	// Version is what an edit names to prove it read the current row.
	Version int
}

// Edited reports whether the comment has been edited at least once.
func (c Comment) Edited() bool { return c.EditedAt != nil }

// AuthoredBy reports whether an account is the comment's author.
//
// An empty account is never the author: a comment whose author was deleted has no author to make
// the author allowance for.
func (c Comment) AuthoredBy(accountID string) bool {
	return accountID != "" && c.AuthorID != "" && c.AuthorID == accountID
}

// Revision is one entry in a comment's edit history.
//
// `Body` is the body **as it was before that edit**: revision 1 holds the original text, revision 2
// the text after the first edit. That is what "records the previous body" means, and it is why the
// history is a sequence rather than a set of snapshots.
type Revision struct {
	ID        string
	CommentID string
	// Revision counts from 1 within one comment.
	Revision int
	Body     string
	// Version is the comment's version when this revision was written, which ties the history to the
	// concurrency guard's numbering rather than to a second scheme.
	Version    int
	EditorID   string
	EditorName string
	CreatedAt  time.Time
}

// ValidateBody checks a comment body and returns it.
//
// It is **not** trimmed, because whitespace inside a body is content — a code block's indentation,
// for one — and removing a trailing newline changes what Markdown renders. A body of only
// whitespace is refused, because it is not text a reader would call a comment.
func ValidateBody(body string) (string, error) {
	if strings.TrimSpace(body) == "" {
		return "", ErrEmptyBody
	}
	if len([]rune(body)) > MaxBodyLength {
		return "", ErrBodyTooLong
	}
	return body, nil
}

// Cursor is a position in the keyset-paginated comment list.
type Cursor struct {
	// CreatedAt and ID are the sort key of the last row a caller saw.
	CreatedAt time.Time
	ID        string
}

// Page is one page of comments.
type Page struct {
	Comments []Comment
	// Next is the cursor for the following page, nil when this is the last one.
	Next *Cursor
	// HasMore reports whether another page exists. The store learns it by fetching one row beyond
	// the limit rather than by counting the collection.
	HasMore bool
}

// SortKeyOf returns a comment's position, so a caller can build a cursor from the last row of a
// page without knowing the sort key's shape.
func SortKeyOf(comment Comment) Cursor {
	return Cursor{CreatedAt: comment.CreatedAt, ID: comment.ID}
}
