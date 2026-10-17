package labels

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/go-gitnest/GitNest/internal/repository"
)

// The label rules.
//
// The acceptance criteria this workstream answers are asserted here against a fake: case-folded
// uniqueness, the colour grammar, and the attachment lifecycle. The claims that are properties of the
// database — that the unique constraint is what decides a conflict, and that a label's deletion
// removes its attachments without touching issues — are asserted against PostgreSQL in
// `postgres_test.go`, because a fake cannot establish either.

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
	labels map[string]Label
	// byName maps a folded name to a label identifier, which is what the unique constraint enforces.
	byName map[string]string
	// attached maps an issue identifier to the set of label identifiers on it.
	attached map[string]map[string]bool
	events   []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		labels: map[string]Label{}, byName: map[string]string{},
		attached: map[string]map[string]bool{},
	}
}

func (f *fakeStore) Create(_ context.Context, input StoreCreate) (Label, error) {
	// The uniqueness decision, in the place the database makes it: on the folded name, so two names
	// differing only by ASCII case collide.
	if _, taken := f.byName[input.NameFolded]; taken {
		return Label{}, ErrNameTaken
	}
	created := Label{
		ID: input.ID, RepositoryID: input.RepositoryID, Name: input.Name,
		Color: input.Color, Description: input.Description,
	}
	f.labels[input.ID] = created
	f.byName[input.NameFolded] = input.ID
	f.events = append(f.events, input.Event.Action)
	return created, nil
}

func (f *fakeStore) Get(_ context.Context, repositoryID, labelID string) (Label, error) {
	label, found := f.labels[labelID]
	if !found || label.RepositoryID != repositoryID {
		return Label{}, ErrNotFound
	}
	return label, nil
}

func (f *fakeStore) FindByName(_ context.Context, _ string, nameFolded string) (Label, error) {
	id, found := f.byName[nameFolded]
	if !found {
		return Label{}, ErrNotFound
	}
	return f.labels[id], nil
}

func (f *fakeStore) List(_ context.Context, _ string) ([]Label, error) {
	// Ordered by folded name, as the query is.
	found := []Label{}
	for _, label := range f.labels {
		found = append(found, label)
	}
	for i := 1; i < len(found); i++ {
		for j := i; j > 0 && Fold(found[j-1].Name) > Fold(found[j].Name); j-- {
			found[j-1], found[j] = found[j], found[j-1]
		}
	}
	return found, nil
}

func (f *fakeStore) Update(_ context.Context, input StoreUpdate) (Label, error) {
	existing, found := f.labels[input.LabelID]
	if !found {
		return Label{}, ErrNotFound
	}
	if holder, taken := f.byName[input.NameFolded]; taken && holder != input.LabelID {
		return Label{}, ErrNameTaken
	}
	delete(f.byName, Fold(existing.Name))
	existing.Name, existing.Color, existing.Description = input.Name, input.Color, input.Description
	f.labels[input.LabelID] = existing
	f.byName[input.NameFolded] = input.LabelID
	return existing, nil
}

func (f *fakeStore) Delete(_ context.Context, input StoreDelete) error {
	if _, found := f.labels[input.LabelID]; !found {
		return ErrNotFound
	}
	delete(f.byName, Fold(f.labels[input.LabelID].Name))
	delete(f.labels, input.LabelID)
	// The attachments go with the label; the issues they pointed at are untouched, which is the
	// acceptance's requirement and the cascade's job in the database.
	for issueID, set := range f.attached {
		delete(set, input.LabelID)
		if len(set) == 0 {
			delete(f.attached, issueID)
		}
	}
	f.events = append(f.events, input.Event.Action)
	return nil
}

func (f *fakeStore) Attach(_ context.Context, issueID, labelID string) (bool, error) {
	if f.attached[issueID] == nil {
		f.attached[issueID] = map[string]bool{}
	}
	if f.attached[issueID][labelID] {
		return false, nil
	}
	f.attached[issueID][labelID] = true
	return true, nil
}

func (f *fakeStore) Detach(_ context.Context, issueID, labelID string) (bool, error) {
	if !f.attached[issueID][labelID] {
		return false, nil
	}
	delete(f.attached[issueID], labelID)
	return true, nil
}

