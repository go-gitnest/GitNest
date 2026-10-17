package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/google/uuid"
)

// auditWorld is a store over a migrated schema with an owner and a second
// account, plus a reader for the ledger.
type auditWorld struct {
	service *repository.Service
	db      *sql.DB
	reader  *audit.Reader
	owner   repository.Principal
	ownerID string
	otherID string
}

func newAuditWorld(t *testing.T) *auditWorld {
	t.Helper()
	schema := postgrestest.NewSchema(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	accounts := identity.NewService(identity.NewPostgresStore(schema.Client.DB), identity.Argon2idHasher{})
	owner, err := accounts.CreateInitialAdmin(ctx, identity.CreateInitialAdminInput{
		Username: "owner", Email: "owner@example.com", Password: "correct horse battery staple",
	})
	if err != nil {
		t.Fatalf("create the owner: %v", err)
	}
	otherID := uuid.NewString()
	if _, err := schema.Client.DB.ExecContext(ctx, `
		INSERT INTO users (id, username, username_folded, email, password_hash)
		VALUES ($1, 'other', 'other', 'other@example.com', 'placeholder')`, otherID); err != nil {
		t.Fatalf("create the second account: %v", err)
	}

	return &auditWorld{
		service: repository.NewService(repository.NewPostgresStore(schema.Client.DB)),
		db:      schema.Client.DB,
		reader:  audit.NewReader(schema.Client.DB),
		owner:   repository.User(owner.ID, false),
		ownerID: owner.ID,
		otherID: otherID,
	}
}

// create makes a repository and returns it.
func (w *auditWorld) create(t *testing.T, name string, visibility repository.Visibility) repository.Repository {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	created, err := w.service.Create(ctx, repository.CreateInput{
		Actor: w.owner, OwnerID: w.ownerID, Name: name, Visibility: visibility,
	})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return created
}

// trail returns the records about one repository.
func (w *auditWorld) trail(t *testing.T, subjectType, subjectID string) []audit.Entry {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	entries, err := w.reader.ForSubject(ctx, subjectType, subjectID)
	if err != nil {
		t.Fatalf("read the trail: %v", err)
	}
	return entries
}

// actionsOf lists the actions in a trail, for a readable assertion.
func actionsOf(entries []audit.Entry) []string {
	actions := make([]string, 0, len(entries))
	for _, entry := range entries {
		actions = append(actions, entry.Action)
	}
	return actions
}

// TestCreationWritesItsRecordInTheSameTransaction is the rule from events.md:
// "a failed audit write fails the operation".
func TestCreationWritesItsRecordInTheSameTransaction(t *testing.T) {
	w := newAuditWorld(t)
	created := w.create(t, "recorded-repo", repository.VisibilityPrivate)

	entries := w.trail(t, audit.SubjectRepository, created.ID)
	if len(entries) != 1 {
		t.Fatalf("the creation produced %d records, want exactly one", len(entries))
	}
	entry := entries[0]
	if entry.Action != audit.ActionRepoCreated {
		t.Errorf("action = %q, want %q", entry.Action, audit.ActionRepoCreated)
	}
	if entry.ActorID != w.ownerID || entry.ActorKind != string(audit.ActorUser) {
		t.Errorf("actor = %q/%q, want the creating account", entry.ActorID, entry.ActorKind)
	}
	if entry.ScopeType != string(audit.ScopeRepository) || entry.ScopeID != created.ID {
		t.Errorf("scope = %q/%q, want the repository", entry.ScopeType, entry.ScopeID)
	}
	if entry.Detail["name"] != "recorded-repo" {
		t.Errorf("detail = %v, want the name", entry.Detail)
	}
}

// TestAFailedChangeLeavesNoRecord is the other half of the same rule: a change
// that never happened must not be recorded, and a record must not survive a
// rolled-back change.
func TestAFailedChangeLeavesNoRecord(t *testing.T) {
	w := newAuditWorld(t)
	w.create(t, "first-repo", repository.VisibilityPrivate)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	before, err := w.reader.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}

	// A duplicate name violates the unique constraint, so the transaction rolls
	// back after the insert fails.
	if _, err := w.service.Create(ctx, repository.CreateInput{
		Actor: w.owner, OwnerID: w.ownerID, Name: "FIRST-REPO",
	}); !errors.Is(err, repository.ErrNameTaken) {
		t.Fatalf("duplicate create = %v, want ErrNameTaken", err)
	}

	after, err := w.reader.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if after != before {
		t.Errorf("the ledger grew from %d to %d records for a change that failed", before, after)
	}
}

