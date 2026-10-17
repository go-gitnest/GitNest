package milestones

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/repository"
)

// The milestone rules.
//
// The acceptance criteria this workstream answers are asserted here against a fake: case-folded
// uniqueness, the closed-milestone rejection, and the progress derivation. The claims that are properties
// of the database — that the constraint decides a title conflict, that the number is gap-free and never
// reused, and that deleting a milestone detaches its issues — are asserted against PostgreSQL in
// `postgres_test.go`.

const (
	repoID  = "01920000-0000-7000-8000-0000000000a1"
	issueID = "01920000-0000-7000-8000-0000000000a2"
	aliceID = "01920000-0000-7000-8000-0000000000a3"
	bobID   = "01920000-0000-7000-8000-0000000000a4"
)

// fakeAccess answers from a level the test sets, collapsing "no access" into not-found the way the
// repository service does.
type fakeAccess struct{ level repository.Level }

func (f *fakeAccess) Require(_ context.Context, _ repository.Principal, _ string, required repository.Level) (repository.Repository, repository.Level, error) {
	if f.level == repository.LevelNone {
		return repository.Repository{}, repository.LevelNone, repository.ErrNotFound
	}
	if !f.level.Allows(required) {
		return repository.Repository{}, f.level, repository.ErrForbidden
	}
	return repository.Repository{ID: repoID, Name: "a-repo"}, f.level, nil
}

// fakeStore is an in-memory Store, with the uniqueness rule the database's constraint implements.
type fakeStore struct {
	milestones map[string]Milestone
	byTitle    map[string]string
	byNumber   map[int64]string
	// attached maps an issue identifier to the milestone identifier it carries.
	attached map[string]string
	// issueStates maps an issue identifier to its state, so progress can be derived.
	issueStates map[string]State
	next        int64
	events      []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		milestones: map[string]Milestone{}, byTitle: map[string]string{},
		byNumber: map[int64]string{}, attached: map[string]string{},
		issueStates: map[string]State{issueID: StateOpen}, next: 1,
	}
}

func (f *fakeStore) Create(_ context.Context, input StoreCreate) (Milestone, error) {
	if _, taken := f.byTitle[input.TitleFolded]; taken {
		return Milestone{}, ErrTitleTaken
	}
	number := f.next
	f.next++

	created := Milestone{
		ID: input.ID, RepositoryID: input.RepositoryID, Number: number,
		Title: input.Title, Description: input.Description, DueOn: input.DueOn,
		State: StateOpen, CreatedAt: time.Now().UTC(),
	}
	f.milestones[input.ID] = created
	f.byTitle[input.TitleFolded] = input.ID
	f.byNumber[number] = input.ID
	return created, nil
}

func (f *fakeStore) GetByNumber(_ context.Context, _ string, number int64) (Milestone, error) {
	id, found := f.byNumber[number]
	if !found {
		return Milestone{}, ErrNotFound
	}
	return f.milestones[id], nil
}

func (f *fakeStore) List(_ context.Context, _ string, state State) ([]Milestone, error) {
	found := []Milestone{}
	for number := f.next - 1; number >= 1; number-- {
		id, present := f.byNumber[number]
		if !present {
			continue
		}
		milestone := f.milestones[id]
		if state != "" && milestone.State != state {
			continue
		}
		found = append(found, milestone)
	}
	return found, nil
}

func (f *fakeStore) Update(_ context.Context, input StoreUpdate) (Milestone, error) {
	id, found := f.byNumber[input.Number]
	if !found {
		return Milestone{}, ErrNotFound
	}
	existing := f.milestones[id]
	if holder, taken := f.byTitle[input.TitleFolded]; taken && holder != id {
		return Milestone{}, ErrTitleTaken
	}
	delete(f.byTitle, Fold(existing.Title))
	existing.Title, existing.Description = input.Title, input.Description
	if input.DueOnSet {
		existing.DueOn = input.DueOn
	}
	f.milestones[id] = existing
	f.byTitle[input.TitleFolded] = id
	return existing, nil
}

func (f *fakeStore) SetState(_ context.Context, input StoreSetState) (Milestone, error) {
	id, found := f.byNumber[input.Number]
	if !found {
		return Milestone{}, ErrNotFound
	}
	existing := f.milestones[id]
	if existing.State != input.Expect {
		return Milestone{}, ErrAlreadyInState
	}
	existing.State = input.State
	if input.State == StateClosed {
		at := time.Now().UTC()
		existing.ClosedAt = &at
	} else {
		existing.ClosedAt = nil
	}
	f.milestones[id] = existing
	if input.Event.Action != "" {
		f.events = append(f.events, input.Event.Action)
	}
	return existing, nil
}