func (f *fakeStore) ForIssue(_ context.Context, issueID string) ([]Label, error) {
	found := []Label{}
	for labelID := range f.attached[issueID] {
		found = append(found, f.labels[labelID])
	}
	for i := 1; i < len(found); i++ {
		for j := i; j > 0 && Fold(found[j-1].Name) > Fold(found[j].Name); j-- {
			found[j-1], found[j] = found[j], found[j-1]
		}
	}
	return found, nil
}

func (f *fakeStore) CountsForRepository(_ context.Context, _ string) (map[string]int, error) {
	counts := map[string]int{}
	for labelID := range f.labels {
		counts[labelID] = 0
	}
	for _, set := range f.attached {
		for labelID := range set {
			counts[labelID]++
		}
	}
	return counts, nil
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

// add creates one label and fails the test if it is refused.
func (f *fixture) add(t *testing.T, name, color string) Label {
	t.Helper()
	created, err := f.service.Create(context.Background(), CreateInput{
		Actor: author(aliceID), RepositoryID: repoID, Name: name, Color: color,
	})
	if err != nil {
		t.Fatalf("Create(%q): %v", name, err)
	}
	return created
}

// TestManagingALabelRequiresRepositoryAdmin covers the level the catalogue's actor column decides.
//
// `label.created` and `label.deleted` say `admin`, and every repository-configuration event that says
// the same is gated by `LevelAdmin` in `internal/repository` — so a writer is refused and an admin is
// allowed.
func TestManagingALabelRequiresRepositoryAdmin(t *testing.T) {
	ctx := context.Background()

	for _, level := range []repository.Level{repository.LevelRead, repository.LevelWrite} {
		t.Run(level.String(), func(t *testing.T) {
			f := newFixture(t, level)
			if _, err := f.service.Create(ctx, CreateInput{
				Actor: author(aliceID), RepositoryID: repoID, Name: "bug", Color: "ff0000",
			}); !errors.Is(err, ErrForbidden) {
				t.Errorf("a create at %s = %v, want ErrForbidden", level, err)
			}
			if len(f.store.labels) != 0 {
				t.Error("a refused create stored a label")
			}
		})
	}

	f := newFixture(t, repository.LevelAdmin)
	f.add(t, "bug", "ff0000")

	if _, err := f.service.Update(ctx, UpdateInput{
		Actor: author(aliceID), RepositoryID: repoID, Name: "bug", Color: ptr("00ff00"),
	}); err != nil {
		t.Errorf("an admin's update: %v", err)
	}
	if err := f.service.Delete(ctx, DeleteInput{
		Actor: author(aliceID), RepositoryID: repoID, Name: "bug",
	}); err != nil {
		t.Errorf("an admin's delete: %v", err)
	}
}

// TestAttachingALabelRequiresRepositoryWrite covers the second level, which is deliberately lower.
func TestAttachingALabelRequiresRepositoryWrite(t *testing.T) {
	ctx := context.Background()

	// An admin creates the label.
	admin := newFixture(t, repository.LevelAdmin)
	admin.add(t, "bug", "ff0000")

	// A reader may see it but not apply it.
	reader := &fixture{service: NewService(&fakeAccess{level: repository.LevelRead}, admin.store),
		store: admin.store, access: &fakeAccess{level: repository.LevelRead}}
	if _, err := reader.service.Attach(ctx, AttachInput{
		Actor: author(bobID), RepositoryID: repoID, IssueID: issueID, Name: "bug",
	}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a reader's attach = %v, want ErrForbidden", err)
	}
	if len(admin.store.attached[issueID]) != 0 {
		t.Error("a refused attach recorded an attachment")
	}

	// A writer may.
	writer := &fixture{service: NewService(&fakeAccess{level: repository.LevelWrite}, admin.store),
		store: admin.store, access: &fakeAccess{level: repository.LevelWrite}}
	if _, err := writer.service.Attach(ctx, AttachInput{
		Actor: author(bobID), RepositoryID: repoID, IssueID: issueID, Name: "bug",
	}); err != nil {
		t.Errorf("a writer's attach: %v", err)
	}
	// And a reader sees the labels on the issue, which is what makes the refusal above a decision
	// rather than a broken query.
	found, err := reader.service.ForIssue(ctx, IssueLabelsInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID,
	})
	if err != nil {
		t.Fatalf("ForIssue: %v", err)
	}
	if len(found) != 1 || found[0].Name != "bug" {
		t.Errorf("a reader sees %+v, want the applied label", found)
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
			_, err := f.service.Get(ctx, GetInput{
				Actor: author(aliceID), RepositoryID: repoID, Name: "bug",
			})
			return err
		}},
		{name: "create", call: func() error {
			_, err := f.service.Create(ctx, CreateInput{
				Actor: author(aliceID), RepositoryID: repoID, Name: "bug", Color: "ff0000",
			})
			return err
		}},
		{name: "attach", call: func() error {
			_, err := f.service.Attach(ctx, AttachInput{
				Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, Name: "bug",
			})
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Nothing is disclosed: an invisible repository and a missing one answer the same.
			if err := test.call(); !errors.Is(err, ErrNotFound) {
				t.Errorf("%s without access = %v, want ErrNotFound", test.name, err)
			}
		})
	}
}

