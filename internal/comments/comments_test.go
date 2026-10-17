package comments

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/repository"
)

// The service rules.
//
// These run against a fake store and a fake access decision, which is what makes the rules testable
// in isolation. The properties that need a real database — that an edit appends its revision
// atomically, that a deletion removes the history, and that ordering is stable under concurrency —
// are asserted in `postgres_test.go`.

const (
	repoID    = "01920000-0000-7000-8000-0000000000a1"
	issueID   = "01920000-0000-7000-8000-0000000000a2"
	commentID = "01920000-0000-7000-8000-0000000000a3"
	aliceID   = "01920000-0000-7000-8000-0000000000a4"
	bobID     = "01920000-0000-7000-8000-0000000000a5"
)

// fakeAccess answers from a level the test sets, collapsing "no access" into not-found the way the
// repository service does.
type fakeAccess struct {
	level repository.Level
}

func (f *fakeAccess) Require(_ context.Context, _ repository.Principal, _ string, required repository.Level) (repository.Repository, repository.Level, error) {
	if f.level == repository.LevelNone {
		return repository.Repository{}, repository.LevelNone, repository.ErrNotFound
	}
	if !f.level.Allows(required) {
		return repository.Repository{}, f.level, repository.ErrForbidden
	}
	return repository.Repository{ID: repoID, Name: "a-repo"}, f.level, nil
}

// fakeStore is an in-memory Store, with the atomicity the database implements.
type fakeStore struct {
	comments  map[string]Comment
	revisions map[string][]Revision
	// events records what the service asked to be written.
	events []auditEvent
	// editErr and deleteErr force a refusal.
	editErr   error
	deleteErr error
}

// auditEvent is the part of a record these tests assert on.
type auditEvent struct {
	Action    string
	ScopeType string
	ScopeID   string
	Detail    map[string]any
}

func newFakeStore() *fakeStore {
	return &fakeStore{comments: map[string]Comment{}, revisions: map[string][]Revision{}}
}

func (f *fakeStore) Create(_ context.Context, input StoreCreate) (Comment, error) {
	// The fake applies the same rule the database's CHECK enforces, so a test cannot pass here and
	// fail against PostgreSQL.
	created := Comment{
		ID: input.ID, IssueID: input.IssueID, AuthorID: input.AuthorID, AuthorName: "a person",
		Body: input.Body, CreatedAt: time.Now().UTC(), Version: 1,
	}
	f.comments[input.ID] = created
	// The number is derived, so it is read back through the same path a caller would use rather than
	// being assigned here — which is what keeps the fake from having a second numbering rule.
	numbered, err := f.GetByNumber(context.Background(), "", input.IssueID, f.positionOf(input.IssueID, input.ID))
	if err != nil {
		return Comment{}, err
	}
	return numbered, nil
}

// positionOf returns where a comment sits in its issue's order.
func (f *fakeStore) positionOf(issueID, commentID string) int {
	for index, comment := range f.ordered(issueID) {
		if comment.ID == commentID {
			return index + 1
		}
	}
	return 0
}

func (f *fakeStore) Get(_ context.Context, query StoreGet) (Comment, error) {
	comment, found := f.comments[query.CommentID]
	if !found || comment.IssueID != query.IssueID {
		return Comment{}, ErrNotFound
	}
	return comment, nil
}

// ordered returns one issue's comments in the order the database produces: by creation time, then by
// identifier.
//
// **It sorts deterministically, and that is what keeps the fake faithful.** A previous version counted
// positions by iterating a Go map, whose order is deliberately randomised — so the position a creation
// reported and the position a read resolved could disagree, and the suite failed intermittently
// depending on which order the runtime chose. A fake that is only sometimes right is worse than no
// fake, because it teaches a reader to distrust the suite.
func (f *fakeStore) ordered(issueID string) []Comment {
	ordered := []Comment{}
	for _, comment := range f.comments {
		if comment.IssueID == issueID {
			ordered = append(ordered, comment)
		}
	}
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].CreatedAt.Equal(ordered[j].CreatedAt) {
			return ordered[i].CreatedAt.Before(ordered[j].CreatedAt)
		}
		return ordered[i].ID < ordered[j].ID
	})
	return ordered
}