func (f *fakeStore) Delete(_ context.Context, input StoreDelete) error {
	id, found := f.byNumber[input.Number]
	if !found {
		return ErrNotFound
	}
	delete(f.byTitle, Fold(f.milestones[id].Title))
	delete(f.byNumber, input.Number)
	delete(f.milestones, id)
	// The issues are detached, not deleted — the cascade's job in the database.
	for issueID, milestoneID := range f.attached {
		if milestoneID == id {
			delete(f.attached, issueID)
		}
	}
	f.events = append(f.events, input.Event.Action)
	return nil
}

func (f *fakeStore) Progress(_ context.Context, _ string) (map[string]Progress, error) {
	// Derived on every call, which is what makes the counts unable to go stale.
	progress := map[string]Progress{}
	for id := range f.milestones {
		progress[id] = Progress{}
	}
	for issueID, milestoneID := range f.attached {
		counts := progress[milestoneID]
		counts.Total++
		if f.issueStates[issueID] == StateClosed {
			counts.Closed++
		}
		progress[milestoneID] = counts
	}
	return progress, nil
}

func (f *fakeStore) Attach(_ context.Context, input StoreAttach) error {
	f.attached[input.IssueID] = input.MilestoneID
	return nil
}

func (f *fakeStore) Detach(_ context.Context, issueID string) (bool, error) {
	if _, found := f.attached[issueID]; !found {
		return false, nil
	}
	delete(f.attached, issueID)
	return true, nil
}

