package issues

import (
	"context"
	"errors"
	"time"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/google/uuid"
)

// The issue service: one authorization decision per operation, and one record per state
// change, written in the same transaction as the change.
//
// # Why the decisions live here rather than in a transport
//
// docs/architecture.md requires that "every access decision comes from the repository
// service; the Git transport, the API, and the browser must not answer it
// independently". An issue has two questions to answer — may this caller see it, and may
// this caller change it — and both are answered here, so a browser page, an API call, and
// a future webhook cannot disagree.
//
// # The author rule, and why it is not a second permission model
//
// W2.2's acceptance says closing, editing, and reopening require "write permission **or
// issue author rules**". The allowance given to an author is not a grant of repository
// write: it applies only to the issue they opened. A caller's repository level remains the
// only source of repository-wide access, so an author holding read cannot touch anybody
// else's issue.
//
// # The concurrency rule
//
// An edit names the version it read. A mismatch is `ErrConflict` rather than a silent
// overwrite, per the data-model rule for "two writers on one editable record". The check is
// in the `UPDATE`'s `WHERE` clause rather than in a read before it, so it cannot race the
// write it guards.
//
// # What an edit does not do, and why
//
// **An edit writes no audit record.** `docs/reference/events.md` defines
// `issue.created`, `issue.closed`, `issue.reopened`, `issue.deleted`, and
// `issue.transferred` — and no `issue.edited`. Inventing the spelling would put a value into
// the contract that W2.15, which owns phase 2's event surface, has not agreed to. The store
// method for an edit therefore takes no record at all, so the omission cannot be mistaken
// for an oversight in the code.

// AccessService answers every access question. It is satisfied by *repository.Service —
// the same decision the API, the browser, and the Git transports use.
type AccessService interface {
	Require(ctx context.Context, actor repository.Principal, repoID string, required repository.Level) (repository.Repository, repository.Level, error)
}

// Store persists issues. It is the seam a fake implements, so the rules above are testable
// without a database.
type Store interface {
	// Create allocates a number and stores the issue, writing the record it is given in the
	// same transaction as the insert.
	Create(ctx context.Context, input StoreCreate) (Issue, error)
	// Get returns one issue by repository and number.
	Get(ctx context.Context, repositoryID string, number int64) (Issue, error)
	// List returns a page, newest first.
	List(ctx context.Context, query StoreList) (Page, error)
	// Edit applies a validated title and body, guarded by the expected version.
	Edit(ctx context.Context, input StoreEdit) (Issue, error)
	// SetState applies a lifecycle transition, writing the record it is given.
	SetState(ctx context.Context, input StoreSetState) (Issue, error)
	// Delete removes an issue, writing the record it is given.
	Delete(ctx context.Context, input StoreDelete) error
	// Count reports how many issues a repository has in a state.
	Count(ctx context.Context, repositoryID string, state State) (int, error)
}

// StoreCreate is what the store needs to open an issue.
//
// It is exported because a fake in another package implements Store, and an unexported
// parameter type would make that impossible. The fields are still only built by this
// package's service.
type StoreCreate struct {
	ID           string
	RepositoryID string
	Title        string
	Body         string
	AuthorID     string
	// Event is the record to write. The store sets its `number` detail once the number is
	// allocated, because the number is what the store produces.
	Event audit.Event
}

// StoreList is a keyset query.
//
// Keyset rather than offset, because `data-model.md` makes it the default for "large or
// continuously growing collections" and an issue list is one. The cursor is the
// (created_at, id) pair of the last row a caller saw, which is also the sort key — so the
// predicate is satisfied by the index and a page costs the same at any depth.
type StoreList struct {
	RepositoryID string
	// State filters the listing. The empty state means every state.
	State State
	// MilestoneID filters the listing to one milestone's issues.
	MilestoneID string
	// NoMilestone filters the listing to issues that carry **no** milestone, which is a distinct question
	// from "no filter": `milestone=none` is a legitimate list a reader asks for, and it cannot be expressed
	// by an absent identifier.
	NoMilestone bool
	Limit       int
	// Before is the cursor: return issues strictly older than this position. Nil for the
	// first page.
	Before *Cursor
}

// StoreEdit is a title and body change guarded by version.
//
// It carries no audit record, deliberately: the catalogue defines no action for an edit of
// an issue's text. See the package comment.
type StoreEdit struct {
	RepositoryID string
	Number       int64
	// Title and Body are the new values, already validated.
	Title string
	Body  string
	// ExpectedVersion is what the caller read. The update refuses a row whose version has
	// moved, which is what makes a concurrent edit a visible conflict.
	ExpectedVersion int
}

