package issues_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/issues"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/google/uuid"
)

// The database-backed tests.
//
// Two of W2.2's acceptance criteria cannot be checked against a fake, because both are
// properties of how PostgreSQL behaves rather than of the code that calls it:
//
//   - "Numbers are contiguous per repository with **no gaps from failed transactions**" is a
//     claim about transactionality, and a fake that simply does not advance its counter proves
//     nothing about the real one.
//   - "Two concurrent creates never share a number" is a claim about row locking, and a fake
//     behind a mutex would trivially satisfy it.
//
// So both are asserted here against a migrated schema with real transactions and real
// concurrency.

// world is a migrated schema with an owner, a repository, and an issue service over it.
type world struct {
	t        *testing.T
	db       *sql.DB
	service  *issues.Service
	stores   *issues.PostgresStore
	owner    repository.Principal
	ownerID  string
	other    repository.Principal
	otherID  string
	repoID   string
	repoName string
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

	store := issues.NewPostgresStore(schema.Client.DB)
	return &world{
		t: t, db: schema.Client.DB, stores: store,
		service: issues.NewService(repositories, store),
		owner:   repository.User(owner.ID, false), ownerID: owner.ID,
		other: repository.User(other.ID, false), otherID: other.ID,
		repoID: row.ID, repoName: row.Name,
	}
}

// insertAccount creates an account row directly, because the invitation path is exercised
// elsewhere and this test only needs the foreign key to be satisfiable.
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

// open creates an issue through the service.
func (w *world) open(actor repository.Principal, title string) issues.Issue {
	w.t.Helper()
	created, err := w.service.Create(context.Background(), issues.CreateInput{
		Actor: actor, RepositoryID: w.repoID, Title: title,
	})
	if err != nil {
		w.t.Fatalf("Create(%q): %v", title, err)
	}
	return created
}

// nextNumber reads the repository's counter directly, which is what the migration added.
func (w *world) nextNumber() int64 {
	w.t.Helper()
	var next int64
	if err := w.db.QueryRowContext(context.Background(),
		`SELECT next_issue_number FROM repositories WHERE id = $1`, w.repoID).Scan(&next); err != nil {
		w.t.Fatalf("read the counter: %v", err)
	}
	return next
}