// TestNamesAreUniqueAfterASCIIFolding is the acceptance criterion.
func TestNamesAreUniqueAfterASCIIFolding(t *testing.T) {
	f := newFixture(t, repository.LevelAdmin)
	ctx := context.Background()

	f.add(t, "Bug", "ff0000")

	// Every ASCII-cased variant collides, including one with surrounding space, which trims to the
	// same name.
	for _, variant := range []string{"bug", "BUG", "Bug", "  bug  ", "bUg"} {
		if _, err := f.service.Create(ctx, CreateInput{
			Actor: author(aliceID), RepositoryID: repoID, Name: variant, Color: "00ff00",
		}); !errors.Is(err, ErrNameTaken) {
			t.Errorf("creating %q = %v, want ErrNameTaken", variant, err)
		}
	}
	if len(f.store.labels) != 1 {
		t.Errorf("the store holds %d labels, want 1", len(f.store.labels))
	}

	// **A non-ASCII difference is not a collision**, because the folding is Unicode-free: §6.1 fixes
	// ASCII folding, so `Ünïcödé` and `ünïcödé` are two labels.
	if _, err := f.service.Create(ctx, CreateInput{
		Actor: author(aliceID), RepositoryID: repoID, Name: "Ünïcödé", Color: "0000ff",
	}); err != nil {
		t.Errorf("a non-ASCII name was refused: %v", err)
	}
	if _, err := f.service.Create(ctx, CreateInput{
		Actor: author(aliceID), RepositoryID: repoID, Name: "ünïcödé", Color: "0000ff",
	}); err != nil {
		t.Errorf("a non-ASCII name differing only in case was refused, but ASCII folding leaves it alone: %v", err)
	}
}

// TestTheNameGrammarIsEnforced covers the bounds, including the boundaries.
func TestTheNameGrammarIsEnforced(t *testing.T) {
	f := newFixture(t, repository.LevelAdmin)
	ctx := context.Background()

	for _, test := range []struct {
		name    string
		input   string
		wantErr error
	}{
		{name: "empty", input: "", wantErr: ErrInvalidName},
		{name: "whitespace only", input: "   ", wantErr: ErrInvalidName},
		{name: "at the bound", input: strings.Repeat("x", MaxNameLength)},
		{name: "over the bound", input: strings.Repeat("x", MaxNameLength+1), wantErr: ErrInvalidName},
		{name: "a control character", input: "a\x01b", wantErr: ErrInvalidName},
		{name: "a null byte", input: "a\x00b", wantErr: ErrInvalidName},
		// §6.5 permits any printable characters, so these are legitimate labels.
		{name: "punctuation", input: "good first issue!"},
		{name: "a slash and quotes", input: `a/b "quoted" 'x'`},
		{name: "an interior space", input: "needs  review"},
		{name: "non-ASCII", input: "🐛 bug"},
		{name: "markup", input: "<script>alert(1)</script>"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.service.Create(ctx, CreateInput{
				Actor: author(aliceID), RepositoryID: repoID, Name: test.input, Color: "ff0000",
			})
			if test.wantErr == nil {
				if err != nil {
					t.Fatalf("a valid name %q was refused: %v", test.input, err)
				}
				return
			}
			if !errors.Is(err, test.wantErr) {
				t.Errorf("error = %v, want %v", err, test.wantErr)
			}
		})
	}

	// A name is trimmed, so one whose only fault is padding is accepted and stored trimmed.
	for i, label := range f.store.labels {
		_ = i
		if label.Name != strings.TrimSpace(label.Name) {
			t.Errorf("the stored name %q is not trimmed", label.Name)
		}
	}
}