// StoreSetState is a close or a reopen.
type StoreSetState struct {
	RepositoryID string
	Number       int64
	// State is the state to move to.
	State State
	// Expect is the state the caller believes the issue is in, checked in the same
	// statement that writes, so a close of an already-closed issue is refused rather than
	// recorded as a change.
	Expect State
	Event  audit.Event
}

// StoreDelete removes an issue.
type StoreDelete struct {
	RepositoryID string
	Number       int64
	Event        audit.Event
}

// Service implements the issue use cases.
type Service struct {
	access AccessService
	store  Store
	// now is the clock. It is used only for the close timestamp carried in a record;
	// `created_at` and `updated_at` are the database's, per the typing rule that the
	// application does not generate "now" when the database can.
	now func() time.Time
}

// NewService returns a service over an access decision and a store.
func NewService(access AccessService, store Store) *Service {
	return &Service{access: access, store: store, now: func() time.Time { return time.Now().UTC() }}
}

// WithClock replaces the clock, for a test.
func (s *Service) WithClock(now func() time.Time) *Service {
	s.now = now
	return s
}

// CreateInput is a request to open an issue.
type CreateInput struct {
	// Actor carries identity only; the level comes from the repository service.
	Actor repository.Principal
	// RepositoryID is the internal identifier, never a name: the caller resolved the
	// repository already, and a name here would let a second route decide access.
	RepositoryID string
	Title        string
	Body         string
	Request      audit.RequestContext
}

// Create opens an issue.
//
// It requires repository read rather than write, because filing a report is something a
// reader does. It also requires an account: an issue needs an author, so a caller with no
// credential is refused where a read would be allowed.
func (s *Service) Create(ctx context.Context, input CreateInput) (Issue, error) {
	if input.Actor.IsAnonymous() {
		return Issue{}, ErrAnonymousAuthor
	}
	title, err := ValidateTitle(input.Title)
	if err != nil {
		return Issue{}, err
	}
	body, err := ValidateBody(input.Body)
	if err != nil {
		return Issue{}, err
	}
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return Issue{}, err
	}

	// The record is built before the store call so the store can write it in the same
	// transaction as the insert: a change that rolled back must leave no record, and a
	// record that failed to write must fail the change — the rule W1.10a established.
	//
	// **The service scopes its own record.** It generates the identifier, so it knows it; the
	// only thing it cannot know is the number, which the store allocates inside the
	// transaction and adds to the detail. Making the service responsible for everything except
	// what only the store can know is what keeps a record valid before it is written — and a
	// fake store then cannot produce a record the database would refuse.
	id := newIdentifier()
	event := s.event(input.Actor, audit.ActionIssueCreated, input.Request)
	event.ScopeID = id
	event.SubjectID = id
	event.Detail = map[string]any{"title": title, "state": string(StateOpen)}

	return s.store.Create(ctx, StoreCreate{
		ID:           id,
		RepositoryID: input.RepositoryID,
		Title:        title,
		Body:         body,
		AuthorID:     input.Actor.UserID,
		Event:        event,
	})
}

// GetInput is a request to read one issue.
type GetInput struct {
	Actor        repository.Principal
	RepositoryID string
	Number       int64
}

// Get returns one issue, requiring repository read.
func (s *Service) Get(ctx context.Context, input GetInput) (Issue, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return Issue{}, err
	}
	return s.store.Get(ctx, input.RepositoryID, input.Number)
}

// ListInput is a request to list a repository's issues.
type ListInput struct {
	Actor        repository.Principal
	RepositoryID string
	// State filters the listing; the empty value means every state.
	State State
	// MilestoneID filters to one milestone's issues, and NoMilestone to issues with none. Both empty means
	// no milestone filter at all.
	MilestoneID string
	NoMilestone bool
	Limit       int
	// Before continues a previous page.
	Before *Cursor
}

// List returns a page of issues, requiring repository read.
//
// A caller who cannot read the repository is refused before any query runs, so a private
// repository's issues are never reached — which is what W2.2's acceptance requires of
// anonymous callers.
func (s *Service) List(ctx context.Context, input ListInput) (Page, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return Page{}, err
	}
	return s.store.List(ctx, StoreList{
		RepositoryID: input.RepositoryID,
		State:        input.State,
		MilestoneID:  input.MilestoneID,
		NoMilestone:  input.NoMilestone,
		Limit:        input.Limit,
		Before:       input.Before,
	})
}

// CountInput is a request for a repository's issue count.
type CountInput struct {
	Actor        repository.Principal
	RepositoryID string
	State        State
}

// Count reports how many issues a repository has in a state.
//
// It goes through the same access decision as the listing rather than querying directly,
// because the count is what W2.2's acceptance protects: "issue counts for a private
// repository are not exposed to anonymous callers".
func (s *Service) Count(ctx context.Context, input CountInput) (int, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return 0, err
	}
	return s.store.Count(ctx, input.RepositoryID, input.State)
}

