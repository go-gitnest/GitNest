package repository

import (
	"context"
	"errors"
	"testing"

	"github.com/go-gitnest/GitNest/internal/audit"
)

// TestVisibilityChangeRequiresTheConfirmationEachDirectionNeeds pins the rule
// the documents state without enumerating: "the confirmation each transition
// requires".
//
// The exposing direction needs the repository's own name; the concealing one
// needs only the acknowledgement word. Requiring more for the direction that can
// leak content is the point.
func TestVisibilityChangeRequiresTheConfirmationEachDirectionNeeds(t *testing.T) {
	for _, test := range []struct {
		name         string
		confirmation string
		wantErr      bool
	}{
		{name: "no confirmation", confirmation: "", wantErr: true},
		{name: "the wrong word", confirmation: "yes", wantErr: true},
		{name: "the acknowledgement word", confirmation: VisibilityConfirmationWord, wantErr: true},
		{name: "a fragment of the name", confirmation: "private-rep", wantErr: true},
		{name: "the repository name", confirmation: "private-repo", wantErr: false},
		{name: "the name in another case", confirmation: "PRIVATE-REPO", wantErr: false},
		{name: "the name with padding", confirmation: "  private-repo  ", wantErr: false},
		{name: "the full name", confirmation: "owner-1/private-repo", wantErr: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			// A fixture per case, because a successful change moves the
			// repository to the target visibility and the next attempt would then
			// be refused for changing nothing rather than for its confirmation.
			f := newFixture(t)
			_, err := f.service.SetVisibility(context.Background(), SetVisibilityInput{
				Actor: f.owner, RepoID: f.private.ID,
				Visibility: VisibilityPublic, Confirmation: test.confirmation,
			})
			if test.wantErr && !errors.Is(err, ErrConfirmationRequired) {
				t.Fatalf("error = %v, want ErrConfirmationRequired", err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("a correct confirmation was refused: %v", err)
			}
		})
	}

	// The concealing direction accepts the word and rejects the name.
	concealing := newFixture(t)
	ctx := context.Background()
	if _, err := concealing.service.SetVisibility(ctx, SetVisibilityInput{
		Actor: concealing.owner, RepoID: concealing.public.ID,
		Visibility: VisibilityPrivate, Confirmation: VisibilityConfirmationWord,
	}); err != nil {
		t.Fatalf("making a repository private was refused: %v", err)
	}

	// A second fixture, because the first one's repository is now private.
	rejecting := newFixture(t)
	if _, err := rejecting.service.SetVisibility(ctx, SetVisibilityInput{
		Actor: rejecting.owner, RepoID: rejecting.public.ID,
		Visibility: VisibilityPrivate, Confirmation: "public-repo",
	}); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("the name was accepted where the acknowledgement is required: %v", err)
	}
}

// TestVisibilityChangeRecordsTheTransition covers both the record and its detail.
func TestVisibilityChangeRecordsTheTransition(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	before := len(f.store.events)
	updated, err := f.service.SetVisibility(ctx, SetVisibilityInput{
		Actor: f.owner, RepoID: f.private.ID,
		Visibility: VisibilityPublic, Confirmation: "private-repo",
		Request: audit.RequestContext{SourceAddress: "203.0.113.7", UserAgent: "test-agent"},
	})
	if err != nil {
		t.Fatalf("SetVisibility: %v", err)
	}
	if updated.Visibility != VisibilityPublic {
		t.Errorf("visibility = %q, want public", updated.Visibility)
	}

	if len(f.store.events) != before+1 {
		t.Fatalf("the change produced %d records, want exactly one", len(f.store.events)-before)
	}
	event := f.store.lastEvent(t)
	if event.Action != audit.ActionRepoVisibilityChanged {
		t.Errorf("action = %q, want %q", event.Action, audit.ActionRepoVisibilityChanged)
	}
	if event.SubjectID != f.private.ID || event.ScopeID != f.private.ID {
		t.Errorf("the record names subject %q and scope %q, want the repository", event.SubjectID, event.ScopeID)
	}
	if event.ActorID != f.ownerID || event.ActorKind != audit.ActorUser {
		t.Errorf("actor = %q/%q, want the acting account", event.ActorID, event.ActorKind)
	}
	// The request is recorded, because an operator asking "who did this from
	// where" needs the answer.
	if event.SourceAddress != "203.0.113.7" || event.UserAgent != "test-agent" {
		t.Errorf("the record lost the request context: %+v", event)
	}
	// The detail names what changed, not merely that something did.
	change, ok := event.Detail["visibility"].(audit.Change)
	if !ok {
		t.Fatalf("the detail does not carry the change: %v", event.Detail)
	}
	if change.From != string(VisibilityPrivate) || change.To != string(VisibilityPublic) {
		t.Errorf("detail = %+v, want private to public", change)
	}
}

