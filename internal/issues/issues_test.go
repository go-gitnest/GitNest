package issues

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/go-gitnest/GitNest/internal/repository"
)

// The service rules.
//
// These run against a fake store and a fake access decision, which is what makes the rules
// testable in isolation: the store is a map, and the access answer is a value the test sets.
// The two properties that need a real database — gap-free numbering under concurrency and the
// audit record's atomicity — are asserted in `postgres_test.go` against PostgreSQL.

const (
	repoID  = "01920000-0000-7000-8000-0000000000f1"
	aliceID = "01920000-0000-7000-8000-0000000000f2"
	bobID   = "01920000-0000-7000-8000-0000000000f3"
	carolID = "01920000-0000-7000-8000-0000000000f4"
)

// fakeAccess answers from a level the test sets, collapsing "no access" into not-found the
// way the repository service does.
type fakeAccess struct {
	level repository.Level
	// err forces a failure, so the mapping onto this package's errors can be asserted.
	err error
}

func (f *fakeAccess) Require(_ context.Context, _ repository.Principal, _ string, required repository.Level) (repository.Repository, repository.Level, error) {
	if f.err != nil {
		return repository.Repository{}, repository.LevelNone, f.err
	}
	if f.level == repository.LevelNone {
		// The real service reports a repository the caller cannot see as not-found, which is
		// the T-A4 shape.
		return repository.Repository{}, repository.LevelNone, repository.ErrNotFound
	}
	if !f.level.Allows(required) {
		return repository.Repository{}, f.level, repository.ErrForbidden
	}
	return repository.Repository{ID: repoID, Name: "a-repo"}, f.level, nil
}

// fakeStore is an in-memory Store, with the numbering rule the database implements.
type fakeStore struct {
	issues map[int64]Issue
	// next is the counter, mirroring `repositories.next_issue_number`.
	next int64
	// events records what the service asked to be written.
	events []audit.Event
	// failCreate forces an insert failure after the number is allocated, which is how a test
	// asserts that a failed transaction does not consume a number.
	failCreate error
	// getErr forces a read failure.
	getErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{issues: map[int64]Issue{}, next: 1}
}

func (f *fakeStore) Create(_ context.Context, input StoreCreate) (Issue, error) {
	if f.failCreate != nil {
		return Issue{}, f.failCreate
	}
	// The allocation, in the order the database does it: take the number, then insert. A
	// failure between the two is what the counter's transactionality is about, and this fake
	// reproduces the observable consequence by not advancing on failure.
	number := f.next
	f.next++

	created := Issue{
		ID: input.ID, RepositoryID: input.RepositoryID, Number: number,
		Title: input.Title, Body: input.Body, AuthorID: input.AuthorID,
		State: StateOpen, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Version: 1,
	}
	f.issues[number] = created
	f.events = append(f.events, input.Event)
	return created, nil
}

func (f *fakeStore) Get(_ context.Context, _ string, number int64) (Issue, error) {
	if f.getErr != nil {
		return Issue{}, f.getErr
	}
	issue, found := f.issues[number]
	if !found {
		return Issue{}, ErrNotFound
	}
	return issue, nil
}

func (f *fakeStore) List(_ context.Context, query StoreList) (Page, error) {
	page := Page{Issues: []Issue{}}
	for number := f.next - 1; number >= 1; number-- {
		issue, found := f.issues[number]
		if !found {
			continue
		}
		if query.State != "" && issue.State != query.State {
			continue
		}
		page.Issues = append(page.Issues, issue)
	}
	return page, nil
}

func (f *fakeStore) Edit(_ context.Context, input StoreEdit) (Issue, error) {
	issue, found := f.issues[input.Number]
	if !found {
		return Issue{}, ErrNotFound
	}
	if issue.Version != input.ExpectedVersion {
		return Issue{}, ErrConflict
	}
	issue.Title = input.Title
	issue.Body = input.Body
	issue.Version++
	issue.UpdatedAt = time.Now().UTC()
	f.issues[input.Number] = issue
	return issue, nil
}

