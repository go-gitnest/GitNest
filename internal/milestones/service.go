package milestones

import (
	"context"
	"errors"
	"time"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/google/uuid"
)

// The milestone service.
//
// The permission level and the record shape are in the package comment. The short version: everything
// that changes a milestone or an issue's milestone needs repository **write**, and only a close or a
// delete writes a ledger record.

// AccessService answers every access question. It is satisfied by *repository.Service — the same
// decision the API, the browser, and the Git transports use.
type AccessService interface {
	Require(ctx context.Context, actor repository.Principal, repoID string, required repository.Level) (repository.Repository, repository.Level, error)
}

// Store persists milestones. It is the seam a fake implements, so the rules above are testable without a
// database.
type Store interface {
	// Create allocates a number and stores the milestone. It carries **no record**, because the catalogue
	// defines no action for creating one.
	Create(ctx context.Context, input StoreCreate) (Milestone, error)
	// GetByNumber returns one milestone by its repository-scoped number.
	GetByNumber(ctx context.Context, repositoryID string, number int64) (Milestone, error)
	// List returns a repository's milestones, newest first.
	List(ctx context.Context, repositoryID string, state State) ([]Milestone, error)
	// Update applies a title, description, and due date. It carries **no record**.
	Update(ctx context.Context, input StoreUpdate) (Milestone, error)
	// SetState applies a close or a reopen, writing the record it is given. A reopen passes an empty one,
	// because the catalogue defines no action for it.
	SetState(ctx context.Context, input StoreSetState) (Milestone, error)
	// Delete removes a milestone, detaching its issues rather than deleting them, and writes the record
	// it is given.
	Delete(ctx context.Context, input StoreDelete) error
	// Progress returns a milestone's issue counts, and the counts for every milestone of a repository.
	Progress(ctx context.Context, repositoryID string) (map[string]Progress, error)
	// Attach sets an issue's milestone, reporting whether the issue moved from a different one.
	Attach(ctx context.Context, input StoreAttach) error
	// Detach clears an issue's milestone, reporting whether it had one.
	Detach(ctx context.Context, issueID string) (bool, error)
	// MilestoneOf returns the milestone number an issue carries, or zero when it carries none.
	MilestoneOf(ctx context.Context, issueID string) (int64, error)
}

// StoreCreate is what the store needs to write a milestone.
type StoreCreate struct {
	ID           string
	RepositoryID string
	Title        string
	TitleFolded  string
	Description  string
	DueOn        *time.Time
}

// StoreUpdate is a milestone change. It carries no record, deliberately.
type StoreUpdate struct {
	RepositoryID string
	Number       int64
	Title        string
	TitleFolded  string
	Description  string
	DueOn        *time.Time
	// DueOnSet distinguishes "leave the due date alone" from "clear it", because a nil pointer alone
	// cannot express both.
	DueOnSet bool
}

// StoreSetState is a close or a reopen.
type StoreSetState struct {
	RepositoryID string
	Number       int64
	State        State
	// Expect is the state the caller believes the milestone is in, checked in the same statement that
	// writes.
	Expect State
	// Event is empty for a reopen, which the catalogue does not record.
	Event audit.Event
}

// StoreDelete removes a milestone.
type StoreDelete struct {
	RepositoryID string
	Number       int64
	Event        audit.Event
}

// StoreAttach sets an issue's milestone.
type StoreAttach struct {
	IssueID     string
	MilestoneID string
}

