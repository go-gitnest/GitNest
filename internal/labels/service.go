package labels

import (
	"context"
	"errors"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/google/uuid"
)

// The label service: one authorization decision per operation, and one record written in the same
// transaction as the change.
//
// The two levels and their derivation are in the package comment. The short version: managing a label
// needs repository **admin**, because the catalogue puts `label.created`/`label.deleted` with the
// repository-configuration events; attaching one to an issue needs repository **write**, because that
// is editing content.
//
// # The record shape, and the one action the catalogue does not define
//
// `docs/reference/events.md` defines exactly two label actions:
//
//	| `label.created` / `label.deleted` | A label is created or deleted | admin | label | 2 |
//
// **There is no `label.edited`**, so an update of a label's name, colour, or description writes no
// ledger record. That is the same shape `issue.edited` and `comment.edited` have, and it is implemented
// the same way: the store's update takes no record at all, so the omission cannot be mistaken for a
// forgotten call at the call site.

// AccessService answers every access question. It is satisfied by *repository.Service — the same
// decision the API, the browser, and the Git transports use.
type AccessService interface {
	Require(ctx context.Context, actor repository.Principal, repoID string, required repository.Level) (repository.Repository, repository.Level, error)
}

// Store persists labels. It is the seam a fake implements, so the rules above are testable without a
// database.
type Store interface {
	// Create stores a label, writing the record it is given.
	Create(ctx context.Context, input StoreCreate) (Label, error)
	// Get returns one label by identifier.
	Get(ctx context.Context, repositoryID, labelID string) (Label, error)
	// FindByName returns one label by its folded name, which is how a URL addresses one.
	FindByName(ctx context.Context, repositoryID, nameFolded string) (Label, error)
	// List returns a repository's labels, ordered by folded name.
	List(ctx context.Context, repositoryID string) ([]Label, error)
	// Update applies a name, colour, and description. It carries **no record**, because the catalogue
	// defines no action for an edit.
	Update(ctx context.Context, input StoreUpdate) (Label, error)
	// Delete removes a label, which removes its attachments and no issues, writing the record it is
	// given.
	Delete(ctx context.Context, input StoreDelete) error
	// Attach applies a label to an issue, reporting whether the attachment was new.
	Attach(ctx context.Context, issueID, labelID string) (bool, error)
	// Detach removes a label from an issue, reporting whether anything was removed.
	Detach(ctx context.Context, issueID, labelID string) (bool, error)
	// ForIssue returns the labels on one issue, ordered by folded name.
	ForIssue(ctx context.Context, issueID string) ([]Label, error)
	// CountsForRepository returns how many issues carry each of a repository's labels, which is what a
	// label list shows beside each one.
	CountsForRepository(ctx context.Context, repositoryID string) (map[string]int, error)
}

// StoreCreate is what the store needs to write a label.
type StoreCreate struct {
	ID           string
	RepositoryID string
	Name         string
	NameFolded   string
	Color        string
	Description  string
	Event        audit.Event
}

// StoreUpdate is a label change.
//
// It carries no record, deliberately: the catalogue defines no action for editing a label. See the
// service comment.
type StoreUpdate struct {
	RepositoryID string
	LabelID      string
	Name         string
	NameFolded   string
	Color        string
	Description  string
}

// StoreDelete removes a label.
type StoreDelete struct {
	RepositoryID string
	LabelID      string
	Event        audit.Event
}

// Service implements the label use cases.
type Service struct {
	access AccessService
	store  Store
}

// NewService returns a service over an access decision and a store.
func NewService(access AccessService, store Store) *Service {
	return &Service{access: access, store: store}
}

// CreateInput is a request to create a label.
type CreateInput struct {
	Actor        repository.Principal
	RepositoryID string
	Name         string
	Color        string
	Description  string
	Request      audit.RequestContext
}

