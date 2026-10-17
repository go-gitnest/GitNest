package repository

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/google/uuid"
)

// Listing bounds.
const (
	// DefaultListLimit is the page size when a caller does not choose one.
	DefaultListLimit = 30
	// MaxListLimit caps a requested page size, so one request cannot ask for an
	// unbounded result set.
	MaxListLimit = 100
)

// Service is the single authority on repository access.
type Service struct {
	store Store
	now   func() time.Time
}

// NewService wires the service to its storage.
func NewService(store Store) *Service {
	return &Service{store: store, now: time.Now}
}

// Create creates a repository owned by the actor's own account.
//
// It is private unless the caller asks for public, which is the documented
// default. A caller may only create repositories for itself in phase 1;
// organizations and a configurable creation policy arrive with their phases.
func (s *Service) Create(ctx context.Context, input CreateInput) (Repository, error) {
	if input.Actor.IsAnonymous() {
		return Repository{}, ErrAnonymous
	}
	if input.OwnerID == "" {
		return Repository{}, errors.New("a repository needs an owner")
	}
	// Only the account itself may create a repository in its own namespace.
	// An instance administrator is not exempt: creating a repository is not an
	// oversight action, and lending the operator's name to a tenant's
	// repository would make the owner field untrue.
	if input.Actor.UserID != input.OwnerID {
		return Repository{}, ErrForbidden
	}

	name := strings.TrimSpace(input.Name)
	if err := ValidateName(name); err != nil {
		return Repository{}, err
	}
	description := strings.TrimSpace(input.Description)
	if err := ValidateDescription(description); err != nil {
		return Repository{}, err
	}
	visibility, err := ParseVisibility(string(input.Visibility))
	if err != nil {
		return Repository{}, err
	}

	id := newID()
	// The record is built here, where the decision was taken, and written by the
	// store in the same transaction as the row.
	event := s.event(input.Actor, input.Request, audit.ActionRepoCreated, id)
	event.Detail = map[string]any{
		"name":       name,
		"visibility": string(visibility),
	}

	return s.store.Create(ctx, New{
		ID:          id,
		OwnerID:     input.OwnerID,
		Name:        name,
		NameFolded:  FoldName(name),
		Description: description,
		Visibility:  visibility,
		// The storage identifier is generated once, here, and never changes:
		// rename and transfer leave the bytes where they are.
		StorageID: newID(),
	}, event)
}

// event assembles an audit record for a repository operation.
func (s *Service) event(actor Principal, request audit.RequestContext, action, subjectID string) audit.Event {
	actorID, kind, address, agent := audit.Actor(actor.UserID, actor.IsInstanceAdmin, request)
	return audit.Event{
		Action:        action,
		ActorID:       actorID,
		ActorKind:     kind,
		ScopeType:     audit.ScopeRepository,
		ScopeID:       subjectID,
		SubjectType:   audit.SubjectRepository,
		SubjectID:     subjectID,
		SourceAddress: address,
		UserAgent:     agent,
	}
}

// Resolve finds a repository by owner and name and returns the caller's level on
// it.
//
// It reports ErrNotFound when the repository does not exist *and* when the
// caller may not know that it does, so the two are indistinguishable. That is
// what keeps a private repository's existence private.
//
// The lookup folds the name, so an alternate casing resolves to the same
// repository; the returned Repository carries the stored casing, which a
// transport uses to redirect to the canonical path.
func (s *Service) Resolve(ctx context.Context, viewer Principal, ownerID, name string) (Repository, Level, error) {
	if ownerID == "" || name == "" {
		return Repository{}, LevelNone, ErrNotFound
	}
	found, err := s.store.FindByOwnerAndFoldedName(ctx, ownerID, FoldName(name))
	if err != nil {
		return Repository{}, LevelNone, err
	}
	return s.decide(ctx, viewer, found)
}

