package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// settingsWorld is a schema with a service whose owner holds administrator level.
type settingsWorld struct {
	service *repository.Service
	reader  *audit.Reader
	owner   repository.Principal
	ownerID string
	otherID string
}

func newSettingsWorld(t *testing.T) *settingsWorld {
	t.Helper()
	schema := postgrestest.NewSchema(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	accounts := identity.NewService(identity.NewPostgresStore(schema.Client.DB), identity.Argon2idHasher{})
	owner, err := accounts.CreateInitialAdmin(ctx, identity.CreateInitialAdminInput{
		Username: "owner", Email: "owner@example.com", Password: endToEndPassword,
	})
	if err != nil {
		t.Fatalf("create the owner: %v", err)
	}

	// A second account that can be granted access, created through the invitation
	// path so it is a real account rather than a bare row.
	_, token, err := accounts.CreateInvitation(ctx, identity.CreateInvitationInput{
		ActorID: owner.ID, Email: "other@example.com",
	})
	if err != nil {
		t.Fatalf("invite the second account: %v", err)
	}
	other, err := accounts.AcceptInvitation(ctx, identity.AcceptInvitationInput{
		Token: token, Username: "other", Password: endToEndPassword,
	})
	if err != nil {
		t.Fatalf("accept the invitation: %v", err)
	}

	return &settingsWorld{
		service: repository.NewService(repository.NewPostgresStore(schema.Client.DB)),
		reader:  audit.NewReader(schema.Client.DB),
		owner:   repository.User(owner.ID, false),
		ownerID: owner.ID,
		otherID: other.ID,
	}
}

// TestArchivedRepositoryRefusesAPushThroughTheRealService covers the acceptance
// criterion at the layer every transport shares: the Git push path, the API, and
// the rendered pages all reach the repository through this decision, so archiving
// it read-only is asserted here rather than three times.
func TestArchivedRepositoryRefusesAPushThroughTheRealService(t *testing.T) {
	w := newSettingsWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	created, err := w.service.Create(ctx, repository.CreateInput{
		Actor: w.owner, OwnerID: w.ownerID, Name: "pushable", Visibility: repository.VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The write requirement is satisfiable before archiving.
	if _, _, err := w.service.ResolveAndRequire(ctx, w.owner, w.ownerID, "pushable", repository.LevelWrite); err != nil {
		t.Fatalf("a write before archiving = %v, want it to succeed", err)
	}

	if _, err := w.service.SetArchived(ctx, repository.SetArchivedInput{
		Actor: w.owner, RepoID: created.ID, Archived: true,
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// After archiving the same call is refused, and the refusal is the archive
	// rather than a permission failure.
	_, _, err = w.service.ResolveAndRequire(ctx, w.owner, w.ownerID, "pushable", repository.LevelWrite)
	if !errors.Is(err, repository.ErrArchived) {
		t.Fatalf("a write while archived = %v, want ErrArchived", err)
	}
	// A fetch is unaffected, so the repository remains readable.
	if _, _, err := w.service.ResolveAndRequire(ctx, repository.Anonymous(), w.ownerID, "pushable", repository.LevelRead); err != nil {
		t.Fatalf("an anonymous fetch while archived = %v, want it to succeed", err)
	}
	// And the owner can still manage it, which is what makes unarchiving possible.
	if _, err := w.service.SetArchived(ctx, repository.SetArchivedInput{
		Actor: w.owner, RepoID: created.ID, Archived: false,
	}); err != nil {
		t.Fatalf("unarchive: %v", err)
	}
	if _, _, err := w.service.ResolveAndRequire(ctx, w.owner, w.ownerID, "pushable", repository.LevelWrite); err != nil {
		t.Fatalf("a write after unarchiving = %v, want it to succeed", err)
	}
}

// TestVisibilityChangeIsAudited is the acceptance criterion "visibility changes
// are audited", checked against the ledger the API would query.
func TestVisibilityChangeIsAudited(t *testing.T) {
	w := newSettingsWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	created, err := w.service.Create(ctx, repository.CreateInput{
		Actor: w.owner, OwnerID: w.ownerID, Name: "audited", Visibility: repository.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := w.service.SetVisibility(ctx, repository.SetVisibilityInput{
		Actor: w.owner, RepoID: created.ID,
		Visibility: repository.VisibilityPublic, Confirmation: "audited",
		Request: audit.RequestContext{SourceAddress: "192.0.2.10", UserAgent: "app-test"},
	}); err != nil {
		t.Fatalf("SetVisibility: %v", err)
	}

	entries, err := w.reader.ForSubject(ctx, audit.SubjectRepository, created.ID)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}

	var found bool
	for _, entry := range entries {
		if entry.Action != audit.ActionRepoVisibilityChanged {
			continue
		}
		found = true
		// The record has to answer "who, from where, and what changed".
		if entry.ActorID != w.ownerID {
			t.Errorf("the record names actor %q, want the owner", entry.ActorID)
		}
		if entry.SourceAddress != "192.0.2.10" {
			t.Errorf("the record lost the source address: %+v", entry)
		}
		transition, ok := entry.Detail["visibility"].(map[string]any)
		if !ok || transition["to"] != "public" {
			t.Errorf("the record does not describe the transition: %v", entry.Detail)
		}
	}
	if !found {
		t.Fatalf("the visibility change was not recorded: %+v", entries)
	}
}

// TestRevokedCollaboratorLosesAccessOnTheNextRequest is the acceptance criterion
// "the effect is visible on the next request", asserted through the same decision
// the browser, the API, and the Git transport use.
func TestRevokedCollaboratorLosesAccessOnTheNextRequest(t *testing.T) {
	w := newSettingsWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	created, err := w.service.Create(ctx, repository.CreateInput{
		Actor: w.owner, OwnerID: w.ownerID, Name: "shared", Visibility: repository.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	collaborator := repository.User(w.otherID, false)

	// Before the grant the repository is invisible.
	if _, _, err := w.service.ResolveAndRequire(ctx, collaborator, w.ownerID, "shared", repository.LevelRead); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("before granting = %v, want ErrNotFound", err)
	}

	if _, err := w.service.GrantAccess(ctx, repository.GrantInput{
		Actor: w.owner, RepoID: created.ID, UserID: w.otherID, Role: repository.LevelWrite,
	}); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, level, err := w.service.ResolveAndRequire(ctx, collaborator, w.ownerID, "shared", repository.LevelWrite); err != nil || level != repository.LevelWrite {
		t.Fatalf("after granting = %v, %s; want write", err, level)
	}

	if err := w.service.RevokeAccess(ctx, repository.RevokeInput{
		Actor: w.owner, RepoID: created.ID, UserID: w.otherID,
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	// The very next request is refused, and refused as not-found rather than as a
	// permission error, so the revocation does not confirm the repository exists.
	if _, _, err := w.service.ResolveAndRequire(ctx, collaborator, w.ownerID, "shared", repository.LevelRead); !errors.Is(err, repository.ErrNotFound) {
		t.Fatalf("after revoking = %v, want ErrNotFound", err)
	}

	// The whole lifecycle is on the record.
	entries, err := w.reader.ForSubject(ctx, audit.SubjectCollaborator, w.otherID)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	actions := make([]string, 0, len(entries))
	for _, entry := range entries {
		actions = append(actions, entry.Action)
	}
	for _, want := range []string{audit.ActionRepoCollaboratorAdded, audit.ActionRepoCollaboratorRemoved} {
		if !containsActionName(actions, want) {
			t.Errorf("the ledger is missing %q: %v", want, actions)
		}
	}
}

// TestVisibilityConfirmationIsRequiredThroughTheService keeps the rule reachable
// from the transport boundary rather than only from a unit test.
func TestVisibilityConfirmationIsRequiredThroughTheService(t *testing.T) {
	w := newSettingsWorld(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	created, err := w.service.Create(ctx, repository.CreateInput{
		Actor: w.owner, OwnerID: w.ownerID, Name: "guarded", Visibility: repository.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Without the confirmation the exposing transition is refused, and nothing is
	// recorded, so a rejected attempt leaves no trace of a change that did not
	// happen.
	before, err := w.reader.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if _, err := w.service.SetVisibility(ctx, repository.SetVisibilityInput{
		Actor: w.owner, RepoID: created.ID, Visibility: repository.VisibilityPublic,
	}); !errors.Is(err, repository.ErrConfirmationRequired) {
		t.Fatalf("an unconfirmed change = %v, want ErrConfirmationRequired", err)
	}
	after, err := w.reader.Count(ctx)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if after != before {
		t.Errorf("a refused change produced %d records", after-before)
	}
	// The repository is unchanged.
	row, _, err := w.service.Resolve(ctx, w.owner, w.ownerID, "guarded")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if row.Visibility != repository.VisibilityPrivate {
		t.Errorf("visibility = %q, want it unchanged", row.Visibility)
	}
}

func containsActionName(actions []string, wanted string) bool {
	for _, action := range actions {
		if action == wanted {
			return true
		}
	}
	return false
}