// Create makes a label.
//
// It requires repository **admin**, which is what the catalogue's actor column and W1.10b's precedent
// together decide — see the package comment.
func (s *Service) Create(ctx context.Context, input CreateInput) (Label, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelAdmin); err != nil {
		return Label{}, err
	}

	name, err := ValidateName(input.Name)
	if err != nil {
		return Label{}, err
	}
	color, err := NormalizeColor(input.Color)
	if err != nil {
		return Label{}, err
	}
	description, err := ValidateDescription(input.Description)
	if err != nil {
		return Label{}, err
	}

	id := newIdentifier()
	event := s.event(input.Actor, audit.ActionLabelCreated, input.Request)
	event.ScopeID = id
	event.SubjectID = id
	event.Detail = map[string]any{"name": name, "color": color}

	return s.store.Create(ctx, StoreCreate{
		ID: id, RepositoryID: input.RepositoryID,
		Name: name, NameFolded: Fold(name), Color: color, Description: description,
		Event: event,
	})
}

// ListInput is a request to list a repository's labels.
type ListInput struct {
	Actor        repository.Principal
	RepositoryID string
}

// List returns a repository's labels, requiring repository read.
func (s *Service) List(ctx context.Context, input ListInput) ([]Label, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return nil, err
	}
	return s.store.List(ctx, input.RepositoryID)
}

// GetInput is a request to read one label.
type GetInput struct {
	Actor        repository.Principal
	RepositoryID string
	// Name is the label's displayed name; the lookup folds it, which is what makes a URL's case
	// irrelevant — `urls-and-identifiers.md` §6.1 folds these names for comparison.
	Name string
}

// Get returns one label by name, requiring repository read.
func (s *Service) Get(ctx context.Context, input GetInput) (Label, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return Label{}, err
	}
	return s.store.FindByName(ctx, input.RepositoryID, Fold(input.Name))
}

// UpdateInput is a request to change a label.
type UpdateInput struct {
	Actor        repository.Principal
	RepositoryID string
	// Name identifies the label, by its current name.
	Name string
	// NewName, Color, and Description are pointers so "not provided" is distinguishable from "set to
	// the empty value": a description may legitimately be cleared, and a name may not be absent.
	NewName     *string
	Color       *string
	Description *string
}

// Update changes a label's name, colour, or description.
//
// It requires repository admin, on the same derivation as `Create`.
func (s *Service) Update(ctx context.Context, input UpdateInput) (Label, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelAdmin); err != nil {
		return Label{}, err
	}
	if input.NewName == nil && input.Color == nil && input.Description == nil {
		return Label{}, ErrNoChange
	}

	existing, err := s.store.FindByName(ctx, input.RepositoryID, Fold(input.Name))
	if err != nil {
		return Label{}, err
	}

	name, folded := existing.Name, Fold(existing.Name)
	if input.NewName != nil {
		validated, err := ValidateName(*input.NewName)
		if err != nil {
			return Label{}, err
		}
		name, folded = validated, Fold(validated)
	}
	color := existing.Color
	if input.Color != nil {
		validated, err := NormalizeColor(*input.Color)
		if err != nil {
			return Label{}, err
		}
		color = validated
	}
	description := existing.Description
	if input.Description != nil {
		validated, err := ValidateDescription(*input.Description)
		if err != nil {
			return Label{}, err
		}
		description = validated
	}
	if name == existing.Name && color == existing.Color && description == existing.Description {
		return Label{}, ErrNoChange
	}

	return s.store.Update(ctx, StoreUpdate{
		RepositoryID: input.RepositoryID, LabelID: existing.ID,
		Name: name, NameFolded: folded, Color: color, Description: description,
	})
}

// DeleteInput is a request to delete a label.
type DeleteInput struct {
	Actor        repository.Principal
	RepositoryID string
	Name         string
	Request      audit.RequestContext
}

// Delete removes a label.
//
// Its attachments go with it and the issues they pointed at do not, which is the acceptance's
// "deleting a label removes its attachments without deleting issues" — enforced by the join table's
// foreign key rather than by this function remembering to clean up.
func (s *Service) Delete(ctx context.Context, input DeleteInput) error {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelAdmin); err != nil {
		return err
	}
	existing, err := s.store.FindByName(ctx, input.RepositoryID, Fold(input.Name))
	if err != nil {
		return err
	}

	event := s.event(input.Actor, audit.ActionLabelDeleted, input.Request)
	event.ScopeID = existing.ID
	event.SubjectID = existing.ID
	event.Detail = map[string]any{"name": existing.Name, "color": existing.Color}

	return s.store.Delete(ctx, StoreDelete{
		RepositoryID: input.RepositoryID, LabelID: existing.ID, Event: event,
	})
}

