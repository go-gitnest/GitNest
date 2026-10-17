package repository

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/audit"
)

// fakeStore is an in-memory Store. It applies the same name-collision and
// visibility-predicate rules as PostgreSQL where those rules are not the point
// of the test.
//
// It also records the audit events the service hands it, which is how a
// service-level test asserts that an operation produced the record the catalogue
// names without standing up a database.
type fakeStore struct {
	repositories []Repository
	folded       map[string]string // repository id -> folded name
	grants       map[string]Level  // "repoID|userID" -> level
	nextID       int
	schemaErr    error
	// events is every audit record the service built, in order.
	events []audit.Event
}

// recorded returns the event types the service produced, in order.
func (f *fakeStore) recorded() []string {
	types := make([]string, 0, len(f.events))
	for _, event := range f.events {
		types = append(types, event.Action)
	}
	return types
}

// lastEvent returns the most recent record, or fails when there is none.
func (f *fakeStore) lastEvent(t *testing.T) audit.Event {
	t.Helper()
	if len(f.events) == 0 {
		t.Fatal("the service recorded nothing")
	}
	return f.events[len(f.events)-1]
}

// record validates and stores a record, which is what the database does.
//
// Validation runs here rather than being skipped, so a service-level test cannot
// pass with a record the real store would have refused.
func (f *fakeStore) record(event audit.Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	f.events = append(f.events, event)
	return nil
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		folded: map[string]string{},
		grants: map[string]Level{},
	}
}

func grantKey(repoID, userID string) string { return repoID + "|" + userID }

func (f *fakeStore) Create(_ context.Context, newRepository New, event audit.Event) (Repository, error) {
	if f.schemaErr != nil {
		return Repository{}, f.schemaErr
	}
	for _, existing := range f.repositories {
		if existing.OwnerID == newRepository.OwnerID && f.folded[existing.ID] == newRepository.NameFolded {
			return Repository{}, ErrNameTaken
		}
	}
	now := time.Unix(0, 0).UTC()
	created := Repository{
		ID: newRepository.ID, OwnerID: newRepository.OwnerID, Name: newRepository.Name,
		Description: newRepository.Description, Visibility: newRepository.Visibility,
		StorageID: newRepository.StorageID, CreatedAt: now, UpdatedAt: now,
	}
	if err := f.record(event); err != nil {
		return Repository{}, err
	}
	f.repositories = append(f.repositories, created)
	f.folded[created.ID] = newRepository.NameFolded
	return created, nil
}

func (f *fakeStore) FindByID(_ context.Context, id string) (Repository, error) {
	if f.schemaErr != nil {
		return Repository{}, f.schemaErr
	}
	for _, existing := range f.repositories {
		if existing.ID == id {
			return existing, nil
		}
	}
	return Repository{}, ErrNotFound
}

func (f *fakeStore) FindByOwnerAndFoldedName(_ context.Context, ownerID, nameFolded string) (Repository, error) {
	if f.schemaErr != nil {
		return Repository{}, f.schemaErr
	}
	for _, existing := range f.repositories {
		if existing.OwnerID == ownerID && f.folded[existing.ID] == nameFolded {
			return existing, nil
		}
	}
	return Repository{}, ErrNotFound
}

func (f *fakeStore) RoleFor(_ context.Context, repoID, userID string) (Level, bool, error) {
	level, found := f.grants[grantKey(repoID, userID)]
	return level, found, nil
}

// ListVisible mirrors the SQL predicate: the visibility filter is applied to
// every candidate before the page is taken.
func (f *fakeStore) ListVisible(_ context.Context, query ListQuery) ([]Repository, error) {
	if f.schemaErr != nil {
		return nil, f.schemaErr
	}
	ordered := make([]Repository, len(f.repositories))
	copy(ordered, f.repositories)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })

	var visible []Repository
	for _, candidate := range ordered {
		if query.OwnerID != "" && candidate.OwnerID != query.OwnerID {
			continue
		}
		if f.visibleTo(query.Viewer, candidate) {
			visible = append(visible, candidate)
		}
	}
	return paginate(visible, query.Offset, query.Limit), nil
}

