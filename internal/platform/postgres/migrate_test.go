package postgres

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoadOrdersMigrationsByName(t *testing.T) {
	files := fstest.MapFS{
		"0010_third.up.sql":  &fstest.MapFile{Data: []byte("SELECT 3;")},
		"0002_second.up.sql": &fstest.MapFile{Data: []byte("SELECT 2;")},
		"0001_first.up.sql":  &fstest.MapFile{Data: []byte("SELECT 1;")},
		"notes.txt":          &fstest.MapFile{Data: []byte("not a migration")},
	}

	loaded, err := Load(files)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"0001_first.up.sql", "0002_second.up.sql", "0010_third.up.sql"}
	if len(loaded) != len(want) {
		t.Fatalf("loaded %d migrations (%v), want %d; non-migration files must be ignored", len(loaded), loaded, len(want))
	}
	for i, name := range want {
		if loaded[i].Name != name {
			t.Errorf("migration %d = %q, want %q", i, loaded[i].Name, name)
		}
	}
}

func TestLoadAcceptsAnEmptySet(t *testing.T) {
	loaded, err := Load(fstest.MapFS{})
	if err != nil {
		t.Fatalf("Load of an empty set: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("loaded %d migrations from an empty set", len(loaded))
	}
}

// TestLoadRejectsBadNames covers files that end in .sql but are not valid
// migrations. They must be reported rather than skipped: a mistyped suffix or
// sequence would otherwise go unnoticed on a case-insensitive filesystem.
func TestLoadRejectsBadNames(t *testing.T) {
	for _, name := range []string{
		"1_first.up.sql",
		"00001_first.up.sql",
		"0001_First.up.sql",
		"0001_first.up.SQL",
		"0001-first.up.sql",
		"0001_.up.sql",
		"first.up.sql",
		"0002_second.sql",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(fstest.MapFS{name: &fstest.MapFile{Data: []byte("SELECT 1;")}})
			if err == nil {
				t.Fatalf("migration name %q was accepted", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error = %v, want it to name the offending file", err)
			}
		})
	}
}

// TestLoadIgnoresFilesThatAreNotSQL keeps the directory usable for a README or
// editor droppings.
func TestLoadIgnoresFilesThatAreNotSQL(t *testing.T) {
	loaded, err := Load(fstest.MapFS{
		"README.md":        &fstest.MapFile{Data: []byte("# Migrations")},
		".gitkeep":         &fstest.MapFile{Data: []byte("")},
		"0001_real.up.sql": &fstest.MapFile{Data: []byte("SELECT 1;")},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Name != "0001_real.up.sql" {
		t.Fatalf("loaded %v, want only the migration", loaded)
	}
}

func TestLoadRecordsAStableChecksum(t *testing.T) {
	first, err := Load(fstest.MapFS{"0001_a.up.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	again, err := Load(fstest.MapFS{"0001_a.up.sql": &fstest.MapFile{Data: []byte("SELECT 1;")}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	changed, err := Load(fstest.MapFS{"0001_a.up.sql": &fstest.MapFile{Data: []byte("SELECT 2;")}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if first[0].Checksum != again[0].Checksum {
		t.Error("identical contents produced different checksums")
	}
	if first[0].Checksum == changed[0].Checksum {
		t.Error("different contents produced the same checksum")
	}
	if len(first[0].Checksum) != 64 {
		t.Errorf("checksum %q is not a hex SHA-256 digest", first[0].Checksum)
	}
}

func TestLoadDetectsTheNoTransactionMarker(t *testing.T) {
	for _, test := range []struct {
		name string
		sql  string
		want bool
	}{
		{
			name: "marker first",
			sql:  NoTransactionMarker + "\nCREATE INDEX CONCURRENTLY i ON t (c);\n",
			want: true,
		},
		{
			name: "marker after a blank line",
			sql:  "\n\n" + NoTransactionMarker + "\nSELECT 1;\n",
			want: true,
		},
		{
			name: "marker inside the leading comment block",
			sql:  "-- a description\n--\n" + NoTransactionMarker + "\n-- why it is needed\nSELECT 1;\n",
			want: true,
		},
		{
			name: "marker with trailing spaces",
			sql:  NoTransactionMarker + "   \nSELECT 1;\n",
			want: true,
		},
		{
			name: "no marker",
			sql:  "-- just a description\nSELECT 1;\n",
			want: false,
		},
		{
			name: "marker after the first statement",
			sql:  "SELECT 1;\n" + NoTransactionMarker + "\n",
			want: false,
		},
		{
			name: "different spelling",
			sql:  "-- gitnest:no_transaction\nSELECT 1;\n",
			want: false,
		},
		{
			name: "empty file",
			sql:  "",
			want: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			loaded, err := Load(fstest.MapFS{"0001_probe.up.sql": &fstest.MapFile{Data: []byte(test.sql)}})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if loaded[0].NoTransaction != test.want {
				t.Errorf("NoTransaction = %t, want %t for %q", loaded[0].NoTransaction, test.want, test.sql)
			}
		})
	}
}