func (f *fakeStore) MilestoneOf(_ context.Context, issueID string) (int64, error) {
	id, found := f.attached[issueID]
	if !found {
		return 0, nil
	}
	return f.milestones[id].Number, nil
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

// add creates one milestone and fails the test if it is refused.
func (f *fixture) add(t *testing.T, title string) Milestone {
	t.Helper()
	created, err := f.service.Create(context.Background(), CreateInput{
		Actor: author(aliceID), RepositoryID: repoID, Title: title,
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", title, err)
	}
	return created
}

// TestManagingAMilestoneRequiresWriteNotAdmin is the level derivation, asserted as a difference.
//
// The catalogue gives a milestone's rows the actor `user` where a label's say `admin`, so managing a
// milestone needs **write** while managing a label needs admin. This asserts both halves in one place, so
// neither can drift into the other.
func TestManagingAMilestoneRequiresWriteNotAdmin(t *testing.T) {
	ctx := context.Background()

	// A reader is refused.
	reader := newFixture(t, repository.LevelRead)
	if _, err := reader.service.Create(ctx, CreateInput{
		Actor: author(aliceID), RepositoryID: repoID, Title: "v1.0",
	}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a reader's create = %v, want ErrForbidden", err)
	}
	if len(reader.store.milestones) != 0 {
		t.Error("a refused create stored a milestone")
	}

	// **A writer is allowed** — which is what distinguishes this from a label, where a writer is refused
	// and only an admin may manage one.
	writer := newFixture(t, repository.LevelWrite)
	writer.add(t, "v1.0")

	if err := writer.service.Delete(ctx, DeleteInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: 1,
	}); err != nil {
		t.Errorf("a writer's delete: %v", err)
	}
}

// TestNoAccessIsNotFoundNotForbidden covers the T-A4 shape.
func TestNoAccessIsNotFoundNotForbidden(t *testing.T) {
	f := newFixture(t, repository.LevelNone)
	ctx := context.Background()

	for _, test := range []struct {
		name string
		call func() error
	}{
		{name: "list", call: func() error {
			_, err := f.service.List(ctx, ListInput{Actor: author(aliceID), RepositoryID: repoID})
			return err
		}},
		{name: "get", call: func() error {
			_, err := f.service.Get(ctx, GetInput{Actor: author(aliceID), RepositoryID: repoID, Number: 1})
			return err
		}},
		{name: "create", call: func() error {
			_, err := f.service.Create(ctx, CreateInput{
				Actor: author(aliceID), RepositoryID: repoID, Title: "v1.0",
			})
			return err
		}},
		{name: "progress", call: func() error {
			_, err := f.service.Progress(ctx, ProgressInput{Actor: author(aliceID), RepositoryID: repoID})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.call(); !errors.Is(err, ErrNotFound) {
				t.Errorf("%s without access = %v, want ErrNotFound", test.name, err)
			}
		})
	}
}

// TestTitlesAreUniqueAfterASCIIFolding covers the uniqueness rule.
func TestTitlesAreUniqueAfterASCIIFolding(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	f.add(t, "Version 1.0")

	for _, variant := range []string{"version 1.0", "VERSION 1.0", "  Version 1.0  ", "vErSiOn 1.0"} {
		if _, err := f.service.Create(ctx, CreateInput{
			Actor: author(aliceID), RepositoryID: repoID, Title: variant,
		}); !errors.Is(err, ErrTitleTaken) {
			t.Errorf("creating %q = %v, want ErrTitleTaken", variant, err)
		}
	}

	// **Uniqueness spans open and closed milestones**, which §6.5 requires: closing one does not free its
	// title for a second.
	if _, err := f.service.Close(ctx, CloseInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: 1,
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := f.service.Create(ctx, CreateInput{
		Actor: author(aliceID), RepositoryID: repoID, Title: "version 1.0",
	}); !errors.Is(err, ErrTitleTaken) {
		t.Errorf("creating a closed milestone's title = %v, want ErrTitleTaken", err)
	}

	// A non-ASCII difference is not a collision, because the folding is Unicode-free.
	if _, err := f.service.Create(ctx, CreateInput{
		Actor: author(aliceID), RepositoryID: repoID, Title: "Vérsion",
	}); err != nil {
		t.Errorf("a non-ASCII title was refused: %v", err)
	}
}

// TestATitleGrammarIsEnforced covers the bounds, including the boundaries.
func TestATitleGrammarIsEnforced(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	for _, test := range []struct {
		name    string
		title   string
		wantErr error
	}{
		{name: "empty", title: "", wantErr: ErrInvalidTitle},
		{name: "whitespace only", title: "   ", wantErr: ErrInvalidTitle},
		{name: "at the bound", title: strings.Repeat("x", MaxTitleLength)},
		{name: "over the bound", title: strings.Repeat("x", MaxTitleLength+1), wantErr: ErrInvalidTitle},
		{name: "a control character", title: "a\x01b", wantErr: ErrInvalidTitle},
		{name: "a null byte", title: "a\x00b", wantErr: ErrInvalidTitle},
		// A milestone title permits more than a label's name does, because §6.5 does not restrict its
		// characters — only its length.
		{name: "punctuation", title: "v2.0 — the \"big\" one!"},
		{name: "non-ASCII", title: "🚀 launch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.service.Create(ctx, CreateInput{
				Actor: author(aliceID), RepositoryID: repoID, Title: test.title,
			})
			if test.wantErr == nil {
				if err != nil {
					t.Fatalf("a valid title %q was refused: %v", test.title, err)
				}
				return
			}
			if !errors.Is(err, test.wantErr) {
				t.Errorf("error = %v, want %v", err, test.wantErr)
			}
		})
	}

	// A title is trimmed.
	created := f.add(t, "  a padded title  ")
	if created.Title != "a padded title" {
		t.Errorf("the stored title is %q, want it trimmed", created.Title)
	}
}

