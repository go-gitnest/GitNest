package comments

import (
	"context"
	"errors"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/google/uuid"
)

// The comment service: one authorization decision per operation, and one record written in the
// same transaction as the change.
//
// The reasoning is in the package comment. The short version: an edit appends a revision and writes
// no ledger record, because the catalogue defines no action for it; a deletion writes the one
// comment action the catalogue does define and removes the history; and both write inside the
// transaction that changes the row, so a change that rolled back leaves no record and a record that
// failed to write fails the change.

// AccessService answers every access question. It is satisfied by *repository.Service — the same
// decision the API, the browser, and the Git transports use.
type AccessService interface {
	Require(ctx context.Context, actor repository.Principal, repoID string, required repository.Level) (repository.Repository, repository.Level, error)
}

// Store persists comments. It is the seam a fake implements, so the rules above are testable
// without a database.
type Store interface {
	// Create stores a comment.
	Create(ctx context.Context, input StoreCreate) (Comment, error)
	// Get returns one comment, scoped to an issue and a repository so a comment cannot be reached
	// through an issue it does not belong to.
	Get(ctx context.Context, query StoreGet) (Comment, error)
	// List returns a page of an issue's comments, in creation order.
	List(ctx context.Context, query StoreList) (Page, error)
	// GetByNumber returns one comment by its position in the issue, which is what a URL carries.
	GetByNumber(ctx context.Context, repositoryID, issueID string, number int) (Comment, error)
	// Edit applies a body, guarded by the expected version, and **appends the revision** it is
	// given — both in one transaction.
	Edit(ctx context.Context, input StoreEdit) (Comment, error)
	// Delete soft-deletes a comment, clearing its body and removing its revisions, and writes the
	// record it is given.
	Delete(ctx context.Context, input StoreDelete) error
	// History returns a comment's revisions, oldest first.
	History(ctx context.Context, commentID string) ([]Revision, error)
	// Count returns how many comments an issue has, for the surfaces that show a count.
	Count(ctx context.Context, issueID string) (int, error)
}

// StoreCreate is what the store needs to write a comment.
type StoreCreate struct {
	ID       string
	IssueID  string
	AuthorID string
	Body     string
}

// GetByNumberInput identifies one comment by position.
type GetByNumberInput struct {
	Actor        repository.Principal
	RepositoryID string
	IssueID      string
	Number       int
}

// StoreGet identifies one comment.
//
// `RepositoryID` is part of the identity rather than a separate check, so a query cannot reach a
// comment through a repository it does not belong to.
type StoreGet struct {
	RepositoryID string
	IssueID      string
	CommentID    string
}

// StoreList is a keyset query for one issue's comments.
type StoreList struct {
	RepositoryID string
	IssueID      string
	Limit        int
	// Before is the cursor: return comments strictly newer than this position, because a
	// conversation reads forwards. Nil for the first page.
	Before *Cursor
}

// StoreEdit is a body change guarded by version, with the revision to append.
type StoreEdit struct {
	RepositoryID string
	IssueID      string
	CommentID    string
	// Body is the new body, already validated.
	Body string
	// Revision is the previous body, which the store appends to the history.
	Revision Revision
	// ExpectedVersion is what the caller read. The update refuses a row whose version has moved,
	// which is what makes a concurrent edit a visible conflict.
	ExpectedVersion int
}

// StoreDelete removes a comment's text and history.
type StoreDelete struct {
	RepositoryID string
	IssueID      string
	CommentID    string
	// DeletedBy is the account the deletion is attributed to.
	DeletedBy string
	Event     audit.Event
}

// Service implements the comment use cases.
type Service struct {
	access AccessService
	store  Store
}

// NewService returns a service over an access decision and a store.
func NewService(access AccessService, store Store) *Service {
	return &Service{access: access, store: store}
}

// CreateInput is a request to write a comment.
type CreateInput struct {
	// Actor carries identity only; the level comes from the repository service.
	Actor repository.Principal
	// RepositoryID and IssueID together are the target: the repository is what the access decision
	// is made against, and the issue is what the comment belongs to.
	RepositoryID string
	IssueID      string
	Body         string
}

// Create writes a comment.
//
// It requires repository read rather than write, because commenting on a report is something a
// reader does. It also requires an account: a comment needs an author, so a caller with no
// credential is refused where a read would be allowed.
func (s *Service) Create(ctx context.Context, input CreateInput) (Comment, error) {
	if input.Actor.IsAnonymous() {
		return Comment{}, ErrAnonymousAuthor
	}
	body, err := ValidateBody(input.Body)
	if err != nil {
		return Comment{}, err
	}
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return Comment{}, err
	}

	return s.store.Create(ctx, StoreCreate{
		ID:       newIdentifier(),
		IssueID:  input.IssueID,
		AuthorID: input.Actor.UserID,
		Body:     body,
	})
}