// EditInput is a request to change an issue's title or body.
type EditInput struct {
	Actor        repository.Principal
	RepositoryID string
	Number       int64
	// Title and Body are pointers so "not provided" is distinguishable from "set to the
	// empty value": a body may legitimately be cleared, and a title may not be absent.
	Title *string
	Body  *string
	// ExpectedVersion is the version the caller read. It is required, because an edit that
	// does not state what it read cannot detect a concurrent change.
	ExpectedVersion int
	// Request is carried for a future audit action. It is not used today, because the
	// catalogue defines no action for an edit; keeping it in the input means adding one is
	// not a signature change.
	Request audit.RequestContext
}

// Edit changes an issue's title or body.
//
// The author may edit their own issue; anybody else needs repository write. The version
// check makes a concurrent edit a conflict rather than an overwrite.
func (s *Service) Edit(ctx context.Context, input EditInput) (Issue, error) {
	if input.Title == nil && input.Body == nil {
		return Issue{}, ErrNoChange
	}
	if input.ExpectedVersion < 1 {
		return Issue{}, ErrConflict
	}

	issue, _, err := s.authorizeChange(ctx, input.Actor, input.RepositoryID, input.Number)
	if err != nil {
		return Issue{}, err
	}

	title := issue.Title
	if input.Title != nil {
		validated, err := ValidateTitle(*input.Title)
		if err != nil {
			return Issue{}, err
		}
		title = validated
	}
	body := issue.Body
	if input.Body != nil {
		validated, err := ValidateBody(*input.Body)
		if err != nil {
			return Issue{}, err
		}
		body = validated
	}
	if title == issue.Title && body == issue.Body {
		// Nothing would change, so nothing is written. A record of a change that did not
		// happen is worse than no record.
		return Issue{}, ErrNoChange
	}

	return s.store.Edit(ctx, StoreEdit{
		RepositoryID:    input.RepositoryID,
		Number:          input.Number,
		Title:           title,
		Body:            body,
		ExpectedVersion: input.ExpectedVersion,
	})
}

// CloseInput is a request to close an issue.
type CloseInput struct {
	Actor        repository.Principal
	RepositoryID string
	Number       int64
	Request      audit.RequestContext
}

// Close closes an open issue.
func (s *Service) Close(ctx context.Context, input CloseInput) (Issue, error) {
	return s.transition(ctx, transition{
		actor: input.Actor, repositoryID: input.RepositoryID, number: input.Number,
		from: StateOpen, to: StateClosed,
		action: audit.ActionIssueClosed, request: input.Request,
	})
}

// ReopenInput is a request to reopen a closed issue.
type ReopenInput struct {
	Actor        repository.Principal
	RepositoryID string
	Number       int64
	Request      audit.RequestContext
}

// Reopen reopens a closed issue.
func (s *Service) Reopen(ctx context.Context, input ReopenInput) (Issue, error) {
	return s.transition(ctx, transition{
		actor: input.Actor, repositoryID: input.RepositoryID, number: input.Number,
		from: StateClosed, to: StateOpen,
		action: audit.ActionIssueReopened, request: input.Request,
	})
}

// transition is one lifecycle change, so close and reopen share every rule rather than each
// implementing it.
type transition struct {
	actor        repository.Principal
	repositoryID string
	number       int64
	// from is the state the caller believes the issue is in, and to is the one it moves to.
	from State
	to   State
	// action is the catalogue type for this direction: a close and a reopen are two events,
	// not one with a field.
	action  string
	request audit.RequestContext
}

// transition applies a lifecycle change.
//
// The `from` state is checked here and again in the store's `WHERE` clause, so a close of an
// already-closed issue is refused rather than recorded as a change. The store's check is the
// one that writes; this one exists so the caller gets an issue-shaped error rather than
// having to interpret a missing row.
func (s *Service) transition(ctx context.Context, t transition) (Issue, error) {
	issue, _, err := s.authorizeChange(ctx, t.actor, t.repositoryID, t.number)
	if err != nil {
		return Issue{}, err
	}
	if issue.State != t.from {
		return Issue{}, ErrAlreadyInState
	}

	event := s.event(t.actor, t.action, t.request)
	event.ScopeID = issue.ID
	event.SubjectID = issue.ID
	event.Detail = map[string]any{"state": audit.Change{From: string(t.from), To: string(t.to)}}

	return s.store.SetState(ctx, StoreSetState{
		RepositoryID: t.repositoryID,
		Number:       t.number,
		State:        t.to,
		Expect:       t.from,
		Event:        event,
	})
}