func (f *fakeStore) visibleTo(viewer Principal, candidate Repository) bool {
	if viewer.IsInstanceAdmin {
		return true
	}
	if candidate.Visibility == VisibilityPublic {
		return true
	}
	if viewer.IsAnonymous() {
		return false
	}
	if candidate.OwnerID == viewer.UserID {
		return true
	}
	_, granted := f.grants[grantKey(candidate.ID, viewer.UserID)]
	return granted
}

func paginate(items []Repository, offset, limit int) []Repository {
	if offset >= len(items) {
		return nil
	}
	items = items[offset:]
	if limit < len(items) {
		items = items[:limit]
	}
	return items
}

func (f *fakeStore) SetCollaborator(_ context.Context, repoID, userID string, role Level, events GrantEvents) (bool, error) {
	key := grantKey(repoID, userID)
	_, existed := f.grants[key]

	// The record that matches what happened is the one written, exactly as the
	// PostgreSQL store decides it inside its transaction.
	event := events.Changed
	if !existed {
		event = events.Added
	}
	if err := f.record(event); err != nil {
		return false, err
	}
	f.grants[key] = role
	return !existed, nil
}

func (f *fakeStore) RemoveCollaborator(_ context.Context, repoID, userID string, event audit.Event) error {
	key := grantKey(repoID, userID)
	if _, found := f.grants[key]; !found {
		// Nothing changed, so nothing is recorded.
		return ErrNotFound
	}
	if err := f.record(event); err != nil {
		return err
	}
	delete(f.grants, key)
	return nil
}

func (f *fakeStore) SetVisibility(_ context.Context, repoID string, visibility Visibility, event audit.Event) (Repository, error) {
	if err := f.record(event); err != nil {
		return Repository{}, err
	}
	for index, existing := range f.repositories {
		if existing.ID == repoID {
			f.repositories[index].Visibility = visibility
			return f.repositories[index], nil
		}
	}
	return Repository{}, ErrNotFound
}

func (f *fakeStore) SetDefaultBranch(_ context.Context, repoID, branch string, event audit.Event) (Repository, error) {
	if err := f.record(event); err != nil {
		return Repository{}, err
	}
	for index, existing := range f.repositories {
		if existing.ID == repoID {
			f.repositories[index].DefaultBranch = branch
			return f.repositories[index], nil
		}
	}
	return Repository{}, ErrNotFound
}

func (f *fakeStore) SetArchived(_ context.Context, repoID string, archived bool, event audit.Event) (Repository, error) {
	if err := f.record(event); err != nil {
		return Repository{}, err
	}
	for index, existing := range f.repositories {
		if existing.ID == repoID {
			f.repositories[index].Archived = archived
			return f.repositories[index], nil
		}
	}
	return Repository{}, ErrNotFound
}

func (f *fakeStore) Rename(_ context.Context, repoID, name, nameFolded string, event audit.Event) (Repository, error) {
	for index, existing := range f.repositories {
		if existing.ID != repoID {
			continue
		}
		for _, other := range f.repositories {
			if other.ID != repoID && other.OwnerID == existing.OwnerID && f.folded[other.ID] == nameFolded {
				return Repository{}, ErrNameTaken
			}
		}
		if err := f.record(event); err != nil {
			return Repository{}, err
		}
		f.repositories[index].Name = name
		f.folded[repoID] = nameFolded
		return f.repositories[index], nil
	}
	return Repository{}, ErrNotFound
}

func (f *fakeStore) Collaborators(_ context.Context, repoID string) ([]Collaborator, error) {
	var found []Collaborator
	for key, level := range f.grants {
		if len(key) > len(repoID) && key[:len(repoID)] == repoID && key[len(repoID)] == '|' {
			found = append(found, Collaborator{UserID: key[len(repoID)+1:], Role: level})
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].UserID < found[j].UserID })
	return found, nil
}

// fixture is a store with an owner and one repository of each visibility.
type fixture struct {
	service *Service
	store   *fakeStore
	owner   Principal
	ownerID string
	public  Repository
	private Repository
}

