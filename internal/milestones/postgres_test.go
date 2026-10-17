package milestones_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/issues"
	"github.com/go-gitnest/GitNest/internal/milestones"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/google/uuid"
)

// The database-backed tests.
//
// Four of this workstream's claims are properties of how PostgreSQL behaves rather than of the code that
// calls it, so a fake cannot establish any of them:
//
//   - The number is **gap-free** and **never reused**, which are claims about transactionality.
//   - Two concurrent creates never share a number, which is a claim about row locking.
//   - Deleting a milestone **detaches** its issues rather than deleting them, which is a claim about a
//     foreign key's action.
//   - A closed milestone rejects an attachment, which is enforced by the writing statement rather than only
//     by a check before it.

// world is a migrated schema with an owner, a repository, an issue, and a milestone service over them.
type world struct {
	t       *testing.T
	db      *sql.DB
	service *milestones.Service
	store   *milestones.PostgresStore
	owner   repository.Principal
	ownerID string
	other   repository.Principal
	otherID string
	repoID  string
	issueID string
	issues  *issues.Service
	repos   *repository.Service
}

func newWorld(t *testing.T, visibility repository.Visibility) *world {
	t.Helper()
	schema := postgrestest.NewSchema(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	accounts := identity.NewService(identity.NewPostgresStore(schema.Client.DB), identity.Argon2idHasher{})
	owner, err := accounts.CreateInitialAdmin(ctx, identity.CreateInitialAdminInput{
		Username: "owner", Email: "owner@example.com", Password: "correct horse battery staple",
	})
	if err != nil {
		t.Fatalf("create the owner: %v", err)
	}
	other, err := insertAccount(ctx, schema.Client.DB, "other")
	if err != nil {
		t.Fatalf("create the second account: %v", err)
	}

	repositories := repository.NewService(repository.NewPostgresStore(schema.Client.DB))
	row, err := repositories.Create(ctx, repository.CreateInput{
		Actor: repository.User(owner.ID, false), OwnerID: owner.ID, Name: "a-repo",
		Visibility: visibility,
	})
	if err != nil {
		t.Fatalf("create the repository: %v", err)
	}

	issueService := issues.NewService(repositories, issues.NewPostgresStore(schema.Client.DB))
	issue, err := issueService.Create(ctx, issues.CreateInput{
		Actor: repository.User(owner.ID, false), RepositoryID: row.ID, Title: "an issue",
	})
	if err != nil {
		t.Fatalf("create the issue: %v", err)
	}

	store := milestones.NewPostgresStore(schema.Client.DB)
	return &world{
		t: t, db: schema.Client.DB, store: store,
		service: milestones.NewService(repositories, store),
		owner:   repository.User(owner.ID, false), ownerID: owner.ID,
		other: repository.User(other.ID, false), otherID: other.ID,
		repoID: row.ID, issueID: issue.ID, issues: issueService, repos: repositories,
	}
}

// insertAccount creates an account row directly, because the invitation path is exercised elsewhere.
func insertAccount(ctx context.Context, db *sql.DB, username string) (identity.User, error) {
	id := uuid.NewString()
	_, err := db.ExecContext(ctx, `
		INSERT INTO users (id, username, username_folded, email, password_hash)
		VALUES ($1, $2, $2, $2 || '@example.com', 'placeholder')`, id, username)
	if err != nil {
		return identity.User{}, err
	}
	return identity.User{ID: id, Username: username, Status: identity.StatusActive}, nil
}

// add creates one milestone through the service.
func (w *world) add(title string) milestones.Milestone {
	w.t.Helper()
	created, err := w.service.Create(context.Background(), milestones.CreateInput{
		Actor: w.owner, RepositoryID: w.repoID, Title: title,
	})
	if err != nil {
		w.t.Fatalf("Create(%q): %v", title, err)
	}
	return created
}

// nextNumber reads the repository's milestone counter directly.
func (w *world) nextNumber() int64 {
	w.t.Helper()
	var next int64
	if err := w.db.QueryRowContext(context.Background(),
		`SELECT next_milestone_number FROM repositories WHERE id = $1`, w.repoID).Scan(&next); err != nil {
		w.t.Fatalf("read the counter: %v", err)
	}
	return next
}

// countIssues reads the issue count directly, which is how "detaches rather than deletes" is checked.
func (w *world) countIssues() int {
	w.t.Helper()
	var count int
	if err := w.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM issues`).Scan(&count); err != nil {
		w.t.Fatalf("count the issues: %v", err)
	}
	return count
}

// attachedIssues counts how many issues carry any milestone.
func (w *world) attachedIssues() int {
	w.t.Helper()
	var count int
	if err := w.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM issues WHERE milestone_id IS NOT NULL`).Scan(&count); err != nil {
		w.t.Fatalf("count the attachments: %v", err)
	}
	return count
}

