package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
	"github.com/go-gitnest/GitNest/internal/repository"
	"github.com/google/uuid"
)

// world is a migrated schema with two accounts and a repository service on it.
type world struct {
	service *repository.Service
	db      *sql.DB
	owner   repository.Principal
	other   repository.Principal
	ownerID string
	otherID string
}

// newWorld provisions two accounts through the real identity path, so the
// foreign keys the repository tables declare are satisfied by real rows.
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
		t.Fatalf("create the owner account: %v", err)
	}
	other, err := insertAccount(t, schema.Client.DB, "other")
	if err != nil {
		t.Fatalf("create the second account: %v", err)
	}

	return &world{
		service: repository.NewService(repository.NewPostgresStore(schema.Client.DB)),
		db:      schema.Client.DB,
		owner:   repository.User(owner.ID, false),
		other:   repository.User(other, false),
		ownerID: owner.ID,
		otherID: other,
	}
}

// insertAccount adds a plain account row, which is enough for the foreign keys
// these tests exercise.
func insertAccount(t *testing.T, db *sql.DB, username string) (string, error) {
	t.Helper()
	id := uuid.NewString()
	_, err := db.Exec(`
		INSERT INTO users (id, username, username_folded, email, password_hash)
		VALUES ($1, $2, $3, $4, 'placeholder')`,
		id, username, strings.ToLower(username), username+"@example.com")
	return id, err
}

func (w *world) create(t *testing.T, name string, visibility repository.Visibility) repository.Repository {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	created, err := w.service.Create(ctx, repository.CreateInput{
		Actor: w.owner, OwnerID: w.ownerID, Name: name, Visibility: visibility,
	})
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	return created
}

// grant gives an account a direct grant, as the repository's owner.
func (w *world) grant(t *testing.T, repoID, userID string, role repository.Level) {
	t.Helper()
	w.grantAs(t, w.owner, repoID, userID, role)
}

// grantAs gives an account a direct grant on behalf of a named actor, so a test
// can also assert what happens when someone else tries.
func (w *world) grantAs(t *testing.T, actor repository.Principal, repoID, userID string, role repository.Level) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if _, err := w.service.GrantAccess(ctx, repository.GrantInput{
		Actor: actor, RepoID: repoID, UserID: userID, Role: role,
	}); err != nil {
		t.Fatalf("grant %s: %v", role, err)
	}
}

// revoke removes a direct grant, as the repository's owner.
func (w *world) revoke(t *testing.T, repoID, userID string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return w.service.RevokeAccess(ctx, repository.RevokeInput{Actor: w.owner, RepoID: repoID, UserID: userID})
}

// TestCreatePersistsEveryColumn checks that the row carries what the migration
// declares, including separate name forms and a distinct storage identifier.
func TestCreatePersistsEveryColumn(t *testing.T) {
	w := newWorld(t)
	created := w.create(t, "My-Repo", repository.VisibilityPublic)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var (
		name, folded, visibility, description string
		storageID                             string
		defaultBranch                         sql.NullString
	)
	err := w.db.QueryRowContext(ctx, `
		SELECT name, name_folded, visibility, description, storage_id, default_branch
		FROM repositories WHERE id = $1`, created.ID,
	).Scan(&name, &folded, &visibility, &description, &storageID, &defaultBranch)
	if err != nil {
		t.Fatalf("read the stored row: %v", err)
	}

	if name != "My-Repo" || folded != "my-repo" {
		t.Errorf("stored %q/%q, want the display form preserved and the folded form stored", name, folded)
	}
	if visibility != "public" {
		t.Errorf("visibility = %q, want public", visibility)
	}
	if description != "" {
		t.Errorf("description = %q, want empty", description)
	}
	if storageID == created.ID {
		t.Error("the storage identifier equals the row identifier")
	}
	if storageID != created.StorageID {
		t.Errorf("stored storage id %q does not match the returned %q", storageID, created.StorageID)
	}
	if defaultBranch.Valid {
		t.Error("a new repository has a default branch; an empty repository has none")
	}
}