// grant gives an account a direct grant, stated as the grant rather than as the
// request struct around it, so an assertion about access stays the visible part
// of the test.
//
// It reports whether the grant was new, which a test uses to check the 201-versus
// -200 distinction a transport makes.
func (f *fixture) grant(ctx context.Context, actor Principal, repoID, userID string, role Level) (bool, error) {
	return f.service.GrantAccess(ctx, GrantInput{
		Actor: actor, RepoID: repoID, UserID: userID, Role: role,
	})
}

// revoke removes a direct grant.
func (f *fixture) revoke(ctx context.Context, actor Principal, repoID, userID string) error {
	return f.service.RevokeAccess(ctx, RevokeInput{Actor: actor, RepoID: repoID, UserID: userID})
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	store := newFakeStore()
	service := NewService(store)
	ownerID := "owner-1"
	owner := User(ownerID, false)
	ctx := context.Background()

	public, err := service.Create(ctx, CreateInput{
		Actor: owner, OwnerID: ownerID, Name: "public-repo", Visibility: VisibilityPublic,
	})
	if err != nil {
		t.Fatalf("create the public repository: %v", err)
	}
	private, err := service.Create(ctx, CreateInput{
		Actor: owner, OwnerID: ownerID, Name: "private-repo",
	})
	if err != nil {
		t.Fatalf("create the private repository: %v", err)
	}
	return &fixture{service: service, store: store, owner: owner, ownerID: ownerID, public: public, private: private}
}