// IssueLabelsInput is a request for one issue's labels.
type IssueLabelsInput struct {
	Actor        repository.Principal
	RepositoryID string
	IssueID      string
}

// ForIssue returns the labels on one issue, requiring repository read.
func (s *Service) ForIssue(ctx context.Context, input IssueLabelsInput) ([]Label, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return nil, err
	}
	return s.store.ForIssue(ctx, input.IssueID)
}

// AttachInput is a request to apply a label to an issue.
type AttachInput struct {
	Actor        repository.Principal
	RepositoryID string
	IssueID      string
	// Name identifies the label by name, which is how the API and the page address it.
	Name string
}

// Attach applies a label to an issue.
//
// It requires repository **write**: applying a label changes an issue's metadata rather than the
// repository's configuration, and write is the level that already guards an issue's own fields.
//
// It reports whether the attachment was new, so a transport can answer `201` or `200` — which is what
// makes applying a label twice idempotent rather than a conflict. Idempotence is the right behaviour
// here because the operation's result is a state, not an event: "this label is on this issue".
func (s *Service) Attach(ctx context.Context, input AttachInput) (bool, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelWrite); err != nil {
		return false, err
	}
	label, err := s.store.FindByName(ctx, input.RepositoryID, Fold(input.Name))
	if err != nil {
		return false, err
	}
	return s.store.Attach(ctx, input.IssueID, label.ID)
}

// DetachInput is a request to remove a label from an issue.
type DetachInput struct {
	Actor        repository.Principal
	RepositoryID string
	IssueID      string
	Name         string
}

// Detach removes a label from an issue, requiring repository write.
//
// Removing a label the issue does not carry is refused rather than silently accepted, so a client that
// lost track of the state is told.
func (s *Service) Detach(ctx context.Context, input DetachInput) error {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelWrite); err != nil {
		return err
	}
	label, err := s.store.FindByName(ctx, input.RepositoryID, Fold(input.Name))
	if err != nil {
		return err
	}
	removed, err := s.store.Detach(ctx, input.IssueID, label.ID)
	if err != nil {
		return err
	}
	if !removed {
		return ErrNotAttached
	}
	return nil
}

// CountsInput is a request for how many issues carry each label.
type CountsInput struct {
	Actor        repository.Principal
	RepositoryID string
}

// Counts reports how many issues carry each label, requiring repository read.
func (s *Service) Counts(ctx context.Context, input CountsInput) (map[string]int, error) {
	if _, _, err := s.require(ctx, input.Actor, input.RepositoryID, repository.LevelRead); err != nil {
		return nil, err
	}
	return s.store.CountsForRepository(ctx, input.RepositoryID)
}

// require asks the repository service and translates its refusals.
//
// **The two are mapped differently, and the difference is the whole reason this function exists.** The
// repository service reports `ErrNotFound` when the caller has no access at all and `ErrForbidden` when
// the level is known but too low, so not-found stays not-found — keeping "you may not see this" and
// "this does not exist" indistinguishable, which T-A4 requires — while forbidden stays forbidden,
// because a caller whose level was resolved can already read the repository and telling them their
// level is too low discloses nothing they did not know.
//
// Collapsing the second into not-found was a real defect in the issue package, found by a refused
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

// event builds a record with the parts every label event shares.
func (s *Service) event(actor repository.Principal, action string, request audit.RequestContext) audit.Event {
	event := audit.Event{
		Action:        action,
		ActorID:       actor.UserID,
		ActorKind:     audit.ActorUser,
		ScopeType:     audit.ScopeLabel,
		SubjectType:   audit.SubjectLabel,
		SourceAddress: request.SourceAddress,
		UserAgent:     request.UserAgent,
	}
	if actor.IsInstanceAdmin {
		event.ActorKind = audit.ActorAdmin
	}
	return event
}

// newIdentifier returns an identifier for a new label.
//
// UUIDv7 rather than v4, per the data-model rule that an identifier is generated in Go as a
// time-ordered UUIDv7 so index inserts stay local.
func newIdentifier() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	return uuid.NewString()
}