// TestAFailedCreateConsumesNoNumber is the gap-free claim against a real transaction.
//
// The failure is forced **after** the number has been allocated, by making the insert violate the primary
// key — which is exactly the window the claim is about. A `SEQUENCE` would have advanced permanently; the
// counter must not have moved.
func TestAFailedCreateConsumesNoNumber(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	first := w.add("v1.0")
	if first.Number != 1 {
		t.Fatalf("the first milestone is number %d, want 1", first.Number)
	}
	if got := w.nextNumber(); got != 2 {
		t.Fatalf("the counter is %d after one milestone, want 2", got)
	}

	// A conflicting identifier makes the insert fail after the allocation, because `Create` allocates
	// before it inserts.
	if _, err := w.store.Create(ctx, milestones.StoreCreate{
		ID: first.ID, RepositoryID: w.repoID, Title: "a doomed insert", TitleFolded: "a doomed insert",
	}); err == nil {
		t.Fatal("a duplicate identifier was accepted, so the failure this test needs did not happen")
	}

	// **The counter did not move**, which is the assertion.
	if got := w.nextNumber(); got != 2 {
		t.Errorf("the counter is %d after a failed create, want 2 — the number was consumed", got)
	}

	// And the next milestone takes the number the failure would have used, so there is no gap.
	second := w.add("v2.0")
	if second.Number != 2 {
		t.Errorf("the next milestone is number %d, want 2 with no gap", second.Number)
	}
}

// TestConcurrentCreatesNeverShareANumber is the other claim that needs a real database.
func TestConcurrentCreatesNeverShareANumber(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	const workers = 20
	numbers := make([]int64, workers)
	errs := make([]error, workers)

	var wait sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			// Every worker waits for the same moment, so the creations genuinely overlap.
			<-start
			created, err := w.service.Create(ctx, milestones.CreateInput{
				Actor: w.owner, RepositoryID: w.repoID, Title: fmt.Sprintf("concurrent-%d", index),
			})
			numbers[index], errs[index] = created.Number, err
		}(i)
	}
	close(start)
	wait.Wait()

	for index, err := range errs {
		if err != nil {
			t.Fatalf("worker %d failed: %v", index, err)
		}
	}

	seen := map[int64]int{}
	for _, number := range numbers {
		seen[number]++
	}
	for want := int64(1); want <= workers; want++ {
		if seen[want] != 1 {
			t.Errorf("number %d was handed out %d times, want once", want, seen[want])
		}
	}
	if got := w.nextNumber(); got != workers+1 {
		t.Errorf("the counter is %d, want %d", got, workers+1)
	}
}