// TestTheColorGrammarIsEnforced is the T-B2 control, asserted at the boundary.
//
// Every value that reaches storage is six lowercase hexadecimal digits, so nothing can carry a quote, a
// semicolon, a parenthesis, or a control character into the context where a colour is rendered.
func TestTheColorGrammarIsEnforced(t *testing.T) {
	f := newFixture(t, repository.LevelAdmin)
	ctx := context.Background()

	// Accepted, and normalized to lowercase with no `#`.
	//
	// **Each attempt needs its own name, and the reason is the folding working.** Naming them
	// `"c-" + input` made `c-ff0000` and `c-FF0000` collide, because those two names differ only by
	// ASCII case — so the uniqueness rule refused the second attempt before the colour was ever
	// examined. The names are numbered instead, which isolates the colour from the name.
	accepted := []struct{ input, want string }{
		{"ff0000", "ff0000"},
		{"FF0000", "ff0000"},
		{"#ff0000", "ff0000"},
		{"#FF0000", "ff0000"},
		{" 00ff00 ", "00ff00"},
		{"AbCdEf", "abcdef"},
	}
	for index, test := range accepted {
		input, want := test.input, test.want
		created, err := f.service.Create(ctx, CreateInput{
			Actor: author(aliceID), RepositoryID: repoID,
			Name: "colour-" + strconv.Itoa(index), Color: input,
		})
		if err != nil {
			t.Errorf("color %q was refused: %v", input, err)
			continue
		}
		if created.Color != want {
			t.Errorf("color %q stored as %q, want %q", input, created.Color, want)
		}
		if created.DisplayColor() != "#"+want {
			t.Errorf("color %q displays as %q, want #%s", input, created.DisplayColor(), want)
		}
	}

	// Refused: everything outside the grammar, including the injection shapes T-B2 names.
	//
	// The name is numbered too, so a long colour cannot push it past the name bound and be refused for
	// the wrong reason — which is exactly what happened when the name was derived from the colour.
	for index, input := range []string{
		"", "red", "ff000", "ff00000", "ff00zz", "gg0000",
		"ff0000;", "ff0000\"", "#ff0000;background:url(x)",
		"red;background:url(javascript:alert(1))",
		"00000000000000000000000000000000000000000000000000000000000000",
		"fff fff", "ff-000", "۰۱۲۳۴۵",
	} {
		if _, err := f.service.Create(ctx, CreateInput{
			Actor: author(aliceID), RepositoryID: repoID,
			Name: "bad-colour-" + strconv.Itoa(index), Color: input,
		}); !errors.Is(err, ErrInvalidColor) {
			t.Errorf("color %q = %v, want ErrInvalidColor", input, err)
		}
	}
}

// TestAttachingAndDetachingAreIdempotent covers the attachment lifecycle.
//
// Applying a label twice succeeds and reports that nothing was new, because the operation's result is a
// state rather than an event; removing a label the issue does not carry is refused, so a client that
// lost track of the state is told.
func TestAttachingAndDetachingAreIdempotent(t *testing.T) {
	// Creating a label needs admin, so the label is made at that level and the level is then lowered to
	// the one attaching requires — which is also the level mapping being exercised.
	f := newFixture(t, repository.LevelAdmin)
	ctx := context.Background()
	f.add(t, "bug", "ff0000")
	f.access.level = repository.LevelWrite

	created, err := f.service.Attach(ctx, AttachInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, Name: "bug",
	})
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if !created {
		t.Error("the first attach reported that nothing was new")
	}

	again, err := f.service.Attach(ctx, AttachInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, Name: "bug",
	})
	if err != nil {
		t.Fatalf("a second Attach: %v", err)
	}
	if again {
		t.Error("a second attach reported that something was new")
	}

	if err := f.service.Detach(ctx, DetachInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, Name: "bug",
	}); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	if err := f.service.Detach(ctx, DetachInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, Name: "bug",
	}); !errors.Is(err, ErrNotAttached) {
		t.Errorf("a second detach = %v, want ErrNotAttached", err)
	}
}