func TestCreateRejectsADuplicateNameCaseInsensitively(t *testing.T) {
	w := newWorld(t)
	w.create(t, "Repo", repository.VisibilityPrivate)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	_, err := w.service.Create(ctx, repository.CreateInput{
		Actor: w.owner, OwnerID: w.ownerID, Name: "REPO",
	})
	if !errors.Is(err, repository.ErrNameTaken) {
		t.Fatalf("duplicate = %v, want ErrNameTaken", err)
	}

	// Another owner may hold the same folded name.
	if _, err := w.service.Create(ctx, repository.CreateInput{
		Actor: w.other, OwnerID: w.otherID, Name: "Repo",
	}); err != nil {
		t.Fatalf("a second owner could not use the same name: %v", err)
	}
}

// TestPrivateRepositoryIsHiddenFromTheDatabaseLevel is the acceptance property
// that visibility is enforced in the query, for every lookup shape.
func TestPrivateRepositoryIsHiddenFromTheDatabaseLevel(t *testing.T) {
	w := newWorld(t)
	private := w.create(t, "secret-repo", repository.VisibilityPrivate)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	for _, test := range []struct {
		name   string
		viewer repository.Principal
	}{
		{name: "anonymous", viewer: repository.Anonymous()},
		{name: "unrelated account", viewer: w.other},
	} {
		t.Run(test.name, func(t *testing.T) {
			// By name, exact casing.
			if _, _, err := w.service.Resolve(ctx, test.viewer, w.ownerID, private.Name); !errors.Is(err, repository.ErrNotFound) {
				t.Errorf("by name = %v, want ErrNotFound", err)
			}
			// By alternate casing.
			if _, _, err := w.service.Resolve(ctx, test.viewer, w.ownerID, "SECRET-REPO"); !errors.Is(err, repository.ErrNotFound) {
				t.Errorf("by alternate casing = %v, want ErrNotFound", err)
			}
			// By internal identifier.
			if _, _, err := w.service.ResolveByID(ctx, test.viewer, private.ID); !errors.Is(err, repository.ErrNotFound) {
				t.Errorf("by identifier = %v, want ErrNotFound", err)
			}
			// By storage identifier: it is not a capability either, and the
			// service never accepts one, so the lookup by identifier is the
			// closest reachable path and it is covered above.
			if _, _, err := w.service.Require(ctx, test.viewer, private.ID, repository.LevelRead); !errors.Is(err, repository.ErrNotFound) {
				t.Errorf("Require = %v, want ErrNotFound", err)
			}
		})
	}

	// The owner reaches it by every route.
	if _, level, err := w.service.Resolve(ctx, w.owner, w.ownerID, "SECRET-REPO"); err != nil || level != repository.LevelOwner {
		t.Errorf("owner by alternate casing = %v, %s", err, level)
	}
	if _, level, err := w.service.ResolveByID(ctx, w.owner, private.ID); err != nil || level != repository.LevelOwner {
		t.Errorf("owner by identifier = %v, %s", err, level)
	}
}