// TestANumberIsNeverReusedAcrossADeletion is the property that distinguishes a counter from a rank.
//
// **This is why the milestone number is a counter and not a derived rank**, which is how a comment's number
// works. `urls-and-identifiers.md` L380 requires "the same contiguity and no-reuse rules" as issues, so a
// deletion must not free its number and must not renumber the milestones after it.
func TestANumberIsNeverReusedAcrossADeletion(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	first := w.add("v1.0")
	second := w.add("v2.0")
	third := w.add("v3.0")

	// Delete the middle one, so a rank-based scheme would renumber the third.
	if err := w.service.Delete(ctx, milestones.DeleteInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: second.Number,
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// **The later number did not move.** A rank would have made `v3.0` number 2.
	after, err := w.service.Get(ctx, milestones.GetInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: third.Number,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.Title != "v3.0" || after.Number != 3 {
		t.Errorf("number 3 now resolves to %q (number %d), want v3.0 — a deletion must not renumber", after.Title, after.Number)
	}

	// The deleted number is gone, not reused.
	if _, err := w.service.Get(ctx, milestones.GetInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: second.Number,
	}); !errors.Is(err, milestones.ErrNotFound) {
		t.Errorf("the deleted number still resolves: %v", err)
	}
	// And the next create takes a fresh number rather than the freed one.
	fourth := w.add("v4.0")
	if fourth.Number == second.Number {
		t.Errorf("number %d was reused after a deletion", fourth.Number)
	}
	if fourth.Number != 4 {
		t.Errorf("the next number is %d, want 4", fourth.Number)
	}
	if first.Number != 1 {
		t.Errorf("the first milestone moved to number %d", first.Number)
	}
}

// TestTheCounterIsSeparateFromTheIssueCounter covers §6.5's "its own sequence".
func TestTheCounterIsSeparateFromTheIssueCounter(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	// Two milestones and two more issues, so both counties advance independently.
	w.add("v1.0")
	w.add("v2.0")
	for index := 0; index < 2; index++ {
		if _, err := w.issues.Create(ctx, issues.CreateInput{
			Actor: w.owner, RepositoryID: w.repoID, Title: fmt.Sprintf("another issue %d", index),
		}); err != nil {
			t.Fatalf("create an issue: %v", err)
		}
	}

	var issueCounter, milestoneCounter int64
	if err := w.db.QueryRowContext(ctx, `
		SELECT next_issue_number, next_milestone_number FROM repositories WHERE id = $1`,
		w.repoID).Scan(&issueCounter, &milestoneCounter); err != nil {
		t.Fatalf("read the counters: %v", err)
	}
	// Three issues exist (the world's plus two) and two milestones, so the counters differ — which is what
	// "its own sequence" means.
	if issueCounter != 4 {
		t.Errorf("the issue counter is %d, want 4", issueCounter)
	}
	if milestoneCounter != 3 {
		t.Errorf("the milestone counter is %d, want 3 — it must not advance with the issues", milestoneCounter)
	}
}

// TestDeletingAMilestoneDetachesItsIssues is the acceptance criterion, proved against the tables.
func TestDeletingAMilestoneDetachesItsIssues(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	milestone := w.add("v1.0")
	second, err := w.issues.Create(ctx, issues.CreateInput{
		Actor: w.owner, RepositoryID: w.repoID, Title: "a second issue",
	})
	if err != nil {
		t.Fatalf("create the second issue: %v", err)
	}
	for _, issueID := range []string{w.issueID, second.ID} {
		if err := w.service.Attach(ctx, milestones.AttachInput{
			Actor: w.owner, RepositoryID: w.repoID, IssueID: issueID, Number: milestone.Number,
		}); err != nil {
			t.Fatalf("Attach: %v", err)
		}
	}
	if got := w.attachedIssues(); got != 2 {
		t.Fatalf("there are %d attachments, want 2", got)
	}

	if err := w.service.Delete(ctx, milestones.DeleteInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: milestone.Number,
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// **The issues are detached…**
	if got := w.attachedIssues(); got != 0 {
		t.Errorf("%d issues are still attached to a deleted milestone", got)
	}
	// **…and they are not deleted**, which is the criterion.
	if got := w.countIssues(); got != 2 {
		t.Errorf("there are %d issues after the deletion, want 2", got)
	}
	// The issues are still readable, so the detachment did not damage them.
	for _, issueID := range []string{w.issueID, second.ID} {
		number, err := w.store.MilestoneOf(ctx, issueID)
		if err != nil {
			t.Fatalf("MilestoneOf: %v", err)
		}
		if number != 0 {
			t.Errorf("issue %s still reports milestone %d", issueID, number)
		}
	}
}

// TestAClosedMilestoneRejectsAnAttachmentAtTheWrite is the acceptance criterion against the statement.
//
// The service checks the state before it writes, so the interesting case is the one the check cannot see: a
// close that lands between the check and the write. **It is forced here** by closing the milestone through
// the store between the two, which is what makes the `WHERE` clause load-bearing rather than decorative.
func TestAClosedMilestoneRejectsAnAttachmentAtTheWrite(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	milestone := w.add("v1.0")

	// The open state is accepted.
	if err := w.service.Attach(ctx, milestones.AttachInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, Number: milestone.Number,
	}); err != nil {
		t.Fatalf("Attach to an open milestone: %v", err)
	}

	// Close it, then attach a different issue. The service's check refuses this, which is the first half.
	second, err := w.issues.Create(ctx, issues.CreateInput{
		Actor: w.owner, RepositoryID: w.repoID, Title: "a second issue",
	})
	if err != nil {
		t.Fatalf("create the second issue: %v", err)
	}
	if _, err := w.service.Close(ctx, milestones.CloseInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: milestone.Number,
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.service.Attach(ctx, milestones.AttachInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: second.ID, Number: milestone.Number,
	}); !errors.Is(err, milestones.ErrClosed) {
		t.Fatalf("an attachment to a closed milestone = %v, want ErrClosed", err)
	}

	// **Now the write itself is asked to do it**, past the service's check — the race the statement exists
	// for. It must refuse, and it must explain why rather than reporting a generic not-found.
	if err := w.store.Attach(ctx, milestones.StoreAttach{
		IssueID: second.ID, MilestoneID: milestone.ID,
	}); !errors.Is(err, milestones.ErrClosed) {
		t.Errorf("the writing statement accepted an attachment to a closed milestone: %v", err)
	}
	// The issue is untouched, which is the property rather than the error's shape.
	number, err := w.store.MilestoneOf(ctx, second.ID)
	if err != nil {
		t.Fatalf("MilestoneOf: %v", err)
	}
	if number != 0 {
		t.Errorf("the issue reports milestone %d after a refused attachment", number)
	}

	// Detaching is still allowed on a closed milestone: removing work is not adding it.
	if err := w.service.Detach(ctx, milestones.DetachInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID,
	}); err != nil {
		t.Errorf("Detach from a closed milestone: %v", err)
	}
}