// GetInput is a request to read one comment.
type GetInput struct {
	Actor        repository.Principal
	RepositoryID string
	IssueID      string
	CommentID    string
}

// Get returns one comment, requiring repository read.
//
// **A caller without read access is refused before the comment is looked up**, which is what W2.3's
// acceptance requires: a caller who cannot read the repository cannot fetch comments, and cannot
// infer a comment count by observing which identifiers resolve.
func (s *Service) Get(ctx context.Context, input GetInput) (Comment, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return Comment{}, err
	}
	return s.store.Get(ctx, StoreGet{
		RepositoryID: input.RepositoryID, IssueID: input.IssueID, CommentID: input.CommentID,
	})
}

// Get the comment at a position, requiring repository read.
//
// It is the read a URL needs: `urls-and-identifiers.md` L29 permits "numbers" in a URL and forbids a
// stable internal UUID, so a comment is addressed by its position rather than by its identifier.
func (s *Service) GetByNumber(ctx context.Context, input GetByNumberInput) (Comment, error) {
	if input.Number < 1 {
		return Comment{}, ErrNotFound
	}
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return Comment{}, err
	}
	return s.store.GetByNumber(ctx, input.RepositoryID, input.IssueID, input.Number)
}

// ListInput is a request to list an issue's comments.
type ListInput struct {
	Actor        repository.Principal
	RepositoryID string
	IssueID      string
	Limit        int
	Before       *Cursor
}

// List returns a page of comments, requiring repository read.
func (s *Service) List(ctx context.Context, input ListInput) (Page, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return Page{}, err
	}
	return s.store.List(ctx, StoreList{
		RepositoryID: input.RepositoryID, IssueID: input.IssueID,
		Limit: input.Limit, Before: input.Before,
	})
}

// CountInput is a request for an issue's comment count.
type CountInput struct {
	Actor        repository.Principal
	RepositoryID string
	IssueID      string
}

// Count reports how many comments an issue has.
//
// It goes through the same access decision as the listing rather than querying directly, because the
// count is exactly what the acceptance protects: "a user without read access ... cannot infer
// comment counts".
func (s *Service) Count(ctx context.Context, input CountInput) (int, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return 0, err
	}
	return s.store.Count(ctx, input.IssueID)
}

// EditInput is a request to change a comment's body.
type EditInput struct {
	Actor        repository.Principal
	RepositoryID string
	IssueID      string
	CommentID    string
	Body         string
	// ExpectedVersion is the version the caller read. It is required, because an edit that does not
	// state what it read cannot detect a concurrent change.
	ExpectedVersion int
}

// Edit changes a comment's body and appends the previous one to its history.
//
// The author may edit their own comment; anybody else needs repository write. The version check
// makes a concurrent edit a conflict rather than an overwrite, and the previous body is recorded so
// the change is visible in the history rather than being a rewrite.
func (s *Service) Edit(ctx context.Context, input EditInput) (Comment, error) {
	if input.ExpectedVersion < 1 {
		return Comment{}, ErrConflict
	}
	body, err := ValidateBody(input.Body)
	if err != nil {
		return Comment{}, err
	}

	comment, _, err := s.authorizeChange(ctx, input.Actor, input.RepositoryID, input.IssueID, input.CommentID)
	if err != nil {
		return Comment{}, err
	}
	// A deleted comment has no text to edit. Restoring one is not offered, because the body was
	// cleared and there is nothing left to restore.
	if comment.Deleted {
		return Comment{}, ErrDeleted
	}
	if body == comment.Body {
		// Nothing would change, so nothing is written and no revision is appended. A history entry
		// for an edit that did not happen is worse than no entry.
		return Comment{}, ErrNoChange
	}

	return s.store.Edit(ctx, StoreEdit{
		RepositoryID: input.RepositoryID, IssueID: input.IssueID, CommentID: input.CommentID,
		Body: body,
		// The revision holds the body **before** this edit, which is what makes the history a record
		// of what changed rather than a log of current state.
		Revision: Revision{
			ID:        newIdentifier(),
			CommentID: input.CommentID,
			Body:      comment.Body,
			Version:   comment.Version,
			EditorID:  input.Actor.UserID,
		},
		ExpectedVersion: input.ExpectedVersion,
	})
}

// DeleteInput is a request to delete a comment.
type DeleteInput struct {
	Actor        repository.Principal
	RepositoryID string
	IssueID      string
	CommentID    string
	Request      audit.RequestContext
}