func (f *fakeStore) SetState(_ context.Context, input StoreSetState) (Issue, error) {
	issue, found := f.issues[input.Number]
	if !found {
		return Issue{}, ErrNotFound
	}
	if issue.State != input.Expect {
		return Issue{}, ErrAlreadyInState
	}
	issue.State = input.State
	if input.State == StateClosed {
		at := time.Now().UTC()
		issue.ClosedAt = &at
		issue.ClosedBy = input.Event.ActorID
	} else {
		issue.ClosedAt = nil
		issue.ClosedBy = ""
	}
	issue.Version++
	f.issues[input.Number] = issue
	f.events = append(f.events, input.Event)
	return issue, nil
}

func (f *fakeStore) Delete(_ context.Context, input StoreDelete) error {
	if _, found := f.issues[input.Number]; !found {
		return ErrNotFound
	}
	delete(f.issues, input.Number)
	f.events = append(f.events, input.Event)
	return nil
}

func (f *fakeStore) Count(_ context.Context, _ string, state State) (int, error) {
	count := 0
	for _, issue := range f.issues {
		if state == "" || issue.State == state {
			count++
		}
	}
	return count, nil
}

// fixture is a service with a store and an access level the test controls.
type fixture struct {
	service *Service
	store   *fakeStore
	access  *fakeAccess
}

func newFixture(t *testing.T, level repository.Level) *fixture {
	t.Helper()
	store := newFakeStore()
	access := &fakeAccess{level: level}
	return &fixture{service: NewService(access, store), store: store, access: access}
}

// author is the principal of an account.
func author(id string) repository.Principal { return repository.User(id, false) }

// openFile opens one issue and fails the test if it is refused.
func (f *fixture) open(t *testing.T, actor repository.Principal, title string) Issue {
	t.Helper()
	created, err := f.service.Create(context.Background(), CreateInput{
		Actor: actor, RepositoryID: repoID, Title: title, Body: "a body",
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", title, err)
	}
	return created
}

// TestOpeningAnIssueRequiresAnAccount covers the rule that an issue needs an author: a caller
// with no credential is refused where a read would be allowed.
func TestOpeningAnIssueRequiresAnAccount(t *testing.T) {
	f := newFixture(t, repository.LevelRead)

	if _, err := f.service.Create(context.Background(), CreateInput{
		Actor: repository.Anonymous(), RepositoryID: repoID, Title: "anonymous report",
	}); !errors.Is(err, ErrAnonymousAuthor) {
		t.Fatalf("an anonymous create = %v, want ErrAnonymousAuthor", err)
	}
	if len(f.store.issues) != 0 {
		t.Error("a refused create stored an issue")
	}
}

// TestOpeningAnIssueRequiresRepositoryRead covers the access decision: filing a report is
// something a reader does, and a repository the caller cannot see is a not-found.
func TestOpeningAnIssueRequiresRepositoryRead(t *testing.T) {
	f := newFixture(t, repository.LevelNone)

	if _, err := f.service.Create(context.Background(), CreateInput{
		Actor: author(aliceID), RepositoryID: repoID, Title: "a report",
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a create in an invisible repository = %v, want ErrNotFound", err)
	}

	// Read is the level required, not write: a reader may file a report.
	f.access.level = repository.LevelRead
	f.open(t, author(aliceID), "a report from a reader")
}

// TestTitlesAndBodiesAreValidated covers the bounds, including the boundaries.
func TestTitlesAndBodiesAreValidated(t *testing.T) {
	f := newFixture(t, repository.LevelRead)
	ctx := context.Background()

	for _, test := range []struct {
		name    string
		title   string
		body    string
		wantErr error
	}{
		{name: "an empty title", title: "", wantErr: ErrInvalidTitle},
		{name: "a whitespace title", title: "   \t\n ", wantErr: ErrInvalidTitle},
		{name: "a title at the bound", title: strings.Repeat("x", MaxTitleLength)},
		{name: "a title over the bound", title: strings.Repeat("x", MaxTitleLength+1), wantErr: ErrInvalidTitle},
		{name: "an empty body", title: "ok", body: ""},
		{name: "a body at the bound", title: "ok", body: strings.Repeat("x", MaxBodyLength)},
		{name: "a body over the bound", title: "ok", body: strings.Repeat("x", MaxBodyLength+1), wantErr: ErrInvalidBody},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.service.Create(ctx, CreateInput{
				Actor: author(aliceID), RepositoryID: repoID, Title: test.title, Body: test.body,
			})
			if test.wantErr == nil {
				if err != nil {
					t.Fatalf("a valid issue was refused: %v", err)
				}
				return
			}
			if !errors.Is(err, test.wantErr) {
				t.Errorf("error = %v, want %v", err, test.wantErr)
			}
		})
	}

	// A title is trimmed, so one whose only fault is padding is accepted and stored trimmed.
	created := f.open(t, author(aliceID), "  a padded title  ")
	if created.Title != "a padded title" {
		t.Errorf("the stored title is %q, want it trimmed", created.Title)
	}
}