// ResolveByID finds a repository by its internal identifier.
//
// An internal identifier is not a secret and not a capability: a caller who
// guesses or learns one still has to pass the same permission decision, so a
// private repository cannot be reached with it either.
func (s *Service) ResolveByID(ctx context.Context, viewer Principal, repoID string) (Repository, Level, error) {
	if repoID == "" {
		return Repository{}, LevelNone, ErrNotFound
	}
	found, err := s.store.FindByID(ctx, repoID)
	if err != nil {
		return Repository{}, LevelNone, err
	}
	return s.decide(ctx, viewer, found)
}

// Permission returns the caller's level on a repository, without reporting
// whether the repository exists.
func (s *Service) Permission(ctx context.Context, viewer Principal, repo Repository) (Level, error) {
	level, err := s.levelFor(ctx, viewer, repo)
	if err != nil {
		return LevelNone, err
	}
	return level, nil
}

// Require resolves a repository and enforces a level.
//
// It returns ErrNotFound when the caller has no access at all, so a denial never
// reveals that the repository exists, and ErrForbidden only when the caller can
// already read the repository: at that point existence is not a secret from
// them.
//
// A write against an archived repository is refused with ErrArchived, which is a
// different answer from ErrForbidden because the caller's permission is not the
// problem. The check lives here rather than in a transport so every transport
// gets it: the Git push path, the API, and the rendered pages all reach the
// repository through this package.
func (s *Service) Require(ctx context.Context, viewer Principal, repoID string, required Level) (Repository, Level, error) {
	found, level, err := s.ResolveByID(ctx, viewer, repoID)
	if err != nil {
		return Repository{}, LevelNone, err
	}
	return s.enforce(ctx, found, level, required)
}

// ResolveAndRequire resolves a repository by owner and name and enforces a level
// in one decision.
//
// It exists for the transports that hold a name rather than an identifier and
// need both the row and the decision: the Git transport needs the storage
// identifier to address the bytes, and it needs the answer to "may this caller
// do this" from the same call, so the two cannot disagree.
func (s *Service) ResolveAndRequire(ctx context.Context, viewer Principal, ownerID, name string, required Level) (Repository, Level, error) {
	found, level, err := s.Resolve(ctx, viewer, ownerID, name)
	if err != nil {
		return Repository{}, LevelNone, err
	}
	return s.enforce(ctx, found, level, required)
}

// enforce applies the level requirement and the archive rule.
//
// The archive rule refuses a requirement of exactly LevelWrite, which is what a
// content change needs: a push, and whatever content API arrives later. Read is
// unaffected, and so is admin, because an archived repository still has to be
// administrable — the owner must be able to unarchive it and to manage who may
// read it. Archiving removes the ability to change content, not the ability to
// run the repository.
func (s *Service) enforce(_ context.Context, found Repository, level Level, required Level) (Repository, Level, error) {
	if !level.Allows(required) {
		return Repository{}, level, ErrForbidden
	}
	if found.Archived && required == LevelWrite {
		return Repository{}, level, ErrArchived
	}
	return found, level, nil
}

// WriteLevel reports the requirement a content change carries.
//
// It exists so a transport asks for "the write requirement" rather than naming a
// level, which keeps the archive rule in one place: a transport that asked for
// LevelWrite by name would still be right today and would silently stop matching
// the rule if the requirement were ever expressed differently.
func WriteLevel() Level { return LevelWrite }

// decide resolves a level and collapses "no access" into not-found.
func (s *Service) decide(ctx context.Context, viewer Principal, found Repository) (Repository, Level, error) {
	level, err := s.levelFor(ctx, viewer, found)
	if err != nil {
		return Repository{}, LevelNone, err
	}
	if level == LevelNone {
		return Repository{}, LevelNone, ErrNotFound
	}
	return found, level, nil
}

