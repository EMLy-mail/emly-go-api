package schema

import (
	"io/fs"
	"strings"
	"testing"
)

// splitStatements cuts on every ";", comments included, so a semicolon in a
// "--" comment turns the rest of that line into SQL and the migration fails
// at startup with a 1064 - which, since main exits on a migration error,
// keeps the whole API down. Catch it here instead of on the server.
func TestNoSemicolonsInSQLComments(t *testing.T) {
	files, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, "init.sql")

	for _, name := range files {
		data, err := migrationsFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "--") && strings.Contains(line, ";") {
				t.Errorf("%s:%d: semicolon inside a comment splits the statement: %s",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// Every statement splitStatements yields from a migration must contain real
// SQL once its comment lines are dropped - a fragment that is only prose
// means the file was cut in the wrong place.
func TestMigrationsSplitIntoSQL(t *testing.T) {
	files, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		data, err := migrationsFS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range splitStatements(string(data)) {
			var sql []string
			for _, line := range strings.Split(stmt, "\n") {
				if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "--") {
					sql = append(sql, l)
				}
			}
			if len(sql) == 0 {
				t.Errorf("%s: statement with no SQL in it:\n%s", name, stmt)
			}
		}
	}
}