// TestVisibilityChangeIsRefusedForNoChange keeps a record from claiming an event
// that did not happen.
func TestVisibilityChangeIsRefusedForNoChange(t *testing.T) {
	f := newFixture(t)
	before := len(f.store.events)

	if _, err := f.service.SetVisibility(context.Background(), SetVisibilityInput{
		Actor: f.owner, RepoID: f.private.ID,
		Visibility: VisibilityPrivate, Confirmation: "private-repo",
	}); !errors.Is(err, ErrNoChange) {
		t.Fatalf("error = %v, want ErrNoChange", err)
	}
	if len(f.store.events) != before {
		t.Error("a request that changed nothing still produced a record")
	}
}

// TestArchivedRepositoryRefusesContentWrites is the acceptance criterion:
// archiving makes a repository read-only.
func TestArchivedRepositoryRefusesContentWrites(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	archived, err := f.service.SetArchived(ctx, SetArchivedInput{
		Actor: f.owner, RepoID: f.public.ID, Archived: true,
	})
	if err != nil {
		t.Fatalf("SetArchived: %v", err)
	}
	if !archived.Archived {
		t.Fatal("the repository is not archived")
	}

	// A content write is refused, and the refusal names the state rather than the
	// permission.
	if _, _, err := f.service.Require(ctx, f.owner, f.public.ID, LevelWrite); !errors.Is(err, ErrArchived) {
		t.Errorf("a write = %v, want ErrArchived", err)
	}
	// Reading still works, so the archive does not hide the repository.
	if _, _, err := f.service.Require(ctx, f.owner, f.public.ID, LevelRead); err != nil {
		t.Errorf("a read = %v, want it to succeed", err)
	}
	if _, _, err := f.service.Require(ctx, Anonymous(), f.public.ID, LevelRead); err != nil {
		t.Errorf("an anonymous read = %v, want it to succeed on a public repository", err)
	}
	// Administration still works, because the owner has to be able to unarchive
	// it and to manage who may read it.
	if _, _, err := f.service.Require(ctx, f.owner, f.public.ID, LevelAdmin); err != nil {
		t.Errorf("an administrative operation = %v, want it to succeed", err)
	}
	// And the name-based path reaches the same decision, which is what the Git
	// transport uses.
	if _, _, err := f.service.ResolveAndRequire(ctx, f.owner, f.ownerID, f.public.Name, LevelWrite); !errors.Is(err, ErrArchived) {
		t.Errorf("the name path = %v, want ErrArchived", err)
	}
}

// TestUnarchivingWorksWhileArchived keeps the escape hatch open.
func TestUnarchivingWorksWhileArchived(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.service.SetArchived(ctx, SetArchivedInput{
		Actor: f.owner, RepoID: f.public.ID, Archived: true,
	}); err != nil {
		t.Fatalf("SetArchived(true): %v", err)
	}
	restored, err := f.service.SetArchived(ctx, SetArchivedInput{
		Actor: f.owner, RepoID: f.public.ID, Archived: false,
	})
	if err != nil {
		t.Fatalf("SetArchived(false) while archived: %v", err)
	}
	if restored.Archived {
		t.Error("the repository is still archived")
	}
	if _, _, err := f.service.Require(ctx, f.owner, f.public.ID, LevelWrite); err != nil {
		t.Errorf("a write after unarchiving = %v, want it to succeed", err)
	}

	// The two directions record different actions, because they are different
	// events.
	types := f.store.recorded()
	if !contains(types, audit.ActionRepoArchived) || !contains(types, audit.ActionRepoUnarchived) {
		t.Errorf("recorded %v, want both archive actions", types)
	}
}