// TestTheLedgerRefusesAnUpdate is the append-only guarantee, asserted against
// the database rather than restated as a comment.
func TestTheLedgerRefusesAnUpdate(t *testing.T) {
	w := newAuditWorld(t)
	created := w.create(t, "immutable-repo", repository.VisibilityPrivate)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := w.reader.UpdateIsRefused(ctx, audit.ActionRepoCreated); err != nil {
		t.Fatalf("the ledger allowed an update: %v", err)
	}

	// The record is unchanged, so the refused update did not half-apply: the
	// creation is still there under its own action and still carries its detail.
	entries := w.trail(t, audit.SubjectRepository, created.ID)
	if len(entries) != 1 || entries[0].Action != audit.ActionRepoCreated {
		t.Fatalf("the creation record was altered: %+v", entries)
	}
	if entries[0].Detail["name"] != "immutable-repo" {
		t.Errorf("the detail was altered: %v", entries[0].Detail)
	}
}

// TestEverySettingsOperationRecordsItsAction covers the catalogue: each phase-1
// operation writes the action the document names.
func TestEverySettingsOperationRecordsItsAction(t *testing.T) {
	w := newAuditWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	created := w.create(t, "settings-repo", repository.VisibilityPrivate)

	if _, err := w.service.SetVisibility(ctx, repository.SetVisibilityInput{
		Actor: w.owner, RepoID: created.ID,
		Visibility: repository.VisibilityPublic, Confirmation: "settings-repo",
		Request: audit.RequestContext{SourceAddress: "198.51.100.4", UserAgent: "settings-test"},
	}); err != nil {
		t.Fatalf("SetVisibility: %v", err)
	}
	if _, err := w.service.SetDefaultBranch(ctx, repository.SetDefaultBranchInput{
		Actor: w.owner, RepoID: created.ID, Branch: "develop",
	}); err != nil {
		t.Fatalf("SetDefaultBranch: %v", err)
	}
	if _, err := w.service.Rename(ctx, repository.RenameInput{
		Actor: w.owner, RepoID: created.ID, Name: "renamed-repo",
	}); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := w.service.SetArchived(ctx, repository.SetArchivedInput{
		Actor: w.owner, RepoID: created.ID, Archived: true,
	}); err != nil {
		t.Fatalf("SetArchived(true): %v", err)
	}
	if _, err := w.service.SetArchived(ctx, repository.SetArchivedInput{
		Actor: w.owner, RepoID: created.ID, Archived: false,
	}); err != nil {
		t.Fatalf("SetArchived(false): %v", err)
	}

	if _, err := w.service.GrantAccess(ctx, repository.GrantInput{
		Actor: w.owner, RepoID: created.ID, UserID: w.otherID, Role: repository.LevelRead,
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := w.service.GrantAccess(ctx, repository.GrantInput{
		Actor: w.owner, RepoID: created.ID, UserID: w.otherID, Role: repository.LevelWrite,
	}); err != nil {
		t.Fatalf("regrant: %v", err)
	}
	if err := w.service.RevokeAccess(ctx, repository.RevokeInput{
		Actor: w.owner, RepoID: created.ID, UserID: w.otherID,
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	actions := actionsOf(w.trail(t, audit.SubjectRepository, created.ID))
	for _, want := range []string{
		audit.ActionRepoCreated,
		audit.ActionRepoVisibilityChanged,
		audit.ActionRepoDefaultBranchChanged,
		audit.ActionRepoRenamed,
		audit.ActionRepoArchived,
		audit.ActionRepoUnarchived,
	} {
		if !containsAction(actions, want) {
			t.Errorf("the trail is missing %q: %v", want, actions)
		}
	}

	// The grants are records about the collaborator, not the repository, which is
	// the "one change, one record, each naming one subject" rule.
	grantActions := actionsOf(w.trail(t, audit.SubjectCollaborator, w.otherID))
	for _, want := range []string{
		audit.ActionRepoCollaboratorAdded,
		audit.ActionRepoCollaboratorRoleChanged,
		audit.ActionRepoCollaboratorRemoved,
	} {
		if !containsAction(grantActions, want) {
			t.Errorf("the collaborator trail is missing %q: %v", want, grantActions)
		}
	}
}

// TestVisibilityChangeStoresTheRequestContext covers the fields an operator
// needs to answer "who did this, from where".
func TestVisibilityChangeStoresTheRequestContext(t *testing.T) {
	w := newAuditWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	created := w.create(t, "context-repo", repository.VisibilityPrivate)
	if _, err := w.service.SetVisibility(ctx, repository.SetVisibilityInput{
		Actor: w.owner, RepoID: created.ID,
		Visibility: repository.VisibilityPublic, Confirmation: "context-repo",
		Request: audit.RequestContext{SourceAddress: "203.0.113.9", UserAgent: "a-browser/1.0"},
	}); err != nil {
		t.Fatalf("SetVisibility: %v", err)
	}

	var found *audit.Entry
	for _, entry := range w.trail(t, audit.SubjectRepository, created.ID) {
		if entry.Action == audit.ActionRepoVisibilityChanged {
			candidate := entry
			found = &candidate
		}
	}
	if found == nil {
		t.Fatal("the visibility change was not recorded")
	}
	if found.SourceAddress != "203.0.113.9" || found.UserAgent != "a-browser/1.0" {
		t.Errorf("the stored context is %q / %q, want what the request carried",
			found.SourceAddress, found.UserAgent)
	}
	// The detail records the transition, which is what makes the record
	// answerable later.
	visibility, ok := found.Detail["visibility"].(map[string]any)
	if !ok {
		t.Fatalf("the detail does not carry the transition: %v", found.Detail)
	}
	if visibility["from"] != "private" || visibility["to"] != "public" {
		t.Errorf("the transition = %v, want private to public", visibility)
	}
}

// TestAnInstanceAdministratorIsRecordedAsSuch keeps the actor kind meaningful.
func TestAnInstanceAdministratorIsRecordedAsSuch(t *testing.T) {
	w := newAuditWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// The owner is the bootstrap administrator, so the principal built from that
	// account carries the flag.
	admin := repository.User(w.ownerID, true)
	created, err := w.service.Create(ctx, repository.CreateInput{
		Actor: admin, OwnerID: w.ownerID, Name: "admin-repo",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	entries := w.trail(t, audit.SubjectRepository, created.ID)
	if len(entries) != 1 {
		t.Fatalf("the creation produced %d records", len(entries))
	}
	if entries[0].ActorKind != string(audit.ActorAdmin) {
		t.Errorf("actor kind = %q, want %q", entries[0].ActorKind, audit.ActorAdmin)
	}
}

// TestArchivedRepositoryRefusesAWriteThroughTheRealStore covers the archive rule
// against the database, which is where the flag lives.
func TestArchivedRepositoryRefusesAWriteThroughTheRealStore(t *testing.T) {
	w := newAuditWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	created := w.create(t, "archive-repo", repository.VisibilityPublic)
	if _, err := w.service.SetArchived(ctx, repository.SetArchivedInput{
		Actor: w.owner, RepoID: created.ID, Archived: true,
	}); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}

	if _, _, err := w.service.Require(ctx, w.owner, created.ID, repository.LevelWrite); !errors.Is(err, repository.ErrArchived) {
		t.Errorf("a write = %v, want ErrArchived", err)
	}
	if _, _, err := w.service.Require(ctx, w.owner, created.ID, repository.LevelRead); err != nil {
		t.Errorf("a read = %v, want it to succeed", err)
	}
	if _, _, err := w.service.Require(ctx, w.owner, created.ID, repository.LevelAdmin); err != nil {
		t.Errorf("an administrative operation = %v, want it to succeed", err)
	}
	// The name-based path the Git transport uses agrees.
	if _, _, err := w.service.ResolveAndRequire(ctx, w.owner, w.ownerID, "archive-repo", repository.LevelWrite); !errors.Is(err, repository.ErrArchived) {
		t.Errorf("the name path = %v, want ErrArchived", err)
	}
}

// TestSchemaWithoutTheLedgerIsReported covers the startup check: a schema whose
// ledger is missing must be reported rather than crashing on the first change.
func TestSchemaWithoutTheLedgerIsReported(t *testing.T) {
	schema := postgrestest.NewSchema(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Drop the ledger, leaving the repository tables in place.
	if _, err := schema.Client.DB.ExecContext(ctx, "DROP TABLE audit_events"); err != nil {
		t.Fatalf("drop the ledger: %v", err)
	}

	service := repository.NewService(repository.NewPostgresStore(schema.Client.DB))
	ownerID := uuid.NewString()
	if _, err := schema.Client.DB.ExecContext(ctx, `
		INSERT INTO users (id, username, username_folded, email, password_hash)
		VALUES ($1, 'owner', 'owner', 'owner@example.com', 'placeholder')`, ownerID); err != nil {
		t.Fatalf("create the account: %v", err)
	}

	if _, err := service.Create(ctx, repository.CreateInput{
		Actor: repository.User(ownerID, false), OwnerID: ownerID, Name: "repo",
	}); !errors.Is(err, repository.ErrSchemaNotMigrated) {
		t.Fatalf("Create = %v, want ErrSchemaNotMigrated", err)
	}
}

func containsAction(actions []string, wanted string) bool {
	for _, action := range actions {
		if action == wanted {
			return true
		}
	}
	return false
}