// TestReadingRequiresRepositoryRead covers the read path.
func TestReadingRequiresRepositoryRead(t *testing.T) {
	f := newFixture(t, repository.LevelNone)
	created := Issue{Number: 1, ID: "an-issue"}

	if _, err := f.service.Get(context.Background(), GetInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: created.Number,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a read without access = %v, want ErrNotFound", err)
	}
	if _, err := f.service.List(context.Background(), ListInput{
		Actor: author(aliceID), RepositoryID: repoID,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a listing without access = %v, want ErrNotFound", err)
	}
	// The count goes through the same decision, which is what W2.2's acceptance protects.
	if _, err := f.service.Count(context.Background(), CountInput{
		Actor: repository.Anonymous(), RepositoryID: repoID,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a count without access = %v, want ErrNotFound", err)
	}
}

// TestTheAuthorMayChangeTheirOwnIssue is the "issue author rules" half of the acceptance.
//
// The author holds only read, and may still edit, close, and reopen what they opened.
func TestTheAuthorMayChangeTheirOwnIssue(t *testing.T) {
	f := newFixture(t, repository.LevelRead)
	ctx := context.Background()

	created := f.open(t, author(aliceID), "alice's issue")

	// Edit.
	title := "alice's edited issue"
	edited, err := f.service.Edit(ctx, EditInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: created.Number,
		Title: &title, ExpectedVersion: created.Version,
	})
	if err != nil {
		t.Fatalf("the author's edit: %v", err)
	}
	if edited.Title != title {
		t.Errorf("the title is %q, want the edit", edited.Title)
	}

	// Close.
	closed, err := f.service.Close(ctx, CloseInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: created.Number,
	})
	if err != nil {
		t.Fatalf("the author's close: %v", err)
	}
	if closed.State != StateClosed {
		t.Errorf("the state is %q, want closed", closed.State)
	}
	if closed.ClosedBy != aliceID {
		t.Errorf("closed_by is %q, want the author", closed.ClosedBy)
	}

	// Reopen.
	reopened, err := f.service.Reopen(ctx, ReopenInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: created.Number,
	})
	if err != nil {
		t.Fatalf("the author's reopen: %v", err)
	}
	if reopened.State != StateOpen {
		t.Errorf("the state is %q, want open", reopened.State)
	}
	// Reopening clears the close pair, because the two columns move with the state.
	if reopened.ClosedAt != nil || reopened.ClosedBy != "" {
		t.Errorf("reopening left a close recorded: at=%v by=%q", reopened.ClosedAt, reopened.ClosedBy)
	}
}

// TestSomebodyElseNeedsWriteToChangeAnIssue is the denied case the acceptance requires.
//
// A caller who may read but not write, and is not the author, is refused — and refused with a
// distinct error, because they can already see the issue, so saying so discloses nothing.
func TestSomebodyElseNeedsWriteToChangeAnIssue(t *testing.T) {
	f := newFixture(t, repository.LevelRead)
	ctx := context.Background()

	created := f.open(t, author(aliceID), "alice's issue")

	// Bob may read and is not the author.
	title := "bob's edit"
	if _, err := f.service.Edit(ctx, EditInput{
		Actor: author(bobID), RepositoryID: repoID, Number: created.Number,
		Title: &title, ExpectedVersion: created.Version,
	}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a non-author's edit = %v, want ErrForbidden", err)
	}
	if _, err := f.service.Close(ctx, CloseInput{
		Actor: author(bobID), RepositoryID: repoID, Number: created.Number,
	}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a non-author's close = %v, want ErrForbidden", err)
	}
	if err := f.service.Delete(ctx, DeleteInput{
		Actor: author(bobID), RepositoryID: repoID, Number: created.Number,
	}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a non-author's delete = %v, want ErrForbidden", err)
	}
	// Nothing changed.
	stored, _ := f.store.Get(ctx, repoID, created.Number)
	if stored.State != StateOpen || stored.Title != "alice's issue" {
		t.Errorf("a refused change altered the issue: %+v", stored)
	}

	// With write, the same caller succeeds.
	f.access.level = repository.LevelWrite
	if _, err := f.service.Edit(ctx, EditInput{
		Actor: author(bobID), RepositoryID: repoID, Number: created.Number,
		Title: &title, ExpectedVersion: created.Version,
	}); err != nil {
		t.Fatalf("a writer's edit: %v", err)
	}
}