// GetByNumber returns the comment at a position, applying the same rank rule the database's window
// function applies.
func (f *fakeStore) GetByNumber(_ context.Context, _ string, issueID string, number int) (Comment, error) {
	ordered := f.ordered(issueID)
	if number < 1 || number > len(ordered) {
		return Comment{}, ErrNotFound
	}
	return ordered[number-1], nil
}
func (f *fakeStore) List(_ context.Context, query StoreList) (Page, error) {
	page := Page{Comments: []Comment{}}
	for _, comment := range f.comments {
		if comment.IssueID == query.IssueID {
			page.Comments = append(page.Comments, comment)
		}
	}
	return page, nil
}

func (f *fakeStore) Edit(_ context.Context, input StoreEdit) (Comment, error) {
	if f.editErr != nil {
		return Comment{}, f.editErr
	}
	comment, found := f.comments[input.CommentID]
	if !found {
		return Comment{}, ErrNotFound
	}
	if comment.Deleted {
		return Comment{}, ErrDeleted
	}
	if comment.Version != input.ExpectedVersion {
		return Comment{}, ErrConflict
	}

	// The revision is appended before the body moves, which is the order the database's transaction
	// makes unobservable from outside.
	revision := input.Revision
	revision.Revision = len(f.revisions[input.CommentID]) + 1
	f.revisions[input.CommentID] = append(f.revisions[input.CommentID], revision)

	comment.Body = input.Body
	comment.Version++
	at := time.Now().UTC()
	comment.EditedAt = &at
	f.comments[input.CommentID] = comment
	return comment, nil
}

func (f *fakeStore) Delete(_ context.Context, input StoreDelete) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	comment, found := f.comments[input.CommentID]
	if !found {
		return ErrNotFound
	}
	if comment.Deleted {
		return ErrDeleted
	}
	// The body and the history go together, which is what makes "deleted" mean the words are gone.
	comment.Body = ""
	comment.Deleted = true
	comment.DeletedBy = input.DeletedBy
	f.comments[input.CommentID] = comment
	delete(f.revisions, input.CommentID)
	f.events = append(f.events, auditEvent{
		Action: input.Event.Action, ScopeType: string(input.Event.ScopeType),
		ScopeID: input.Event.ScopeID, Detail: input.Event.Detail,
	})
	return nil
}

func (f *fakeStore) History(_ context.Context, commentID string) ([]Revision, error) {
	return f.revisions[commentID], nil
}