// levelFor computes the caller's level on one repository. It is the only place
// that answers an access question, and every entry point above routes through
// it.
//
// The order of the checks encodes the model:
//
//  1. an instance administrator may administer any repository, which is the
//     oversight the administration document grants;
//  2. the owner holds owner level;
//  3. a direct collaborator grant holds the level its role carries;
//  4. a public repository grants read to everyone else, including anonymous
//     callers;
//  5. anything else is no access.
func (s *Service) levelFor(ctx context.Context, viewer Principal, found Repository) (Level, error) {
	if viewer.IsInstanceAdmin {
		return LevelAdmin, nil
	}
	if viewer.IsAnonymous() {
		if found.Visibility == VisibilityPublic {
			return LevelRead, nil
		}
		return LevelNone, nil
	}
	if found.OwnerID == viewer.UserID {
		return LevelOwner, nil
	}

	role, granted, err := s.store.RoleFor(ctx, found.ID, viewer.UserID)
	if err != nil {
		return LevelNone, err
	}
	if granted && role.Allows(LevelRead) {
		return role, nil
	}
	if found.Visibility == VisibilityPublic {
		return LevelRead, nil
	}
	return LevelNone, nil
}

// List returns one page of the repositories the viewer may read.
//
// Visibility is part of the query that paginates, not a filter applied
// afterwards, and no total is computed: the page size is met by asking for one
// row more than the caller requested. Both properties exist so a private
// repository can never shift a count or a page boundary
// (docs/architecture/urls-and-identifiers.md, "Filter before count").
func (s *Service) List(ctx context.Context, query ListQuery) (Page, error) {
	limit := query.Limit
	switch {
	case limit <= 0:
		limit = DefaultListLimit
	case limit > MaxListLimit:
		limit = MaxListLimit
	}
	if query.Offset < 0 {
		query.Offset = 0
	}

	// One extra row turns "is there another page" into a fact rather than a
	// count.
	query.Limit = limit + 1
	found, err := s.store.ListVisible(ctx, query)
	if err != nil {
		return Page{}, err
	}

	page := Page{Repositories: found}
	if len(found) > limit {
		page.Repositories = found[:limit]
		page.HasMore = true
	}
	if page.Repositories == nil {
		page.Repositories = []Repository{}
	}
	return page, nil
}

// Rename changes a repository's name.
//
// The owner and the storage identifier are untouched, so the repository's bytes
// stay where they are and only the metadata moves. Recording the redirect that
// lets an old remote keep working is the repository-settings workstream's
// deliverable; until then a rename does not leave one, which is safe only
// because no Git remote can reach this repository yet.
func (s *Service) Rename(ctx context.Context, input RenameInput) (Repository, error) {
	found, _, err := s.Require(ctx, input.Actor, input.RepoID, LevelAdmin)
	if err != nil {
		return Repository{}, err
	}

	name := strings.TrimSpace(input.Name)
	if err := ValidateName(name); err != nil {
		return Repository{}, err
	}
	folded := FoldName(name)
	if folded == FoldName(found.Name) {
		return found, nil
	}

	event := s.event(input.Actor, input.Request, audit.ActionRepoRenamed, found.ID)
	event.Detail = map[string]any{"name": audit.Change{From: found.Name, To: name}}
	return s.store.Rename(ctx, found.ID, name, folded, event)
}

// SetVisibility changes a repository's visibility.
//
// It requires administrator level and an explicit confirmation, because the
// transition can change who may read content that was private a moment earlier.
// Making a repository public requires the repository's own full name as the
// confirmation; making it private requires the acknowledgement word. Requiring
// more for the exposing direction than for the concealing one is deliberate: a
// mistake in one direction leaks content and in the other does not.
func (s *Service) SetVisibility(ctx context.Context, input SetVisibilityInput) (Repository, error) {
	found, _, err := s.Require(ctx, input.Actor, input.RepoID, LevelAdmin)
	if err != nil {
		return Repository{}, err
	}
	target, err := ParseVisibility(string(input.Visibility))
	if err != nil {
		return Repository{}, err
	}
	if target == found.Visibility {
		return Repository{}, ErrNoChange
	}
	if !confirmsVisibilityChange(found, target, input.Confirmation) {
		return Repository{}, ErrConfirmationRequired
	}

	event := s.event(input.Actor, input.Request, audit.ActionRepoVisibilityChanged, found.ID)
	event.Detail = map[string]any{
		"visibility": audit.Change{From: string(found.Visibility), To: string(target)},
	}
	return s.store.SetVisibility(ctx, found.ID, target, event)
}