func TestArchiveIsIdempotentWithoutARecord(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.service.SetArchived(ctx, SetArchivedInput{
		Actor: f.owner, RepoID: f.public.ID, Archived: true,
	}); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}
	before := len(f.store.events)

	if _, err := f.service.SetArchived(ctx, SetArchivedInput{
		Actor: f.owner, RepoID: f.public.ID, Archived: true,
	}); !errors.Is(err, ErrNoChange) {
		t.Fatalf("archiving twice = %v, want ErrNoChange", err)
	}
	if len(f.store.events) != before {
		t.Error("a request that changed nothing produced a record")
	}
}

// TestArchivedRepositoryStillAcceptsAdministration covers the operations that
// must keep working on an archived repository.
func TestArchivedRepositoryStillAcceptsAdministration(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.service.SetArchived(ctx, SetArchivedInput{
		Actor: f.owner, RepoID: f.public.ID, Archived: true,
	}); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}

	// Visibility, the default branch, the name, and access all remain
	// administrable, because archiving removes the ability to change content and
	// not the ability to run the repository.
	if _, err := f.service.SetVisibility(ctx, SetVisibilityInput{
		Actor: f.owner, RepoID: f.public.ID,
		Visibility: VisibilityPrivate, Confirmation: VisibilityConfirmationWord,
	}); err != nil {
		t.Errorf("changing visibility while archived: %v", err)
	}
	if _, err := f.service.SetDefaultBranch(ctx, SetDefaultBranchInput{
		Actor: f.owner, RepoID: f.public.ID, Branch: "develop",
	}); err != nil {
		t.Errorf("changing the default branch while archived: %v", err)
	}
	if _, err := f.service.Rename(ctx, RenameInput{
		Actor: f.owner, RepoID: f.public.ID, Name: "renamed-while-archived",
	}); err != nil {
		t.Errorf("renaming while archived: %v", err)
	}
	if _, err := f.grant(ctx, f.owner, f.public.ID, "someone", LevelRead); err != nil {
		t.Errorf("granting access while archived: %v", err)
	}
}

func TestDefaultBranchChangeValidatesAndRecords(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for _, branch := range []string{"", "..", "a..b", "main.lock", "a b", "a\\b"} {
		t.Run(branch, func(t *testing.T) {
			if _, err := f.service.SetDefaultBranch(ctx, SetDefaultBranchInput{
				Actor: f.owner, RepoID: f.private.ID, Branch: branch,
			}); err == nil {
				t.Errorf("the branch %q was accepted", branch)
			}
		})
	}

	// A valid branch is stored and recorded.
	if _, err := f.service.SetDefaultBranch(ctx, SetDefaultBranchInput{
		Actor: f.owner, RepoID: f.private.ID, Branch: "release-1.0",
	}); err != nil {
		t.Fatalf("SetDefaultBranch: %v", err)
	}
	event := f.store.lastEvent(t)
	if event.Action != audit.ActionRepoDefaultBranchChanged {
		t.Errorf("action = %q, want %q", event.Action, audit.ActionRepoDefaultBranchChanged)
	}
	change, ok := event.Detail["default_branch"].(audit.Change)
	if !ok || change.To != "release-1.0" {
		t.Errorf("the detail does not name the new branch: %v", event.Detail)
	}

	// Setting the same branch again changes nothing and records nothing.
	before := len(f.store.events)
	if _, err := f.service.SetDefaultBranch(ctx, SetDefaultBranchInput{
		Actor: f.owner, RepoID: f.private.ID, Branch: "release-1.0",
	}); !errors.Is(err, ErrNoChange) {
		t.Fatalf("setting the same branch = %v, want ErrNoChange", err)
	}
	if len(f.store.events) != before {
		t.Error("a request that changed nothing produced a record")
	}
}

