package postgrestest

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// helperEnv marks the subprocess that deliberately fails.
const helperEnv = "POSTGRESTEST_FAILURE_HELPER"

func TestRequiredReadsTheEnvironment(t *testing.T) {
	for _, test := range []struct {
		name       string
		value      string
		capability string
		want       bool
	}{
		{name: "unset", value: "", capability: "postgres", want: false},
		{name: "exact", value: "postgres", capability: "postgres", want: true},
		{name: "case and padding", value: " POSTGRES ", capability: "postgres", want: true},
		{name: "in a list", value: "container,postgres", capability: "postgres", want: true},
		{name: "all", value: "all", capability: "postgres", want: true},
		{name: "another capability", value: "container", capability: "postgres", want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(EnvRequire, test.value)
			if got := Required(test.capability); got != test.want {
				t.Errorf("Required(%q) with %s=%q = %t, want %t",
					test.capability, EnvRequire, test.value, got, test.want)
			}
		})
	}
}

func TestConcurrencyLimitParsesTheEnvironment(t *testing.T) {
	for _, test := range []struct {
		value string
		want  int
	}{
		{value: "", want: 0},
		{value: "4", want: 4},
		{value: " 8 ", want: 8},
		{value: "unlimited", want: 0},
		{value: "-2", want: 0},
	} {
		t.Run(test.value, func(t *testing.T) {
			t.Setenv(EnvConcurrency, test.value)
			if got := concurrencyLimit(); got != test.want {
				t.Errorf("concurrencyLimit() with %q = %d, want %d", test.value, got, test.want)
			}
		})
	}
}

func TestQuoteIdentifierEscapesQuotes(t *testing.T) {
	if got := quoteIdentifier(`odd"name`); got != `"odd""name"` {
		t.Errorf("quoteIdentifier = %s, want the quote doubled", got)
	}
}

// TestSchemaProvidesAMigratedSchemaAndDropsIt covers the common path: the schema
// carries the shipped tables, and it is gone once the test that created it ends.
func TestSchemaProvidesAMigratedSchemaAndDropsIt(t *testing.T) {
	schema := NewSchema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if !strings.HasPrefix(schema.Name, schemaPrefix) {
		t.Errorf("schema name %q does not carry the harness prefix", schema.Name)
	}
	exists, err := TableExists(ctx, schema.Client.DB, "instance_settings")
	if err != nil {
		t.Fatalf("check the migrated table: %v", err)
	}
	if !exists {
		t.Error("NewSchema did not apply the shipped migrations")
	}
	// The schema's own removal is proven by TestSchemaIsDroppedWhenATestFails,
	// which observes it from outside the test that created it.
}

// TestEmptySchemaHasNoTables covers the variant used by migration tests.
func TestEmptySchemaHasNoTables(t *testing.T) {
	schema := NewEmptySchema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	exists, err := TableExists(ctx, schema.Client.DB, "instance_settings")
	if err != nil {
		t.Fatalf("check the table: %v", err)
	}
	if exists {
		t.Error("NewEmptySchema applied migrations")
	}
}

// TestSchemasAreIsolated proves the harness's reason for existing: two tests in
// the same process cannot see each other's tables.
func TestSchemasAreIsolated(t *testing.T) {
	first := NewSchema(t)
	second := NewEmptySchema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if first.Name == second.Name {
		t.Fatalf("two schemas share the name %q", first.Name)
	}
	exists, err := TableExists(ctx, second.Client.DB, "instance_settings")
	if err != nil {
		t.Fatalf("check the second schema: %v", err)
	}
	if exists {
		t.Error("the empty schema sees the migrated schema's table")
	}
}

// TestFailureHelper is a helper process, not a test of its own: when
// helperEnv is set it creates a schema and then fails, so the parent can prove
// that the schema was dropped anyway.
func TestFailureHelper(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("helper process only")
	}
	schema := NewEmptySchema(t)
	t.Logf("helper created %s", schema.Name)
	t.Fatal("deliberate failure: the schema must be dropped anyway")
}

// TestSchemaIsDroppedWhenATestFails is the acceptance criterion that a failing
// test still cleans up. The failing test has to run in another process, because
// a failing subtest would fail this one too; the helper re-executes this test
// binary with a filter and the helper environment variable set.
func TestSchemaIsDroppedWhenATestFails(t *testing.T) {
	dsn := Require(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open the maintenance connection: %v", err)
	}
	defer func() { _ = admin.Close() }()

	helper := exec.Command(os.Args[0], "-test.run=TestFailureHelper", "-test.v")
	helper.Env = append(os.Environ(), helperEnv+"=1")
	output, runErr := helper.CombinedOutput()
	if runErr == nil {
		t.Fatalf("the helper process was expected to fail\n%s", output)
	}

	name := helperSchemaName(string(output))
	if name == "" {
		t.Fatalf("the helper did not report a schema name\n%s", output)
	}

	after, err := schemas(ctx, admin)
	if err != nil {
		t.Fatalf("list schemas after: %v", err)
	}
	for _, leaked := range after {
		if leaked == name {
			t.Errorf("schema %s survived the failing test", name)
		}
	}
	// Only the helper's own name is checked. Counting the schemas before and
	// after would be unsound: `go test ./...` runs packages in parallel, so
	// another package creates and drops its schemas while this one waits, and
	// the total can legitimately grow.
}

// helperSchemaName extracts the name the helper logged.
func helperSchemaName(output string) string {
	const marker = "helper created "
	index := strings.Index(output, marker)
	if index < 0 {
		return ""
	}
	rest := output[index+len(marker):]
	if end := strings.IndexAny(rest, "\r\n"); end >= 0 {
		rest = rest[:end]
	}
	return strings.TrimSpace(rest)
}
