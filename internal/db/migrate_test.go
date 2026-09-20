package db

import (
	"io/fs"
	"path"
	"regexp"
	"testing"
)

// Migrate itself needs a real Postgres and is exercised by every
// internal/user integration test (each newTestStore call runs it). What's
// testable without a database is the part that decides *what* to run, and
// the shape of the migrations directory itself — both of which are where
// the real hazards are.

func TestPendingMigrations(t *testing.T) {
	all := []string{
		"migrations/0002_second.sql",
		"migrations/0001_first.sql",
		"migrations/0003_third.sql",
	}

	tests := []struct {
		name    string
		applied []string
		want    []string
	}{
		{
			name:    "nothing applied yet runs everything, in filename order",
			applied: nil,
			want:    []string{"migrations/0001_first.sql", "migrations/0002_second.sql", "migrations/0003_third.sql"},
		},
		{
			name:    "an applied prefix leaves only the tail",
			applied: []string{"migrations/0001_first.sql", "migrations/0002_second.sql"},
			want:    []string{"migrations/0003_third.sql"},
		},
		{
			name:    "a fully applied set runs nothing",
			applied: all,
			want:    []string{},
		},
		{
			// The gap case: a migration recorded out of order (e.g. two
			// branches merged) must not cause the ones around it to be
			// skipped or re-run.
			name:    "a hole in the middle runs only the hole",
			applied: []string{"migrations/0001_first.sql", "migrations/0003_third.sql"},
			want:    []string{"migrations/0002_second.sql"},
		},
		{
			// schema_migrations can name a version whose file is gone
			// (deleted migration). That's not an error — there's nothing
			// to apply.
			name:    "a recorded version with no embedded file is ignored",
			applied: []string{"migrations/0001_first.sql", "migrations/9999_deleted.sql"},
			want:    []string{"migrations/0002_second.sql", "migrations/0003_third.sql"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			applied := make(map[string]struct{}, len(tc.applied))
			for _, v := range tc.applied {
				applied[v] = struct{}{}
			}

			got := pendingMigrations(all, applied)
			if len(got) != len(tc.want) {
				t.Fatalf("pendingMigrations() = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("pendingMigrations() = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestPendingMigrationsDoesNotMutateItsInput(t *testing.T) {
	// Migrate passes fs.Glob's slice straight in; reordering the caller's
	// slice in place would be a nasty thing to leave behind.
	paths := []string{"migrations/0002_b.sql", "migrations/0001_a.sql"}
	pendingMigrations(paths, map[string]struct{}{})

	if paths[0] != "migrations/0002_b.sql" || paths[1] != "migrations/0001_a.sql" {
		t.Errorf("input slice was reordered: %v", paths)
	}
}

var migrationPrefix = regexp.MustCompile(`^(\d+)_`)

// TestMigrationsHaveUniqueNumericPrefixes guards the one way this
// directory can go wrong silently.
//
// Migrate applies files in sorted-path order, and records the path itself
// as the version id. Two files sharing a numeric prefix still sort
// deterministically (by the rest of the name), so nothing breaks the day
// they're added — but a third file with that same prefix can later sort
// *between* two already-applied ones, changing the order the schema is
// built in for any fresh database while leaving existing ones alone.
//
// Fixing a collision needs care, because renaming a file changes its
// version id and so re-runs that migration once against every existing
// database. That's only safe when every statement in it is guarded
// (IF NOT EXISTS / ADD COLUMN IF NOT EXISTS), in which case the re-run
// is a no-op. It is NOT safe for a bare `ALTER TABLE ... ADD COLUMN`
// — 0007, 0009 and 0014_dossier_public are all in that category — where
// the re-run errors, the transaction rolls back, and
// cmd/server/main.go's log.Fatalf kills startup on every deploy until
// the rename is reverted.
//
// That's exactly how the original 0014/0014 collision was resolved:
// friend_requests (fully guarded) was renumbered to 0015 and
// dossier_public (unguarded) was left alone. When in doubt, renumber the
// guarded one, or give the *new* file an unused number and leave both
// existing files untouched.
func TestMigrationsHaveUniqueNumericPrefixes(t *testing.T) {
	paths, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatalf("listing embedded migrations: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no embedded migrations found — is the //go:embed directive still correct?")
	}

	seen := make(map[string]string, len(paths))
	for _, p := range paths {
		name := path.Base(p)
		match := migrationPrefix.FindStringSubmatch(name)
		if match == nil {
			t.Errorf("migration %q has no leading NNNN_ number — Migrate orders by filename, so every migration needs one", name)
			continue
		}
		prefix := match[1]
		if other, dup := seen[prefix]; dup {
			t.Errorf("migrations %q and %q share the numeric prefix %q — renumber whichever is safe to re-run, or give the newer one an unused number; see this test's doc comment", other, name, prefix)
			continue
		}
		seen[prefix] = name
	}
}