// DeleteInput is a request to delete an issue.
type DeleteInput struct {
	Actor        repository.Principal
	RepositoryID string
	Number       int64
	Request      audit.RequestContext
}

// Delete removes an issue.
//
// It requires repository write, with no author allowance: deleting the record of a
// discussion is a repository-level act rather than the author's to take. The ledger keeps
// the fact, because the record has no foreign key to the issue and therefore outlives it.
func (s *Service) Delete(ctx context.Context, input DeleteInput) error {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelWrite); err != nil {
		return err
	}
	issue, err := s.store.Get(ctx, input.RepositoryID, input.Number)
	if err != nil {
		return err
	}

	event := s.event(input.Actor, audit.ActionIssueDeleted, input.Request)
	event.ScopeID = issue.ID
	event.SubjectID = issue.ID
	event.Detail = map[string]any{"number": issue.Number, "title": issue.Title}

	return s.store.Delete(ctx, StoreDelete{
		RepositoryID: input.RepositoryID, Number: input.Number, Event: event,
	})
}

// authorizeChange resolves an issue and checks the caller may change it.
//
// It returns the level alongside the issue so a caller does not ask twice, which would be two
// decisions that could disagree.
func (s *Service) authorizeChange(ctx context.Context, actor repository.Principal, repositoryID string, number int64) (Issue, repository.Level, error) {
	// The repository is checked first, so a caller with no access gets the refusal the
	// repository service produces before the issue is looked up — which is why a private
	// repository's issue numbers cannot be probed by observing which ones resolve.
	_, level, err := s.require(ctx, actor, repositoryID, repository.LevelRead)
	if err != nil {
		return Issue{}, repository.LevelNone, err
	}

	issue, err := s.store.Get(ctx, repositoryID, number)
	if err != nil {
		return Issue{}, level, err
	}

	// Write permission, or being the author. The author allowance is scoped to this issue,
	// so it confers nothing repository-wide.
	if level.Allows(repository.LevelWrite) || issue.AuthoredBy(actor.UserID) {
		return issue, level, nil
	}
	return Issue{}, level, ErrForbidden
}

// require asks the repository service and translates its two refusals.
//
// **The two are mapped differently, and the difference is the whole reason this function
// exists.** The repository service reports `ErrNotFound` when the caller has no access at all
// and `ErrForbidden` when the caller's level is known but too low — so:
//
//   - `ErrNotFound` becomes this package's `ErrNotFound`, keeping "you may not see this" and
//     "this does not exist" indistinguishable, which T-A4 requires.
//   - `ErrForbidden` becomes this package's `ErrForbidden`, because a caller whose level was
//     resolved can already read the repository. Telling them their level is too low discloses
//     nothing they did not know, and collapsing it into not-found would be a lie about a
//     repository they can see.
//
// Getting the second mapping wrong was a real defect: a delete by a reader reported that the
// issue did not exist, which is both false and unhelpful.
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

// event builds a record with the parts every issue event shares.
//
// The scope and subject are filled in by the caller once the issue's identifier is known,
// because an open issue has no number until the store allocates one.
func (s *Service) event(actor repository.Principal, action string, request audit.RequestContext) audit.Event {
	event := audit.Event{
		Action:        action,
		ActorID:       actor.UserID,
		ActorKind:     audit.ActorUser,
		ScopeType:     audit.ScopeIssue,
		SubjectType:   audit.SubjectIssue,
		SourceAddress: request.SourceAddress,
		UserAgent:     request.UserAgent,
	}
	if actor.IsInstanceAdmin {
		event.ActorKind = audit.ActorAdmin
	}
	return event
}

// newIdentifier returns an identifier for a new issue.
//
// UUIDv7 rather than v4, per the data-model rule that an identifier is "generated in Go as a
// time-ordered UUIDv7 so index inserts stay local". The fallback is unreachable in practice
// and exists so a failure to generate cannot silently produce an empty identifier.
func newIdentifier() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}

// Cursor is a position in a keyset-paginated listing.
type Cursor struct {
	// CreatedAt and ID are the sort key of the last row a caller saw.
	CreatedAt time.Time
	ID        string
}

// Page is one page of issues.
type Page struct {
	Issues []Issue
	// Next is the cursor for the following page, nil when this is the last one.
	Next *Cursor
	// HasMore reports whether another page exists. The store learns it by fetching one row
	// beyond the limit rather than by counting the collection, so a page costs the same at
	// any depth.
	HasMore bool
}

// SortKeyOf returns an issue's position, so a caller can build a cursor from the last row of
// a page without knowing the sort key's shape.
func SortKeyOf(issue Issue) Cursor {
	return Cursor{CreatedAt: issue.CreatedAt, ID: issue.ID}
}