// TestTheDueDateIsACalendarDate covers the date grammar.
func TestTheDueDateIsACalendarDate(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	// Accepted: a calendar date, or nothing.
	for _, test := range []struct {
		input string
		want  string
	}{
		{input: "", want: ""},
		{input: "2026-10-14", want: "2026-10-14"},
		{input: "  2026-10-14  ", want: "2026-10-14"},
	} {
		created, err := f.service.Create(ctx, CreateInput{
			Actor: author(aliceID), RepositoryID: repoID,
			Title: "due-" + test.input, DueDate: test.input,
		})
		if err != nil {
			t.Errorf("due date %q was refused: %v", test.input, err)
			continue
		}
		if got := FormatDueDate(created.DueOn); got != test.want {
			t.Errorf("due date %q stored as %q, want %q", test.input, got, test.want)
		}
	}

	// **A timestamp is refused rather than truncated**, so a client that sends an instant is told the
	// field's shape instead of having its value silently changed.
	for _, input := range []string{
		"2026-10-14T12:00:00Z", "2026-10-14 12:00", "14/10/2026", "2026-13-01", "2026-02-30", "not-a-date",
	} {
		if _, err := f.service.Create(ctx, CreateInput{
			Actor: author(aliceID), RepositoryID: repoID, Title: "bad-" + input, DueDate: input,
		}); !errors.Is(err, ErrInvalidDueDate) {
			t.Errorf("due date %q = %v, want ErrInvalidDueDate", input, err)
		}
	}

	// The stored value is midnight UTC, which is what the payload catalogue's `due_on` carries.
	created, err := f.service.Create(ctx, CreateInput{
		Actor: author(aliceID), RepositoryID: repoID, Title: "a date", DueDate: "2026-10-14",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.DueOn == nil || created.DueOn.UTC().Hour() != 0 || created.DueOn.Location() != time.UTC {
		t.Errorf("the stored due date is %v, want midnight UTC", created.DueOn)
	}
}

// TestAClosedMilestoneRejectsNewAttachments is W2.4's acceptance criterion.
func TestAClosedMilestoneRejectsNewAttachments(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	milestone := f.add(t, "v1.0")

	// Open: an attachment is accepted.
	if err := f.service.Attach(ctx, AttachInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, Number: milestone.Number,
	}); err != nil {
		t.Fatalf("Attach to an open milestone: %v", err)
	}

	// Close it, then a **different** issue must be refused.
	if _, err := f.service.Close(ctx, CloseInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: milestone.Number,
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}

	const otherIssue = "01920000-0000-7000-8000-0000000000a5"
	if err := f.service.Attach(ctx, AttachInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: otherIssue, Number: milestone.Number,
	}); !errors.Is(err, ErrClosed) {
		t.Errorf("an attachment to a closed milestone = %v, want ErrClosed", err)
	}
	if _, found := f.store.attached[otherIssue]; found {
		t.Error("a refused attachment was recorded")
	}

	// **Detaching is still allowed**, because removing work from a milestone is not adding it — and
	// refusing would strand an issue on a milestone nobody can change.
	if err := f.service.Detach(ctx, DetachInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID,
	}); err != nil {
		t.Errorf("Detach from a closed milestone: %v", err)
	}

	// Reopening makes it accept issues again.
	if _, err := f.service.Reopen(ctx, ReopenInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: milestone.Number,
	}); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if err := f.service.Attach(ctx, AttachInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: otherIssue, Number: milestone.Number,
	}); err != nil {
		t.Errorf("Attach after a reopen: %v", err)
	}
}

// TestProgressIsDerivedAndStaysCorrect is the other acceptance criterion.
//
// "Progress counts stay correct after an issue is closed, reopened, or relabeled" — the counts are derived
// from the attachment and the issue's state on every read, so they cannot go stale, and a relabeling is a
// different table entirely.
func TestProgressIsDerivedAndStaysCorrect(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	milestone := f.add(t, "v1.0")

	// Two issues, one of them closed.
	const secondIssue = "01920000-0000-7000-8000-0000000000a6"
	f.store.issueStates[secondIssue] = StateClosed
	f.store.issueStates[issueID] = StateOpen
	for _, id := range []string{issueID, secondIssue} {
		if err := f.service.Attach(ctx, AttachInput{
			Actor: author(aliceID), RepositoryID: repoID, IssueID: id, Number: milestone.Number,
		}); err != nil {
			t.Fatalf("Attach: %v", err)
		}
	}

	progress, err := f.service.Progress(ctx, ProgressInput{
		Actor: author(aliceID), RepositoryID: repoID,
	})
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	counts := progress[milestone.ID]
	if counts.Total != 2 || counts.Closed != 1 || counts.Open() != 1 {
		t.Errorf("the progress is %+v, want 2 total, 1 closed, 1 open", counts)
	}
	if counts.PercentComplete() != 50 {
		t.Errorf("the completion is %d%%, want 50%%", counts.PercentComplete())
	}

	// **Closing the open issue changes the split.**
	f.store.issueStates[issueID] = StateClosed
	progress, _ = f.service.Progress(ctx, ProgressInput{Actor: author(aliceID), RepositoryID: repoID})
	counts = progress[milestone.ID]
	if counts.Closed != 2 || counts.PercentComplete() != 100 {
		t.Errorf("after closing: %+v, want 2 closed and 100%%", counts)
	}

	// **Reopening one changes it back.**
	f.store.issueStates[issueID] = StateOpen
	progress, _ = f.service.Progress(ctx, ProgressInput{Actor: author(aliceID), RepositoryID: repoID})
	counts = progress[milestone.ID]
	if counts.Closed != 1 || counts.PercentComplete() != 50 {
		t.Errorf("after reopening: %+v, want 1 closed and 50%%", counts)
	}

	// **Detaching removes it from the count**, which is the "relabeled" half: moving an issue to another
	// milestone (or none) must change both milestones' counts and nothing else.
	if err := f.service.Detach(ctx, DetachInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: secondIssue,
	}); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	progress, _ = f.service.Progress(ctx, ProgressInput{Actor: author(aliceID), RepositoryID: repoID})
	counts = progress[milestone.ID]
	if counts.Total != 1 {
		t.Errorf("after detaching: %+v, want 1 total", counts)
	}
	// The remaining one is open, so the completion is 0%.
	if counts.PercentComplete() != 0 {
		t.Errorf("the completion is %d%%, want 0%% with one open issue", counts.PercentComplete())
	}
}