// TestCreateDefaultsToPrivate pins the documented product rule.
func TestCreateDefaultsToPrivate(t *testing.T) {
	store := newFakeStore()
	service := NewService(store)

	created, err := service.Create(context.Background(), CreateInput{
		Actor: User("u1", false), OwnerID: "u1", Name: "  my-repo  ", Description: "  d  ",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Visibility != VisibilityPrivate {
		t.Errorf("visibility = %q, want private by default", created.Visibility)
	}
	if created.Name != "my-repo" {
		t.Errorf("name = %q, want the trimmed display form", created.Name)
	}
	if created.StorageID == "" || created.StorageID == created.ID {
		t.Error("the storage identifier is missing or is the row identifier")
	}
}

func TestCreateDeniesAnonymousAndForeignOwners(t *testing.T) {
	service := NewService(newFakeStore())
	ctx := context.Background()

	if _, err := service.Create(ctx, CreateInput{Actor: Anonymous(), OwnerID: "u1", Name: "r"}); !errors.Is(err, ErrAnonymous) {
		t.Errorf("anonymous create = %v, want ErrAnonymous", err)
	}
	if _, err := service.Create(ctx, CreateInput{Actor: User("u2", false), OwnerID: "u1", Name: "r"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("create in another owner's namespace = %v, want ErrForbidden", err)
	}
	// An instance administrator is not exempt either: creating a repository is
	// not an oversight action, and it would make the owner field untrue.
	if _, err := service.Create(ctx, CreateInput{Actor: User("admin", true), OwnerID: "u1", Name: "r"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("instance administrator create = %v, want ErrForbidden", err)
	}
}

func TestCreateRejectsAnInvalidName(t *testing.T) {
	service := NewService(newFakeStore())
	ctx := context.Background()

	for _, name := range []string{"", "-bad", "bad.git", "..", "admin", "has space"} {
		t.Run(name, func(t *testing.T) {
			if _, err := service.Create(ctx, CreateInput{
				Actor: User("u1", false), OwnerID: "u1", Name: name,
			}); err == nil {
				t.Fatalf("Create accepted the name %q", name)
			}
		})
	}
}

func TestCreateRejectsADuplicateNameForTheSameOwner(t *testing.T) {
	store := newFakeStore()
	service := NewService(store)
	ctx := context.Background()
	actor := User("u1", false)

	if _, err := service.Create(ctx, CreateInput{Actor: actor, OwnerID: "u1", Name: "repo"}); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	// Uniqueness is on the folded form.
	if _, err := service.Create(ctx, CreateInput{Actor: actor, OwnerID: "u1", Name: "REPO"}); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("case-different duplicate = %v, want ErrNameTaken", err)
	}
	// A different owner may hold the same name.
	if _, err := service.Create(ctx, CreateInput{Actor: User("u2", false), OwnerID: "u2", Name: "repo"}); err != nil {
		t.Fatalf("another owner could not use the name: %v", err)
	}
}

// TestPermissionMatrix is the acceptance table: every principal kind at every
// level, for a public and a private repository.
func TestPermissionMatrix(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A collaborator at each level on the private repository.
	if _, err := f.grant(ctx, f.owner, f.private.ID, "reader", LevelRead); err != nil {
		t.Fatalf("grant read: %v", err)
	}
	if _, err := f.grant(ctx, f.owner, f.private.ID, "writer", LevelWrite); err != nil {
		t.Fatalf("grant write: %v", err)
	}
	if _, err := f.grant(ctx, f.owner, f.private.ID, "admin", LevelAdmin); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	// The same grants on the public repository, so the matrix covers both.
	for user, role := range map[string]Level{"reader": LevelRead, "writer": LevelWrite, "admin": LevelAdmin} {
		if _, err := f.grant(ctx, f.owner, f.public.ID, user, role); err != nil {
			t.Fatalf("grant %s on the public repository: %v", user, err)
		}
	}

	for _, test := range []struct {
		name     string
		viewer   Principal
		repo     Repository
		want     Level
		wantSeen bool
	}{
		{name: "anonymous on public", viewer: Anonymous(), repo: f.public, want: LevelRead, wantSeen: true},
		{name: "anonymous on private", viewer: Anonymous(), repo: f.private, want: LevelNone},
		{name: "owner on own public", viewer: f.owner, repo: f.public, want: LevelOwner, wantSeen: true},
		{name: "owner on own private", viewer: f.owner, repo: f.private, want: LevelOwner, wantSeen: true},
		{name: "reader collaborator on private", viewer: User("reader", false), repo: f.private, want: LevelRead, wantSeen: true},
		{name: "writer collaborator on private", viewer: User("writer", false), repo: f.private, want: LevelWrite, wantSeen: true},
		{name: "admin collaborator on private", viewer: User("admin", false), repo: f.private, want: LevelAdmin, wantSeen: true},
		{name: "reader collaborator on public", viewer: User("reader", false), repo: f.public, want: LevelRead, wantSeen: true},
		{name: "writer collaborator on public", viewer: User("writer", false), repo: f.public, want: LevelWrite, wantSeen: true},
		{name: "admin collaborator on public", viewer: User("admin", false), repo: f.public, want: LevelAdmin, wantSeen: true},
		{name: "unrelated user on public", viewer: User("stranger", false), repo: f.public, want: LevelRead, wantSeen: true},
		{name: "unrelated user on private", viewer: User("stranger", false), repo: f.private, want: LevelNone},
		{name: "instance admin on public", viewer: User("operator", true), repo: f.public, want: LevelAdmin, wantSeen: true},
		{name: "instance admin on private", viewer: User("operator", true), repo: f.private, want: LevelAdmin, wantSeen: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			// By name.
			_, level, err := f.service.Resolve(ctx, test.viewer, f.ownerID, test.repo.Name)
			if test.wantSeen {
				if err != nil {
					t.Fatalf("Resolve: %v", err)
				}
				if level != test.want {
					t.Errorf("by name level = %s, want %s", level, test.want)
				}
			} else {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("Resolve = %v, want ErrNotFound so existence stays hidden", err)
				}
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("level leaked for a hidden repository: %s", level)
				}
			}

			// By identifier: the same answer, so an internal identifier is not a
			// capability.
			_, level, err = f.service.ResolveByID(ctx, test.viewer, test.repo.ID)
			if test.wantSeen {
				if err != nil {
					t.Fatalf("ResolveByID: %v", err)
				}
				if level != test.want {
					t.Errorf("by id level = %s, want %s", level, test.want)
				}
			} else if !errors.Is(err, ErrNotFound) {
				t.Fatalf("ResolveByID = %v, want ErrNotFound", err)
			}
		})
	}
}