// TestAnIssueAndAMilestoneMustShareARepository covers the same-repository predicate.
//
// The statement requires it, so an identifier from one repository cannot be attached to an issue in
// another even if a caller obtains one — which is a real risk, because a milestone identifier is
// reachable through a repository the caller *may* access.
func TestAnIssueAndAMilestoneMustShareARepository(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	// A second repository, with its own milestone.
	second, err := w.repos.Create(ctx, repository.CreateInput{
		Actor: w.owner, OwnerID: w.ownerID, Name: "another-repo",
		Visibility: repository.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create the second repository: %v", err)
	}
	foreign, err := w.service.Create(ctx, milestones.CreateInput{
		Actor: w.owner, RepositoryID: second.ID, Title: "v1.0",
	})
	if err != nil {
		t.Fatalf("create the foreign milestone: %v", err)
	}

	// The cross-repository attachment is refused, and the statement is what refuses it.
	if err := w.store.Attach(ctx, milestones.StoreAttach{
		IssueID: w.issueID, MilestoneID: foreign.ID,
	}); !errors.Is(err, milestones.ErrNotFound) {
		t.Errorf("a cross-repository attachment = %v, want ErrNotFound", err)
	}
	number, err := w.store.MilestoneOf(ctx, w.issueID)
	if err != nil {
		t.Fatalf("MilestoneOf: %v", err)
	}
	if number != 0 {
		t.Errorf("the issue reports milestone %d after a refused cross-repository attachment", number)
	}

	// The same repository's milestone is accepted, which is what makes the assertion above meaningful.
	local := w.add("v1.0")
	if err := w.service.Attach(ctx, milestones.AttachInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, Number: local.Number,
	}); err != nil {
		t.Errorf("Attach within one repository: %v", err)
	}
}