// Delete removes a comment's text and history.
//
// The author may delete their own comment, and a repository writer may delete anybody's — which is
// the catalogue's "deleted by its author **or a moderator**". It is a soft deletion: the row
// remains so the conversation keeps its shape, and the body and the history are cleared so the words
// are gone rather than merely hidden.
func (s *Service) Delete(ctx context.Context, input DeleteInput) error {
	comment, _, err := s.authorizeChange(ctx, input.Actor, input.RepositoryID, input.IssueID, input.CommentID)
	if err != nil {
		return err
	}
	if comment.Deleted {
		// Deleting twice is refused rather than accepted silently, so a client that lost track of
		// the state is told rather than being told it succeeded.
		return ErrDeleted
	}

	event := audit.Event{
		Action:      audit.ActionCommentDeleted,
		ActorID:     input.Actor.UserID,
		ActorKind:   audit.ActorUser,
		ScopeType:   audit.ScopeComment,
		ScopeID:     comment.ID,
		SubjectType: audit.SubjectComment,
		SubjectID:   comment.ID,
		// The record names the comment's issue, which is what makes it answerable after the comment's
		// text is gone: an operator asking "what was removed from this conversation" has the issue.
		Detail:        map[string]any{"issue_id": comment.IssueID},
		SourceAddress: input.Request.SourceAddress,
		UserAgent:     input.Request.UserAgent,
	}
	if input.Actor.IsInstanceAdmin {
		event.ActorKind = audit.ActorAdmin
	}

	return s.store.Delete(ctx, StoreDelete{
		RepositoryID: input.RepositoryID, IssueID: input.IssueID, CommentID: input.CommentID,
		DeletedBy: input.Actor.UserID,
		Event:     event,
	})
}

// HistoryInput is a request to read a comment's edit history.
type HistoryInput struct {
	Actor        repository.Principal
	RepositoryID string
	IssueID      string
	CommentID    string
}

// History returns a comment's revisions, oldest first.
func (s *Service) History(ctx context.Context, input HistoryInput) ([]Revision, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return nil, err
	}
	return s.store.History(ctx, input.CommentID)
}

// authorizeChange resolves a comment and checks the caller may change it.
//
// It returns the level alongside the comment so a caller does not ask twice, which would be two
// decisions that could disagree.
func (s *Service) authorizeChange(ctx context.Context, actor repository.Principal, repositoryID, issueID, commentID string) (Comment, repository.Level, error) {
	// The repository is checked first, so a caller with no access gets the refusal the repository
	// service produces before the comment is looked up — which is why a private repository's comment
	// identifiers cannot be probed by observing which ones resolve.
	_, level, err := s.require(ctx, actor, repositoryID, repository.LevelRead)
	if err != nil {
		return Comment{}, repository.LevelNone, err
	}

	comment, err := s.store.Get(ctx, StoreGet{
		RepositoryID: repositoryID, IssueID: issueID, CommentID: commentID,
	})
	if err != nil {
		return Comment{}, level, err
	}

	// Repository write, which is the catalogue's "moderator", or being the author. The author
	// allowance is scoped to this comment, so it confers nothing repository-wide.
	if level.Allows(repository.LevelWrite) || comment.AuthoredBy(actor.UserID) {
		return comment, level, nil
	}
	return Comment{}, level, ErrForbidden
}

// require asks the repository service and translates its refusals.
//
// **The two are mapped differently, and the difference is the whole reason this function exists.**
// The repository service reports `ErrNotFound` when the caller has no access at all and
// `ErrForbidden` when the level is known but too low, so:
//
//   - `ErrNotFound` becomes this package's `ErrNotFound`, keeping "you may not see this" and "this
//     does not exist" indistinguishable, which T-A4 requires.
//   - `ErrForbidden` becomes this package's `ErrForbidden`, because a caller whose level was
//     resolved can already read the repository. Telling them their level is too low discloses
//     nothing they did not know, and collapsing it into not-found would be a lie about a repository
//     they can see.
//
// Getting the second mapping wrong was a real defect in the issue package, found by a refused
// deletion that claimed the thing did not exist.
func (s *Service) require(ctx context.Context, actor repository.Principal, repositoryID string, required repository.Level) (repository.Repository, repository.Level, error) {
	found, level, err := s.access.Require(ctx, actor, repositoryID, required)
	if err != nil {
		switch {
		case errors.Is(err, repository.ErrNotFound):
			return repository.Repository{}, repository.LevelNone, ErrNotFound
		case errors.Is(err, repository.ErrForbidden):
			return repository.Repository{}, repository.LevelNone, ErrForbidden
		default:
			return repository.Repository{}, repository.LevelNone, err
		}
	}
	return found, level, nil
}

// newIdentifier returns an identifier for a new comment or revision.
//
// UUIDv7 rather than v4, per the data-model rule that an identifier is generated in Go as a
// time-ordered UUIDv7 so index inserts stay local.
func newIdentifier() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}
