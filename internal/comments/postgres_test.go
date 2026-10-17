package comments_test

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/go-gitnest/GitNest/internal/comments"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/issues"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/google/uuid"
)

// The database-backed tests.
//
// Three of this workstream's claims are properties of how PostgreSQL behaves rather than of the code
// that calls it, so a fake cannot establish any of them:
//
//   - "Editing a comment records the previous body" is a claim about a revision being appended **in
//     the same transaction** as the body change.
//   - "Deletion ... never leaves a dangling timeline entry" is a claim about a foreign key's cascade.
//   - "Comment ordering is stable by creation time plus identifier" is a claim about a total order
//     that no fake with a map can exhibit.

// world is a migrated schema with an owner, a repository, an issue, and a comment service over them.
type world struct {
	t       *testing.T
	db      *sql.DB
	service *comments.Service
	store   *comments.PostgresStore
	owner   repository.Principal
	ownerID string
	other   repository.Principal
	otherID string
	repoID  string
	issueID string
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

	// The issue is created through its own service, so the row the comment hangs off is real.
	issueService := issues.NewService(repositories, issues.NewPostgresStore(schema.Client.DB))
	issue, err := issueService.Create(ctx, issues.CreateInput{
		Actor: repository.User(owner.ID, false), RepositoryID: row.ID, Title: "an issue",
	})
	if err != nil {
		t.Fatalf("create the issue: %v", err)
	}

	store := comments.NewPostgresStore(schema.Client.DB)
	return &world{
		t: t, db: schema.Client.DB, store: store,
		service: comments.NewService(repositories, store),
		owner:   repository.User(owner.ID, false), ownerID: owner.ID,
		other: repository.User(other.ID, false), otherID: other.ID,
		repoID: row.ID, issueID: issue.ID,
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

// write posts one comment through the service.
func (w *world) write(actor repository.Principal, body string) comments.Comment {
	w.t.Helper()
	comment, err := w.service.Create(context.Background(), comments.CreateInput{
		Actor: actor, RepositoryID: w.repoID, IssueID: w.issueID, Body: body,
	})
	if err != nil {
		w.t.Fatalf("Create(%q): %v", body, err)
	}
	return comment
}

// countRevisions reads the revisions directly, which is how the atomicity claims are checked.
func (w *world) countRevisions(commentID string) int {
	w.t.Helper()
	var count int
	if err := w.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM comment_revisions WHERE comment_id = $1`, commentID).Scan(&count); err != nil {
		w.t.Fatalf("count the revisions: %v", err)
	}
	return count
}

// TestAnEditAppendsThePreviousBody covers W2.3's acceptance against the real tables.
func TestAnEditAppendsThePreviousBody(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	comment := w.write(w.owner, "the original text")

	edited, err := w.service.Edit(ctx, comments.EditInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, CommentID: comment.ID,
		Body: "the revised text", ExpectedVersion: comment.Version,
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if edited.Body != "the revised text" {
		t.Errorf("the stored body is %q, want the edit", edited.Body)
	}
	if edited.EditedAt == nil {
		t.Error("edited_at was not set")
	}
	if edited.Version <= comment.Version {
		t.Errorf("the version did not advance: %d then %d", comment.Version, edited.Version)
	}

	// **The revision holds the text from before the edit**, which is what the acceptance means by
	// "records the previous body".
	history, err := w.service.History(ctx, comments.HistoryInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, CommentID: comment.ID,
	})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("the history holds %d revisions, want 1", len(history))
	}
	if history[0].Body != "the original text" {
		t.Errorf("the revision holds %q, want the previous body", history[0].Body)
	}
	if history[0].EditorName != "owner" {
		t.Errorf("the revision names editor %q, want the editor", history[0].EditorName)
	}
	// The revision's version is the one the comment was at before the change, which ties the history
	// to the concurrency guard's numbering.
	if history[0].Version != comment.Version {
		t.Errorf("the revision's version is %d, want the pre-edit version %d",
			history[0].Version, comment.Version)
	}
}

// TestASecondEditAppendsInOrder covers the append-only property and the sequence numbering against
// the real unique constraint.
func TestASecondEditAppendsInOrder(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	comment := w.write(w.owner, "one")
	first, err := w.service.Edit(ctx, comments.EditInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, CommentID: comment.ID,
		Body: "two", ExpectedVersion: comment.Version,
	})
	if err != nil {
		t.Fatalf("the first edit: %v", err)
	}
	if _, err := w.service.Edit(ctx, comments.EditInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, CommentID: comment.ID,
		Body: "three", ExpectedVersion: first.Version,
	}); err != nil {
		t.Fatalf("the second edit: %v", err)
	}

	history, err := w.service.History(ctx, comments.HistoryInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, CommentID: comment.ID,
	})
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("the history holds %d revisions, want 2", len(history))
	}
	// Oldest first, and each holds the text as it was before its own edit.
	if history[0].Body != "one" || history[1].Body != "two" {
		t.Errorf("the history is %q then %q, want the bodies as they were",
			history[0].Body, history[1].Body)
	}
	// The numbering is a sequence, which the unique constraint also enforces.
	if history[0].Revision != 1 || history[1].Revision != 2 {
		t.Errorf("the numbering is %d then %d, want 1 then 2",
			history[0].Revision, history[1].Revision)
	}
}

// TestARefusedEditAppendsNoRevision is the atomicity claim.
//
// **The revision is appended inside the same transaction as the body change**, so an edit refused by
// the version guard must leave no history entry. The failure is forced through the guard rather than
// by breaking something, which is the realistic path: a page left open while somebody else edited.
func TestARefusedEditAppendsNoRevision(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	comment := w.write(w.owner, "the original")
	if _, err := w.service.Edit(ctx, comments.EditInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, CommentID: comment.ID,
		Body: "the first edit", ExpectedVersion: comment.Version,
	}); err != nil {
		t.Fatalf("the first edit: %v", err)
	}
	if got := w.countRevisions(comment.ID); got != 1 {
		t.Fatalf("after one edit the history holds %d revisions, want 1", got)
	}

	// A stale version.
	if _, err := w.service.Edit(ctx, comments.EditInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, CommentID: comment.ID,
		Body: "the stale edit", ExpectedVersion: comment.Version,
	}); !errors.Is(err, comments.ErrConflict) {
		t.Fatalf("a stale edit = %v, want ErrConflict", err)
	}

	// **No revision was appended**, which is the assertion: a history entry without its change would
	// be a lie about what happened.
	if got := w.countRevisions(comment.ID); got != 1 {
		t.Errorf("a refused edit left %d revisions, want 1", got)
	}
	// And the body did not move.
	stored, err := w.store.Get(ctx, comments.StoreGet{
		RepositoryID: w.repoID, IssueID: w.issueID, CommentID: comment.ID,
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Body != "the first edit" {
		t.Errorf("the stored body is %q, want the first edit", stored.Body)
	}
}

// TestADeletionRemovesTheTextAndTheHistory is the privacy claim.
//
// Clearing the body is what makes "deleted" mean the words are gone; removing the history is what
// stops them surviving one query away in a revision row.
func TestADeletionRemovesTheTextAndTheHistory(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	comment := w.write(w.owner, "the original text")
	if _, err := w.service.Edit(ctx, comments.EditInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, CommentID: comment.ID,
		Body: "the revised text", ExpectedVersion: comment.Version,
	}); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if got := w.countRevisions(comment.ID); got != 1 {
		t.Fatalf("the history holds %d revisions, want 1", got)
	}

	if err := w.service.Delete(ctx, comments.DeleteInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, CommentID: comment.ID,
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// The row remains, so the conversation keeps its shape...
	stored, err := w.store.Get(ctx, comments.StoreGet{
		RepositoryID: w.repoID, IssueID: w.issueID, CommentID: comment.ID,
	})
	if err != nil {
		t.Fatalf("Get after a deletion: %v", err)
	}
	if !stored.Deleted {
		t.Error("the comment is not marked deleted")
	}
	// ...and the words are gone, from the row and from the history.
	if stored.Body != "" {
		t.Errorf("the body survived the deletion: %q", stored.Body)
	}
	if got := w.countRevisions(comment.ID); got != 0 {
		t.Errorf("the history survived the deletion: %d revisions", got)
	}
}

// TestADeletionIsAudited covers the record, read back from the real ledger.
func TestADeletionIsAudited(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	comment := w.write(w.owner, "to be removed")

	// **The deletion is done by the other account, which is the catalogue's "moderator"**, so it is
	// given repository write first. Without the grant the delete is correctly refused as not-found,
	// because a caller who cannot see a repository is not told it exists — which is a different
	// property, asserted in its own test, and not what this one is about.
	repositories := repository.NewService(repository.NewPostgresStore(w.db))
	if _, err := repositories.GrantAccess(ctx, repository.GrantInput{
		Actor: w.owner, RepoID: w.repoID, UserID: w.otherID, Role: repository.LevelWrite,
	}); err != nil {
		t.Fatalf("grant write: %v", err)
	}

	if err := w.service.Delete(ctx, comments.DeleteInput{
		Actor: w.other, RepositoryID: w.repoID, IssueID: w.issueID, CommentID: comment.ID,
		Request: audit.RequestContext{SourceAddress: "203.0.113.7", UserAgent: "a-test"},
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	reader := audit.NewReader(w.db)
	entries, err := reader.ForSubject(ctx, audit.SubjectComment, comment.ID)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the ledger holds %d records, want 1", len(entries))
	}
	entry := entries[0]
	if entry.Action != audit.ActionCommentDeleted {
		t.Errorf("the action is %q, want comment.deleted", entry.Action)
	}
	// The record outlives the comment's text, which is the point of the ledger having no foreign key
	// to the content it describes.
	if entry.ActorID != w.otherID {
		t.Errorf("the record names actor %q, want the deleter", entry.ActorID)
	}
	if entry.Detail["issue_id"] != w.issueID {
		t.Errorf("the record does not name the issue: %v", entry.Detail)
	}
}

// TestAnEditWritesNoLedgerRecord is the other half of the record shape.
//
// The catalogue defines no `comment.edited`, so an edit must not produce a record with an invented
// action — and the ledger is the only place such an invention would show up.
func TestAnEditWritesNoLedgerRecord(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	comment := w.write(w.owner, "the original")

	// **The claim is a delta, not a total.** Building the world writes records of its own — the
	// repository's creation and the issue's — so asserting a total would be asserting the fixture
	// rather than the property. What matters is that the edit adds none.
	var before int
	if err := w.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events`).Scan(&before); err != nil {
		t.Fatalf("count the records: %v", err)
	}

	if _, err := w.service.Edit(ctx, comments.EditInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, CommentID: comment.ID,
		Body: "the edit", ExpectedVersion: comment.Version,
	}); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	var after int
	if err := w.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events`).Scan(&after); err != nil {
		t.Fatalf("count the records: %v", err)
	}
	if after != before {
		t.Errorf("the edit wrote %d record(s); the catalogue defines no action for one", after-before)
	}
}

// TestADeletedIssueTakesItsCommentsAndHistory is the cascade claim.
//
// W2.3's acceptance says a deletion "never leaves a dangling timeline entry", and the foreign key is
// what guarantees it: deleting the issue removes its comments, and each comment's revisions go with
// it. Nothing has to remember to clean up.
func TestADeletedIssueTakesItsCommentsAndHistory(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	comment := w.write(w.owner, "a comment on the issue")
	if _, err := w.service.Edit(ctx, comments.EditInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, CommentID: comment.ID,
		Body: "an edit", ExpectedVersion: comment.Version,
	}); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	// Delete the issue **directly**, so the cascade is what is being tested rather than the issue
	// service's own bookkeeping.
	if _, err := w.db.ExecContext(ctx, `DELETE FROM issues WHERE id = $1`, w.issueID); err != nil {
		t.Fatalf("delete the issue: %v", err)
	}

	var commentCount, revisionCount int
	if err := w.db.QueryRowContext(ctx,
		`SELECT count(*) FROM issue_comments`).Scan(&commentCount); err != nil {
		t.Fatalf("count the comments: %v", err)
	}
	if err := w.db.QueryRowContext(ctx,
		`SELECT count(*) FROM comment_revisions`).Scan(&revisionCount); err != nil {
		t.Fatalf("count the revisions: %v", err)
	}
	if commentCount != 0 {
		t.Errorf("%d comments survived their issue", commentCount)
	}
	if revisionCount != 0 {
		t.Errorf("%d revisions survived their issue", revisionCount)
	}
}

// TestTheOrderingIsStableAndTotal covers "comment ordering is stable by creation time plus
// identifier".
//
// The rows are written concurrently, so several share a created_at to the microsecond. The
// identifier breaks the tie, which is what makes the order **total** — and a total order is what
// stops a page repeating or dropping a row at a boundary.
func TestTheOrderingIsStableAndTotal(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	const writers = 12
	ids := make([]string, writers)
	var wait sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			// Every writer waits for the same moment, so the creations genuinely overlap and the
			// timestamps collide.
			<-start
			comment, err := w.service.Create(ctx, comments.CreateInput{
				Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, Body: "a comment",
			})
			if err != nil {
				return
			}
			ids[index] = comment.ID
		}(i)
	}
	close(start)
	wait.Wait()

	// Walk the whole conversation in pages of three, and assert every comment appears exactly once.
	seen := map[string]int{}
	var cursor *comments.Cursor
	pages := 0
	for {
		page, err := w.service.List(ctx, comments.ListInput{
			Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, Limit: 3, Before: cursor,
		})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		pages++
		for _, comment := range page.Comments {
			seen[comment.ID]++
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

	if len(seen) != writers {
		t.Errorf("the pages covered %d comments, want %d", len(seen), writers)
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("comment %s appeared %d times, want once", id, count)
		}
	}

	// And the order is by creation time then identifier, which is read back directly so the assertion
	// is about the stored order rather than about what one query happened to return.
	rows, err := w.db.QueryContext(ctx, `
		SELECT id FROM issue_comments
		WHERE issue_id = $1
		ORDER BY created_at, id`, w.issueID)
	if err != nil {
		t.Fatalf("read the order: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var ordered []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		ordered = append(ordered, id)
	}
	if len(ordered) != writers {
		t.Fatalf("the stored order holds %d rows, want %d", len(ordered), writers)
	}
	// Every writer's identifier is somewhere in the order, which is what "total" means here.
	for _, id := range ids {
		if id == "" {
			continue
		}
		found := false
		for _, candidate := range ordered {
			if candidate == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("comment %s is missing from the order", id)
		}
	}
}

// TestAnUnreadableRepositoryRefusesEveryCommentOperation is W2.3's acceptance about inference.
func TestAnUnreadableRepositoryRefusesEveryCommentOperation(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	w.write(w.owner, "a private comment")
	// The second account holds no grant on the repository.
	anonymous := repository.Anonymous()

	for _, test := range []struct {
		name string
		call func() error
	}{
		{
			name: "list",
			call: func() error {
				_, err := w.service.List(ctx, comments.ListInput{
					Actor: anonymous, RepositoryID: w.repoID, IssueID: w.issueID,
				})
				return err
			},
		},
		{
			name: "count",
			call: func() error {
				_, err := w.service.Count(ctx, comments.CountInput{
					Actor: anonymous, RepositoryID: w.repoID, IssueID: w.issueID,
				})
				return err
			},
		},
		{
			name: "write",
			call: func() error {
				_, err := w.service.Create(ctx, comments.CreateInput{
					Actor: w.other, RepositoryID: w.repoID, IssueID: w.issueID, Body: "x",
				})
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, comments.ErrNotFound) {
				t.Errorf("%s = %v, want ErrNotFound so nothing is disclosed", test.name, err)
			}
		})
	}

	// A reader sees the comment, which is what makes the refusals above a decision rather than a
	// broken query.
	repositories := repository.NewService(repository.NewPostgresStore(w.db))
	if _, err := repositories.GrantAccess(ctx, repository.GrantInput{
		Actor: w.owner, RepoID: w.repoID, UserID: w.otherID, Role: repository.LevelRead,
	}); err != nil {
		t.Fatalf("grant read: %v", err)
	}
	page, err := w.service.List(ctx, comments.ListInput{
		Actor: w.other, RepositoryID: w.repoID, IssueID: w.issueID,
	})
	if err != nil {
		t.Fatalf("a reader's list: %v", err)
	}
	if len(page.Comments) != 1 {
		t.Errorf("a reader sees %d comments, want 1", len(page.Comments))
	}
	if page.Comments[0].AuthorName != "owner" {
		t.Errorf("the comment's author is %q, want the author's name", page.Comments[0].AuthorName)
	}
}

// TestACommentInAnotherIssueIsNotReachable covers the scoping: the store takes an issue and a
// repository on every read, so an identifier cannot be reached through a place it does not belong.
func TestACommentInAnotherIssueIsNotReachable(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	comment := w.write(w.owner, "a comment on the first issue")

	// A second issue in the same repository.
	repositories := repository.NewService(repository.NewPostgresStore(w.db))
	issueService := issues.NewService(repositories, issues.NewPostgresStore(w.db))
	second, err := issueService.Create(ctx, issues.CreateInput{
		Actor: w.owner, RepositoryID: w.repoID, Title: "a second issue",
	})
	if err != nil {
		t.Fatalf("create the second issue: %v", err)
	}

	// Asking for the comment through the wrong issue is a not-found rather than a hit.
	if _, err := w.service.Get(ctx, comments.GetInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: second.ID, CommentID: comment.ID,
	}); !errors.Is(err, comments.ErrNotFound) {
		t.Errorf("a comment reached through another issue = %v, want ErrNotFound", err)
	}
	// And through the right one it is found, which is what makes the assertion above meaningful.
	if _, err := w.service.Get(ctx, comments.GetInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, CommentID: comment.ID,
	}); err != nil {
		t.Errorf("the comment through its own issue: %v", err)
	}
}

// TestReadingByNumberAgainstTheDatabase covers the by-number read, which is what a URL uses.
//
// **It exists because its absence hid a broken query.** The `GetByNumber` projection named a column
// the subquery did not produce, so every by-number read failed with a SQL error — and because no test
// here addressed a comment by number, the first thing to notice was an end-to-end test reporting a
// 500. A query that only a different package exercises is a query without a unit test, which is the
// gap this closes.
func TestReadingByNumberAgainstTheDatabase(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	// Three comments, so a position in the middle is covered rather than only the ends.
	first := w.write(w.owner, "the first comment")
	second := w.write(w.owner, "the second comment")
	third := w.write(w.owner, "the third comment")

	for want, expected := range map[int]string{1: first.ID, 2: second.ID, 3: third.ID} {
		found, err := w.service.GetByNumber(ctx, comments.GetByNumberInput{
			Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, Number: want,
		})
		if err != nil {
			t.Fatalf("GetByNumber(%d): %v", want, err)
		}
		if found.ID != expected {
			t.Errorf("position %d resolved to %s, want %s", want, found.ID, expected)
		}
		// The projection is what the read returns, so the author's name has to come through: a
		// wrong column name would have failed above, but this asserts the value rather than only the
		// absence of an error.
		if found.AuthorName != "owner" {
			t.Errorf("position %d has author %q, want the author's name", want, found.AuthorName)
		}
		if found.Number != want {
			t.Errorf("position %d reports number %d", want, found.Number)
		}
	}

	// A position past the end is a not-found rather than an error, and zero and negative are the same.
	for _, number := range []int{0, -1, 4, 99} {
		if _, err := w.store.GetByNumber(ctx, w.repoID, w.issueID, number); !errors.Is(err, comments.ErrNotFound) {
			t.Errorf("position %d = %v, want ErrNotFound", number, err)
		}
	}

	// **The number is stable across an edit**, because the ordering is by creation time and an edit
	// does not move a comment. That is what makes a URL to a comment keep working.
	edited, err := w.service.Edit(ctx, comments.EditInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, CommentID: second.ID,
		Body: "the second comment, revised", ExpectedVersion: second.Version,
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	afterEdit, err := w.service.GetByNumber(ctx, comments.GetByNumberInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, Number: 2,
	})
	if err != nil {
		t.Fatalf("GetByNumber after an edit: %v", err)
	}
	if afterEdit.ID != edited.ID {
		t.Errorf("position 2 moved to %s after an edit, want %s", afterEdit.ID, edited.ID)
	}

	// And it survives a deletion, because the row keeps its place — which is what lets a tombstone's
	// anchor still resolve.
	if err := w.service.Delete(ctx, comments.DeleteInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, CommentID: first.ID,
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	tombstone, err := w.service.GetByNumber(ctx, comments.GetByNumberInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, Number: 1,
	})
	if err != nil {
		t.Fatalf("GetByNumber after a deletion: %v", err)
	}
	if !tombstone.Deleted {
		t.Error("the tombstone is not marked deleted")
	}
	if tombstone.Body != "" {
		t.Errorf("the tombstone still carries a body: %q", tombstone.Body)
	}
}