// TestCanNeverDistinguishesMissingFromHidden is the core leak-resistance
// property: a private repository and a repository that does not exist produce
// the same error, the same message, and the same answer for every entry point.
func TestCanNeverDistinguishesMissingFromHidden(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	stranger := User("stranger", false)

	byName, _, nameErr := f.service.Resolve(ctx, stranger, f.ownerID, f.private.Name)
	missing, _, missingErr := f.service.Resolve(ctx, stranger, f.ownerID, "no-such-repository")
	if nameErr == nil || missingErr == nil {
		t.Fatal("a hidden or missing repository resolved for a stranger")
	}
	if nameErr.Error() != missingErr.Error() {
		t.Errorf("the messages differ: %q vs %q", nameErr, missingErr)
	}
	if byName.ID != "" || missing.ID != "" {
		t.Error("a repository value was returned alongside the error")
	}

	byID, _, idErr := f.service.ResolveByID(ctx, stranger, f.private.ID)
	missingByID, _, missingIDErr := f.service.ResolveByID(ctx, stranger, "01920000-0000-7000-8000-0000000000ff")
	if idErr == nil || missingIDErr == nil {
		t.Fatal("a hidden or missing identifier resolved for a stranger")
	}
	if idErr.Error() != missingIDErr.Error() {
		t.Errorf("the identifier messages differ: %q vs %q", idErr, missingIDErr)
	}
	if byID.ID != "" || missingByID.ID != "" {
		t.Error("a repository value was returned alongside the identifier error")
	}

	// The same holds anonymously.
	_, _, anonErr := f.service.Resolve(ctx, Anonymous(), f.ownerID, f.private.Name)
	_, _, anonMissing := f.service.Resolve(ctx, Anonymous(), f.ownerID, "no-such-repository")
	if anonErr == nil || anonMissing == nil || anonErr.Error() != anonMissing.Error() {
		t.Errorf("anonymous answers differ: %v vs %v", anonErr, anonMissing)
	}
}

// TestAlternateCasingResolvesOnlyWhenVisible covers the specification's rule
// that a case-different path resolves to the canonical repository, while still
// not disclosing a private one.
func TestAlternateCasingResolvesOnlyWhenVisible(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// The owner created `public-repo`; an alternate casing resolves to the
	// stored casing.
	for _, spelling := range []string{"PUBLIC-REPO", "Public-Repo", "public-REPO"} {
		found, level, err := f.service.Resolve(ctx, Anonymous(), f.ownerID, spelling)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", spelling, err)
		}
		if level != LevelRead {
			t.Errorf("Resolve(%q) level = %s, want read", spelling, level)
		}
		if found.Name != "public-repo" {
			t.Errorf("Resolve(%q) returned %q, want the canonical casing so the caller can redirect",
				spelling, found.Name)
		}
	}

	// The private repository's alternate casing is as invisible as its exact
	// name.
	for _, spelling := range []string{"private-repo", "PRIVATE-REPO", "Private-Repo"} {
		if _, _, err := f.service.Resolve(ctx, Anonymous(), f.ownerID, spelling); !errors.Is(err, ErrNotFound) {
			t.Errorf("Resolve(%q) = %v, want ErrNotFound", spelling, err)
		}
	}
}

func TestRequireEnforcesTheLevel(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.grant(ctx, f.owner, f.private.ID, "reader", LevelRead); err != nil {
		t.Fatalf("grant read: %v", err)
	}
	reader := User("reader", false)

	// At the required level.
	if _, level, err := f.service.Require(ctx, reader, f.private.ID, LevelRead); err != nil || level != LevelRead {
		t.Fatalf("Require(read) = %v, %s", err, level)
	}
	// Above it: the caller can already read, so existence is not a secret and
	// the refusal names the permission problem.
	if _, _, err := f.service.Require(ctx, reader, f.private.ID, LevelWrite); !errors.Is(err, ErrForbidden) {
		t.Fatalf("Require(write) as a reader = %v, want ErrForbidden", err)
	}
	// No access at all: indistinguishable from a missing repository.
	if _, _, err := f.service.Require(ctx, User("stranger", false), f.private.ID, LevelRead); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Require as a stranger = %v, want ErrNotFound", err)
	}
	if _, _, err := f.service.Require(ctx, Anonymous(), f.private.ID, LevelRead); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Require anonymously = %v, want ErrNotFound", err)
	}
}