// ConfirmationForVisibility is the value a caller must supply to make a given
// transition.
//
// It is exported so a settings surface can tell the caller what to type rather
// than leaving them to discover it from a rejection.
func ConfirmationForVisibility(found Repository, target Visibility) string {
	if target == VisibilityPublic && found.Visibility != VisibilityPublic {
		// The exposing direction: the caller types the repository's own name.
		return found.OwnerID + "/" + found.Name
	}
	return VisibilityConfirmationWord
}

// VisibilityConfirmationWord is the acknowledgement for a transition that
// reduces exposure.
const VisibilityConfirmationWord = "change-visibility"

// confirmsVisibilityChange checks a confirmation value.
//
// The comparison folds case and trims whitespace, so a caller who types the name
// correctly with different spacing is not refused for the wrong reason; it does
// not accept a prefix or a fragment, because the point of the confirmation is
// that the caller reproduced the name.
func confirmsVisibilityChange(found Repository, target Visibility, confirmation string) bool {
	expected := ConfirmationForVisibility(found, target)
	if expected == VisibilityConfirmationWord {
		return strings.EqualFold(strings.TrimSpace(confirmation), expected)
	}
	// The name form is checked against both the display name and the folded one,
	// so the confirmation is the repository's identity rather than its casing.
	provided := strings.TrimSpace(confirmation)
	if strings.EqualFold(provided, expected) {
		return true
	}
	// A caller naming only the repository name is accepted too, because the owner
	// segment is not always known to a settings form.
	return strings.EqualFold(provided, found.Name) || strings.EqualFold(provided, FoldName(found.Name))
}

// SetDefaultBranch changes a repository's default branch.
//
// The branch is validated against the Git ref rules before it is stored, and it
// must already exist: recording a branch that is not in the repository would
// make every later page address a revision that does not resolve.
func (s *Service) SetDefaultBranch(ctx context.Context, input SetDefaultBranchInput) (Repository, error) {
	found, _, err := s.Require(ctx, input.Actor, input.RepoID, LevelAdmin)
	if err != nil {
		return Repository{}, err
	}
	branch := strings.TrimSpace(input.Branch)
	if err := ValidateBranchName(branch); err != nil {
		return Repository{}, err
	}
	if branch == found.DefaultBranch {
		return Repository{}, ErrNoChange
	}

	event := s.event(input.Actor, input.Request, audit.ActionRepoDefaultBranchChanged, found.ID)
	event.Detail = map[string]any{"default_branch": audit.Change{From: found.DefaultBranch, To: branch}}
	return s.store.SetDefaultBranch(ctx, found.ID, branch, event)
}

// SetArchived archives or unarchives a repository.
//
// Archiving is idempotent in the sense that asking for the state a repository is
// already in reports ErrNoChange rather than writing a record claiming something
// happened when nothing did. Both directions need administrator level, and both
// work on an archived repository, because unarchiving is the escape hatch from
// the state itself.
func (s *Service) SetArchived(ctx context.Context, input SetArchivedInput) (Repository, error) {
	found, _, err := s.Require(ctx, input.Actor, input.RepoID, LevelAdmin)
	if err != nil {
		return Repository{}, err
	}
	if found.Archived == input.Archived {
		return Repository{}, ErrNoChange
	}

	action := audit.ActionRepoArchived
	if !input.Archived {
		action = audit.ActionRepoUnarchived
	}
	event := s.event(input.Actor, input.Request, action, found.ID)
	event.Detail = map[string]any{"archived": audit.Change{From: found.Archived, To: input.Archived}}
	return s.store.SetArchived(ctx, found.ID, input.Archived, event)
}