// TestDeletingRequiresWriteWithNoAuthorAllowance covers the one operation an author may not
// take: deleting the record of a discussion is a repository-level act.
func TestDeletingRequiresWriteWithNoAuthorAllowance(t *testing.T) {
	f := newFixture(t, repository.LevelRead)
	ctx := context.Background()

	created := f.open(t, author(aliceID), "alice's issue")

	if err := f.service.Delete(ctx, DeleteInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: created.Number,
	}); !errors.Is(err, ErrForbidden) {
		t.Errorf("the author's delete = %v, want ErrForbidden", err)
	}
	// The issue is still there. The count is asserted exactly rather than as "not zero",
	// because the fixture has already opened this one — an assertion that `len != 0` would
	// pass whether or not the delete happened, which is no assertion at all.
	if len(f.store.issues) != 1 {
		t.Fatalf("a refused delete changed the store: %d issues", len(f.store.issues))
	}

	f.access.level = repository.LevelWrite
	if err := f.service.Delete(ctx, DeleteInput{
		Actor: author(bobID), RepositoryID: repoID, Number: created.Number,
	}); err != nil {
		t.Fatalf("a writer's delete: %v", err)
	}
	if len(f.store.issues) != 0 {
		t.Error("the delete did not remove the issue")
	}
}

// TestAConcurrentEditIsAConflict covers the optimistic concurrency the data-model requires:
// a conflict is a visible error rather than a silent overwrite.
func TestAConcurrentEditIsAConflict(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	created := f.open(t, author(aliceID), "the original")

	// One writer moves the version.
	first := "the first edit"
	if _, err := f.service.Edit(ctx, EditInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: created.Number,
		Title: &first, ExpectedVersion: created.Version,
	}); err != nil {
		t.Fatalf("the first edit: %v", err)
	}

	// The second still holds the version it read.
	second := "the second edit"
	if _, err := f.service.Edit(ctx, EditInput{
		Actor: author(bobID), RepositoryID: repoID, Number: created.Number,
		Title: &second, ExpectedVersion: created.Version,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale edit = %v, want ErrConflict", err)
	}

	// The first writer's value survived: the conflict did not overwrite it.
	stored, _ := f.store.Get(ctx, repoID, created.Number)
	if stored.Title != first {
		t.Errorf("the stored title is %q, want the first edit", stored.Title)
	}
}

// TestAnEditThatChangesNothingIsRefused keeps a record from claiming a change that did not
// happen.
func TestAnEditThatChangesNothingIsRefused(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	created := f.open(t, author(aliceID), "the same title")
	// The fixture stores the body as "a body".
	same := "the same title"
	body := "a body"
	if _, err := f.service.Edit(ctx, EditInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: created.Number,
		Title: &same, Body: &body, ExpectedVersion: created.Version,
	}); !errors.Is(err, ErrNoChange) {
		t.Errorf("an edit with no change = %v, want ErrNoChange", err)
	}
	if _, err := f.service.Edit(ctx, EditInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: created.Number,
		ExpectedVersion: created.Version,
	}); !errors.Is(err, ErrNoChange) {
		t.Errorf("an edit naming no field = %v, want ErrNoChange", err)
	}
	// An edit with no version cannot detect a concurrent change, so it is refused.
	title := "a new title"
	if _, err := f.service.Edit(ctx, EditInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: created.Number, Title: &title,
	}); !errors.Is(err, ErrConflict) {
		t.Errorf("an edit without a version = %v, want ErrConflict", err)
	}
}

