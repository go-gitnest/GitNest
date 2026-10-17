// Package repository owns repository metadata and answers every access question
// about a repository.
//
// It is the single authority the architecture requires: the browser, the REST
// API, and every Git transport call this package rather than deciding access
// themselves (docs/architecture.md, "one authorization decision function for
// every principal kind"). A transport that inspects a role, a visibility value,
// or a token scope to decide something is a defect.
package repository

import (
	"context"
	"errors"
	"time"

	"github.com/go-gitnest/GitNest/internal/audit"
)

// Visibility is who may read a repository without a grant.
type Visibility string

const (
	// VisibilityPrivate is readable only by the owner and direct collaborators.
	VisibilityPrivate Visibility = "private"
	// VisibilityPublic is readable by anyone, including anonymous callers.
	VisibilityPublic Visibility = "public"
)

// Level is a resolved permission. The order matters: a higher level implies
// every lower one.
type Level int

const (
	// LevelNone is no access at all. It is also what an unauthorized caller
	// receives instead of an explanation.
	LevelNone Level = iota
	// LevelRead may read content and metadata.
	LevelRead
	// LevelWrite may push commits and change content.
	LevelWrite
	// LevelAdmin may change settings, visibility, and collaborators.
	LevelAdmin
	// LevelOwner is the account that owns the repository. It carries everything
	// admin carries plus ownership-only operations such as transfer.
	LevelOwner
)

// String renders a level for logs and error messages.
func (l Level) String() string {
	switch l {
	case LevelRead:
		return "read"
	case LevelWrite:
		return "write"
	case LevelAdmin:
		return "admin"
	case LevelOwner:
		return "owner"
	default:
		return "none"
	}
}

// Allows reports whether a resolved level satisfies a requirement.
func (l Level) Allows(required Level) bool {
	return l >= required
}

// PrincipalKind distinguishes the kinds of caller that may act on a repository.
//
// Phase 1 has two kinds. Personal access tokens, fine-grained tokens, OAuth
// tokens, app installation tokens, deploy keys, workflow tokens, runners, and
// workspaces join this enum in later phases, and each is expected to reach the
// same decision function rather than a parallel one
// (docs/security/threat-model.md, R1).
type PrincipalKind string

const (
	// KindAnonymous is a caller with no credential.
	KindAnonymous PrincipalKind = "anonymous"
	// KindUser is an authenticated account.
	KindUser PrincipalKind = "user"
)

// Principal is who is asking.
//
// It carries identity only. It deliberately does not carry a role, a scope, or
// a pre-computed permission, because those are what this package derives; a
// principal that arrived with its own answer would be the parallel decision
// path the architecture forbids.
type Principal struct {
	Kind PrincipalKind
	// UserID is set for KindUser and empty otherwise.
	UserID string
	// IsInstanceAdmin marks an operator account. The administration document
	// grants instance administrators repository oversight (list, metadata,
	// visibility, quarantine), so they resolve to at least admin on every
	// repository. They are not the owner, so an ownership-only operation still
	// refuses them.
	IsInstanceAdmin bool
}

// Anonymous is the principal of a caller with no credential.
func Anonymous() Principal {
	return Principal{Kind: KindAnonymous}
}

// User returns the principal of an authenticated account.
func User(id string, isInstanceAdmin bool) Principal {
	return Principal{Kind: KindUser, UserID: id, IsInstanceAdmin: isInstanceAdmin}
}

// IsAnonymous reports whether the principal has no credential.
func (p Principal) IsAnonymous() bool {
	return p.Kind != KindUser || p.UserID == ""
}