// TestVisibilityFilterRunsInsideTheQuery proves the SQL predicate, not just the
// service's behaviour: the page must be filled from visible rows only.
func TestVisibilityFilterRunsInsideTheQuery(t *testing.T) {
	w := newWorld(t)

	// Five repositories, interleaved so that a per-page boundary falls between
	// them. Only two are public.
	public := map[string]bool{}
	for i, spec := range []struct {
		name       string
		visibility repository.Visibility
	}{
		{"a-private", repository.VisibilityPrivate},
		{"b-public", repository.VisibilityPublic},
		{"c-private", repository.VisibilityPrivate},
		{"d-public", repository.VisibilityPublic},
		{"e-private", repository.VisibilityPrivate},
	} {
		created := w.create(t, spec.name, spec.visibility)
		if spec.visibility == repository.VisibilityPublic {
			public[created.Name] = true
		}
		// Keep insertion order deterministic relative to updated_at.
		time.Sleep(2 * time.Millisecond)
		_ = i
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Paging one at a time must never yield a private repository, and must
	// total exactly the public ones.
	var seen []string
	for offset := 0; offset < 10; offset++ {
		page, err := w.service.List(ctx, repository.ListQuery{
			Viewer: repository.Anonymous(), OwnerID: w.ownerID, Limit: 1, Offset: offset,
		})
		if err != nil {
			t.Fatalf("List(offset=%d): %v", offset, err)
		}
		if len(page.Repositories) == 0 {
			break
		}
		got := page.Repositories[0]
		if got.Visibility != repository.VisibilityPublic {
			t.Fatalf("offset %d returned the private repository %q", offset, got.Name)
		}
		if !public[got.Name] {
			t.Fatalf("offset %d returned %q, which is not one of the public repositories", offset, got.Name)
		}
		seen = append(seen, got.Name)
	}
	if len(seen) != len(public) {
		t.Errorf("paging yielded %d repositories (%v), want %d", len(seen), seen, len(public))
	}

	// The owner sees all five.
	page, err := w.service.List(ctx, repository.ListQuery{
		Viewer: w.owner, OwnerID: w.ownerID, Limit: 100,
	})
	if err != nil {
		t.Fatalf("List as the owner: %v", err)
	}
	if len(page.Repositories) != 5 {
		t.Errorf("the owner sees %d repositories, want 5", len(page.Repositories))
	}
}

// TestGrantAndRevokeThroughPostgres covers the persistence of collaborator
// grants and the accepted roles.
func TestGrantAndRevokeThroughPostgres(t *testing.T) {
	w := newWorld(t)
	private := w.create(t, "shared-repo", repository.VisibilityPrivate)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	for _, role := range []repository.Level{repository.LevelRead, repository.LevelWrite, repository.LevelAdmin} {
		w.grant(t, private.ID, w.otherID, role)
		if _, level, err := w.service.Resolve(ctx, w.other, w.ownerID, private.Name); err != nil || level != role {
			t.Errorf("after granting %s: level = %s, err = %v", role, level, err)
		}
	}

	// The role is stored, and ownership is never one of them.
	var stored string
	if err := w.db.QueryRowContext(ctx,
		"SELECT role FROM repository_collaborators WHERE repository_id = $1 AND user_id = $2",
		private.ID, w.otherID).Scan(&stored); err != nil {
		t.Fatalf("read the stored role: %v", err)
	}
	if stored != "admin" {
		t.Errorf("stored role = %q, want admin", stored)
	}
	// The CHECK constraint refuses an owner row, so ownership cannot be
	// duplicated into the grant table by any path.
	if _, err := w.db.ExecContext(ctx, `
		INSERT INTO repository_collaborators (repository_id, user_id, role)
		VALUES ($1, $2, 'owner')`, private.ID, w.otherID); err == nil {
		t.Error("the database accepted an owner role in the grant table")
	}

	if err := w.revoke(t, private.ID, w.otherID); err != nil {
		t.Fatalf("RevokeAccess: %v", err)
	}
	if _, _, err := w.service.Resolve(ctx, w.other, w.ownerID, private.Name); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("after revocation = %v, want ErrNotFound", err)
	}
}