// TestProgressOfAnEmptyMilestoneIsZero keeps an empty milestone from claiming completion.
func TestProgressOfAnEmptyMilestoneIsZero(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	milestone := f.add(t, "v1.0")

	progress, err := f.service.Progress(context.Background(), ProgressInput{
		Actor: author(aliceID), RepositoryID: repoID,
	})
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	counts := progress[milestone.ID]
	if counts.Total != 0 {
		t.Errorf("the total is %d, want 0", counts.Total)
	}
	if counts.PercentComplete() != 0 {
		t.Errorf("an empty milestone is %d%% complete, want 0%% — nothing has been done", counts.PercentComplete())
	}
}

// TestTheRecordShape covers which operations write a ledger record and which do not.
//
// The catalogue defines `milestone.closed` and `milestone.deleted` and nothing else, so a create, an
// update, and a reopen write nothing.
func TestTheRecordShape(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	milestone := f.add(t, "v1.0")
	if len(f.store.events) != 0 {
		t.Fatalf("a create wrote %v; the catalogue defines no milestone.created", f.store.events)
	}

	// An update writes nothing.
	title := "v1.0.0"
	if _, err := f.service.Update(ctx, UpdateInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: milestone.Number, Title: &title,
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(f.store.events) != 0 {
		t.Errorf("an update wrote %v; the catalogue defines no milestone.edited", f.store.events)
	}

	// A close writes the one state action.
	if _, err := f.service.Close(ctx, CloseInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: milestone.Number,
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(f.store.events) != 1 || f.store.events[0] != "milestone.closed" {
		t.Errorf("the events are %v, want one milestone.closed", f.store.events)
	}

	// **A reopen writes nothing**, because the catalogue defines no `milestone.opened`.
	if _, err := f.service.Reopen(ctx, ReopenInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: milestone.Number,
	}); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if len(f.store.events) != 1 {
		t.Errorf("a reopen wrote %v; the catalogue defines no milestone.opened", f.store.events)
	}

	// A delete writes the other one.
	if err := f.service.Delete(ctx, DeleteInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: milestone.Number,
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(f.store.events) != 2 || f.store.events[1] != "milestone.deleted" {
		t.Errorf("the events are %v, want milestone.closed then milestone.deleted", f.store.events)
	}
}

// TestAnUpdateThatChangesNothingIsRefused keeps a write from happening for a change that did not occur.
func TestAnUpdateThatChangesNothingIsRefused(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	milestone := f.add(t, "v1.0")

	if _, err := f.service.Update(ctx, UpdateInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: milestone.Number,
	}); !errors.Is(err, ErrNoChange) {
		t.Errorf("an update naming no field = %v, want ErrNoChange", err)
	}
	same := "v1.0"
	if _, err := f.service.Update(ctx, UpdateInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: milestone.Number, Title: &same,
	}); !errors.Is(err, ErrNoChange) {
		t.Errorf("an update with the same title = %v, want ErrNoChange", err)
	}

	// A rename onto another milestone's folded title is a conflict.
	f.add(t, "v2.0")
	conflict := "V2.0"
	if _, err := f.service.Update(ctx, UpdateInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: milestone.Number, Title: &conflict,
	}); !errors.Is(err, ErrTitleTaken) {
		t.Errorf("a rename onto an existing title = %v, want ErrTitleTaken", err)
	}
}