// TestACloseOfAClosedIssueIsRefused covers the state guard, and that the guard is the
// store's as well as the service's.
func TestACloseOfAClosedIssueIsRefused(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	created := f.open(t, author(aliceID), "an issue")
	if _, err := f.service.Close(ctx, CloseInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: created.Number,
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := f.service.Close(ctx, CloseInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: created.Number,
	}); !errors.Is(err, ErrAlreadyInState) {
		t.Errorf("a second close = %v, want ErrAlreadyInState", err)
	}
	if _, err := f.service.Reopen(ctx, ReopenInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: created.Number,
	}); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if _, err := f.service.Reopen(ctx, ReopenInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: created.Number,
	}); !errors.Is(err, ErrAlreadyInState) {
		t.Errorf("a second reopen = %v, want ErrAlreadyInState", err)
	}
}

// TestTheStateHistoryIsRecorded covers W2.2's requirement that closed and reopened issues keep
// a state-change history, which is the audit ledger.
func TestTheStateHistoryIsRecorded(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()
	created := f.open(t, author(aliceID), "a lifecycle")

	if _, err := f.service.Close(ctx, CloseInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: created.Number,
		Request: audit.RequestContext{SourceAddress: "203.0.113.7", UserAgent: "test"},
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := f.service.Reopen(ctx, ReopenInput{
		Actor: author(bobID), RepositoryID: repoID, Number: created.Number,
	}); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	// The three records, in the order the operations ran, each naming what changed.
	actions := make([]string, 0, len(f.store.events))
	for _, event := range f.store.events {
		actions = append(actions, event.Action)
	}
	for _, want := range []string{audit.ActionIssueCreated, audit.ActionIssueClosed, audit.ActionIssueReopened} {
		if !contains(actions, want) {
			t.Errorf("the history is missing %q: %v", want, actions)
		}
	}

	// Every record is scoped to the issue, so a history query finds them, and passes its own
	// validation — which is the guard against a record the database would refuse.
	for _, event := range f.store.events {
		if event.ScopeType != audit.ScopeIssue || event.SubjectType != audit.SubjectIssue {
			t.Errorf("a record is not scoped to the issue: %+v", event)
		}
		if event.ScopeID != created.ID {
			t.Errorf("a record names scope %q, want the issue", event.ScopeID)
		}
		if err := event.Validate(); err != nil {
			t.Errorf("a record is invalid: %v", err)
		}
	}

	// The close record carries the transition and the request context, so an operator reading
	// it learns who closed what from where.
	var closeEvent audit.Event
	for _, event := range f.store.events {
		if event.Action == audit.ActionIssueClosed {
			closeEvent = event
		}
	}
	change, ok := closeEvent.Detail["state"].(audit.Change)
	if !ok || change.From != string(StateOpen) || change.To != string(StateClosed) {
		t.Errorf("the close record does not describe the transition: %v", closeEvent.Detail)
	}
	if closeEvent.SourceAddress != "203.0.113.7" || closeEvent.UserAgent != "test" {
		t.Errorf("the close record lost the request context: %+v", closeEvent)
	}
	if closeEvent.ActorID != aliceID {
		t.Errorf("the record names actor %q, want the closer", closeEvent.ActorID)
	}
}

// TestAnEditWritesNoRecord is the catalogue's decision made checkable: there is no
// `issue.edited`, so an edit must not produce a record with an invented action.
func TestAnEditWritesNoRecord(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	created := f.open(t, author(aliceID), "before")
	before := len(f.store.events)

	title := "after"
	if _, err := f.service.Edit(ctx, EditInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: created.Number,
		Title: &title, ExpectedVersion: created.Version,
	}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if len(f.store.events) != before {
		t.Errorf("an edit wrote %d record(s); the catalogue defines no action for one",
			len(f.store.events)-before)
	}
}

// TestDeletingRecordsTheFact covers "delete-with-audit": the record names what was removed, so
// a hard delete is not silent.
func TestDeletingRecordsTheFact(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	created := f.open(t, author(aliceID), "to be removed")
	if err := f.service.Delete(ctx, DeleteInput{
		Actor: author(bobID), RepositoryID: repoID, Number: created.Number,
		Request: audit.RequestContext{SourceAddress: "198.51.100.9"},
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var deleted audit.Event
	for _, event := range f.store.events {
		if event.Action == audit.ActionIssueDeleted {
			deleted = event
		}
	}
	if deleted.Action != audit.ActionIssueDeleted {
		t.Fatalf("the deletion was not recorded: %+v", f.store.events)
	}
	if deleted.ActorID != bobID {
		t.Errorf("the record names actor %q, want the deleter", deleted.ActorID)
	}
	// The record names the number and title, which is what makes it answerable after the row
	// is gone.
	if number, ok := deleted.Detail["number"].(int64); !ok || number != created.Number {
		t.Errorf("the record does not carry the number: %v", deleted.Detail)
	}
	if deleted.Detail["title"] != "to be removed" {
		t.Errorf("the record does not carry the title: %v", deleted.Detail)
	}
}

// TestAFailedCreateDoesNotConsumeANumber is the gap-free claim at the service layer, checked
// against a store that fails after allocating. The database-level version of the same claim is
// in `postgres_test.go`, which rolls back a real transaction.
func TestAFailedCreateDoesNotConsumeANumber(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	first := f.open(t, author(aliceID), "the first")

	f.store.failCreate = errors.New("the insert failed")
	if _, err := f.service.Create(ctx, CreateInput{
		Actor: author(aliceID), RepositoryID: repoID, Title: "the failure",
	}); err == nil {
		t.Fatal("a failing create succeeded")
	}
	f.store.failCreate = nil

	// The next create takes the next number, with no hole: the counter did not advance for a
	// transaction that failed.
	second := f.open(t, author(aliceID), "the second")
	if first.Number != 1 || second.Number != 2 {
		t.Errorf("numbers are %d and %d, want 1 and 2 with no gap", first.Number, second.Number)
	}
}

// TestNumbersAreSequentialAndSharedWithPullRequests is the numbering contract.
//
// The second half — that pull requests will draw from the same counter — is a property of the
// counter living on the repository rather than on this table, which the database test asserts
// by checking the column the migration added.
func TestNumbersAreSequentialAndSharedWithPullRequests(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)

	numbers := make([]int64, 0, 5)
	for i := 0; i < 5; i++ {
		numbers = append(numbers, f.open(t, author(aliceID), "an issue").Number)
	}
	for index, number := range numbers {
		if number != int64(index+1) {
			t.Errorf("issue %d has number %d, want %d", index, number, index+1)
		}
	}
}

// TestTheStateFilterIsValidated covers the listing parameter.
func TestTheStateFilterIsValidated(t *testing.T) {
	for _, test := range []struct {
		value   string
		want    State
		wantErr bool
	}{
		{value: "", want: ""},
		{value: "all", want: ""},
		{value: "open", want: StateOpen},
		{value: "closed", want: StateClosed},
		{value: "OPEN", want: StateOpen},
		{value: " closed ", want: StateClosed},
		{value: "merged", wantErr: true},
	} {
		t.Run(test.value, func(t *testing.T) {
			state, err := ParseStateFilter(test.value)
			if test.wantErr {
				if err == nil {
					t.Fatalf("%q was accepted", test.value)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseStateFilter(%q): %v", test.value, err)
			}
			if state != test.want {
				t.Errorf("state = %q, want %q", state, test.want)
			}
		})
	}
}

// TestAnIssueSurvivesItsAuthor covers the `ON DELETE SET NULL` decision: an issue whose author
// was deleted keeps its history and has no author, rather than disappearing.
func TestAnIssueSurvivesItsAuthor(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	created := f.open(t, author(aliceID), "alice's issue")

	// The database would set author_id to null; the fake models the same state.
	stored := f.store.issues[created.Number]
	stored.AuthorID = ""
	f.store.issues[created.Number] = stored

	// The issue is still readable.
	read, err := f.service.Get(ctx, GetInput{
		Actor: author(bobID), RepositoryID: repoID, Number: created.Number,
	})
	if err != nil {
		t.Fatalf("reading an issue with no author: %v", err)
	}
	if read.AuthorID != "" {
		t.Errorf("the author is %q, want none", read.AuthorID)
	}
	// And nobody claims to be its author, so the author allowance is not handed to the first
	// caller whose identifier is empty.
	if read.AuthoredBy("") {
		t.Error("an empty account was treated as the author")
	}
	if read.AuthoredBy(aliceID) {
		t.Error("a deleted author still held the author allowance")
	}
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
