package identity_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-gitnest/GitNest/internal/identity"
	"github.com/go-gitnest/GitNest/internal/platform/postgrestest"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// testPassword satisfies the documented minimum length.
const testPassword = "correct horse battery staple"

// migratedStore returns a store backed by a schema that already has the shipped
// migrations applied, plus the pool for assertions.
func migratedStore(t *testing.T) (*identity.PostgresStore, *sql.DB) {
	t.Helper()
	schema := postgrestest.NewSchema(t)
	return identity.NewPostgresStore(schema.Client.DB), schema.Client.DB
}

// admin builds a prepared administrator the way the service would.
func admin(username string) identity.NewAdmin {
	return identity.NewAdmin{
		ID:             uuid.NewString(),
		Username:       username,
		UsernameFolded: identity.FoldName(username),
		Email:          identity.NormalizeEmail(username + "@example.com"),
		PasswordHash:   "$argon2id$v=19$m=65536,t=3,p=2$c2FsdHNhbHRzYWx0c2E$dmVyaWZpZXJ2ZXJpZmllcg",
	}
}

func countAdmins(t *testing.T, db *sql.DB) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var count int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE is_admin").Scan(&count); err != nil {
		t.Fatalf("count administrators: %v", err)
	}
	return count
}

func TestCreateInitialAdminPersistsTheAccount(t *testing.T) {
	store, db := migratedStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	prepared := admin("root")
	user, err := store.CreateInitialAdmin(ctx, prepared)
	if err != nil {
		t.Fatalf("CreateInitialAdmin: %v", err)
	}
	if !user.IsAdmin {
		t.Error("the created account is not an administrator")
	}
	if user.CreatedAt.IsZero() {
		t.Error("the created account has no creation timestamp")
	}

	var username, folded, email, hash string
	if err := db.QueryRowContext(ctx,
		"SELECT username, username_folded, email, password_hash FROM users WHERE id = $1", prepared.ID,
	).Scan(&username, &folded, &email, &hash); err != nil {
		t.Fatalf("read the stored account: %v", err)
	}
	if username != "root" || folded != "root" || email != "root@example.com" {
		t.Errorf("stored %q/%q/%q, want root/root/root@example.com", username, folded, email)
	}
	if hash != prepared.PasswordHash {
		t.Error("the stored hash differs from the one supplied")
	}
}

// TestCreateInitialAdminRefusesASecondAdministrator is the core bootstrap rule.
func TestCreateInitialAdminRefusesASecondAdministrator(t *testing.T) {
	store, db := migratedStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := store.CreateInitialAdmin(ctx, admin("root")); err != nil {
		t.Fatalf("first CreateInitialAdmin: %v", err)
	}
	if _, err := store.CreateInitialAdmin(ctx, admin("second")); !errors.Is(err, identity.ErrAdminExists) {
		t.Fatalf("second CreateInitialAdmin = %v, want ErrAdminExists", err)
	}

	if admins := countAdmins(t, db); admins != 1 {
		t.Errorf("administrator count = %d, want 1", admins)
	}
	var accounts int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users").Scan(&accounts); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if accounts != 1 {
		t.Errorf("account count = %d, want 1; the refused bootstrap left a row behind", accounts)
	}
}

// TestCreateInitialAdminSerializesConcurrentAttempts proves the advisory lock:
// parallel bootstraps leave exactly one administrator.
func TestCreateInitialAdminSerializesConcurrentAttempts(t *testing.T) {
	store, db := migratedStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const attempts = 4
	var (
		wait    sync.WaitGroup
		mu      sync.Mutex
		created int
		refused int
		other   []error
	)
	for i := 0; i < attempts; i++ {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			_, err := store.CreateInitialAdmin(ctx, admin(fmt.Sprintf("root%d", i)))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case errors.Is(err, identity.ErrAdminExists):
				refused++
			default:
				other = append(other, err)
			}
		}(i)
	}
	wait.Wait()

	for _, err := range other {
		t.Errorf("unexpected error from a concurrent attempt: %v", err)
	}
	if created != 1 || refused != attempts-1 {
		t.Fatalf("created = %d, refused = %d; want 1 and %d", created, refused, attempts-1)
	}
	if admins := countAdmins(t, db); admins != 1 {
		t.Errorf("administrator count = %d, want 1", admins)
	}
}

// TestCreateInitialAdminRequiresMigratedSchema covers the operator-facing case
// that the database has not been migrated yet.
func TestCreateInitialAdminRequiresMigratedSchema(t *testing.T) {
	schema := postgrestest.NewEmptySchema(t)
	store := identity.NewPostgresStore(schema.Client.DB)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := store.CreateInitialAdmin(ctx, admin("root")); !errors.Is(err, identity.ErrSchemaNotMigrated) {
		t.Fatalf("error = %v, want ErrSchemaNotMigrated", err)
	}
}

// TestCreateInitialAdminReportsAccountConflicts covers the case where a
// non-administrator already holds the name or the address.
func TestCreateInitialAdminReportsAccountConflicts(t *testing.T) {
	store, db := migratedStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, username, username_folded, email, password_hash)
		VALUES ($1, $2, $3, $4, 'placeholder')`,
		uuid.NewString(), "existing", "existing", "existing@example.com",
	); err != nil {
		t.Fatalf("insert an existing account: %v", err)
	}

	if _, err := store.CreateInitialAdmin(ctx, admin("existing")); !errors.Is(err, identity.ErrUsernameTaken) {
		t.Errorf("username conflict = %v, want ErrUsernameTaken", err)
	}

	conflicting := admin("another")
	conflicting.Email = "existing@example.com"
	if _, err := store.CreateInitialAdmin(ctx, conflicting); !errors.Is(err, identity.ErrEmailTaken) {
		t.Errorf("email conflict = %v, want ErrEmailTaken", err)
	}
	if admins := countAdmins(t, db); admins != 0 {
		t.Errorf("administrator count = %d, want 0 after refused attempts", admins)
	}
}

// TestCreateInitialAdminFoldsTheNameForLookup proves uniqueness acts on the
// folded form: a case-different repeat of the same name collides.
func TestCreateInitialAdminFoldsTheNameForLookup(t *testing.T) {
	store, db := migratedStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO users (id, username, username_folded, email, password_hash)
		VALUES ($1, 'Existing', 'existing', 'existing@example.com', 'placeholder')`,
		uuid.NewString(),
	); err != nil {
		t.Fatalf("insert an existing account: %v", err)
	}

	if _, err := store.CreateInitialAdmin(ctx, admin("Existing")); !errors.Is(err, identity.ErrUsernameTaken) {
		t.Errorf("case-different name = %v, want ErrUsernameTaken", err)
	}
}

// TestCreateInitialAdminRejectsAFoldedMismatch documents the check constraint:
// a row whose folded form disagrees with its display form cannot be written.
func TestCreateInitialAdminRejectsAFoldedMismatch(t *testing.T) {
	store, _ := migratedStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	broken := admin("root")
	broken.UsernameFolded = "not-the-folded-form"
	if _, err := store.CreateInitialAdmin(ctx, broken); err == nil {
		t.Fatal("a row with a folded form that disagrees with the display name was accepted")
	}
}