// TestProgressIsDerivedAgainstTheDatabase is the acceptance criterion against real rows.
//
// It asserts that closing, reopening, and detaching change the counts, and that **relabeling an issue does
// not** — the last of which is the point: labels are a different table, so a derived count cannot be
// affected by them.
func TestProgressIsDerivedAgainstTheDatabase(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	milestone := w.add("v1.0")
	second, err := w.issues.Create(ctx, issues.CreateInput{
		Actor: w.owner, RepositoryID: w.repoID, Title: "a second issue",
	})
	if err != nil {
		t.Fatalf("create the second issue: %v", err)
	}
	for _, issueID := range []string{w.issueID, second.ID} {
		if err := w.service.Attach(ctx, milestones.AttachInput{
			Actor: w.owner, RepositoryID: w.repoID, IssueID: issueID, Number: milestone.Number,
		}); err != nil {
			t.Fatalf("Attach: %v", err)
		}
	}

	progress := func() milestones.Progress {
		w.t.Helper()
		counts, err := w.service.Progress(ctx, milestones.ProgressInput{
			Actor: w.owner, RepositoryID: w.repoID,
		})
		if err != nil {
			t.Fatalf("Progress: %v", err)
		}
		return counts[milestone.ID]
	}

	if got := progress(); got.Total != 2 || got.Closed != 0 || got.PercentComplete() != 0 {
		t.Fatalf("the initial progress is %+v, want 2 total and 0%%", got)
	}

	// Close one issue through the issue service, so a real state change is what moves the count.
	if _, err := w.issues.Close(ctx, issues.CloseInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: second.Number,
	}); err != nil {
		t.Fatalf("closing the issue: %v", err)
	}
	if got := progress(); got.Closed != 1 || got.PercentComplete() != 50 {
		t.Errorf("after closing one issue: %+v, want 1 closed and 50%%", got)
	}

	// Reopen it, which moves the count back.
	if _, err := w.issues.Reopen(ctx, issues.ReopenInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: second.Number,
	}); err != nil {
		t.Fatalf("reopening the issue: %v", err)
	}
	if got := progress(); got.Closed != 0 {
		t.Errorf("after reopening: %+v, want 0 closed", got)
	}

	// **Relabeling an issue does not change the milestone count**, because labels are a different table.
	// The label service is exercised through the store directly, since this package does not depend on it.
	var labelID string
	if err := w.db.QueryRowContext(ctx, `
		INSERT INTO labels (id, repository_id, name, name_folded, color)
		VALUES (gen_random_uuid(), $1, 'bug', 'bug', 'ff0000')
		RETURNING id`, w.repoID).Scan(&labelID); err != nil {
		t.Fatalf("create a label: %v", err)
	}
	if _, err := w.db.ExecContext(ctx, `
		INSERT INTO issue_labels (issue_id, label_id) VALUES ($1, $2)`, second.ID, labelID); err != nil {
		t.Fatalf("apply the label: %v", err)
	}
	if got := progress(); got.Total != 2 || got.Closed != 0 {
		t.Errorf("relabeling changed the milestone progress: %+v", got)
	}

	// Detaching removes it from the count.
	if err := w.service.Detach(ctx, milestones.DetachInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: second.ID,
	}); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if got := progress(); got.Total != 1 {
		t.Errorf("after detaching: %+v, want 1 total", got)
	}
}