func (f *fakeStore) Count(_ context.Context, issueID string) (int, error) {
	count := 0
	for _, comment := range f.comments {
		if comment.IssueID == issueID {
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

// write posts one comment as the given account and fails the test if it is refused.
func (f *fixture) write(t *testing.T, actor repository.Principal, body string) Comment {
	t.Helper()
	comment, err := f.service.Create(context.Background(), CreateInput{
		Actor: actor, RepositoryID: repoID, IssueID: issueID, Body: body,
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", body, err)
	}
	return comment
}

// TestWritingACommentRequiresAnAccount covers the rule that a comment needs an author.
func TestWritingACommentRequiresAnAccount(t *testing.T) {
	f := newFixture(t, repository.LevelRead)

	if _, err := f.service.Create(context.Background(), CreateInput{
		Actor: repository.Anonymous(), RepositoryID: repoID, IssueID: issueID, Body: "hello",
	}); !errors.Is(err, ErrAnonymousAuthor) {
		t.Fatalf("an anonymous comment = %v, want ErrAnonymousAuthor", err)
	}
	if len(f.store.comments) != 0 {
		t.Error("a refused comment was stored")
	}
}

// TestWritingACommentRequiresRepositoryRead covers the access decision: commenting is something a
// reader does.
func TestWritingACommentRequiresRepositoryRead(t *testing.T) {
	f := newFixture(t, repository.LevelNone)

	if _, err := f.service.Create(context.Background(), CreateInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, Body: "hello",
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a comment in an invisible repository = %v, want ErrNotFound", err)
	}

	f.access.level = repository.LevelRead
	f.write(t, author(aliceID), "a comment from a reader")
}

// TestTheBodyBoundsAreEnforced covers the bounds, including the boundaries.
func TestTheBodyBoundsAreEnforced(t *testing.T) {
	f := newFixture(t, repository.LevelRead)
	ctx := context.Background()

	for _, test := range []struct {
		name    string
		body    string
		wantErr error
	}{
		{name: "empty", body: "", wantErr: ErrEmptyBody},
		{name: "whitespace only", body: "   \t\n ", wantErr: ErrEmptyBody},
		{name: "at the bound", body: strings.Repeat("x", MaxBodyLength)},
		{name: "over the bound", body: strings.Repeat("x", MaxBodyLength+1), wantErr: ErrBodyTooLong},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.service.Create(ctx, CreateInput{
				Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, Body: test.body,
			})
			if test.wantErr == nil {
				if err != nil {
					t.Fatalf("a valid comment was refused: %v", err)
				}
				return
			}
			if !errors.Is(err, test.wantErr) {
				t.Errorf("error = %v, want %v", err, test.wantErr)
			}
		})
	}

	// A body is **not** trimmed: whitespace inside it is content, and removing a trailing newline
	// changes what Markdown renders.
	comment := f.write(t, author(aliceID), "  indented and trailing  \n")
	if comment.Body != "  indented and trailing  \n" {
		t.Errorf("the stored body is %q, want it untouched", comment.Body)
	}
}

// TestReadingRequiresRepositoryRead is W2.3's acceptance about listing and fetching.
func TestReadingRequiresRepositoryRead(t *testing.T) {
	f := newFixture(t, repository.LevelNone)
	ctx := context.Background()

	if _, err := f.service.Get(ctx, GetInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, CommentID: commentID,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a fetch without access = %v, want ErrNotFound", err)
	}
	if _, err := f.service.List(ctx, ListInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a listing without access = %v, want ErrNotFound", err)
	}
	// The count goes through the same decision, which is what stops a caller inferring comment
	// counts from a repository they cannot read.
	if _, err := f.service.Count(ctx, CountInput{
		Actor: repository.Anonymous(), RepositoryID: repoID, IssueID: issueID,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a count without access = %v, want ErrNotFound", err)
	}
	if _, err := f.service.History(ctx, HistoryInput{
		Actor: repository.Anonymous(), RepositoryID: repoID, IssueID: issueID, CommentID: commentID,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a history without access = %v, want ErrNotFound", err)
	}
}

// TestTheAuthorMayEditTheirOwnComment is the author half of the permission rule.
func TestTheAuthorMayEditTheirOwnComment(t *testing.T) {
	f := newFixture(t, repository.LevelRead)
	ctx := context.Background()

	comment := f.write(t, author(aliceID), "the original")

	edited, err := f.service.Edit(ctx, EditInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, CommentID: comment.ID,
		Body: "the revision", ExpectedVersion: comment.Version,
	})
	if err != nil {
		t.Fatalf("the author's edit: %v", err)
	}
	if edited.Body != "the revision" {
		t.Errorf("the body is %q, want the edit", edited.Body)
	}
	if !edited.Edited() {
		t.Error("the comment does not report that it was edited")
	}
}

// TestSomebodyElseNeedsWriteToEdit covers the denied case, and that the refusal is distinct from
// absence.
func TestSomebodyElseNeedsWriteToEdit(t *testing.T) {
	f := newFixture(t, repository.LevelRead)
	ctx := context.Background()

	comment := f.write(t, author(aliceID), "alice's comment")

	// Bob may read and is not the author.
	for _, test := range []struct {
		name string
		call func() error
	}{
		{
			name: "edit",
			call: func() error {
				_, err := f.service.Edit(ctx, EditInput{
					Actor: author(bobID), RepositoryID: repoID, IssueID: issueID,
					CommentID: comment.ID, Body: "bob's edit", ExpectedVersion: comment.Version,
				})
				return err
			},
		},
		{
			name: "delete",
			call: func() error {
				return f.service.Delete(ctx, DeleteInput{
					Actor: author(bobID), RepositoryID: repoID, IssueID: issueID, CommentID: comment.ID,
				})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			// **Forbidden, not not-found**: the caller's level was resolved, so they can already see
			// the repository, and telling them their level is too low discloses nothing.
			if err := test.call(); !errors.Is(err, ErrForbidden) {
				t.Errorf("a non-author's %s = %v, want ErrForbidden", test.name, err)
			}
		})
	}

	// Nothing changed.
	stored := f.store.comments[comment.ID]
	if stored.Body != "alice's comment" || stored.Deleted {
		t.Errorf("a refused change altered the comment: %+v", stored)
	}

	// With write, the same caller succeeds — which is the catalogue's "moderator".
	f.access.level = repository.LevelWrite
	if _, err := f.service.Edit(ctx, EditInput{
		Actor: author(bobID), RepositoryID: repoID, IssueID: issueID,
		CommentID: comment.ID, Body: "a moderator's edit", ExpectedVersion: stored.Version,
	}); err != nil {
		t.Fatalf("a writer's edit: %v", err)
	}
}

// TestTheAuthorMayDeleteTheirOwnComment covers the deletion allowance, which is a comment's author
// or a moderator.
func TestTheAuthorMayDeleteTheirOwnComment(t *testing.T) {
	f := newFixture(t, repository.LevelRead)
	ctx := context.Background()

	comment := f.write(t, author(aliceID), "alice's comment")
	if err := f.service.Delete(ctx, DeleteInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, CommentID: comment.ID,
	}); err != nil {
		t.Fatalf("the author's delete: %v", err)
	}

	stored := f.store.comments[comment.ID]
	if !stored.Deleted {
		t.Error("the comment is not marked deleted")
	}
	if stored.Body != "" {
		t.Errorf("the body survived the deletion: %q", stored.Body)
	}
	if len(f.store.revisions[comment.ID]) != 0 {
		t.Error("the history survived the deletion")
	}
}

// TestEditingWritesARevisionAndNoAuditRecord is the record shape the package documents.
func TestEditingWritesARevisionAndNoAuditRecord(t *testing.T) {
	f := newFixture(t, repository.LevelRead)
	ctx := context.Background()

	comment := f.write(t, author(aliceID), "the original")
	before := len(f.store.events)

	if _, err := f.service.Edit(ctx, EditInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, CommentID: comment.ID,
		Body: "the edit", ExpectedVersion: comment.Version,
	}); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	// **A revision holds the body from before the edit**, which is what "records the previous body"
	// means.
	history := f.store.revisions[comment.ID]
	if len(history) != 1 {
		t.Fatalf("the history holds %d revisions, want 1", len(history))
	}
	if history[0].Body != "the original" {
		t.Errorf("the revision holds %q, want the previous body", history[0].Body)
	}
	if history[0].EditorID != aliceID {
		t.Errorf("the revision names editor %q, want the editor", history[0].EditorID)
	}
	if history[0].Revision != 1 {
		t.Errorf("the revision is numbered %d, want 1", history[0].Revision)
	}

	// And the catalogue defines no `comment.edited`, so no ledger record was written.
	if len(f.store.events) != before {
		t.Errorf("an edit wrote %d record(s); the catalogue defines no action for one",
			len(f.store.events)-before)
	}
}

// TestASecondEditAppendsRatherThanReplaces is the append-only property.
func TestASecondEditAppendsRatherThanReplaces(t *testing.T) {
	f := newFixture(t, repository.LevelRead)
	ctx := context.Background()

	comment := f.write(t, author(aliceID), "one")
	first, err := f.service.Edit(ctx, EditInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, CommentID: comment.ID,
		Body: "two", ExpectedVersion: comment.Version,
	})
	if err != nil {
		t.Fatalf("the first edit: %v", err)
	}
	if _, err := f.service.Edit(ctx, EditInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, CommentID: comment.ID,
		Body: "three", ExpectedVersion: first.Version,
	}); err != nil {
		t.Fatalf("the second edit: %v", err)
	}

	history := f.store.revisions[comment.ID]
	if len(history) != 2 {
		t.Fatalf("the history holds %d revisions, want 2", len(history))
	}
	// Oldest first, each holding the text as it was before that edit.
	if history[0].Body != "one" || history[1].Body != "two" {
		t.Errorf("the history is %q then %q, want the bodies as they were", history[0].Body, history[1].Body)
	}
	// Numbering is a sequence, not a set of snapshots.
	if history[0].Revision != 1 || history[1].Revision != 2 {
		t.Errorf("the numbering is %d then %d, want 1 then 2",
			history[0].Revision, history[1].Revision)
	}
}