// Repository is repository metadata.
//
// It carries no permission of its own: the caller's level is computed by the
// service for a specific principal, so a repository value cannot be reused
// across callers with a stale grant attached.
type Repository struct {
	ID            string
	OwnerID       string
	Name          string
	Description   string
	Visibility    Visibility
	DefaultBranch string
	// StorageID addresses the repository's bytes. It is immutable: a rename and
	// a transfer both leave it unchanged, which is why the storage location
	// does not move when the name does.
	StorageID string
	// Archived marks a repository that refuses content changes. Its name, its
	// visibility, and its access remain administrable, because the owner has to
	// be able to unarchive it.
	Archived  bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// New is a repository prepared for insertion.
type New struct {
	ID          string
	OwnerID     string
	Name        string
	NameFolded  string
	Description string
	Visibility  Visibility
	StorageID   string
}

// CreateInput is a caller's request to create a repository, before validation.
type CreateInput struct {
	Actor       Principal
	OwnerID     string
	Name        string
	Description string
	Visibility  Visibility
	Request     audit.RequestContext
}

// RenameInput is a caller's request to rename a repository.
type RenameInput struct {
	Actor   Principal
	RepoID  string
	Name    string
	Request audit.RequestContext
}

// SetVisibilityInput is a caller's request to change a repository's visibility.
//
// Confirmation is a separate value rather than a boolean so the caller has to
// state what it is confirming. Making a repository public is the transition that
// can expose content that was private a moment earlier, so it requires the
// repository's own full name as the confirmation; the other direction requires
// only the word the caller was asked for. The rule is recorded in the roadmap
// because the documents require "the confirmation each transition requires"
// without enumerating them.
type SetVisibilityInput struct {
	Actor        Principal
	RepoID       string
	Visibility   Visibility
	Confirmation string
	Request      audit.RequestContext
}

// SetDefaultBranchInput is a caller's request to change the default branch.
type SetDefaultBranchInput struct {
	Actor   Principal
	RepoID  string
	Branch  string
	Request audit.RequestContext
}

// SetArchivedInput is a caller's request to archive or unarchive a repository.
type SetArchivedInput struct {
	Actor    Principal
	RepoID   string
	Archived bool
	Request  audit.RequestContext
}

// GrantInput is a caller's request to give an account a direct grant.
type GrantInput struct {
	Actor   Principal
	RepoID  string
	UserID  string
	Role    Level
	Request audit.RequestContext
}

// RevokeInput is a caller's request to remove a direct grant.
type RevokeInput struct {
	Actor   Principal
	RepoID  string
	UserID  string
	Request audit.RequestContext
}

// GrantEvents carries the two records a grant can produce.
//
// The store decides which applies, because only it knows whether the row already
// existed, and it makes that decision inside the transaction that writes both the
// grant and the record. Passing both keeps the choice next to the SQL that
// answers it instead of racing a read against it.
type GrantEvents struct {
	Added   audit.Event
	Changed audit.Event
}

// ListQuery filters a repository listing.//
// It carries the viewer rather than a pre-filtered owner, because the
// visibility filter has to be part of the same query that paginates. Filtering
// after fetching, or computing a total before filtering, is how a private
// repository's existence leaks through a count
// (docs/architecture/urls-and-identifiers.md, "Filter before count").
type ListQuery struct {
	Viewer Principal
	// OwnerID, when set, restricts the list to one owner's repositories. The
	// visibility filter still applies.
	OwnerID string
	// Limit is the page size. The service asks the store for one row more than
	// this, so it can report whether another page exists without ever computing
	// a total.
	Limit  int
	Offset int
}

// Page is one page of a listing.
type Page struct {
	Repositories []Repository
	// HasMore reports whether at least one more visible repository exists. It
	// is derived from an extra row, not from a count, so no total that could
	// include hidden repositories is ever computed.
	HasMore bool
}

// Collaborator is one direct grant.
type Collaborator struct {
	UserID string
	Role   Level
}

var (
	// ErrNotFound reports that a repository does not exist, or that it exists
	// but the caller may not know that. The two cases are indistinguishable on
	// purpose: it is what keeps a private repository's existence private.
	ErrNotFound = errors.New("repository not found")
	// ErrForbidden reports an authenticated caller that lacks the required
	// level on a repository it can otherwise see. It is returned only when the
	// caller already has read access, so it never discloses existence.
	ErrForbidden = errors.New("you do not have permission to do this")
	// ErrAnonymous reports an action that requires an account.
	ErrAnonymous = errors.New("this action requires an account")
	// ErrNameTaken reports that the owner already has a repository with this
	// name, compared on the folded form.
	ErrNameTaken = errors.New("this owner already has a repository with that name")
	// ErrSchemaNotMigrated reports that the repository tables are missing.
	ErrSchemaNotMigrated = errors.New("database schema is not migrated")
	// ErrArchived reports a write attempted against an archived repository.
	// It is distinct from ErrForbidden because the caller's permission is not
	// the problem: the repository's state is, and the message says so.
	ErrArchived = errors.New("this repository is archived and read-only")
	// ErrConfirmationRequired reports a consequential change submitted without
	// the confirmation it requires.
	ErrConfirmationRequired = errors.New("this change requires an explicit confirmation")
	// ErrNoChange reports an operation that would not change anything.
	ErrNoChange = errors.New("the repository already has that value")
)

// Store persists repositories and their collaborator grants.
//
// Every method that changes something takes the audit record to write with it.
// docs/reference/events.md is explicit that a failed audit write fails the
// operation, so the change and its record share one transaction; a method that
// took only the change would force the record to be written afterwards, which is
// the design that document forbids.
type Store interface {
	// Create inserts a repository, reporting ErrNameTaken when the owner
	// already has a repository whose folded name matches.
	Create(ctx context.Context, newRepository New, event audit.Event) (Repository, error)

	// FindByID returns one repository by its internal identifier, or
	// ErrNotFound. The caller still applies the permission decision.
	FindByID(ctx context.Context, id string) (Repository, error)

	// FindByOwnerAndFoldedName resolves a repository from an owner and the
	// folded form of a name. It returns the stored casing in Repository.Name so
	// a transport can redirect an alternate casing to the canonical one.
	FindByOwnerAndFoldedName(ctx context.Context, ownerID, nameFolded string) (Repository, error)

	// RoleFor returns the direct grant a user holds on a repository. The
	// boolean reports whether a grant exists at all, so the caller can tell "no
	// grant" from "a grant that grants nothing".
	RoleFor(ctx context.Context, repoID, userID string) (Level, bool, error)

	// Rename changes the display and folded name, leaving owner_id and
	// storage_id untouched, and records the change.
	Rename(ctx context.Context, repoID, name, nameFolded string, event audit.Event) (Repository, error)

	// SetVisibility changes the visibility and records the change.
	SetVisibility(ctx context.Context, repoID string, visibility Visibility, event audit.Event) (Repository, error)

	// SetDefaultBranch changes the default branch and records the change.
	SetDefaultBranch(ctx context.Context, repoID, branch string, event audit.Event) (Repository, error)

	// SetArchived sets or clears the archive flag and records the change.
	SetArchived(ctx context.Context, repoID string, archived bool, event audit.Event) (Repository, error)

	// ListVisible returns at most query.Limit repositories the viewer may read,
	// filtered before it paginates. It fetches one row beyond the limit so the
	// caller can report HasMore without a count.
	ListVisible(ctx context.Context, query ListQuery) ([]Repository, error)

	// SetCollaborator adds or updates a direct grant and records it, using
	// events.Added when the grant is new and events.Changed when it already
	// existed. It reports which of the two applied.
	SetCollaborator(ctx context.Context, repoID, userID string, role Level, events GrantEvents) (bool, error)

	// RemoveCollaborator revokes a direct grant, reporting ErrNotFound when
	// there was none, and records the revocation.
	RemoveCollaborator(ctx context.Context, repoID, userID string, event audit.Event) error

	// Collaborators lists a repository's direct grants.
	Collaborators(ctx context.Context, repoID string) ([]Collaborator, error)
}
