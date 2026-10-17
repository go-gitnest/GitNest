package references_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/issues"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
	"github.com/go-gitnest/GitNest/internal/references"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/google/uuid"
)

// Recording and reading references against a real schema.
//
// The claims that need a database are the ones a fake cannot establish: that recording **replaces** rather
// than accumulates, that the unique constraint collapses a text's repeated mention of one target into one
// relationship, and that a deletion's cascade removes the outgoing rows while leaving the target's own
// rows alone.

// world is a migrated schema with two repositories, an owner, and a references store.
type world struct {
	t       *testing.T
	db      *sql.DB
	store   *references.Sources
	owner   repository.Principal
	ownerID string
	// first and second are two repositories, so a cross-repository reference is expressible.
	first, second           repository.Repository
	firstIssue, secondIssue issues.Issue
	issues                  *issues.Service
}

func newWorld(t *testing.T) *world {
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

	repositories := repository.NewService(repository.NewPostgresStore(schema.Client.DB))
	first, err := repositories.Create(ctx, repository.CreateInput{
		Actor: repository.User(owner.ID, false), OwnerID: owner.ID, Name: "first",
		Visibility: repository.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create the first repository: %v", err)
	}
	second, err := repositories.Create(ctx, repository.CreateInput{
		Actor: repository.User(owner.ID, false), OwnerID: owner.ID, Name: "second",
		Visibility: repository.VisibilityPrivate,
	})
	if err != nil {
		t.Fatalf("create the second repository: %v", err)
	}

	issueService := issues.NewService(repositories, issues.NewPostgresStore(schema.Client.DB))
	firstIssue, err := issueService.Create(ctx, issues.CreateInput{
		Actor: repository.User(owner.ID, false), RepositoryID: first.ID, Title: "first issue",
	})
	if err != nil {
		t.Fatalf("create the first issue: %v", err)
	}
	secondIssue, err := issueService.Create(ctx, issues.CreateInput{
		Actor: repository.User(owner.ID, false), RepositoryID: second.ID, Title: "second issue",
	})
	if err != nil {
		t.Fatalf("create the second issue: %v", err)
	}

	return &world{
		t: t, db: schema.Client.DB, store: references.NewSources(schema.Client.DB),
		owner: repository.User(owner.ID, false), ownerID: owner.ID,
		first: first, second: second, firstIssue: firstIssue, secondIssue: secondIssue,
		issues: issueService,
	}
}

// record writes a reference from the first issue's body to a target.
func (w *world) record(targetRepositoryID string, number int64) {
	w.t.Helper()
	if err := w.store.Record(context.Background(), references.RecordInput{
		SourceIssueID: w.firstIssue.ID,
		References: []references.Reference{{
			TargetRepositoryID: targetRepositoryID, Number: number,
		}},
		Written: time.Now().UTC(),
	}); err != nil {
		w.t.Fatalf("Record: %v", err)
	}
}

// count reads the table's size directly.
func (w *world) count() int {
	w.t.Helper()
	var total int
	if err := w.db.QueryRowContext(context.Background(),
		`SELECT count(*) FROM issue_references`).Scan(&total); err != nil {
		w.t.Fatalf("count: %v", err)
	}
	return total
}

// TestRecordingIsAReplaceNotAnAccumulation is the property the design rests on.
//
// A text's references are derived from its **current** content, so re-recording must produce the new set
// rather than adding to the old. An insert-only design would leave a reference behind after the words
// naming it were edited away, and the target's timeline would keep showing a link nobody can find.
func TestRecordingIsAReplaceNotAnAccumulation(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	// A first version naming two targets.
	if err := w.store.Record(ctx, references.RecordInput{
		SourceIssueID: w.firstIssue.ID,
		References: []references.Reference{
			{TargetRepositoryID: w.second.ID, Number: 1},
			{TargetRepositoryID: w.second.ID, Number: 2},
		},
		Written: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := w.count(); got != 2 {
		t.Fatalf("the table holds %d rows, want 2", got)
	}

	// The text is edited so it names only the second.
	if err := w.store.Record(ctx, references.RecordInput{
		SourceIssueID: w.firstIssue.ID,
		References: []references.Reference{
			{TargetRepositoryID: w.second.ID, Number: 2},
		},
		Written: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// **The first reference is gone**, which is what makes the table a statement about what the text says
	// now.
	if got := w.count(); got != 1 {
		t.Errorf("the table holds %d rows after the edit, want 1", got)
	}
	outgoing, err := w.store.Outgoing(ctx, w.firstIssue.ID)
	if err != nil {
		t.Fatalf("Outgoing: %v", err)
	}
	if len(outgoing) != 1 || outgoing[0].TargetNumber != 2 {
		t.Errorf("the outgoing references are %+v, want only number 2", outgoing)
	}

	// And a text that names nothing clears the set.
	if err := w.store.Record(ctx, references.RecordInput{
		SourceIssueID: w.firstIssue.ID, Written: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := w.count(); got != 0 {
		t.Errorf("the table holds %d rows, want none", got)
	}
}

// TestRepeatedMentionsCollapseToOneRow covers the unique constraint.
//
// A text that says `#5` three times is **one relationship**. Recording it three times would show the same
// fact three times in a timeline and make the "recorded in both timelines" link appear thrice.
func TestRepeatedMentionsCollapseToOneRow(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	same := references.Reference{TargetRepositoryID: w.second.ID, Number: 1}
	if err := w.store.Record(ctx, references.RecordInput{
		SourceIssueID: w.firstIssue.ID,
		References:    []references.Reference{same, same, same},
		Written:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := w.count(); got != 1 {
		t.Errorf("the table holds %d rows for one repeated target, want 1", got)
	}
}

// TestABodyAndACommentAreSeparateSources covers the source distinction.
//
// A comment's reference is also the issue's, but the two are distinguishable so the timeline can say which
// text made it — and editing one must not wipe the other's rows, which is what the delete's null test
// decides.
func TestABodyAndACommentAreSeparateSources(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	// A comment on the first issue, created directly so this test does not depend on the comment service's
	// signature.
	commentID := uuid.NewString()
	if _, err := w.db.ExecContext(ctx, `
		INSERT INTO issue_comments (id, issue_id, author_id, body)
		VALUES ($1, $2, $3, 'a body naming #1')`, commentID, w.firstIssue.ID, w.ownerID); err != nil {
		t.Fatalf("create the comment: %v", err)
	}

	// The issue body names one target.
	w.record(w.second.ID, 1)
	// The comment names another.
	if err := w.store.Record(ctx, references.RecordInput{
		SourceIssueID: w.firstIssue.ID, SourceCommentID: commentID,
		References: []references.Reference{{TargetRepositoryID: w.second.ID, Number: 2}},
		Written:    time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := w.count(); got != 2 {
		t.Fatalf("the table holds %d rows, want 2", got)
	}

	// **Re-recording the body must not remove the comment's row**, which is what the `IS NULL` test in the
	// delete decides.
	if err := w.store.Record(ctx, references.RecordInput{
		SourceIssueID: w.firstIssue.ID,
		References:    []references.Reference{{TargetRepositoryID: w.second.ID, Number: 3}},
		Written:       time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := w.count(); got != 2 {
		t.Fatalf("the table holds %d rows after re-recording the body, want 2", got)
	}

	outgoing, err := w.store.Outgoing(ctx, w.firstIssue.ID)
	if err != nil {
		t.Fatalf("Outgoing: %v", err)
	}
	// Both are visible from the issue, and the comment's is labelled as such — which is what
	// "recorded in both timelines" needs.
	if len(outgoing) != 2 {
		t.Fatalf("the outgoing references are %+v, want 2", outgoing)
	}
	var fromComment int
	for _, entry := range outgoing {
		if entry.SourceCommentID == commentID {
			fromComment++
		}
	}
	if fromComment != 1 {
		t.Errorf("%d outgoing references name the comment, want 1", fromComment)
	}
}

// TestDeletingTheSourceRemovesItsReferences covers the cascade.
func TestDeletingTheSourceRemovesItsReferences(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	w.record(w.second.ID, 1)
	if got := w.count(); got != 1 {
		t.Fatalf("the table holds %d rows, want 1", got)
	}

	// Delete the source issue **directly**, so the cascade is what is under test.
	if _, err := w.db.ExecContext(ctx, `DELETE FROM issues WHERE id = $1`, w.firstIssue.ID); err != nil {
		t.Fatalf("delete the issue: %v", err)
	}

	if got := w.count(); got != 0 {
		t.Errorf("%d references survived their source issue", got)
	}
	// **The target is untouched.** A cross-reference is evidence of the text that made it, and once that
	// text is gone the evidence is gone — the target keeps its own row.
	if _, err := w.issues.Get(ctx, issues.GetInput{
		Actor: w.owner, RepositoryID: w.second.ID, Number: 1,
	}); err != nil {
		t.Errorf("the target issue was affected: %v", err)
	}
}

// TestIncomingAndOutgoingAreTheSameRelationshipFromBothEnds is the "recorded in both timelines" property.
func TestIncomingAndOutgoingAreTheSameRelationshipFromBothEnds(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	// The first issue's body names issue 1 of the second repository.
	w.record(w.second.ID, 1)

	outgoing, err := w.store.Outgoing(ctx, w.firstIssue.ID)
	if err != nil {
		t.Fatalf("Outgoing: %v", err)
	}
	if len(outgoing) != 1 {
		t.Fatalf("the outgoing references are %+v", outgoing)
	}
	// The target's identity is resolvable, which is what a timeline entry needs to link.
	if outgoing[0].TargetRepositoryID != w.second.ID || outgoing[0].TargetNumber != 1 {
		t.Errorf("the outgoing reference is %+v", outgoing[0])
	}
	if outgoing[0].TargetRepository != "second" {
		t.Errorf("the outgoing reference does not name the target repository: %+v", outgoing[0])
	}

	incoming, err := w.store.Incoming(ctx, w.second.ID, 1)
	if err != nil {
		t.Fatalf("Incoming: %v", err)
	}
	// **The other end sees it**, which is what makes both timelines true.
	if len(incoming) != 1 {
		t.Fatalf("the incoming references are %+v", incoming)
	}
	if incoming[0].SourceRepositoryID != w.first.ID || incoming[0].SourceNumber != w.firstIssue.Number {
		t.Errorf("the incoming reference is %+v", incoming[0])
	}
	if incoming[0].SourceRepository != "first" {
		t.Errorf("the incoming reference does not name the source repository: %+v", incoming[0])
	}
}

// TestAnIncomingReadIsScopedToItsTarget keeps one target's references off another's timeline.
func TestAnIncomingReadIsScopedToItsTarget(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	// **Both references go in one call, because a second call replaces the first.** Writing them
	// separately was the first version of this test, and it failed for the right reason: recording is a
	// replace, so the second `record` removed what the first wrote. That is also why the test asserts the
	// row count before it reads, so it cannot pass while the premise is wrong.
	if err := w.store.Record(ctx, references.RecordInput{
		SourceIssueID: w.firstIssue.ID,
		References: []references.Reference{
			{TargetRepositoryID: w.second.ID, Number: 1},
			{TargetRepositoryID: w.first.ID, Number: 5},
		},
		Written: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := w.count(); got != 2 {
		t.Fatalf("the table holds %d rows, want 2", got)
	}

	// The second repository's issue 1 sees exactly one.
	incoming, err := w.store.Incoming(ctx, w.second.ID, 1)
	if err != nil {
		t.Fatalf("Incoming: %v", err)
	}
	if len(incoming) != 1 {
		t.Errorf("issue 1 of the second repository sees %d incoming references, want 1", len(incoming))
	}

	// And a number nobody referenced sees none.
	empty, err := w.store.Incoming(ctx, w.second.ID, 99)
	if err != nil {
		t.Fatalf("Incoming: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("an unreferenced number sees %+v", empty)
	}
	// The first repository's issue 5 sees its own, which is a different target from the second's issue 1.
	own, err := w.store.Incoming(ctx, w.first.ID, 5)
	if err != nil {
		t.Fatalf("Incoming: %v", err)
	}
	if len(own) != 1 {
		t.Errorf("the same-repository target sees %d incoming references, want 1", len(own))
	}
}

// TestATargetThatDoesNotExistCanBeRecorded covers the reason the target is not a foreign key.
//
// GitHub's references are routinely written before the target exists — "the same bug as #12" where `#12`
// is filed an hour later. A foreign key would refuse the row, and the reference would be invisible forever
// rather than resolving when the target appears.
func TestATargetThatDoesNotExistCanBeRecorded(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	// Number 999 does not exist and never will in this world.
	w.record(w.second.ID, 999)
	if got := w.count(); got != 1 {
		t.Fatalf("a reference to a nonexistent target was refused: %d rows", got)
	}

	// And it reads back, so the forward reference is preserved until the target exists.
	outgoing, err := w.store.Outgoing(ctx, w.firstIssue.ID)
	if err != nil {
		t.Fatalf("Outgoing: %v", err)
	}
	if len(outgoing) != 1 || outgoing[0].TargetNumber != 999 {
		t.Errorf("the outgoing reference is %+v", outgoing)
	}
}

// TestTheWrittenInstantIsTheTextsOwn covers why `created_at` is passed in rather than defaulted.
//
// A reference recorded by a later re-parse must still belong where it appeared in the conversation, so the
// order a timeline shows is the conversation's rather than the order rows happened to be inserted.
func TestTheWrittenInstantIsTheTextsOwn(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	// Two references written an hour apart, recorded in the opposite order.
	early := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	late := time.Now().UTC().Truncate(time.Microsecond)
	if err := w.store.Record(ctx, references.RecordInput{
		SourceIssueID: w.firstIssue.ID,
		References:    []references.Reference{{TargetRepositoryID: w.second.ID, Number: 2}},
		Written:       late,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := w.store.Record(ctx, references.RecordInput{
		SourceIssueID: w.firstIssue.ID,
		References: []references.Reference{
			{TargetRepositoryID: w.second.ID, Number: 1},
		},
		Written: early,
	}); err != nil {
		// The second recording replaces the first, so both must be written together for this test to say
		// anything — which is itself the replace semantics being exercised.
		t.Fatalf("Record: %v", err)
	}

	// One row, because the second record replaced the first: that is the replace semantics, and it is
	// asserted explicitly here so this test cannot pass for the wrong reason.
	if got := w.count(); got != 1 {
		t.Fatalf("the table holds %d rows, want 1 — recording replaces", got)
	}

	// Now write both in one call, with the earlier one's instant preserved.
	if err := w.store.Record(ctx, references.RecordInput{
		SourceIssueID: w.firstIssue.ID,
		References: []references.Reference{
			{TargetRepositoryID: w.second.ID, Number: 1},
			{TargetRepositoryID: w.second.ID, Number: 2},
		},
		// One instant for the whole call, which is what a body's references all share.
		Written: early,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	var stored time.Time
	if err := w.db.QueryRowContext(ctx, `
		SELECT created_at FROM issue_references
		WHERE source_issue_id = $1 AND target_number = 1`, w.firstIssue.ID).Scan(&stored); err != nil {
		t.Fatalf("read the instant: %v", err)
	}
	if !stored.UTC().Truncate(time.Microsecond).Equal(early) {
		t.Errorf("the stored instant is %v, want the text's own %v", stored.UTC(), early)
	}
}

// TestASelfReferenceIsSkippedByTheCaller covers where that rule is decided.
//
// A `CHECK` cannot express it — the source is an identifier in this table while the target is a
// `(repository, number)` pair, so the comparison needs a join — so the caller holds both facts and marks
// the reference. GitHub treats a self-reference as ordinary text.
func TestASelfReferenceIsSkippedByTheCaller(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()

	if err := w.store.Record(ctx, references.RecordInput{
		SourceIssueID: w.firstIssue.ID,
		References: []references.Reference{{
			TargetRepositoryID: w.first.ID, Number: w.firstIssue.Number, SelfTarget: true,
		}},
		Written: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if got := w.count(); got != 0 {
		t.Errorf("a self-reference was recorded: %d rows", got)
	}

	// A reference from the same issue to a **different** number in the same repository is not a
	// self-reference and is recorded.
	w.record(w.first.ID, 7)
	if got := w.count(); got != 1 {
		t.Errorf("an ordinary same-repository reference was skipped: %d rows", got)
	}
}