// TestGrantAccessCannotConferOwnership keeps ownership out of the grant table.
func TestGrantAccessCannotConferOwnership(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.grant(ctx, f.owner, f.private.ID, "someone", LevelOwner); err == nil {
		t.Error("a grant conferred ownership")
	}
	if _, err := f.grant(ctx, f.owner, f.private.ID, "someone", LevelNone); err == nil {
		t.Error("a grant carried no permission")
	}
	// Only an admin may grant.
	if _, err := f.grant(ctx, User("stranger", false), f.private.ID, "someone", LevelRead); !errors.Is(err, ErrNotFound) {
		t.Errorf("a stranger granted access = %v, want ErrNotFound", err)
	}
	if _, err := f.grant(ctx, User("reader", false), f.private.ID, "someone", LevelRead); !errors.Is(err, ErrNotFound) {
		t.Errorf("a non-collaborator granted access = %v, want ErrNotFound", err)
	}
}

// TestGrantConfersNothingElse is the outside-collaborator rule: a grant on one
// repository must not reach another of the same owner.
func TestGrantConfersNothingElse(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.grant(ctx, f.owner, f.private.ID, "outsider", LevelAdmin); err != nil {
		t.Fatalf("grant: %v", err)
	}
	outsider := User("outsider", false)

	if _, level, err := f.service.Resolve(ctx, outsider, f.ownerID, f.private.Name); err != nil || level != LevelAdmin {
		t.Fatalf("the granted repository = %v, %s", err, level)
	}
	// A second private repository of the same owner stays invisible, even
	// though the grant there is admin-level.
	other, err := f.service.Create(ctx, CreateInput{Actor: f.owner, OwnerID: f.ownerID, Name: "other-private"})
	if err != nil {
		t.Fatalf("create a second private repository: %v", err)
	}
	if _, _, err := f.service.Resolve(ctx, outsider, f.ownerID, other.Name); !errors.Is(err, ErrNotFound) {
		t.Errorf("a grant reached another repository: %v", err)
	}
}

func TestRevokeAccessTakesEffectImmediately(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.grant(ctx, f.owner, f.private.ID, "reader", LevelRead); err != nil {
		t.Fatalf("grant: %v", err)
	}
	reader := User("reader", false)
	if _, _, err := f.service.Resolve(ctx, reader, f.ownerID, f.private.Name); err != nil {
		t.Fatalf("the grant did not take effect: %v", err)
	}

	if err := f.revoke(ctx, f.owner, f.private.ID, "reader"); err != nil {
		t.Fatalf("RevokeAccess: %v", err)
	}
	// The very next request is refused, as the acceptance criteria require.
	if _, _, err := f.service.Resolve(ctx, reader, f.ownerID, f.private.Name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after revocation = %v, want ErrNotFound", err)
	}
	// Revoking twice reports the missing grant rather than pretending success.
	if err := f.revoke(ctx, f.owner, f.private.ID, "reader"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second RevokeAccess = %v, want ErrNotFound", err)
	}
}

// TestRenameKeepsIdentityAndStorage is the acceptance property that a rename
// moves the name and nothing else.
func TestRenameKeepsIdentityAndStorage(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	renamed, err := f.service.Rename(ctx, RenameInput{Actor: f.owner, RepoID: f.private.ID, Name: "renamed-repo"})
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.Name != "renamed-repo" {
		t.Errorf("name = %q, want the new display name", renamed.Name)
	}
	if renamed.ID != f.private.ID {
		t.Errorf("the identifier changed: %q -> %q", f.private.ID, renamed.ID)
	}
	if renamed.StorageID != f.private.StorageID {
		t.Errorf("the storage identifier changed: %q -> %q; the bytes would move",
			f.private.StorageID, renamed.StorageID)
	}
	if renamed.OwnerID != f.private.OwnerID {
		t.Errorf("the owner changed: %q -> %q", f.private.OwnerID, renamed.OwnerID)
	}

	// The old name no longer resolves and the new one does.
	if _, _, err := f.service.Resolve(ctx, f.owner, f.ownerID, f.private.Name); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old name still resolves: %v", err)
	}
	if _, _, err := f.service.Resolve(ctx, f.owner, f.ownerID, "renamed-repo"); err != nil {
		t.Errorf("the new name does not resolve: %v", err)
	}
}