// TestDeletingWritesTheOneCommentRecord covers the audit half: the catalogue defines exactly
// `comment.deleted`, and the deletion is what writes it.
func TestDeletingWritesTheOneCommentRecord(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	comment := f.write(t, author(aliceID), "to be removed")
	if err := f.service.Delete(ctx, DeleteInput{
		Actor: author(bobID), RepositoryID: repoID, IssueID: issueID, CommentID: comment.ID,
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if len(f.store.events) != 1 {
		t.Fatalf("the deletion wrote %d records, want 1: %+v", len(f.store.events), f.store.events)
	}
	event := f.store.events[0]
	if event.Action != "comment.deleted" {
		t.Errorf("the record's action is %q, want comment.deleted", event.Action)
	}
	// The scope and subject are the comment, which is what the catalogue lists.
	if event.ScopeType != "comment" || event.ScopeID != comment.ID {
		t.Errorf("the record is scoped %q/%q, want the comment", event.ScopeType, event.ScopeID)
	}
	// The record names the issue, which is what makes it answerable after the text is gone.
	if event.Detail["issue_id"] != issueID {
		t.Errorf("the record does not name the issue: %v", event.Detail)
	}
}

// TestAConcurrentEditIsAConflict covers the optimistic concurrency.
func TestAConcurrentEditIsAConflict(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	comment := f.write(t, author(aliceID), "the original")

	if _, err := f.service.Edit(ctx, EditInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, CommentID: comment.ID,
		Body: "the first edit", ExpectedVersion: comment.Version,
	}); err != nil {
		t.Fatalf("the first edit: %v", err)
	}
	if _, err := f.service.Edit(ctx, EditInput{
		Actor: author(bobID), RepositoryID: repoID, IssueID: issueID, CommentID: comment.ID,
		Body: "the stale edit", ExpectedVersion: comment.Version,
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a stale edit = %v, want ErrConflict", err)
	}

	// The first writer's value survived, and one revision was appended rather than two.
	stored := f.store.comments[comment.ID]
	if stored.Body != "the first edit" {
		t.Errorf("the stored body is %q, want the first edit", stored.Body)
	}
	if len(f.store.revisions[comment.ID]) != 1 {
		t.Errorf("the history holds %d revisions, want 1", len(f.store.revisions[comment.ID]))
	}
}

// TestAnEditThatChangesNothingIsRefused keeps a history entry from recording an edit that did not
// happen.
func TestAnEditThatChangesNothingIsRefused(t *testing.T) {
	f := newFixture(t, repository.LevelRead)
	ctx := context.Background()

	comment := f.write(t, author(aliceID), "the same text")
	if _, err := f.service.Edit(ctx, EditInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, CommentID: comment.ID,
		Body: "the same text", ExpectedVersion: comment.Version,
	}); !errors.Is(err, ErrNoChange) {
		t.Errorf("an edit with no change = %v, want ErrNoChange", err)
	}
	if len(f.store.revisions[comment.ID]) != 0 {
		t.Error("an edit with no change appended a revision")
	}
	// An edit with no version cannot detect a concurrent change, so it is refused.
	if _, err := f.service.Edit(ctx, EditInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, CommentID: comment.ID,
		Body: "a new body",
	}); !errors.Is(err, ErrConflict) {
		t.Errorf("an edit without a version = %v, want ErrConflict", err)
	}
}