// TestTheStateMachine covers the close and reopen guards.
func TestTheStateMachine(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()
	milestone := f.add(t, "v1.0")

	if _, err := f.service.Close(ctx, CloseInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: milestone.Number,
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := f.service.Close(ctx, CloseInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: milestone.Number,
	}); !errors.Is(err, ErrAlreadyInState) {
		t.Errorf("a second close = %v, want ErrAlreadyInState", err)
	}
	if _, err := f.service.Reopen(ctx, ReopenInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: milestone.Number,
	}); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if _, err := f.service.Reopen(ctx, ReopenInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: milestone.Number,
	}); !errors.Is(err, ErrAlreadyInState) {
		t.Errorf("a second reopen = %v, want ErrAlreadyInState", err)
	}

	// Reopening clears the close, because the pair constraint requires the two to move together.
	reopened, _ := f.store.GetByNumber(ctx, repoID, milestone.Number)
	if reopened.ClosedAt != nil {
		t.Errorf("reopening left a close recorded: %v", reopened.ClosedAt)
	}
}

// TestTheOverdueComparison covers the boundary, with an injected clock so the test does not depend on the
// wall clock.
func TestTheOverdueComparison(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	due := "2026-10-14"
	created, err := f.service.Create(ctx, CreateInput{
		Actor: author(aliceID), RepositoryID: repoID, Title: "with a due date", DueDate: due,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The day itself is not overdue, and neither is the moment before the next day begins.
	for _, when := range []string{"2026-10-14T00:00:00Z", "2026-10-14T23:59:59Z"} {
		at, _ := time.Parse(time.RFC3339, when)
		if f.service.WithClock(func() time.Time { return at }).Overdue(created) {
			t.Errorf("at %s the milestone is reported overdue, but the date has not passed", when)
		}
	}
	// The next day it is.
	after, _ := time.Parse(time.RFC3339, "2026-10-15T00:00:00Z")
	if !f.service.WithClock(func() time.Time { return after }).Overdue(created) {
		t.Error("the day after the due date the milestone is not reported overdue")
	}
	// A closed milestone is not overdue, because there is nothing left to be late for.
	closed, err := f.service.Close(ctx, CloseInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: created.Number,
	})
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if f.service.WithClock(func() time.Time { return after }).Overdue(closed) {
		t.Error("a closed milestone is reported overdue")
	}
	// A milestone with no due date is never overdue.
	noDue := f.add(t, "no due date")
	if f.service.WithClock(func() time.Time { return after }).Overdue(noDue) {
		t.Error("a milestone with no due date is reported overdue")
	}
}

// TestTheStateFilterAndList covers the listing and its filter.
func TestTheStateFilterAndList(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)
	ctx := context.Background()

	first := f.add(t, "v1.0")
	f.add(t, "v2.0")
	if _, err := f.service.Close(ctx, CloseInput{
		Actor: author(aliceID), RepositoryID: repoID, Number: first.Number,
	}); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, test := range []struct {
		state State
		want  int
	}{
		{state: "", want: 2},
		{state: StateOpen, want: 1},
		{state: StateClosed, want: 1},
	} {
		found, err := f.service.List(ctx, ListInput{
			Actor: author(aliceID), RepositoryID: repoID, State: test.state,
		})
		if err != nil {
			t.Fatalf("List(%q): %v", test.state, err)
		}
		if len(found) != test.want {
			t.Errorf("the %q filter returned %d milestones, want %d", test.state, len(found), test.want)
		}
	}

	// The filter parser accepts the empty value and `all` as no filter.
	for _, value := range []string{"", "all", "ALL", " open ", "closed"} {
		if _, err := ParseStateFilter(value); err != nil {
			t.Errorf("ParseStateFilter(%q): %v", value, err)
		}
	}
	if _, err := ParseStateFilter("merged"); !errors.Is(err, ErrInvalidState) {
		t.Errorf("an unknown filter = %v, want ErrInvalidState", err)
	}
}

// TestDetachingAnIssueThatHasNoneIsRefused keeps a client from being told it succeeded when nothing
// happened.
func TestDetachingAnIssueThatHasNoneIsRefused(t *testing.T) {
	f := newFixture(t, repository.LevelWrite)

	if err := f.service.Detach(context.Background(), DetachInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID,
	}); !errors.Is(err, ErrNotAttached) {
		t.Errorf("detaching an unattached issue = %v, want ErrNotAttached", err)
	}
}