func TestRenameRequiresAdminAndAValidName(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if _, err := f.grant(ctx, f.owner, f.private.ID, "writer", LevelWrite); err != nil {
		t.Fatalf("grant write: %v", err)
	}
	// Write is not enough: renaming is an administrative change.
	if _, err := f.service.Rename(ctx, RenameInput{
		Actor: User("writer", false), RepoID: f.private.ID, Name: "taken-over",
	}); !errors.Is(err, ErrForbidden) {
		t.Errorf("a writer renamed the repository: %v", err)
	}
	// An administrator collaborator may.
	if _, err := f.grant(ctx, f.owner, f.private.ID, "admin", LevelAdmin); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	if _, err := f.service.Rename(ctx, RenameInput{
		Actor: User("admin", false), RepoID: f.private.ID, Name: "renamed-by-admin",
	}); err != nil {
		t.Errorf("an administrator collaborator could not rename: %v", err)
	}
	// Invalid and duplicate names are refused.
	if _, err := f.service.Rename(ctx, RenameInput{Actor: f.owner, RepoID: f.public.ID, Name: "bad.git"}); err == nil {
		t.Error("an invalid name was accepted")
	}
	if _, err := f.service.Rename(ctx, RenameInput{Actor: f.owner, RepoID: f.public.ID, Name: "renamed-by-admin"}); !errors.Is(err, ErrNameTaken) {
		t.Errorf("a duplicate name = %v, want ErrNameTaken", err)
	}
}

// TestListFiltersBeforePaginating is the acceptance property that no private
// repository shifts a page or a count.
func TestListFiltersBeforePaginating(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// The owner holds eight repositories; exactly two are public. They are
	// created interleaved so a page boundary falls between them.
	for i := 0; i < 8; i++ {
		visibility := VisibilityPrivate
		if i == 3 || i == 6 {
			visibility = VisibilityPublic
		}
		if _, err := f.service.Create(ctx, CreateInput{
			Actor: f.owner, OwnerID: f.ownerID,
			Name:       "repo-" + string(rune('a'+i)),
			Visibility: visibility,
		}); err != nil {
			t.Fatalf("create repository %d: %v", i, err)
		}
	}

	// The anonymous viewer sees exactly the public repositories, plus the one
	// the fixture created.
	page, err := f.service.List(ctx, ListQuery{Viewer: Anonymous(), OwnerID: f.ownerID, Limit: 10})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, item := range page.Repositories {
		if item.Visibility != VisibilityPublic {
			t.Fatalf("an anonymous listing returned the private repository %q", item.Name)
		}
	}
	if len(page.Repositories) != 3 {
		t.Fatalf("anonymous listing returned %d repositories, want the 3 public ones", len(page.Repositories))
	}

	// Paginating one at a time returns only public repositories: the private
	// ones occupy no page slot.
	var counted []string
	for offset := 0; offset < 10; offset++ {
		page, err := f.service.List(ctx, ListQuery{Viewer: Anonymous(), OwnerID: f.ownerID, Limit: 1, Offset: offset})
		if err != nil {
			t.Fatalf("List(offset=%d): %v", offset, err)
		}
		if len(page.Repositories) == 0 {
			if page.HasMore {
				t.Fatalf("offset %d reported more pages but returned none", offset)
			}
			break
		}
		counted = append(counted, page.Repositories[0].Name)
		if page.Repositories[0].Visibility != VisibilityPublic {
			t.Fatalf("offset %d returned the private repository %q", offset, page.Repositories[0].Name)
		}
	}
	if len(counted) != 3 {
		t.Errorf("paging one at a time yielded %d repositories (%v), want 3", len(counted), counted)
	}

	// The owner sees everything it owns.
	ownerPage, err := f.service.List(ctx, ListQuery{Viewer: f.owner, OwnerID: f.ownerID, Limit: 100})
	if err != nil {
		t.Fatalf("List as the owner: %v", err)
	}
	if len(ownerPage.Repositories) != 10 {
		t.Errorf("the owner sees %d repositories, want 10", len(ownerPage.Repositories))
	}

	// A collaborator sees its one grant plus the public repositories, and
	// nothing else.
	if _, err := f.grant(ctx, f.owner, f.private.ID, "outsider", LevelRead); err != nil {
		t.Fatalf("grant: %v", err)
	}
	outsiderPage, err := f.service.List(ctx, ListQuery{Viewer: User("outsider", false), OwnerID: f.ownerID, Limit: 100})
	if err != nil {
		t.Fatalf("List as the collaborator: %v", err)
	}
	if len(outsiderPage.Repositories) != 4 {
		t.Errorf("the collaborator sees %d repositories, want 4 (3 public + 1 grant)", len(outsiderPage.Repositories))
	}
}