// Service implements the milestone use cases.
type Service struct {
	access AccessService
	store  Store
	// now is the clock, used for the overdue comparison. It is injected so a test can assert the boundary
	// rather than depend on the wall clock.
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

// Overdue reports whether a milestone's due date has passed.
func (s *Service) Overdue(m Milestone) bool { return m.Overdue(s.now()) }

// CreateInput is a request to create a milestone.
type CreateInput struct {
	Actor        repository.Principal
	RepositoryID string
	Title        string
	Description  string
	// DueDate is a calendar date as `YYYY-MM-DD`, or empty for none.
	DueDate string
}

// Create makes a milestone.
//
// It requires repository write — see the package comment for why that is not the admin a label needs. It
// writes **no ledger record**, because the catalogue defines no `milestone.created`.
func (s *Service) Create(ctx context.Context, input CreateInput) (Milestone, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelWrite); err != nil {
		return Milestone{}, err
	}

	title, err := ValidateTitle(input.Title)
	if err != nil {
		return Milestone{}, err
	}
	description, err := ValidateDescription(input.Description)
	if err != nil {
		return Milestone{}, err
	}
	due, err := ParseDueDate(input.DueDate)
	if err != nil {
		return Milestone{}, err
	}

	return s.store.Create(ctx, StoreCreate{
		ID: newIdentifier(), RepositoryID: input.RepositoryID,
		Title: title, TitleFolded: Fold(title), Description: description, DueOn: due,
	})
}

// GetInput is a request to read one milestone.
type GetInput struct {
	Actor        repository.Principal
	RepositoryID string
	Number       int64
}

// Get returns one milestone, requiring repository read.
func (s *Service) Get(ctx context.Context, input GetInput) (Milestone, error) {
	if input.Number < 1 {
		return Milestone{}, ErrNotFound
	}
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return Milestone{}, err
	}
	return s.store.GetByNumber(ctx, input.RepositoryID, input.Number)
}

// ListInput is a request to list a repository's milestones.
type ListInput struct {
	Actor        repository.Principal
	RepositoryID string
	// State filters the listing; the empty value means every state.
	State State
}

// List returns a repository's milestones, requiring repository read.
func (s *Service) List(ctx context.Context, input ListInput) ([]Milestone, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return nil, err
	}
	return s.store.List(ctx, input.RepositoryID, input.State)
}

// ProgressInput is a request for a repository's milestone progress.
type ProgressInput struct {
	Actor        repository.Principal
	RepositoryID string
}

// Progress returns how many issues carry each milestone, requiring repository read.
//
// The counts are derived from a join on every read, which is what makes the acceptance criterion
// "progress counts stay correct after an issue is closed, reopened, or relabeled" true by construction.
func (s *Service) Progress(ctx context.Context, input ProgressInput) (map[string]Progress, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return nil, err
	}
	return s.store.Progress(ctx, input.RepositoryID)
}

// UpdateInput is a request to change a milestone.
type UpdateInput struct {
	Actor        repository.Principal
	RepositoryID string
	Number       int64
	// Title, Description, and DueDate are pointers so "not provided" is distinguishable from "set to the
	// empty value": a description and a due date may both legitimately be cleared, and a title may not be
	// absent.
	Title       *string
	Description *string
	DueDate     *string
}

// Update changes a milestone's title, description, or due date.
//
// It requires repository write and writes **no ledger record**, because the catalogue defines no
// `milestone.edited`.
func (s *Service) Update(ctx context.Context, input UpdateInput) (Milestone, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelWrite); err != nil {
		return Milestone{}, err
	}
	if input.Title == nil && input.Description == nil && input.DueDate == nil {
		return Milestone{}, ErrNoChange
	}

	existing, err := s.store.GetByNumber(ctx, input.RepositoryID, input.Number)
	if err != nil {
		return Milestone{}, err
	}

	title, folded := existing.Title, Fold(existing.Title)
	if input.Title != nil {
		validated, err := ValidateTitle(*input.Title)
		if err != nil {
			return Milestone{}, err
		}
		title, folded = validated, Fold(validated)
	}
	description := existing.Description
	if input.Description != nil {
		validated, err := ValidateDescription(*input.Description)
		if err != nil {
			return Milestone{}, err
		}
		description = validated
	}
	due, dueSet := existing.DueOn, false
	if input.DueDate != nil {
		parsed, err := ParseDueDate(*input.DueDate)
		if err != nil {
			return Milestone{}, err
		}
		due, dueSet = parsed, true
	}

	sameDue := !dueSet || sameDueDate(existing.DueOn, due)
	if title == existing.Title && description == existing.Description && sameDue {
		return Milestone{}, ErrNoChange
	}

	return s.store.Update(ctx, StoreUpdate{
		RepositoryID: input.RepositoryID, Number: input.Number,
		Title: title, TitleFolded: folded, Description: description,
		DueOn: due, DueOnSet: dueSet,
	})
}