// TestRenameKeepsStorageAndIdentityInTheDatabase is the acceptance property
// checked against the real row.
func TestRenameKeepsStorageAndIdentityInTheDatabase(t *testing.T) {
	w := newWorld(t)
	original := w.create(t, "before-rename", repository.VisibilityPrivate)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	renamed, err := w.service.Rename(ctx, repository.RenameInput{
		Actor: w.owner, RepoID: original.ID, Name: "After-Rename",
	})
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.StorageID != original.StorageID {
		t.Fatalf("the storage identifier changed: %q -> %q", original.StorageID, renamed.StorageID)
	}
	if renamed.ID != original.ID {
		t.Fatalf("the row identifier changed: %q -> %q", original.ID, renamed.ID)
	}

	var name, folded, storageID, ownerID string
	if err := w.db.QueryRowContext(ctx,
		"SELECT name, name_folded, storage_id, owner_id FROM repositories WHERE id = $1", original.ID,
	).Scan(&name, &folded, &storageID, &ownerID); err != nil {
		t.Fatalf("read the renamed row: %v", err)
	}
	if name != "After-Rename" || folded != "after-rename" {
		t.Errorf("stored %q/%q, want the new display and folded forms", name, folded)
	}
	if storageID != original.StorageID {
		t.Errorf("the stored storage identifier changed to %q", storageID)
	}
	if ownerID != w.ownerID {
		t.Errorf("the owner changed to %q", ownerID)
	}

	// The old name is free again and the new one resolves.
	if _, err := w.service.Create(ctx, repository.CreateInput{
		Actor: w.owner, OwnerID: w.ownerID, Name: "before-rename",
	}); err != nil {
		t.Errorf("the old name is still taken: %v", err)
	}
	if _, level, err := w.service.Resolve(ctx, w.owner, w.ownerID, "after-rename"); err != nil || level != repository.LevelOwner {
		t.Errorf("the new name does not resolve: %v, %s", err, level)
	}
}

func TestSchemaNotMigratedIsReportedBeforeQuerying(t *testing.T) {
	schema := postgrestest.NewEmptySchema(t)
	service := repository.NewService(repository.NewPostgresStore(schema.Client.DB))

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// The actor owns the namespace it is creating in, so the ownership check
	// passes and the missing schema is what stops the request.
	ownerID := uuid.NewString()
	if _, err := service.Create(ctx, repository.CreateInput{
		Actor: repository.User(ownerID, false), OwnerID: ownerID, Name: "repo",
	}); !errors.Is(err, repository.ErrSchemaNotMigrated) {
		t.Fatalf("Create = %v, want ErrSchemaNotMigrated", err)
	}
	if _, _, err := service.Resolve(ctx, repository.Anonymous(), uuid.NewString(), "repo"); !errors.Is(err, repository.ErrSchemaNotMigrated) {
		t.Fatalf("Resolve = %v, want ErrSchemaNotMigrated", err)
	}
	if _, err := service.List(ctx, repository.ListQuery{Viewer: repository.Anonymous()}); !errors.Is(err, repository.ErrSchemaNotMigrated) {
		t.Fatalf("List = %v, want ErrSchemaNotMigrated", err)
	}
}

// TestListFiltersByOwner covers the owner-scoped listing against the real query.
func TestListFiltersByOwner(t *testing.T) {
	w := newWorld(t)
	w.create(t, "owner-public", repository.VisibilityPublic)
	if _, err := w.service.Create(context.Background(), repository.CreateInput{
		Actor: w.other, OwnerID: w.otherID, Name: "other-public", Visibility: repository.VisibilityPublic,
	}); err != nil {
		t.Fatalf("create for the other owner: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	page, err := w.service.List(ctx, repository.ListQuery{
		Viewer: repository.Anonymous(), OwnerID: w.ownerID, Limit: 100,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Repositories) != 1 || page.Repositories[0].Name != "owner-public" {
		t.Fatalf("owner-scoped listing returned %+v, want only owner-public", page.Repositories)
	}

	// Without an owner filter, both public repositories are visible.
	page, err = w.service.List(ctx, repository.ListQuery{Viewer: repository.Anonymous(), Limit: 100})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Repositories) != 2 {
		t.Errorf("unscoped listing returned %d repositories, want 2", len(page.Repositories))
	}
}