// TestListReportsHasMoreWithoutCounting pins that a page tells the caller
// whether more exists without ever computing a total, so no count that could
// include a hidden repository is produced.
func TestListReportsHasMoreWithoutCounting(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		if _, err := f.service.Create(ctx, CreateInput{
			Actor: f.owner, OwnerID: f.ownerID,
			Name:       "pub-" + string(rune('a'+i)),
			Visibility: VisibilityPublic,
		}); err != nil {
			t.Fatalf("create a public repository: %v", err)
		}
	}

	page, err := f.service.List(ctx, ListQuery{Viewer: Anonymous(), OwnerID: f.ownerID, Limit: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Repositories) != 2 {
		t.Fatalf("page size = %d, want 2", len(page.Repositories))
	}
	if !page.HasMore {
		t.Error("the page did not report that more repositories exist")
	}

	// The last page reports no more.
	page, err = f.service.List(ctx, ListQuery{Viewer: Anonymous(), OwnerID: f.ownerID, Limit: 3, Offset: 2})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Repositories) != 3 {
		t.Fatalf("page size = %d, want the remaining 3", len(page.Repositories))
	}
	if page.HasMore {
		t.Error("the final page reported more repositories")
	}
}

func TestListBoundsThePageSize(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// An over-large request is capped rather than refused, and a negative
	// offset is treated as the start.
	if _, err := f.service.List(ctx, ListQuery{Viewer: Anonymous(), Limit: MaxListLimit + 1000, Offset: -5}); err != nil {
		t.Fatalf("List: %v", err)
	}
}

func TestSchemaNotMigratedIsReported(t *testing.T) {
	store := newFakeStore()
	store.schemaErr = ErrSchemaNotMigrated
	service := NewService(store)

	if _, err := service.Create(context.Background(), CreateInput{
		Actor: User("u1", false), OwnerID: "u1", Name: "repo",
	}); !errors.Is(err, ErrSchemaNotMigrated) {
		t.Fatalf("Create = %v, want ErrSchemaNotMigrated", err)
	}
	if _, _, err := service.Resolve(context.Background(), Anonymous(), "u1", "repo"); !errors.Is(err, ErrSchemaNotMigrated) {
		t.Fatalf("Resolve = %v, want ErrSchemaNotMigrated", err)
	}
}

func TestLevelAllows(t *testing.T) {
	for _, test := range []struct {
		level    Level
		required Level
		want     bool
	}{
		{LevelNone, LevelRead, false},
		{LevelRead, LevelRead, true},
		{LevelRead, LevelWrite, false},
		{LevelWrite, LevelRead, true},
		{LevelAdmin, LevelWrite, true},
		{LevelOwner, LevelAdmin, true},
		{LevelAdmin, LevelOwner, false},
	} {
		if got := test.level.Allows(test.required); got != test.want {
			t.Errorf("%s.Allows(%s) = %t, want %t", test.level, test.required, got, test.want)
		}
	}
}

func TestLevelString(t *testing.T) {
	for _, test := range []struct {
		level Level
		want  string
	}{
		{LevelNone, "none"}, {LevelRead, "read"}, {LevelWrite, "write"},
		{LevelAdmin, "admin"}, {LevelOwner, "owner"},
	} {
		if got := test.level.String(); got != test.want {
			t.Errorf("Level(%d).String() = %q, want %q", test.level, got, test.want)
		}
	}
}

func TestPrincipalIsAnonymous(t *testing.T) {
	if !Anonymous().IsAnonymous() {
		t.Error("the anonymous principal is not anonymous")
	}
	if User("u1", false).IsAnonymous() {
		t.Error("an authenticated principal is anonymous")
	}
	// A principal with an empty identifier cannot be authenticated by accident.
	if !User("", false).IsAnonymous() {
		t.Error("a user principal with no identifier is not anonymous")
	}
}