// sameDueDate reports whether two due dates are the same calendar date.
func sameDueDate(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.UTC().Format(dueDateLayout) == right.UTC().Format(dueDateLayout)
}

// CloseInput is a request to close a milestone.
type CloseInput struct {
	Actor        repository.Principal
	RepositoryID string
	Number       int64
	Request      audit.RequestContext
}

// Close closes an open milestone, recording `milestone.closed`.
func (s *Service) Close(ctx context.Context, input CloseInput) (Milestone, error) {
	return s.setState(ctx, setState{
		actor: input.Actor, repositoryID: input.RepositoryID, number: input.Number,
		from: StateOpen, to: StateClosed,
		// The one action a state change writes, because the catalogue defines it.
		action: audit.ActionMilestoneClosed, request: input.Request,
	})
}

// ReopenInput is a request to reopen a closed milestone.
type ReopenInput struct {
	Actor        repository.Principal
	RepositoryID string
	Number       int64
}

// Reopen reopens a closed milestone.
//
// **It writes no ledger record**, because the catalogue defines `milestone.closed` and no
// `milestone.opened` — the reopening is visible in the milestone's own state rather than in the ledger.
func (s *Service) Reopen(ctx context.Context, input ReopenInput) (Milestone, error) {
	return s.setState(ctx, setState{
		actor: input.Actor, repositoryID: input.RepositoryID, number: input.Number,
		from: StateClosed, to: StateOpen, action: "",
	})
}

// setState is one lifecycle change, so close and reopen share every rule rather than each implementing it.
type setState struct {
	actor        repository.Principal
	repositoryID string
	number       int64
	from         State
	to           State
	// action is the catalogue type for this direction, or empty when there is none.
	action  string
	request audit.RequestContext
}

// setState applies one lifecycle change.
//
// **The milestone is resolved before the record is built, and that order is load-bearing.** A record
// scoped to a milestone needs the milestone's identifier, and `audit.Event.Validate` refuses a scoped
// record without one — so an event built before the row was read failed validation with "a scoped audit
// record needs its scope". Reading first is also what gives the caller a clean not-found and a clean
// already-in-state refusal rather than a driver-level explanation from the write.
//
// The store's `WHERE state = $expect` still guards the write itself, so a state change racing this read
// cannot both win.
func (s *Service) setState(ctx context.Context, t setState) (Milestone, error) {
	if _, _, err := s.require(ctx, t.actor, t.repositoryID, repository.LevelWrite); err != nil {
		return Milestone{}, err
	}

	existing, err := s.store.GetByNumber(ctx, t.repositoryID, t.number)
	if err != nil {
		return Milestone{}, err
	}
	if existing.State != t.from {
		return Milestone{}, ErrAlreadyInState
	}

	// An empty action means this transition writes no record, which is what a reopen passes: the catalogue
	// defines `milestone.closed` and no `milestone.opened`.
	event := audit.Event{}
	if t.action != "" {
		event = audit.Event{
			Action:        t.action,
			ActorID:       t.actor.UserID,
			ActorKind:     audit.ActorUser,
			ScopeType:     audit.ScopeMilestone,
			ScopeID:       existing.ID,
			SubjectType:   audit.SubjectMilestone,
			SubjectID:     existing.ID,
			SourceAddress: t.request.SourceAddress,
			UserAgent:     t.request.UserAgent,
			Detail:        map[string]any{"state": audit.Change{From: string(t.from), To: string(t.to)}},
		}
		if t.actor.IsInstanceAdmin {
			event.ActorKind = audit.ActorAdmin
		}
	}

	return s.store.SetState(ctx, StoreSetState{
		RepositoryID: t.repositoryID, Number: t.number,
		State: t.to, Expect: t.from, Event: event,
	})
}