// TestTheRecordIsWrittenAndReadBack covers the two catalogue actions against the real ledger.
func TestTheRecordIsWrittenAndReadBack(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	milestone := w.add("v1.0")
	// A create writes nothing, and neither does a rename.
	title := "v1.0.0"
	if _, err := w.service.Update(ctx, milestones.UpdateInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: milestone.Number, Title: &title,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if _, err := w.service.Close(ctx, milestones.CloseInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: milestone.Number,
		Request: audit.RequestContext{SourceAddress: "203.0.113.7", UserAgent: "a-test"},
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A reopen writes nothing, so it must not add a record.
	if _, err := w.service.Reopen(ctx, milestones.ReopenInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: milestone.Number,
	}); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if err := w.service.Delete(ctx, milestones.DeleteInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: milestone.Number,
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	reader := audit.NewReader(w.db)
	entries, err := reader.ForSubject(ctx, audit.SubjectMilestone, milestone.ID)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	// Exactly two: a close and a delete. A create, an update, or a reopen would have added one with an
	// invented action, which is what this asserts cannot happen.
	if len(entries) != 2 {
		t.Fatalf("the ledger holds %d records, want 2: %+v", len(entries), entries)
	}
	if entries[0].Action != audit.ActionMilestoneClosed {
		t.Errorf("the first record is %q, want milestone.closed", entries[0].Action)
	}
	if entries[1].Action != audit.ActionMilestoneDeleted {
		t.Errorf("the second record is %q, want milestone.deleted", entries[1].Action)
	}
	// The records are scoped to the milestone, and the close carries the request context and the
	// transition.
	if entries[0].ScopeType != string(audit.ScopeMilestone) {
		t.Errorf("the record's scope is %q, want milestone", entries[0].ScopeType)
	}
	if entries[0].SourceAddress != "203.0.113.7" || entries[0].UserAgent != "a-test" {
		t.Errorf("the close record lost the request context: %+v", entries[0])
	}
	transition, ok := entries[0].Detail["state"].(map[string]any)
	if !ok || transition["to"] != string(milestones.StateClosed) {
		t.Errorf("the close record does not describe the transition: %v", entries[0].Detail)
	}
	// The deletion names what was removed, which is what makes it answerable once the row is gone.
	if entries[1].Detail["title"] != "v1.0.0" {
		t.Errorf("the deletion record does not name the milestone: %v", entries[1].Detail)
	}
}

// TestTheTitleConstraintDecidesAConflict covers the constraint rather than a serialized check.
func TestTheTitleConstraintDecidesAConflict(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	w.add("Version 1.0")
	for _, variant := range []string{"version 1.0", "VERSION 1.0", "  Version 1.0  "} {
		if _, err := w.service.Create(ctx, milestones.CreateInput{
			Actor: w.owner, RepositoryID: w.repoID, Title: variant,
		}); !errors.Is(err, milestones.ErrTitleTaken) {
			t.Errorf("creating %q = %v, want ErrTitleTaken", variant, err)
		}
	}

	// **Concurrently**, so the constraint rather than a check is what refuses the losers.
	const writers = 8
	results := make([]error, writers)
	var wait sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, err := w.service.Create(ctx, milestones.CreateInput{
				Actor: w.owner, RepositoryID: w.repoID, Title: "concurrent-title",
			})
			results[index] = err
		}(i)
	}
	close(start)
	wait.Wait()

	succeeded := 0
	for _, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, milestones.ErrTitleTaken):
		default:
			t.Errorf("a concurrent create failed with %v, want ErrTitleTaken", err)
		}
	}
	if succeeded != 1 {
		t.Errorf("%d concurrent creates of one title succeeded, want exactly 1", succeeded)
	}
}

// TestThePermissionLevelAgainstTheRealService covers the write level end to end.
func TestThePermissionLevelAgainstTheRealService(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	// The second account holds read only.
	if _, err := w.repos.GrantAccess(ctx, repository.GrantInput{
		Actor: w.owner, RepoID: w.repoID, UserID: w.otherID, Role: repository.LevelRead,
	}); err != nil {
		t.Fatalf("grant read: %v", err)
	}

	// A reader may see milestones and may not manage one — refused as **forbidden**, because their level
	// was resolved and they can already see the repository.
	if _, err := w.service.Create(ctx, milestones.CreateInput{
		Actor: w.other, RepositoryID: w.repoID, Title: "v1.0",
	}); !errors.Is(err, milestones.ErrForbidden) {
		t.Errorf("a reader's create = %v, want ErrForbidden", err)
	}

	w.add("v1.0")
	if _, err := w.service.List(ctx, milestones.ListInput{
		Actor: w.other, RepositoryID: w.repoID,
	}); err != nil {
		t.Errorf("a reader's list: %v", err)
	}

	// **Promoting to write is enough**, which is the difference from a label: a writer may manage a
	// milestone, where a writer may not manage a label.
	if _, err := w.repos.GrantAccess(ctx, repository.GrantInput{
		Actor: w.owner, RepoID: w.repoID, UserID: w.otherID, Role: repository.LevelWrite,
	}); err != nil {
		t.Fatalf("grant write: %v", err)
	}
	if _, err := w.service.Create(ctx, milestones.CreateInput{
		Actor: w.other, RepositoryID: w.repoID, Title: "v2.0",
	}); err != nil {
		t.Errorf("a writer's create: %v", err)
	}
}

