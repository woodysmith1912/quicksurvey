package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// schemaV1 is the users table exactly as 0.1.0 created it: no sessions_from.
//
// This test exists because a release shipped that could not read its own users
// table. CREATE TABLE IF NOT EXISTS does nothing to a table that already
// exists, so a column added later never appeared in a database created by an
// earlier version; every SELECT naming it failed, and the failure surfaced as
// "no accounts" rather than as an error. Nobody could sign in, and nothing said
// why.
const schemaV1 = `
CREATE TABLE meta (key TEXT PRIMARY KEY, value BLOB NOT NULL);
CREATE TABLE users (
  name                 TEXT PRIMARY KEY,
  role                 TEXT NOT NULL,
  hash                 TEXT NOT NULL,
  created              TEXT NOT NULL,
  must_change_password INTEGER NOT NULL DEFAULT 0,
  pending              INTEGER NOT NULL DEFAULT 0,
  invited_by           TEXT NOT NULL DEFAULT ''
);
`

func TestUpgradeFromAnEarlierSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, dbFile)

	// Build a database the way the previous release left it.
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	hash, err := hashPassword("password123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO users (name, role, hash, created, must_change_password, pending, invited_by)
		 VALUES ('woody', 'admin', ?, '2026-09-09T00:00:00Z', 0, 0, '')`, hash); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Opening it must upgrade it, not quietly lose everything in it.
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("opening a pre-existing database failed: %v", err)
	}
	defer s.Close()

	users := s.Users()
	if len(users) != 1 || users[0].Name != "woody" {
		t.Fatalf("Users() = %v, want the account that was already there — "+
			"an unreadable table must not look like an empty one", users)
	}
	if _, ok := s.Authenticate("woody", "password123"); !ok {
		t.Error("the existing account cannot sign in after the upgrade")
	}
	// And the feature the new column is for actually works.
	before, _ := s.User("woody")
	if err := s.RevokeSessions("woody"); err != nil {
		t.Fatalf("RevokeSessions after upgrade: %v", err)
	}
	after, _ := s.User("woody")
	if after.SessionKey() == before.SessionKey() {
		t.Error("sessions_from was added but does not work")
	}

	// Opening again is a no-op, not a second migration attempt.
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("second open failed: %v", err)
	}
	defer s2.Close()
	if len(s2.Users()) != 1 {
		t.Error("a second open lost the account")
	}
}

// Every column the code selects has to exist after a migration, whatever
// version the database started life as.
func TestEveryQueriedColumnExistsAfterMigration(t *testing.T) {
	s := newStore(t)
	for _, c := range addedColumns {
		var n int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`,
			c.table, c.column).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("%s.%s missing after migration", c.table, c.column)
		}
	}
	// The queries the application actually runs must all succeed. A schema
	// mismatch shows up here rather than as empty pages in production.
	for name, q := range map[string]string{
		"users":     `SELECT ` + userColumns + ` FROM users LIMIT 1`,
		"invites":   `SELECT ` + inviteColumns + ` FROM invites LIMIT 1`,
		"resets":    `SELECT ` + resetColumns + ` FROM resets LIMIT 1`,
		"surveys":   `SELECT ` + surveyColumns + ` FROM surveys LIMIT 1`,
		"responses": `SELECT id, voter, comment, created, updated FROM responses LIMIT 1`,
		"options":   `SELECT id, text, status, source, merged_into, created FROM options LIMIT 1`,
	} {
		rows, err := s.db.Query(q)
		if err != nil {
			t.Errorf("%s: the application's own query does not run: %v", name, err)
			continue
		}
		rows.Close()
	}
}