// TestADeletedCommentCannotBeEditedOrDeletedAgain covers the deleted state.
func TestADeletedCommentCannotBeEditedOrDeletedAgain(t *testing.T) {
	f := newFixture(t, repository.LevelRead)
	ctx := context.Background()

	comment := f.write(t, author(aliceID), "to be removed")
	if err := f.service.Delete(ctx, DeleteInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, CommentID: comment.ID,
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := f.service.Edit(ctx, EditInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, CommentID: comment.ID,
		Body: "resurrected", ExpectedVersion: 2,
	}); !errors.Is(err, ErrDeleted) {
		t.Errorf("editing a deleted comment = %v, want ErrDeleted", err)
	}
	// Deleting twice is refused rather than accepted silently, so a client that lost track of the
	// state is told rather than being told it succeeded.
	if err := f.service.Delete(ctx, DeleteInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, CommentID: comment.ID,
	}); !errors.Is(err, ErrDeleted) {
		t.Errorf("deleting twice = %v, want ErrDeleted", err)
	}
	// And exactly one record was written, because the second attempt was refused.
	if len(f.store.events) != 1 {
		t.Errorf("the ledger holds %d records, want 1", len(f.store.events))
	}
}

// TestTheHistoryIsReadable covers the read path for a comment's revisions.
func TestTheHistoryIsReadable(t *testing.T) {
	f := newFixture(t, repository.LevelRead)
	ctx := context.Background()

	comment := f.write(t, author(aliceID), "the original")
	edited, err := f.service.Edit(ctx, EditInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, CommentID: comment.ID,
		Body: "the edit", ExpectedVersion: comment.Version,
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}

	history, err := f.service.History(ctx, HistoryInput{
		Actor: author(bobID), RepositoryID: repoID, IssueID: issueID, CommentID: edited.ID,
	})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 1 || history[0].Body != "the original" {
		t.Errorf("the history is %+v, want the previous body", history)
	}

	// A caller who cannot read the repository cannot read the history either.
	f.access.level = repository.LevelNone
	if _, err := f.service.History(ctx, HistoryInput{
		Actor: author(bobID), RepositoryID: repoID, IssueID: issueID, CommentID: edited.ID,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a history without access = %v, want ErrNotFound", err)
	}
}

// TestTheCountIncludesDeletedComments keeps the count agreeing with the page: a reader sees a
// tombstone, so omitting it would make the number wrong.
func TestTheCountIncludesDeletedComments(t *testing.T) {
	f := newFixture(t, repository.LevelRead)
	ctx := context.Background()

	first := f.write(t, author(aliceID), "one")
	f.write(t, author(aliceID), "two")
	if err := f.service.Delete(ctx, DeleteInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, CommentID: first.ID,
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	count, err := f.service.Count(ctx, CountInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID,
	})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 2 {
		t.Errorf("the count is %d, want 2 — a deleted comment still occupies its place", count)
	}
}
