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
		"seen":      `SELECT response_id, option_id FROM seen LIMIT 1`,
	} {
		rows, err := s.db.Query(q)
		if err != nil {
			t.Errorf("%s: the application's own query does not run: %v", name, err)
			continue
		}
		rows.Close()
	}
}

// Responses recorded before the seen table existed carry no record of what
// was on their ballot. For an approved option — both of this survey's options
// are — the upgrade treats every pre-existing response as having seen it,
// since it was on the ballot for everyone who answered. (Pending and rejected
// write-ins are narrower; see
// TestBackfillMarksPendingAndRejectedOptionsSeenOnlyByTheirProposer.) The
// backfill runs exactly once, so options added later are not retroactively
// marked as shown to people who answered before they existed.
func TestSeenIsBackfilledOnceForResponsesFromBeforeItExisted(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sv := mustSurvey(t, s, "Alpha", "Bravo")
	v := s.VoterID(sv.ID, "a")
	if _, err := s.SaveResponse(sv.ID, v, []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Turn it into what the previous release would have left behind.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE seen`, `DELETE FROM meta WHERE key = 'seen_backfilled'`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err = Open(dir)
	if err != nil {
		t.Fatalf("opening a database without the seen table failed: %v", err)
	}
	r, ok := s.ResponseFor(sv.ID, v)
	if !ok {
		t.Fatal("the response is gone")
	}
	if len(r.Seen) != 2 {
		t.Fatalf("seen = %v after upgrade, want both options backfilled", r.Seen)
	}

	// An option added after the upgrade was not shown to anyone who answered
	// before it. A second open must not backfill again.
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		_, err := AddOption(d, "Charlie", OptApproved, "editor")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if r, _ := s.ResponseFor(sv.ID, v); len(r.Seen) != 2 {
		t.Errorf("seen = %v after a second open, want the backfill to have run only once", r.Seen)
	}
}

// TestBackfillMarksPendingAndRejectedOptionsSeenOnlyByTheirProposer pins the
// current, project-owner-confirmed behaviour of backfillSeen: an option that
// was pending or rejected at the moment of the upgrade is backfilled as seen
// only by the response that actually voted for it — which can only be its
// proposer, since a pending write-in is visible only to the person who
// proposed it and a rejected one was only ever visible to its proposer before
// a moderator refused it. A live upgrade test against a real 0.3.0 container
// showed what the old blanket rule cost here: a write-in left pending across
// the upgrade and approved afterwards reported shown 5 and interest 20% for a
// single voter who had, in truth, seen and picked it — interest 100%.
//
// Every other status keeps the old "everyone who answered saw it" rule,
// including OptRemoved: it was on the ballot until an editor deleted it, so
// everyone who answered really did see it, and this test also pins that a
// careless narrowing must not sweep removed options in with pending and
// rejected ones.
func TestBackfillMarksPendingAndRejectedOptionsSeenOnlyByTheirProposer(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	sv := mustSurvey(t, s, "Alpha", "Bravo")
	a := s.VoterID(sv.ID, "a")
	if _, err := s.SaveResponse(sv.ID, a, []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	c := s.VoterID(sv.ID, "c")
	if _, err := s.SaveResponse(sv.ID, c, []string{sv.Options[1].ID}, ""); err != nil {
		t.Fatal(err)
	}

	// b proposes Charlie; it is still pending when the upgrade runs.
	b := s.VoterID(sv.ID, "b")
	charlie, err := s.AddWriteIn(sv.ID, b, "Charlie")
	if err != nil {
		t.Fatal(err)
	}
	// d proposes Delta; a moderator rejects it before the upgrade runs.
	d := s.VoterID(sv.ID, "d")
	delta, err := s.AddWriteIn(sv.ID, d, "Delta")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSurvey(sv.ID, func(sur *Survey) error {
		return SetOptionStatus(sur, delta, OptRejected, "")
	}); err != nil {
		t.Fatal(err)
	}
	// An editor removes Bravo after c has already voted for it.
	if _, err := s.UpdateSurvey(sv.ID, func(sur *Survey) error {
		return SetOptionStatus(sur, sv.Options[1].ID, OptRemoved, "")
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Turn it into what the previous release would have left behind.
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE seen`, `DELETE FROM meta WHERE key = 'seen_backfilled'`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err = Open(dir)
	if err != nil {
		t.Fatalf("opening a database without the seen table failed: %v", err)
	}
	defer s.Close()

	// Alpha (approved) and Bravo (removed) were on the ballot for everyone who
	// answered, whatever their status is now, so every pre-existing response
	// is backfilled as having seen both. Charlie and Delta, still pending and
	// rejected respectively, are backfilled as seen only by the response that
	// actually voted for them: their own proposer.
	for _, voter := range []string{a, b, c, d} {
		got := seenByText(t, s, sv.ID, voter)
		if !got["Alpha"] || !got["Bravo"] {
			t.Errorf("%s: seen = %v, want Alpha and Bravo (approved / removed are seen by everyone)", voter, got)
		}
		if got["Charlie"] != (voter == b) {
			t.Errorf("%s: seen[Charlie] = %v, want true only for its proposer b", voter, got["Charlie"])
		}
		if got["Delta"] != (voter == d) {
			t.Errorf("%s: seen[Delta] = %v, want true only for its proposer d", voter, got["Delta"])
		}
	}
	checkTallyInvariant(t, s, sv.ID)

	// A moderator approves both write-ins. Shown must reflect only the one
	// respondent who could really have seen each of them — not every
	// pre-existing respondent — so interest comes out as 100%, not diluted by
	// respondents who never had it on their ballot.
	if _, err := s.UpdateSurvey(sv.ID, func(sur *Survey) error {
		if err := SetOptionStatus(sur, charlie, OptApproved, ""); err != nil {
			return err
		}
		return SetOptionStatus(sur, delta, OptApproved, "")
	}); err != nil {
		t.Fatal(err)
	}

	shown := shownByText(t, s, sv.ID)
	results, respondents := s.Tally(sv.ID)
	if respondents != 4 {
		t.Fatalf("respondents = %d, want 4", respondents)
	}
	byText := map[string]Result{}
	for _, r := range results {
		byText[r.Option.Text] = r
	}
	for _, text := range []string{"Charlie", "Delta"} {
		r := byText[text]
		if r.Votes != 1 || r.Shown != 1 || r.ShownPercent != 100 {
			t.Errorf("%s = %+v, want votes 1, shown 1, interest (ShownPercent) 100 — "+
				"not votes 1 against shown %d", text, r, respondents)
		}
		if shown[text] != 1 {
			t.Errorf("shownByText[%s] = %d, want 1", text, shown[text])
		}
	}
	checkTallyInvariant(t, s, sv.ID)
}