// TestCollaboratorChangesRecordTheRightAction is the "one change, one record"
// rule applied to a grant, which has three possible outcomes.
func TestCollaboratorChangesRecordTheRightAction(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A new grant.
	if _, err := f.grant(ctx, f.owner, f.private.ID, "newcomer", LevelRead); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if got := f.store.lastEvent(t).Action; got != audit.ActionRepoCollaboratorAdded {
		t.Errorf("a new grant recorded %q, want %q", got, audit.ActionRepoCollaboratorAdded)
	}

	// A changed role.
	if _, err := f.grant(ctx, f.owner, f.private.ID, "newcomer", LevelWrite); err != nil {
		t.Fatalf("regrant: %v", err)
	}
	if got := f.store.lastEvent(t).Action; got != audit.ActionRepoCollaboratorRoleChanged {
		t.Errorf("a changed role recorded %q, want %q", got, audit.ActionRepoCollaboratorRoleChanged)
	}

	// A revocation.
	if err := f.revoke(ctx, f.owner, f.private.ID, "newcomer"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := f.store.lastEvent(t).Action; got != audit.ActionRepoCollaboratorRemoved {
		t.Errorf("a revocation recorded %q, want %q", got, audit.ActionRepoCollaboratorRemoved)
	}

	// A revocation of a grant that is not there changes nothing and records
	// nothing, because a record claiming a removal that did not happen is worse
	// than no record.
	before := len(f.store.events)
	if err := f.revoke(ctx, f.owner, f.private.ID, "newcomer"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking twice = %v, want ErrNotFound", err)
	}
	if len(f.store.events) != before {
		t.Error("a failed revocation produced a record")
	}

	// The grant is the subject of the record, not the repository, so a query for
	// one collaborator's history finds it.
	subject := ""
	for _, event := range f.store.events {
		if event.Action == audit.ActionRepoCollaboratorRemoved {
			subject = event.SubjectType + ":" + event.SubjectID
		}
	}
	if subject != audit.SubjectCollaborator+":newcomer" {
		t.Errorf("the record's subject = %q, want the grant", subject)
	}
}

// TestEveryRecordPassesItsOwnValidation is the guard against a service building
// a record the database would refuse.
func TestEveryRecordPassesItsOwnValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Exercise every operation that records something.
	if _, err := f.grant(ctx, f.owner, f.private.ID, "reader", LevelRead); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := f.service.SetVisibility(ctx, SetVisibilityInput{
		Actor: f.owner, RepoID: f.private.ID,
		Visibility: VisibilityPublic, Confirmation: "private-repo",
	}); err != nil {
		t.Fatalf("SetVisibility: %v", err)
	}
	if _, err := f.service.SetDefaultBranch(ctx, SetDefaultBranchInput{
		Actor: f.owner, RepoID: f.private.ID, Branch: "develop",
	}); err != nil {
		t.Fatalf("SetDefaultBranch: %v", err)
	}
	if _, err := f.service.SetArchived(ctx, SetArchivedInput{
		Actor: f.owner, RepoID: f.private.ID, Archived: true,
	}); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}
	if _, err := f.service.Rename(ctx, RenameInput{
		Actor: f.owner, RepoID: f.private.ID, Name: "renamed",
	}); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if err := f.revoke(ctx, f.owner, f.private.ID, "reader"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	if len(f.store.events) < 6 {
		t.Fatalf("only %d records were produced, want one per operation", len(f.store.events))
	}
	for index, event := range f.store.events {
		if err := event.Validate(); err != nil {
			t.Errorf("record %d (%s) is invalid: %v", index, event.Action, err)
		}
	}
}

// TestEventValidationRefusesIncoherentRecords covers the checks that turn a
// database constraint violation into a named field.
func TestEventValidationRefusesIncoherentRecords(t *testing.T) {
	valid := audit.Event{
		Action: audit.ActionRepoCreated, ActorID: "am-actor", ActorKind: audit.ActorUser,
		ScopeType: audit.ScopeRepository, ScopeID: "a-repository",
		SubjectType: audit.SubjectRepository, SubjectID: "a-repository",
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a coherent record was refused: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*audit.Event)
	}{
		{name: "no action", mutate: func(e *audit.Event) { e.Action = "" }},
		{name: "no subject type", mutate: func(e *audit.Event) { e.SubjectType = "" }},
		{name: "account actor without an account", mutate: func(e *audit.Event) { e.ActorID = "" }},
		{name: "system actor naming an account", mutate: func(e *audit.Event) { e.ActorKind = audit.ActorSystem }},
		{name: "unknown actor kind", mutate: func(e *audit.Event) { e.ActorKind = "robot" }},
		{name: "scoped record without a scope", mutate: func(e *audit.Event) { e.ScopeID = "" }},
		{name: "instance record naming a scope", mutate: func(e *audit.Event) { e.ScopeType = audit.ScopeInstance }},
		{name: "unknown scope type", mutate: func(e *audit.Event) { e.ScopeType = "galaxy" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := valid
			test.mutate(&event)
			if err := event.Validate(); err == nil {
				t.Error("an incoherent record was accepted")
			}
		})
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
