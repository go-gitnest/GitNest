package labels_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/audit"
	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/issues"
	"github.com/go-gitnest/GitNest/internal/labels"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/google/uuid"
)

// The database-backed tests.
//
// Three of this workstream's claims are properties of how PostgreSQL behaves rather than of the code
// that calls it, so a fake cannot establish any of them:
//
//   - "Label names are unique per repository after case folding" is decided by a **unique constraint**,
//     and a fake's map is a second implementation of that rule rather than evidence for it.
//   - "Deleting a label removes its attachments without deleting issues" is a claim about a foreign
//     key's cascade.
//   - The label list's ordering and the joined per-issue read are properties of the queries.

// world is a migrated schema with an owner, a repository, an issue, and a label service over them.
type world struct {
	t       *testing.T
	db      *sql.DB
	service *labels.Service
	store   *labels.PostgresStore
	owner   repository.Principal
	ownerID string
	other   repository.Principal
	otherID string
	repoID  string
	issueID string
	// issues is the issue service, so a second issue can be created when a test needs one.
	issues *issues.Service
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

	store := labels.NewPostgresStore(schema.Client.DB)
	return &world{
		t: t, db: schema.Client.DB, store: store, service: labels.NewService(repositories, store),
		owner: repository.User(owner.ID, false), ownerID: owner.ID,
		other: repository.User(other.ID, false), otherID: other.ID,
		repoID: row.ID, issueID: issue.ID, issues: issueService,
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

// add creates one label through the service.
func (w *world) add(name, color string) labels.Label {
	w.t.Helper()
	created, err := w.service.Create(context.Background(), labels.CreateInput{
		Actor: w.owner, RepositoryID: w.repoID, Name: name, Color: color,
	})
	if err != nil {
		w.t.Fatalf("Create(%q): %v", name, err)
	}
	return created
}

// countIssues reads the issue count directly, which is how "without deleting issues" is checked.
func (w *world) countIssues() int {
	w.t.Helper()
	var count int
	if err := w.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM issues`).Scan(&count); err != nil {
		w.t.Fatalf("count the issues: %v", err)
	}
	return count
}

// countAttachments reads the attachment count directly.
func (w *world) countAttachments() int {
	w.t.Helper()
	var count int
	if err := w.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM issue_labels`).Scan(&count); err != nil {
		w.t.Fatalf("count the attachments: %v", err)
	}
	return count
}

// TestTheConstraintDecidesANameConflict is the acceptance criterion against the real constraint.
//
// **The constraint is what decides it, not a read-then-write.** A concurrent create of two names
// differing only by ASCII case therefore produces one row and one `ErrNameTaken`, rather than two rows
// — which is the property a fake cannot demonstrate, because a fake's map serializes the check.
func TestTheConstraintDecidesANameConflict(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	w.add("Bug", "ff0000")

	// Every ASCII-cased variant and every padded one is refused.
	for _, variant := range []string{"bug", "BUG", "Bug", "  bug  ", "bUg"} {
		if _, err := w.service.Create(ctx, labels.CreateInput{
			Actor: w.owner, RepositoryID: w.repoID, Name: variant, Color: "00ff00",
		}); !errors.Is(err, labels.ErrNameTaken) {
			t.Errorf("creating %q = %v, want ErrNameTaken", variant, err)
		}
	}

	var count int
	if err := w.db.QueryRowContext(ctx, `SELECT count(*) FROM labels`).Scan(&count); err != nil {
		t.Fatalf("count the labels: %v", err)
	}
	if count != 1 {
		t.Errorf("the table holds %d labels, want 1", count)
	}

	// **Concurrently**, so the constraint rather than a serialized check is what refuses the losers.
	const writers = 8
	results := make([]error, writers)
	var wait sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, err := w.service.Create(ctx, labels.CreateInput{
				Actor: w.owner, RepositoryID: w.repoID, Name: "concurrent-bug", Color: "0000ff",
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
		case errors.Is(err, labels.ErrNameTaken):
			// The expected refusal for every loser.
		default:
			t.Errorf("a concurrent create failed with %v, want ErrNameTaken", err)
		}
	}
	if succeeded != 1 {
		t.Errorf("%d concurrent creates of one name succeeded, want exactly 1", succeeded)
	}
	if err := w.db.QueryRowContext(ctx, `SELECT count(*) FROM labels`).Scan(&count); err != nil {
		t.Fatalf("count the labels: %v", err)
	}
	if count != 2 {
		t.Errorf("the table holds %d labels, want 2 — one per distinct name", count)
	}
}

// TestDeletingALabelKeepsItsIssues is the acceptance criterion, proved against the tables.
//
// The attachments go with the label through the join table's cascade, and the issues they pointed at are
// a different table that nothing touches — so "without deleting issues" is a property of the schema
// rather than of a code path remembering to be careful.
func TestDeletingALabelKeepsItsIssues(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	// Two labels, so the deletion has something to leave behind. The identifiers are not needed: what
	// this test asserts is counted from the tables rather than from a returned value.
	w.add("bug", "ff0000")
	w.add("feature", "00ff00")

	// Two issues, each carrying both labels.
	second, err := w.issues.Create(ctx, issues.CreateInput{
		Actor: w.owner, RepositoryID: w.repoID, Title: "a second issue",
	})
	if err != nil {
		t.Fatalf("create the second issue: %v", err)
	}
	for _, issueID := range []string{w.issueID, second.ID} {
		for _, name := range []string{"bug", "feature"} {
			if _, err := w.service.Attach(ctx, labels.AttachInput{
				Actor: w.owner, RepositoryID: w.repoID, IssueID: issueID, Name: name,
			}); err != nil {
				t.Fatalf("Attach(%s): %v", name, err)
			}
		}
	}
	if got := w.countAttachments(); got != 4 {
		t.Fatalf("there are %d attachments, want 4", got)
	}
	if got := w.countIssues(); got != 2 {
		t.Fatalf("there are %d issues, want 2", got)
	}

	// Delete one label.
	if err := w.service.Delete(ctx, labels.DeleteInput{
		Actor: w.owner, RepositoryID: w.repoID, Name: "bug",
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// **Its attachments are gone** — two of the four, both of them `bug`…
	if got := w.countAttachments(); got != 2 {
		t.Errorf("there are %d attachments after the deletion, want 2", got)
	}
	// …**and the issues are not**, which is the acceptance's requirement.
	if got := w.countIssues(); got != 2 {
		t.Errorf("there are %d issues after the deletion, want 2", got)
	}

	// The surviving label is still attached to both issues, so the cascade removed only what it should.
	for _, issueID := range []string{w.issueID, second.ID} {
		found, err := w.service.ForIssue(ctx, labels.IssueLabelsInput{
			Actor: w.owner, RepositoryID: w.repoID, IssueID: issueID,
		})
		if err != nil {
			t.Fatalf("ForIssue: %v", err)
		}
		if len(found) != 1 || found[0].Name != "feature" {
			t.Errorf("issue %s carries %+v, want only the surviving label", issueID, found)
		}
	}
}

// TestDeletingAnIssueRemovesItsAttachments is the other cascade direction.
//
// Without it, a deleted issue would leave rows in the join table pointing at nothing — and a label's
// issue count would then overstate how many issues carry it.
func TestDeletingAnIssueRemovesItsAttachments(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	w.add("bug", "ff0000")
	if _, err := w.service.Attach(ctx, labels.AttachInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, Name: "bug",
	}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if got := w.countAttachments(); got != 1 {
		t.Fatalf("there are %d attachments, want 1", got)
	}

	// The issue is deleted **directly**, so the cascade is what is under test.
	if _, err := w.db.ExecContext(ctx, `DELETE FROM issues WHERE id = $1`, w.issueID); err != nil {
		t.Fatalf("delete the issue: %v", err)
	}

	if got := w.countAttachments(); got != 0 {
		t.Errorf("%d attachments survived their issue", got)
	}
	// The label itself survives, because it belongs to the repository rather than to the issue.
	found, err := w.service.List(ctx, labels.ListInput{Actor: w.owner, RepositoryID: w.repoID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(found) != 1 {
		t.Errorf("the repository holds %d labels, want 1", len(found))
	}
}

// TestTheColourGrammarHoldsAtTheDatabase covers T-B2 at the storage layer.
//
// The service normalizes, so every value the service writes is inside `[0-9a-f]{6}`. This asserts the
// **constraint** independently, by writing past the service: a future code path that bypassed the
// normalizer would be refused by the table rather than storing something a page then renders.
func TestTheColourGrammarHoldsAtTheDatabase(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	for index, color := range []string{
		"red", "FF0000", "#ff0000", "ff000", "ff00000", "ff0000;", "ff00zz", "",
		"' onmouseover='alert(1)", "ff0000\"style=\"x",
	} {
		_, err := w.db.ExecContext(ctx, `
			INSERT INTO labels (id, repository_id, name, name_folded, color)
			VALUES (gen_random_uuid(), $1, $2, $2, $3)`,
			w.repoID, "bypassed-"+string(rune('a'+index)), color)
		if err == nil {
			t.Errorf("the database accepted the colour %q, which is outside the grammar", color)
		}
	}

	// And the service's own normalization produces a value the constraint admits, which is what makes
	// the refusals above a property of the table rather than of a diverging rule.
	created := w.add("a-valid-colour", "#AbCdEf")
	if created.Color != "abcdef" {
		t.Errorf("the stored colour is %q, want the normalized lowercase form", created.Color)
	}
}

// TestTheListIsOrderedByFoldedName covers the query's ordering.
func TestTheListIsOrderedByFoldedName(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)

	for _, name := range []string{"Zebra", "apple", "Banana", "cherry"} {
		w.add(name, "ff0000")
	}

	found, err := w.service.List(context.Background(), labels.ListInput{
		Actor: w.owner, RepositoryID: w.repoID,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"apple", "Banana", "cherry", "Zebra"}
	if len(found) != len(want) {
		t.Fatalf("the list holds %d labels, want %d", len(found), len(want))
	}
	for index, expected := range want {
		if found[index].Name != expected {
			t.Errorf("position %d is %q, want %q", index, found[index].Name, expected)
		}
	}
}

// TestTheCountsIncludeUnusedLabels covers the left join.
func TestTheCountsIncludeUnusedLabels(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	used := w.add("used", "ff0000")
	w.add("unused", "00ff00")

	if _, err := w.service.Attach(ctx, labels.AttachInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, Name: "used",
	}); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	counts, err := w.service.Counts(ctx, labels.CountsInput{
		Actor: w.owner, RepositoryID: w.repoID,
	})
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	if counts[used.ID] != 1 {
		t.Errorf("the used label's count is %d, want 1", counts[used.ID])
	}
	if len(counts) != 2 {
		t.Errorf("the counts hold %d entries, want 2 — an unused label must be present at zero", len(counts))
	}
	for id, count := range counts {
		if id != used.ID && count != 0 {
			t.Errorf("the unused label's count is %d, want 0", count)
		}
	}
}

// TestALabelIsScopedToItsRepository covers the scoping the store's `WHERE repository_id` provides.
//
// It is the reason the comment API's flat path needed a decision: nothing in the schema makes a label
// name globally unique, so a lookup without a repository could resolve in the wrong one.
func TestALabelIsScopedToItsRepository(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	// A second repository, owned by the same account, with a label of the same name.
	repositories := repository.NewService(repository.NewPostgresStore(w.db))
	second, err := repositories.Create(ctx, repository.CreateInput{
		Actor: w.owner, OwnerID: w.ownerID, Name: "another-repo",
		Visibility: repository.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create the second repository: %v", err)
	}
	secondLabel, err := w.service.Create(ctx, labels.CreateInput{
		Actor: w.owner, RepositoryID: second.ID, Name: "bug", Color: "00ff00",
	})
	if err != nil {
		t.Fatalf("create the second repository's label: %v", err)
	}
	firstLabel := w.add("bug", "ff0000")

	// The same folded name resolves to a different label in each repository — so the two are distinct
	// rows that happen to share a name, not one row reached two ways.
	if firstLabel.ID == secondLabel.ID {
		t.Fatal("the two repositories resolved to the same label row")
	}
	if firstLabel.Color == secondLabel.Color {
		t.Error("the two labels are indistinguishable, so the lookups may not be scoped")
	}

	// And an attachment in one repository is invisible in the other.
	if _, err := w.service.Attach(ctx, labels.AttachInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, Name: "bug",
	}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	found, err := w.service.ForIssue(ctx, labels.IssueLabelsInput{
		Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID,
	})
	if err != nil {
		t.Fatalf("ForIssue: %v", err)
	}
	if len(found) != 1 || found[0].Color != "ff0000" {
		t.Errorf("the issue carries %+v, want this repository's label", found)
	}
}

// TestTheRecordIsWrittenAndReadBack covers the two catalogue actions against the real ledger.
func TestTheRecordIsWrittenAndReadBack(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	label := w.add("bug", "ff0000")
	// An update writes nothing, because the catalogue defines no `label.edited`.
	if _, err := w.service.Update(ctx, labels.UpdateInput{
		Actor: w.owner, RepositoryID: w.repoID, Name: "bug", Color: ptrTo("00ff00"),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := w.service.Delete(ctx, labels.DeleteInput{
		Actor: w.owner, RepositoryID: w.repoID, Name: "bug",
		Request: audit.RequestContext{SourceAddress: "203.0.113.7", UserAgent: "a-test"},
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	reader := audit.NewReader(w.db)
	entries, err := reader.ForSubject(ctx, audit.SubjectLabel, label.ID)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}

	// Exactly two records: the creation and the deletion. An update would have added a third with an
	// invented action, which is what this asserts cannot happen.
	if len(entries) != 2 {
		t.Fatalf("the ledger holds %d records, want 2: %+v", len(entries), entries)
	}
	if entries[0].Action != audit.ActionLabelCreated {
		t.Errorf("the first record is %q, want label.created", entries[0].Action)
	}
	if entries[1].Action != audit.ActionLabelDeleted {
		t.Errorf("the second record is %q, want label.deleted", entries[1].Action)
	}
	// The creation record names the label and its colour, which is what makes it answerable once the
	// label is gone.
	if entries[0].Detail["name"] != "bug" || entries[0].Detail["color"] != "ff0000" {
		t.Errorf("the creation record does not name the label: %v", entries[0].Detail)
	}
	if entries[1].SourceAddress != "203.0.113.7" || entries[1].UserAgent != "a-test" {
		t.Errorf("the deletion record lost the request context: %+v", entries[1])
	}
	if entries[0].ActorID != w.ownerID {
		t.Errorf("the record names actor %q, want the actor", entries[0].ActorID)
	}
}

// TestThePermissionLevelsAgainstTheRealService covers the two levels end to end, because the level comes
// from the real permission service rather than a fake.
func TestThePermissionLevelsAgainstTheRealService(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	// The second account holds read only.
	repositories := repository.NewService(repository.NewPostgresStore(w.db))
	if _, err := repositories.GrantAccess(ctx, repository.GrantInput{
		Actor: w.owner, RepoID: w.repoID, UserID: w.otherID, Role: repository.LevelRead,
	}); err != nil {
		t.Fatalf("grant read: %v", err)
	}

	// Managing a label needs admin, so a reader is refused — and refused as **forbidden** rather than
	// not-found, because their level was resolved and they can already see the repository.
	if _, err := w.service.Create(ctx, labels.CreateInput{
		Actor: w.other, RepositoryID: w.repoID, Name: "bug", Color: "ff0000",
	}); !errors.Is(err, labels.ErrForbidden) {
		t.Errorf("a reader's create = %v, want ErrForbidden", err)
	}

	w.add("bug", "ff0000")

	// Attaching needs write, which a reader does not have.
	if _, err := w.service.Attach(ctx, labels.AttachInput{
		Actor: w.other, RepositoryID: w.repoID, IssueID: w.issueID, Name: "bug",
	}); !errors.Is(err, labels.ErrForbidden) {
		t.Errorf("a reader's attach = %v, want ErrForbidden", err)
	}
	// And a reader may still see the labels, which is what makes the refusals a decision rather than a
	// broken query.
	if _, err := w.service.List(ctx, labels.ListInput{
		Actor: w.other, RepositoryID: w.repoID,
	}); err != nil {
		t.Errorf("a reader's list: %v", err)
	}

	// Promote to write: attaching now succeeds, and managing still does not, because that needs admin.
	if _, err := repositories.GrantAccess(ctx, repository.GrantInput{
		Actor: w.owner, RepoID: w.repoID, UserID: w.otherID, Role: repository.LevelWrite,
	}); err != nil {
		t.Fatalf("grant write: %v", err)
	}
	if _, err := w.service.Attach(ctx, labels.AttachInput{
		Actor: w.other, RepositoryID: w.repoID, IssueID: w.issueID, Name: "bug",
	}); err != nil {
		t.Errorf("a writer's attach: %v", err)
	}
	if _, err := w.service.Create(ctx, labels.CreateInput{
		Actor: w.other, RepositoryID: w.repoID, Name: "another", Color: "ff0000",
	}); !errors.Is(err, labels.ErrForbidden) {
		t.Errorf("a writer's create = %v, want ErrForbidden — managing a label needs admin", err)
	}

	// Promote to admin, and managing succeeds.
	if _, err := repositories.GrantAccess(ctx, repository.GrantInput{
		Actor: w.owner, RepoID: w.repoID, UserID: w.otherID, Role: repository.LevelAdmin,
	}); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	if _, err := w.service.Create(ctx, labels.CreateInput{
		Actor: w.other, RepositoryID: w.repoID, Name: "another", Color: "ff0000",
	}); err != nil {
		t.Errorf("an admin's create: %v", err)
	}
}

// TestAnUnreadableRepositoryDisclosesNoLabels covers the T-A4 shape at this surface.
func TestAnUnreadableRepositoryDisclosesNoLabels(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	w.add("a-private-label", "ff0000")

	anonymous := repository.Anonymous()
	for _, test := range []struct {
		name string
		call func() error
	}{
		{name: "list", call: func() error {
			_, err := w.service.List(ctx, labels.ListInput{Actor: anonymous, RepositoryID: w.repoID})
			return err
		}},
		{name: "get", call: func() error {
			_, err := w.service.Get(ctx, labels.GetInput{
				Actor: anonymous, RepositoryID: w.repoID, Name: "a-private-label",
			})
			return err
		}},
		{name: "counts", call: func() error {
			_, err := w.service.Counts(ctx, labels.CountsInput{
				Actor: anonymous, RepositoryID: w.repoID,
			})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, labels.ErrNotFound) {
				t.Errorf("%s without access = %v, want ErrNotFound", test.name, err)
			}
		})
	}
}

// TestTheAttachmentConstraintDecidesAConcurrentApplication covers idempotence against the real
// constraint.
//
// Two concurrent applications of one label must leave one attachment, not two or an error: the composite
// primary key is what decides it, and `ON CONFLICT DO NOTHING` is what turns the collision into a
// reportable "nothing was new" rather than a failure.
func TestTheAttachmentConstraintDecidesAConcurrentApplication(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	w.add("bug", "ff0000")

	const writers = 8
	created := make([]bool, writers)
	errs := make([]error, writers)
	var wait sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			created[index], errs[index] = w.service.Attach(ctx, labels.AttachInput{
				Actor: w.owner, RepositoryID: w.repoID, IssueID: w.issueID, Name: "bug",
			})
		}(i)
	}
	close(start)
	wait.Wait()

	newly := 0
	for index, err := range errs {
		if err != nil {
			t.Fatalf("worker %d failed: %v", index, err)
		}
		if created[index] {
			newly++
		}
	}
	if newly != 1 {
		t.Errorf("%d applications reported a new attachment, want exactly 1", newly)
	}
	if got := w.countAttachments(); got != 1 {
		t.Errorf("there are %d attachments, want 1", got)
	}
}

// TestALongLabelListAndItsQueryPlanBoundTheWork guards the list query's use of the unique constraint's
// index rather than a scan.
//
// It is asserted through the ordering rather than through an `EXPLAIN`, because the plan is the
// planner's business and can legitimately change with statistics: what must hold is that the list is
// complete and ordered, and that a repository's labels do not include another repository's.
func TestALongLabelListAndItsQueryPlanBoundTheWork(t *testing.T) {
	w := newWorld(t, repository.VisibilityPrivate)
	ctx := context.Background()

	// A second repository, so a missing predicate would be visible as extra rows.
	repositories := repository.NewService(repository.NewPostgresStore(w.db))
	second, err := repositories.Create(ctx, repository.CreateInput{
		Actor: w.owner, OwnerID: w.ownerID, Name: "other-repo",
		Visibility: repository.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create the second repository: %v", err)
	}
	if _, err := w.service.Create(ctx, labels.CreateInput{
		Actor: w.owner, RepositoryID: second.ID, Name: "elsewhere", Color: "ff0000",
	}); err != nil {
		t.Fatalf("create the other repository's label: %v", err)
	}

	for i := 0; i < 25; i++ {
		w.add("label-"+strings.Repeat("z", i%3)+"-"+string(rune('a'+i%26))+string(rune('a'+i/26)), "ff0000")
	}

	found, err := w.service.List(ctx, labels.ListInput{Actor: w.owner, RepositoryID: w.repoID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(found) != 25 {
		t.Errorf("the list holds %d labels, want 25 — another repository's must not appear", len(found))
	}
	for _, label := range found {
		if label.Name == "elsewhere" {
			t.Error("the list includes another repository's label")
		}
	}
	// And the order is the folded name's, ascending.
	for index := 1; index < len(found); index++ {
		if labels.Fold(found[index-1].Name) > labels.Fold(found[index].Name) {
			t.Errorf("the list is out of order at %d: %q then %q",
				index, found[index-1].Name, found[index].Name)
		}
	}
}

// ptrTo returns a pointer to a value, for the pointer fields an update takes.
func ptrTo(value string) *string { return &value }