// GrantAccess gives a user a direct grant on a repository.
//
// The grant is exactly the granted role and carries nothing else: it confers no
// visibility on the granter's other repositories, which is what confines an
// outside collaborator (docs/glossary.md).
//
// It reports whether the grant was new. A transport answers `201` for a new grant
// and `200` for a role change, and that distinction is produced by the same
// statement that writes the row — so it cannot be a second query that races the
// first.
//
// Ownership is not grantable. It follows from the repository's owner identifier,
// and docs/glossary.md is explicit that roles are stored while permissions are
// derived, so a stored `owner` row would be duplicated truth. Moving ownership
// is a transfer, which phase 2 owns.
func (s *Service) GrantAccess(ctx context.Context, input GrantInput) (bool, error) {
	if input.Role > LevelAdmin {
		return false, errors.New("ownership cannot be granted; it follows from the repository's owner")
	}
	if !input.Role.Allows(LevelRead) {
		return false, errors.New("a grant must carry at least read")
	}
	if input.UserID == "" {
		return false, errors.New("a grant needs an account")
	}
	found, _, err := s.Require(ctx, input.Actor, input.RepoID, LevelAdmin)
	if err != nil {
		return false, err
	}

	// Both records are built here and the store picks one, because only the
	// insert knows whether the row was already there and it has to decide inside
	// the same transaction that writes the grant.
	added := s.collaboratorEvent(input.Actor, input.Request, audit.ActionRepoCollaboratorAdded, found.ID, input.UserID)
	added.Detail = map[string]any{"role": audit.Change{From: "", To: input.Role.String()}}
	changed := s.collaboratorEvent(input.Actor, input.Request, audit.ActionRepoCollaboratorRoleChanged, found.ID, input.UserID)
	changed.Detail = map[string]any{"role": audit.Change{From: "previous", To: input.Role.String()}}

	return s.store.SetCollaborator(ctx, found.ID, input.UserID, input.Role, GrantEvents{
		Added: added, Changed: changed,
	})
}

// RevokeAccess removes a direct grant.
func (s *Service) RevokeAccess(ctx context.Context, input RevokeInput) error {
	found, _, err := s.Require(ctx, input.Actor, input.RepoID, LevelAdmin)
	if err != nil {
		return err
	}
	event := s.collaboratorEvent(input.Actor, input.Request, audit.ActionRepoCollaboratorRemoved, found.ID, input.UserID)
	return s.store.RemoveCollaborator(ctx, found.ID, input.UserID, event)
}

// collaboratorEvent assembles a record whose subject is one account's grant.
//
// The scope is the repository and the subject is the grant, so a query for a
// repository's history and a query for one collaborator's history both find it.
func (s *Service) collaboratorEvent(actor Principal, request audit.RequestContext, action, repoID, userID string) audit.Event {
	event := s.event(actor, request, action, repoID)
	event.SubjectType = audit.SubjectCollaborator
	event.SubjectID = userID
	return event
}

// Collaborators lists a repository's direct grants, for an admin or above.
func (s *Service) Collaborators(ctx context.Context, actor Principal, repoID string) ([]Collaborator, error) {
	if _, _, err := s.Require(ctx, actor, repoID, LevelAdmin); err != nil {
		return nil, err
	}
	return s.store.Collaborators(ctx, repoID)
}

// newID returns the identifier for a new row.
//
// A UUIDv7 is time-ordered, which keeps index inserts local, and PostgreSQL
// stores it natively as uuid (docs/architecture/data-model.md).
func newID() string {
	if id, err := uuid.NewV7(); err == nil {
		return id.String()
	}
	// A random v4 is a fine fallback: it loses index locality, not correctness.
	return uuid.NewString()
}