// TestAFailedCreateConsumesNoNumber is the gap-free claim, proved end to end.
//
// The failure is forced in the strongest way available: the insert is made to violate the
// primary key, so the transaction fails **after** the number has been allocated — which is
// exactly the window the claim is about. A `SEQUENCE` would have advanced permanently; the
// counter must not have moved.
func TestAFailedCreateConsumesNoNumber(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	first := w.open(w.owner, "the first issue")
	if first.Number != 1 {
		t.Fatalf("the first issue is number %d, want 1", first.Number)
	}
	if got := w.nextNumber(); got != 2 {
		t.Fatalf("the counter is %d after one issue, want 2", got)
	}

	// A conflicting identifier makes the insert fail after the allocation. The service
	// generates identifiers, so the collision is set up through the store, which is the layer
	// that holds both the allocation and the insert.
	if _, err := w.stores.Create(ctx, issues.StoreCreate{
		ID: first.ID, RepositoryID: w.repoID, Title: "a doomed insert",
		AuthorID: w.ownerID,
		Event:    audit.Event{Action: audit.ActionIssueCreated, ActorID: w.ownerID, ActorKind: audit.ActorUser, ScopeType: audit.ScopeIssue, ScopeID: first.ID, SubjectType: audit.SubjectIssue, SubjectID: first.ID},
	}); err == nil {
		t.Fatal("a duplicate identifier was accepted, so the failure this test needs did not happen")
	}

	// **The counter did not move.** This is the assertion: a rolled-back transaction gave its
	// number back rather than burning it.
	if got := w.nextNumber(); got != 2 {
		t.Errorf("the counter is %d after a failed create, want 2 — the number was consumed", got)
	}

	// And the next issue takes the number the failure would have used, so there is no gap.
	second := w.open(w.owner, "the second issue")
	if second.Number != 2 {
		t.Errorf("the next issue is number %d, want 2 with no gap", second.Number)
	}

	// The ledger did not record the change that did not happen.
	var count int
	if err := w.db.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_events WHERE action = $1`, audit.ActionIssueCreated).Scan(&count); err != nil {
		t.Fatalf("count the records: %v", err)
	}
	if count != 2 {
		t.Errorf("the ledger holds %d creation records, want 2 — a failed change was recorded", count)
	}
}

// TestConcurrentCreatesNeverShareANumber is the other claim that needs a real database.
//
// Many goroutines create at once. A read-then-write would hand the same number to two of them;
// the row lock on the counter is what serializes the allocation, so the numbers must come out
// as exactly 1..N.
func TestConcurrentCreatesNeverShareANumber(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	const workers = 24
	numbers := make([]int64, workers)
	errs := make([]error, workers)

	var wait sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			// Every worker waits for the same moment, so the creations genuinely overlap
			// rather than running one after another.
			<-start
			created, err := w.service.Create(ctx, issues.CreateInput{
				Actor: w.owner, RepositoryID: w.repoID, Title: "concurrent",
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

	// Every number appears exactly once.
	seen := map[int64]int{}
	for _, number := range numbers {
		seen[number]++
	}
	for number, count := range seen {
		if count != 1 {
			t.Errorf("number %d was handed out %d times", number, count)
		}
	}
	// And they are exactly 1..N, so there is neither a gap nor a repeat.
	for want := int64(1); want <= workers; want++ {
		if seen[want] != 1 {
			t.Errorf("number %d was handed out %d times, want once", want, seen[want])
		}
	}
	if got := w.nextNumber(); got != workers+1 {
		t.Errorf("the counter is %d, want %d", got, workers+1)
	}
}

// TestTheCounterBelongsToTheRepositoryNotTheIssue covers the sharing requirement: the counter
// is a repository column, so phase 3's pull requests can draw from the same sequence rather
// than needing a second scheme to reconcile.
//
// It is asserted by reading the column the migration added, because the claim is about where
// the counter lives.
func TestTheCounterBelongsToTheRepositoryNotTheIssue(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)

	if got := w.nextNumber(); got != 1 {
		t.Fatalf("a fresh repository's counter is %d, want 1", got)
	}
	w.open(w.owner, "an issue")

	// The column is on `repositories`, so a second repository has its own sequence.
	second, err := insertRepository(context.Background(), w, "another-repo")
	if err != nil {
		t.Fatalf("create a second repository: %v", err)
	}
	var secondCounter int64
	if err := w.db.QueryRowContext(context.Background(),
		`SELECT next_issue_number FROM repositories WHERE id = $1`, second).Scan(&secondCounter); err != nil {
		t.Fatalf("read the second counter: %v", err)
	}
	if secondCounter != 1 {
		t.Errorf("a second repository's counter is %d, want its own starting at 1", secondCounter)
	}
}

// insertRepository creates a second repository through the service.
func insertRepository(ctx context.Context, w *world, name string) (string, error) {
	repositories := repository.NewService(repository.NewPostgresStore(w.db))
	row, err := repositories.Create(ctx, repository.CreateInput{
		Actor: w.owner, OwnerID: w.ownerID, Name: name,
	})
	if err != nil {
		return "", err
	}
	return row.ID, nil
}

// TestEveryColumnRoundTrips covers the storage the migration declares, so a scan cannot drift
// from the projection.
func TestEveryColumnRoundTrips(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	created := w.open(w.owner, "the round trip")
	if created.ID == "" || created.RepositoryID != w.repoID || created.Number != 1 {
		t.Fatalf("the created issue is %+v", created)
	}
	if created.AuthorID != w.ownerID {
		t.Errorf("the author is %q, want the creator", created.AuthorID)
	}
	if created.State != issues.StateOpen || created.Version != 1 {
		t.Errorf("a new issue is %q version %d, want open version 1", created.State, created.Version)
	}
	if created.CreatedAt.IsZero() || created.UpdatedAt.IsZero() {
		t.Error("the timestamps were not set by the database")
	}
	if created.ClosedAt != nil || created.ClosedBy != "" {
		t.Errorf("a new issue carries a close: at=%v by=%q", created.ClosedAt, created.ClosedBy)
	}

	// Read it back, so a value that only round-tripped through the RETURNING clause is caught.
	loaded, err := w.stores.Get(ctx, w.repoID, created.Number)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if loaded.Title != created.Title || loaded.Body != created.Body || loaded.ID != created.ID {
		t.Errorf("the read differs from the write:\n wrote %+v\n read  %+v", created, loaded)
	}

	// Close, then confirm the pair is stored and the version advanced.
	closed, err := w.service.Close(ctx, issues.CloseInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: created.Number,
	})
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if closed.State != issues.StateClosed || closed.ClosedAt == nil || closed.ClosedBy != w.ownerID {
		t.Errorf("the close is not stored: %+v", closed)
	}
	if closed.Version <= created.Version {
		t.Errorf("the version did not advance on a close: %d then %d", created.Version, closed.Version)
	}

	// Reopen, and confirm the pair is cleared rather than left behind.
	reopened, err := w.service.Reopen(ctx, issues.ReopenInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: created.Number,
	})
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if reopened.State != issues.StateOpen || reopened.ClosedAt != nil || reopened.ClosedBy != "" {
		t.Errorf("reopening left the close in place: %+v", reopened)
	}
}

// TestTheLedgerIsTheStateHistory covers the acceptance requirement against the real table: the
// records are queryable by issue, which is what makes them a history rather than a log.
func TestTheLedgerIsTheStateHistory(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	created := w.open(w.owner, "a lifecycle")
	if _, err := w.service.Close(ctx, issues.CloseInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: created.Number,
		Request: audit.RequestContext{SourceAddress: "203.0.113.7", UserAgent: "a-test"},
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The reopen is done by the **other** account, so the history names two different actors
	// and the record's attribution is actually exercised rather than assumed. That account
	// holds no grant in this world, so it is given write first — without it the reopen is
	// correctly refused as not-found, because a caller who cannot see a repository is not told
	// it exists.
	repositories := repository.NewService(repository.NewPostgresStore(w.db))
	if _, err := repositories.GrantAccess(ctx, repository.GrantInput{
		Actor: w.owner, RepoID: w.repoID, UserID: w.otherID, Role: repository.LevelWrite,
	}); err != nil {
		t.Fatalf("grant write: %v", err)
	}
	if _, err := w.service.Reopen(ctx, issues.ReopenInput{
		Actor: w.other, RepositoryID: w.repoID, Number: created.Number,
	}); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	reader := audit.NewReader(w.db)
	entries, err := reader.ForSubject(ctx, audit.SubjectIssue, created.ID)
	if err != nil {
		t.Fatalf("read the history: %v", err)
	}

	actions := make([]string, 0, len(entries))
	for _, entry := range entries {
		actions = append(actions, entry.Action)
	}
	// The order is the database's, because the ledger has no application-side clock.
	for _, want := range []string{audit.ActionIssueCreated, audit.ActionIssueClosed, audit.ActionIssueReopened} {
		if !contains(actions, want) {
			t.Errorf("the history is missing %q: %v", want, actions)
		}
	}

	// The close record carries the request context and the transition, which is what makes a
	// history answerable.
	for _, entry := range entries {
		if entry.Action != audit.ActionIssueClosed {
			continue
		}
		if entry.SourceAddress != "203.0.113.7" || entry.UserAgent != "a-test" {
			t.Errorf("the close record lost the request context: %+v", entry)
		}
		if entry.ActorID != w.ownerID {
			t.Errorf("the close record names actor %q, want the closer", entry.ActorID)
		}
		transition, ok := entry.Detail["state"].(map[string]any)
		if !ok || transition["to"] != string(issues.StateClosed) {
			t.Errorf("the close record does not describe the transition: %v", entry.Detail)
		}
	}
}

// TestADeletionKeepsItsRecord covers "delete-with-audit": the row goes and the record stays,
// because the ledger has no foreign key to the issue.
func TestADeletionKeepsItsRecord(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	created := w.open(w.owner, "to delete")
	if err := w.service.Delete(ctx, issues.DeleteInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: created.Number,
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// The row is gone.
	if _, err := w.stores.Get(ctx, w.repoID, created.Number); !errors.Is(err, issues.ErrNotFound) {
		t.Errorf("the issue is still readable: %v", err)
	}
	// The record survives, naming what was removed.
	reader := audit.NewReader(w.db)
	entries, err := reader.ForSubject(ctx, audit.SubjectIssue, created.ID)
	if err != nil {
		t.Fatalf("read the history: %v", err)
	}
	var found bool
	for _, entry := range entries {
		if entry.Action == audit.ActionIssueDeleted {
			found = true
			if entry.Detail["title"] != "to delete" {
				t.Errorf("the record does not name what was removed: %v", entry.Detail)
			}
		}
	}
	if !found {
		t.Error("the deletion was not recorded")
	}
}

// TestTheNumberIsNeverReused covers the numbering rule after a deletion: a number is spent for
// good, because a later issue reusing one would make every reference to it ambiguous.
func TestTheNumberIsNeverReused(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	first := w.open(w.owner, "the first")
	if err := w.service.Delete(ctx, issues.DeleteInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: first.Number,
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	second := w.open(w.owner, "the next")
	if second.Number == first.Number {
		t.Errorf("number %d was reused after a deletion", second.Number)
	}
	if second.Number != 2 {
		t.Errorf("the next number is %d, want 2", second.Number)
	}
}

// TestTheListingPaginatesByKeyset covers the list query and its cursor against real rows.
func TestTheListingPaginatesByKeyset(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	// Enough issues to make two pages of three.
	for i := 0; i < 7; i++ {
		w.open(w.owner, "an issue")
	}

	seen := map[int64]bool{}
	var cursor *issues.Cursor
	pages := 0
	for {
		page, err := w.service.List(ctx, issues.ListInput{
			Actor: w.owner, RepositoryID: w.repoID, Limit: 3, Before: cursor,
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		pages++
		for _, issue := range page.Issues {
			if seen[issue.Number] {
				t.Errorf("number %d appeared on two pages", issue.Number)
			}
			seen[issue.Number] = true
		}
		if !page.HasMore {
			break
		}
		if page.Next == nil {
			t.Fatal("a page reported more rows without a cursor")
		}
		cursor = page.Next
		if pages > 10 {
			t.Fatal("the pagination did not terminate")
		}
	}

	if len(seen) != 7 {
		t.Errorf("the pages covered %d issues, want all 7", len(seen))
	}
	// Newest first, so the last page holds the earliest issues.
	if !seen[1] {
		t.Error("the first issue was never returned")
	}
}

// TestTheStateFilterApplies covers the filter and the count, which are what a listing page
// shows.
func TestTheStateFilterApplies(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	first := w.open(w.owner, "to close")
	w.open(w.owner, "to keep open")
	if _, err := w.service.Close(ctx, issues.CloseInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: first.Number,
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, test := range []struct {
		state issues.State
		want  int
	}{
		{state: "", want: 2},
		{state: issues.StateOpen, want: 1},
		{state: issues.StateClosed, want: 1},
	} {
		page, err := w.service.List(ctx, issues.ListInput{
			Actor: w.owner, RepositoryID: w.repoID, State: test.state,
		})
		if err != nil {
			t.Fatalf("List(%q): %v", test.state, err)
		}
		if len(page.Issues) != test.want {
			t.Errorf("the %q filter returned %d issues, want %d", describe(test.state), len(page.Issues), test.want)
		}

		count, err := w.service.Count(ctx, issues.CountInput{
			Actor: w.owner, RepositoryID: w.repoID, State: test.state,
		})
		if err != nil {
			t.Fatalf("Count(%q): %v", test.state, err)
		}
		if count != test.want {
			t.Errorf("the %q count is %d, want %d", describe(test.state), count, test.want)
		}
	}
}

// TestAPrivateRepositoryIsInvisibleToAnonymousCallers is W2.2's acceptance requirement about
// counts: an anonymous caller learns nothing, because the refusal happens before any query.
func TestAPrivateRepositoryIsInvisibleToAnonymousCallers(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	w.open(w.owner, "a private issue")

	anonymous := repository.Anonymous()
	if _, err := w.service.List(ctx, issues.ListInput{
		Actor: anonymous, RepositoryID: w.repoID,
	}); !errors.Is(err, issues.ErrNotFound) {
		t.Errorf("an anonymous listing = %v, want ErrNotFound", err)
	}
	if _, err := w.service.Count(ctx, issues.CountInput{
		Actor: anonymous, RepositoryID: w.repoID,
	}); !errors.Is(err, issues.ErrNotFound) {
		t.Errorf("an anonymous count = %v, want ErrNotFound", err)
	}
	if _, err := w.service.Get(ctx, issues.GetInput{
		Actor: anonymous, RepositoryID: w.repoID, Number: 1,
	}); !errors.Is(err, issues.ErrNotFound) {
		t.Errorf("an anonymous read = %v, want ErrNotFound", err)
	}

	// A collaborator holding only read sees it, which is what makes the refusals above a
	// decision rather than a broken query.
	repositories := repository.NewService(repository.NewPostgresStore(w.db))
	if _, err := repositories.GrantAccess(ctx, repository.GrantInput{
		Actor: w.owner, RepoID: w.repoID, UserID: w.otherID, Role: repository.LevelRead,
	}); err != nil {
		t.Fatalf("grant read: %v", err)
	}
	count, err := w.service.Count(ctx, issues.CountInput{
		Actor: w.other, RepositoryID: w.repoID,
	})
	if err != nil {
		t.Fatalf("a reader's count: %v", err)
	}
	if count != 1 {
		t.Errorf("a reader sees %d issues, want 1", count)
	}
}

// TestTheAuthorAndWriterRulesAgainstTheRealStore covers the authorization paths end to end,
// because the level comes from the real permission service rather than a fake.
func TestTheAuthorAndWriterRulesAgainstTheRealStore(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	// The second account holds read only.
	repositories := repository.NewService(repository.NewPostgresStore(w.db))
	if _, err := repositories.GrantAccess(ctx, repository.GrantInput{
		Actor: w.owner, RepoID: w.repoID, UserID: w.otherID, Role: repository.LevelRead,
	}); err != nil {
		t.Fatalf("grant read: %v", err)
	}

	// The owner opens an issue, so the other account is a reader and not the author.
	created := w.open(w.owner, "an issue")

	title := "an edit"
	if _, err := w.service.Edit(ctx, issues.EditInput{
		Actor: w.other, RepositoryID: w.repoID, Number: created.Number,
		Title: &title, ExpectedVersion: created.Version,
	}); !errors.Is(err, issues.ErrForbidden) {
		t.Errorf("a reader's edit = %v, want ErrForbidden", err)
	}
	if _, err := w.service.Close(ctx, issues.CloseInput{
		Actor: w.other, RepositoryID: w.repoID, Number: created.Number,
	}); !errors.Is(err, issues.ErrForbidden) {
		t.Errorf("a reader's close = %v, want ErrForbidden", err)
	}

	// The author may edit their own issue while holding only read.
	if _, err := w.service.Edit(ctx, issues.EditInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: created.Number,
		Title: &title, ExpectedVersion: created.Version,
	}); err != nil {
		t.Errorf("the author's edit: %v", err)
	}

	// Promote the reader to write, and the same operation succeeds.
	if _, err := repositories.GrantAccess(ctx, repository.GrantInput{
		Actor: w.owner, RepoID: w.repoID, UserID: w.otherID, Role: repository.LevelWrite,
	}); err != nil {
		t.Fatalf("grant write: %v", err)
	}
	if _, err := w.service.Close(ctx, issues.CloseInput{
		Actor: w.other, RepositoryID: w.repoID, Number: created.Number,
	}); err != nil {
		t.Errorf("a writer's close: %v", err)
	}
}

// TestAnEditAgainstTheRealStoreIsGuardedByVersion covers the optimistic concurrency against
// real rows, where the check is in the UPDATE's WHERE clause.
func TestAnEditAgainstTheRealStoreIsGuardedByVersion(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	created := w.open(w.owner, "the original")

	first := "the first writer"
	if _, err := w.service.Edit(ctx, issues.EditInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: created.Number,
		Title: &first, ExpectedVersion: created.Version,
	}); err != nil {
		t.Fatalf("the first edit: %v", err)
	}

	second := "the second writer"
	if _, err := w.service.Edit(ctx, issues.EditInput{
		Actor: w.owner, RepositoryID: w.repoID, Number: created.Number,
		Title: &second, ExpectedVersion: created.Version,
	}); !errors.Is(err, issues.ErrConflict) {
		t.Fatalf("a stale edit = %v, want ErrConflict", err)
	}

	stored, err := w.stores.Get(ctx, w.repoID, created.Number)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Title != first {
		t.Errorf("the stored title is %q, want the first writer's", stored.Title)
	}
}

// describe renders a state filter for a message.
func describe(state issues.State) string {
	if state == "" {
		return "all"
	}
	return string(state)
}

// contains reports whether a slice holds a value.
func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