// TestAttachingALabelFromAnotherRepositoryIsRefused covers the scoping: a label is resolved within the
// repository the caller named, so a label identifier cannot be reached through a repository it does not
// belong to.
func TestAttachingALabelFromAnotherRepositoryIsRefused(t *testing.T) {
	f := newFixture(t, repository.LevelAdmin)
	ctx := context.Background()
	f.access.level = repository.LevelWrite

	// The store holds a label, but `FindByName` is scoped to a repository — so asking for a name that is
	// not in *this* repository is a not-found. The fake keys by folded name alone, which is why this test
	// asserts the service's refusal through an absent name rather than through the fake's internals: the
	// real scoping is the store's `WHERE repository_id = $1`, and `postgres_test.go` asserts it there.
	if _, err := f.service.Attach(ctx, AttachInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, Name: "not-in-this-repo",
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("attaching an absent label = %v, want ErrNotFound", err)
	}
}

// TestDeletingALabelRemovesItsAttachments covers the acceptance criterion at the service layer, with the
// database proving the cascade separately.
func TestDeletingALabelRemovesItsAttachments(t *testing.T) {
	f := newFixture(t, repository.LevelAdmin)
	ctx := context.Background()
	label := f.add(t, "bug", "ff0000")

	// A writer applies it.
	f.access.level = repository.LevelWrite
	if _, err := f.service.Attach(ctx, AttachInput{
		Actor: author(bobID), RepositoryID: repoID, IssueID: issueID, Name: "bug",
	}); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// An admin deletes the label.
	f.access.level = repository.LevelAdmin
	if err := f.service.Delete(ctx, DeleteInput{
		Actor: author(aliceID), RepositoryID: repoID, Name: "bug",
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// The attachment is gone...
	if len(f.store.attached[issueID]) != 0 {
		t.Errorf("the attachment survived: %v", f.store.attached[issueID])
	}
	// ...and the issue is not. The fake has no issue store, so what is asserted is that nothing about
	// the label's deletion touched an issue — which is the property, and the database's cascade is the
	// guarantee. `postgres_test.go` asserts the issue row itself.
	if _, err := f.service.Get(ctx, GetInput{
		Actor: author(aliceID), RepositoryID: repoID, Name: label.Name,
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("the label is still readable: %v", err)
	}
}

// TestTheRecordShape covers which operations write a ledger record and which do not.
//
// The catalogue defines `label.created` and `label.deleted` and **no** `label.edited`, so an update
// writes nothing.
func TestTheRecordShape(t *testing.T) {
	f := newFixture(t, repository.LevelAdmin)
	ctx := context.Background()

	f.add(t, "bug", "ff0000")
	if len(f.store.events) != 1 || f.store.events[0] != "label.created" {
		t.Fatalf("the events are %v, want one label.created", f.store.events)
	}

	// An update writes nothing.
	if _, err := f.service.Update(ctx, UpdateInput{
		Actor: author(aliceID), RepositoryID: repoID, Name: "bug",
		NewName: ptr("defect"), Color: ptr("00ff00"),
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(f.store.events) != 1 {
		t.Errorf("an update wrote %d record(s); the catalogue defines no action for one",
			len(f.store.events)-1)
	}

	// A delete writes the other one.
	if err := f.service.Delete(ctx, DeleteInput{
		Actor: author(aliceID), RepositoryID: repoID, Name: "defect",
	}); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(f.store.events) != 2 || f.store.events[1] != "label.deleted" {
		t.Errorf("the events are %v, want label.created then label.deleted", f.store.events)
	}
}

// TestAnUpdateThatChangesNothingIsRefused keeps a write from happening for a change that did not occur.
func TestAnUpdateThatChangesNothingIsRefused(t *testing.T) {
	f := newFixture(t, repository.LevelAdmin)
	ctx := context.Background()
	f.add(t, "bug", "ff0000")

	if _, err := f.service.Update(ctx, UpdateInput{
		Actor: author(aliceID), RepositoryID: repoID, Name: "bug",
	}); !errors.Is(err, ErrNoChange) {
		t.Errorf("an update naming no field = %v, want ErrNoChange", err)
	}
	if _, err := f.service.Update(ctx, UpdateInput{
		Actor: author(aliceID), RepositoryID: repoID, Name: "bug",
		NewName: ptr("bug"), Color: ptr("ff0000"),
	}); !errors.Is(err, ErrNoChange) {
		t.Errorf("an update with the same values = %v, want ErrNoChange", err)
	}
	// A rename onto an existing other label's folded name is a conflict.
	f.add(t, "feature", "0000ff")
	if _, err := f.service.Update(ctx, UpdateInput{
		Actor: author(aliceID), RepositoryID: repoID, Name: "feature", NewName: ptr("BUG"),
	}); !errors.Is(err, ErrNameTaken) {
		t.Errorf("a rename onto an existing name = %v, want ErrNameTaken", err)
	}
}

// TestTheCountsIncludeUnusedLabels keeps a label list showing every label.
func TestTheCountsIncludeUnusedLabels(t *testing.T) {
	f := newFixture(t, repository.LevelAdmin)
	ctx := context.Background()
	f.add(t, "used", "ff0000")
	f.add(t, "unused", "00ff00")

	f.access.level = repository.LevelWrite
	if _, err := f.service.Attach(ctx, AttachInput{
		Actor: author(aliceID), RepositoryID: repoID, IssueID: issueID, Name: "used",
	}); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	f.access.level = repository.LevelRead
	counts, err := f.service.Counts(ctx, CountsInput{Actor: author(aliceID), RepositoryID: repoID})
	if err != nil {
		t.Fatalf("Counts: %v", err)
	}
	found := map[string]int{}
	for id, count := range counts {
		found[f.store.labels[id].Name] = count
	}
	// A label nothing uses is present with a zero count, which is what the left join provides.
	if found["used"] != 1 {
		t.Errorf("the used label's count is %d, want 1", found["used"])
	}
	if _, present := found["unused"]; !present {
		t.Error("the unused label is absent from the counts, so a list would omit it")
	}
	if found["unused"] != 0 {
		t.Errorf("the unused label's count is %d, want 0", found["unused"])
	}
}

// TestTheDescriptionBoundIsEnforced covers the one bound the documents require but do not fix.
func TestTheDescriptionBoundIsEnforced(t *testing.T) {
	f := newFixture(t, repository.LevelAdmin)
	ctx := context.Background()

	for _, test := range []struct {
		name        string
		description string
		wantErr     error
	}{
		{name: "empty", description: ""},
		{name: "at the bound", description: strings.Repeat("x", MaxDescriptionLength)},
		{name: "over the bound", description: strings.Repeat("x", MaxDescriptionLength+1), wantErr: ErrInvalidDescription},
		{name: "a null byte", description: "a\x00b", wantErr: ErrInvalidDescription},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := f.service.Create(ctx, CreateInput{
				Actor: author(aliceID), RepositoryID: repoID, Name: "d-" + test.name,
				Color: "ff0000", Description: test.description,
			})
			if test.wantErr == nil {
				if err != nil {
					t.Fatalf("a valid description was refused: %v", err)
				}
				return
			}
			if !errors.Is(err, test.wantErr) {
				t.Errorf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

// TestTheListIsOrderedByFoldedName covers the ordering: a reader sees a case-insensitive order, which is
// what makes `Bug` and `bug` impossible to confuse.
func TestTheListIsOrderedByFoldedName(t *testing.T) {
	f := newFixture(t, repository.LevelAdmin)
	ctx := context.Background()

	for _, name := range []string{"Zebra", "apple", "Banana", "cherry"} {
		f.add(t, name, "ff0000")
	}

	found, err := f.service.List(ctx, ListInput{Actor: author(aliceID), RepositoryID: repoID})
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

// ptr returns a pointer to a value, for the pointer fields an update takes.
func ptr(value string) *string { return &value }