// DeleteInput is a request to delete a milestone.
type DeleteInput struct {
	Actor        repository.Principal
	RepositoryID string
	Number       int64
	Request      audit.RequestContext
}

// Delete removes a milestone, recording `milestone.deleted`.
//
// Its issues are **detached, not deleted**, which the `ON DELETE SET NULL` foreign key provides rather
// than this function remembering to clean up.
func (s *Service) Delete(ctx context.Context, input DeleteInput) error {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelWrite); err != nil {
		return err
	}
	existing, err := s.store.GetByNumber(ctx, input.RepositoryID, input.Number)
	if err != nil {
		return err
	}

	event := audit.Event{
		Action:        audit.ActionMilestoneDeleted,
		ActorID:       input.Actor.UserID,
		ActorKind:     audit.ActorUser,
		ScopeType:     audit.ScopeMilestone,
		ScopeID:       existing.ID,
		SubjectType:   audit.SubjectMilestone,
		SubjectID:     existing.ID,
		Detail:        map[string]any{"title": existing.Title, "number": existing.Number},
		SourceAddress: input.Request.SourceAddress,
		UserAgent:     input.Request.UserAgent,
	}
	if input.Actor.IsInstanceAdmin {
		event.ActorKind = audit.ActorAdmin
	}

	return s.store.Delete(ctx, StoreDelete{
		RepositoryID: input.RepositoryID, Number: input.Number, Event: event,
	})
}

// MilestoneOfInput is a request for the milestone an issue carries.
type MilestoneOfInput struct {
	Actor        repository.Principal
	RepositoryID string
	IssueID      string
}

// MilestoneOf reports the milestone an issue carries, or zero when it carries none.
//
// It requires repository read, because it discloses an issue's milestone — and a caller who cannot read the
// repository is refused before the lookup, so the answer is the same not-found as everywhere else.
func (s *Service) MilestoneOf(ctx context.Context, input MilestoneOfInput) (int64, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return 0, err
	}
	return s.store.MilestoneOf(ctx, input.IssueID)
}

// AttachInput is a request to put an issue on a milestone.
type AttachInput struct {
	Actor        repository.Principal
	RepositoryID string
	IssueID      string
	// Number identifies the milestone, which is how the route addresses one.
	Number int64
}

// Attach puts an issue on a milestone.
//
// **A closed milestone rejects new attachments**, which is W2.4's acceptance criterion. The check is here
// and in the store's `WHERE` clause, so a close racing an attachment cannot slip past: the store's is the
// one that writes.
func (s *Service) Attach(ctx context.Context, input AttachInput) error {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelWrite); err != nil {
		return err
	}
	milestone, err := s.store.GetByNumber(ctx, input.RepositoryID, input.Number)
	if err != nil {
		return err
	}
	if !milestone.IsOpen() {
		return ErrClosed
	}
	return s.store.Attach(ctx, StoreAttach{IssueID: input.IssueID, MilestoneID: milestone.ID})
}

// DetachInput is a request to take an issue off its milestone.
type DetachInput struct {
	Actor        repository.Principal
	RepositoryID string
	IssueID      string
}

// Detach takes an issue off its milestone.
//
// A detachment is allowed even when the milestone is closed, because removing work from a milestone is not
// adding it — and refusing would strand an issue on a milestone nobody can change.
func (s *Service) Detach(ctx context.Context, input DetachInput) error {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelWrite); err != nil {
		return err
	}
	removed, err := s.store.Detach(ctx, input.IssueID)
	if err != nil {
		return err
	}
	if !removed {
		return ErrNotAttached
	}
	return nil
}

// require asks the repository service and translates its refusals.
//
// The two are mapped differently, and the difference is the whole reason this function exists: not-found
// stays not-found, keeping "you may not see this" and "this does not exist" indistinguishable, which T-A4
// requires — while forbidden stays forbidden, because a caller whose level was resolved can already read
// the repository.
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

// newIdentifier returns an identifier for a new milestone.
func newIdentifier() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}