// TestAnUnreadableRepositoryDisclosesNoMilestones covers the T-A4 shape.
func TestAnUnreadableRepositoryDisclosesNoMilestones(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	w.add("a private milestone")

	anonymous := repository.Anonymous()
	for _, test := range []struct {
		name string
		call func() error
	}{
		{name: "list", call: func() error {
			_, err := w.service.List(ctx, milestones.ListInput{Actor: anonymous, RepositoryID: w.repoID})
			return err
		}},
		{name: "get", call: func() error {
			_, err := w.service.Get(ctx, milestones.GetInput{
				Actor: anonymous, RepositoryID: w.repoID, Number: 1,
			})
			return err
		}},
		{name: "progress", call: func() error {
			_, err := w.service.Progress(ctx, milestones.ProgressInput{
				Actor: anonymous, RepositoryID: w.repoID,
			})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, milestones.ErrNotFound) {
				t.Errorf("%s without access = %v, want ErrNotFound", test.name, err)
			}
		})
	}
}

// TestTheDueDateRoundTrips covers the storage of a date as midnight UTC.
func TestTheDueDateRoundTrips(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	created, err := w.service.Create(ctx, milestones.CreateInput{
		Actor: w.owner, RepositoryID: w.repoID, Title: "with a due date",
		Description: "a description that will be cleared", DueDate: "2026-10-14",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	loaded, err := w.service.Get(ctx, milestones.GetInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: created.Number,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := milestones.FormatDueDate(loaded.DueOn); got != "2026-10-14" {
		t.Errorf("the due date round-tripped as %q, want 2026-10-14", got)
	}
	// **The instant is midnight UTC, and the assertion is on the instant rather than on a local hour.** The
	// driver returns the value in the session's location, so asserting `Hour()` alone would test the
	// server's timezone rather than the storage — which is what the first version of this assertion did,
	// reporting `08:00:00 +0800 CST` for a value that was already correct.
	if loaded.DueOn.UTC().Hour() != 0 || loaded.DueOn.UTC().Minute() != 0 {
		t.Errorf("the stored due date is %v, want midnight UTC", loaded.DueOn.UTC())
	}
	if loaded.DueOn.Location() != time.UTC {
		t.Errorf("the read value's location is %v, want UTC — a read must normalize it", loaded.DueOn.Location())
	}

	// Clearing it through an update leaves no due date, which is the `DueOnSet` case.
	empty := ""
	cleared, err := w.service.Update(ctx, milestones.UpdateInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: created.Number, DueDate: &empty,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if cleared.DueOn != nil {
		t.Errorf("the due date was not cleared: %v", cleared.DueOn)
	}

	// And a description may be cleared, distinguishing "absent" from "set to empty".
	description := ""
	if _, err := w.service.Update(ctx, milestones.UpdateInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: created.Number, Description: &description,
	}); err != nil {
		t.Fatalf("clearing the description: %v", err)
	}
}

// TestTheListOrderAndFilter covers the query's ordering and its filter.
func TestTheListOrderAndFilter(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	first := w.add("v1.0")
	second := w.add("v2.0")
	if _, err := w.service.Close(ctx, milestones.CloseInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: first.Number,
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Newest first, so the one being worked on is at the top.
	found, err := w.service.List(ctx, milestones.ListInput{Actor: w.owner, RepositoryID: w.repoID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(found) != 2 {
		t.Fatalf("the list holds %d milestones, want 2", len(found))
	}
	if found[0].Number != second.Number {
		t.Errorf("the newest milestone is not first: %d then %d", found[0].Number, found[1].Number)
	}

	// The filter narrows it.
	open, err := w.service.List(ctx, milestones.ListInput{
		Actor: w.owner, RepositoryID: w.repoID, State: milestones.StateOpen,
	})
	if err != nil {
		t.Fatalf("List(open): %v", err)
	}
	if len(open) != 1 || open[0].Number != second.Number {
		t.Errorf("the open filter returned %+v", open)
	}
	closed, err := w.service.List(ctx, milestones.ListInput{
		Actor: w.owner, RepositoryID: w.repoID, State: milestones.StateClosed,
	})
	if err != nil {
		t.Fatalf("List(closed): %v", err)
	}
	if len(closed) != 1 || closed[0].Number != first.Number {
		t.Errorf("the closed filter returned %+v", closed)
	}
}
