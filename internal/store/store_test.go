package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// reopen closes over the same directory to prove that state survives a restart,
// which is the whole point of writing files at all.
func reopen(t *testing.T, s *Store) *Store {
	t.Helper()
	s2, err := Open(s.Dir())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	return s2
}

func TestUsersRoundTripAcrossRestart(t *testing.T) {
	s := newStore(t)
	if _, err := s.AddUser("alice", RoleEditor, "correct horse"); err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	if _, err := s.AddUser("alice", RoleViewer, "another one"); err == nil {
		t.Error("adding a duplicate username should fail")
	}
	if _, err := s.AddUser("bob", RoleViewer, "short"); err == nil {
		t.Error("an 8-character minimum should reject a 5-character password")
	}

	s2 := reopen(t, s)
	u, ok := s2.Authenticate("alice", "correct horse")
	if !ok {
		t.Fatal("alice should authenticate after a restart")
	}
	if u.Role != RoleEditor {
		t.Errorf("role = %q, want editor", u.Role)
	}
	if _, ok := s2.Authenticate("alice", "wrong"); ok {
		t.Error("the wrong password authenticated")
	}
	if _, ok := s2.Authenticate("nobody", "correct horse"); ok {
		t.Error("an unknown user authenticated")
	}
}

func TestPasswordChangeInvalidatesSessionKey(t *testing.T) {
	s := newStore(t)
	u, _ := s.AddUser("alice", RoleAdmin, "first password")
	before := u.SessionKey()
	if err := s.SetPassword("alice", "second password"); err != nil {
		t.Fatalf("SetPassword: %v", err)
	}
	after, _ := s.User("alice")
	if before == after.SessionKey() {
		t.Error("the session key must change with the password, or old cookies stay valid")
	}
	if _, ok := s.Authenticate("alice", "first password"); ok {
		t.Error("the old password still works")
	}
}

func TestCannotDeleteLastAdmin(t *testing.T) {
	s := newStore(t)
	if _, err := s.AddUser("root", RoleAdmin, "password123"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddUser("ed", RoleEditor, "password123"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser("root"); err == nil {
		t.Fatal("deleting the only admin should be refused")
	}
	if _, err := s.AddUser("root2", RoleAdmin, "password123"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser("root"); err != nil {
		t.Fatalf("with a second admin present, deletion should succeed: %v", err)
	}
}

func TestRoleOrdering(t *testing.T) {
	for _, c := range []struct {
		have, want Role
		ok         bool
	}{
		{RoleViewer, RoleViewer, true},
		{RoleViewer, RoleEditor, false},
		{RoleEditor, RoleViewer, true},
		{RoleEditor, RoleAdmin, false},
		{RoleAdmin, RoleEditor, true},
		{Role("bogus"), RoleViewer, false},
	} {
		if got := c.have.AtLeast(c.want); got != c.ok {
			t.Errorf("%s.AtLeast(%s) = %v, want %v", c.have, c.want, got, c.ok)
		}
	}
}

func TestVoterIDIsPerSurveyAndOpaque(t *testing.T) {
	s := newStore(t)
	const token = "a-browser-token"
	a := s.VoterID("survey-one", token)
	b := s.VoterID("survey-two", token)
	if a == b {
		t.Error("the same browser must produce different voter IDs on different surveys")
	}
	if a != s.VoterID("survey-one", token) {
		t.Error("voter ID must be stable for the same browser and survey")
	}
	if strings.Contains(a, token) {
		t.Error("the stored voter ID leaks the browser token")
	}
}

func TestVoterIDDoesNotSurviveSecretLoss(t *testing.T) {
	s := newStore(t)
	before := s.VoterID("s1", "tok")

	// Losing the instance key is the documented way voter identities break.
	if _, err := s.db.Exec(`DELETE FROM meta WHERE key = 'secret'`); err != nil {
		t.Fatal(err)
	}
	s2 := reopen(t, s)
	if before == s2.VoterID("s1", "tok") {
		t.Error("a regenerated secret should produce different voter IDs (documented in DESIGN.md)")
	}
}

func mustSurvey(t *testing.T, s *Store, opts ...string) *Survey {
	t.Helper()
	sv, err := s.CreateSurvey("Lunch options", "Pick what you'd eat", opts)
	if err != nil {
		t.Fatalf("CreateSurvey: %v", err)
	}
	sv, err = s.UpdateSurvey(sv.ID, func(d *Survey) error { d.State = StateOpen; return nil })
	if err != nil {
		t.Fatalf("open survey: %v", err)
	}
	return sv
}

func TestCreateSurveyRejectsEmptyTitleAndBlankOptions(t *testing.T) {
	s := newStore(t)
	if _, err := s.CreateSurvey("   ", "", []string{"a"}); err == nil {
		t.Error("a survey with no title should be refused")
	}
	sv, err := s.CreateSurvey("T", "", []string{"a", "", "  ", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sv.Options) != 2 {
		t.Fatalf("got %d options, want 2 — blank lines should be dropped", len(sv.Options))
	}
	if sv.State != StateDraft {
		t.Errorf("new survey state = %q, want draft", sv.State)
	}
}

func TestResponseReplacesRatherThanAccumulates(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza", "Tacos", "Salad")
	v := s.VoterID(sv.ID, "browser-1")

	first, err := s.SaveResponse(sv.ID, v, []string{sv.Options[0].ID}, "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SaveResponse(sv.ID, v, []string{sv.Options[1].ID, sv.Options[2].ID}, "second")
	if err != nil {
		t.Fatal(err)
	}
	if n := s.Count(sv.ID); n != 1 {
		t.Fatalf("respondents = %d, want 1 — a second submission must replace the first", n)
	}
	// The same person keeps the same response identity across an edit.
	if second.ID != first.ID {
		t.Errorf("response ID changed on update: %s -> %s", first.ID, second.ID)
	}
	if !second.Created.Equal(first.Created) || !second.Updated.After(first.Updated) &&
		second.Updated.Equal(first.Updated) == false {
		t.Errorf("created should be preserved and updated should move: %v -> %v", first, second)
	}

	got, _ := s.ResponseFor(sv.ID, v)
	if len(got.Choices) != 2 || !got.Chose(sv.Options[1].ID) || got.Chose(sv.Options[0].ID) {
		t.Errorf("choices = %v, want the second submission's", got.Choices)
	}
	if got.Comment != "second" {
		t.Errorf("comment = %q, want %q", got.Comment, "second")
	}

	// Superseded answers are not retained: exactly one row, and its choices
	// are only the current ones.
	var choices int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM choices c JOIN responses r ON r.id = c.response_id
		 WHERE r.survey_id = ?`, sv.ID).Scan(&choices); err != nil {
		t.Fatal(err)
	}
	if choices != 2 {
		t.Errorf("stored choices = %d, want 2 — the replaced answer should leave nothing behind", choices)
	}
	if n := reopen(t, s).Count(sv.ID); n != 1 {
		t.Errorf("after restart respondents = %d, want 1", n)
	}
}

func TestDifferentVotersCountSeparately(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza", "Tacos")
	for _, browser := range []string{"b1", "b2", "b3"} {
		if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, browser), []string{sv.Options[0].ID}, ""); err != nil {
			t.Fatal(err)
		}
	}
	results, voters := s.Tally(sv.ID)
	if voters != 3 {
		t.Fatalf("respondents = %d, want 3", voters)
	}
	if results[0].Votes != 3 || results[0].Percent != 100 {
		t.Errorf("top result = %d votes / %.0f%%, want 3 / 100%%", results[0].Votes, results[0].Percent)
	}
}

func TestSaveResponseIgnoresUnknownAndClosedOptions(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza", "Tacos")
	removed := sv.Options[1].ID
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, removed, OptRemoved, "")
	}); err != nil {
		t.Fatal(err)
	}
	v := s.VoterID(sv.ID, "b1")
	r, err := s.SaveResponse(sv.ID, v, []string{sv.Options[0].ID, removed, "not-an-option", sv.Options[0].ID}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Choices) != 1 || r.Choices[0] != sv.Options[0].ID {
		t.Errorf("choices = %v, want only the one live option (no duplicates, no removed, no unknown)", r.Choices)
	}
}

func TestClosedSurveyRejectsResponses(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza")
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error { d.State = StateClosed; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "b1"), []string{sv.Options[0].ID}, ""); err == nil {
		t.Error("a closed survey should refuse responses")
	}
}

func TestScheduledCloseStopsAcceptingWithoutStateChange(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza")
	past := time.Now().Add(-time.Minute).UTC()
	sv, err := s.UpdateSurvey(sv.ID, func(d *Survey) error { d.CloseAt = past; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if sv.Accepting(time.Now()) {
		t.Error("a survey past its close time should not accept responses")
	}
	if !sv.ClosedByClock(time.Now()) {
		t.Error("ClosedByClock should report the difference between state and clock")
	}
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "b1"), []string{sv.Options[0].ID}, ""); err == nil {
		t.Error("SaveResponse should honour the scheduled close")
	}
}

func TestCommentIsTruncatedNotRejected(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza")
	r, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "b1"), nil, strings.Repeat("x", MaxComment+500))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Comment) != MaxComment {
		t.Errorf("comment length = %d, want %d", len(r.Comment), MaxComment)
	}
}

func TestCommentDroppedWhenSurveyDisallowsIt(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza")
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error { d.AllowComment = false; return nil }); err != nil {
		t.Fatal(err)
	}
	r, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "b1"), nil, "should not be kept")
	if err != nil {
		t.Fatal(err)
	}
	if r.Comment != "" {
		t.Errorf("comment = %q, want empty", r.Comment)
	}
}

func TestDeleteSurveyRemovesResponses(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza")
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "b1"), []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSurvey(sv.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Survey(sv.ID); ok {
		t.Error("survey still present after delete")
	}
	// Options and responses must go with it, via ON DELETE CASCADE, rather
	// than being orphaned rows that a later survey ID could collide with.
	for _, table := range []string{"options", "responses"} {
		var n int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM `+table+` WHERE survey_id = ?`, sv.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d rows left in %s after deleting the survey", n, table)
		}
	}
	var orphans int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM choices c LEFT JOIN responses r ON r.id = c.response_id
		 WHERE r.id IS NULL`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("%d orphaned choice rows after deleting the survey", orphans)
	}
	if _, ok := reopen(t, s).Survey(sv.ID); ok {
		t.Error("deleted survey came back after a restart")
	}
}

// Two processes against one data directory is what Kubernetes will eventually
// do to you: a rollout, or someone scaling to two replicas. Against the old
// file-based store their writes interleaved and corrupted each other silently.
// SQLite serialises them, so both succeed and nothing is lost.
func TestTwoStoresOnOneDatabaseBothWriteSafely(t *testing.T) {
	a := newStore(t)
	sv := mustSurvey(t, a, "Pizza", "Tacos")
	b := reopen(t, a) // a second process, same directory

	const each = 25
	var wg sync.WaitGroup
	errs := make(chan error, 2*each)
	for i := range each {
		wg.Add(2)
		go func() {
			defer wg.Done()
			v := a.VoterID(sv.ID, fmt.Sprintf("a-%d", i))
			if _, err := a.SaveResponse(sv.ID, v, []string{sv.Options[0].ID}, "from a"); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			v := b.VoterID(sv.ID, fmt.Sprintf("b-%d", i))
			if _, err := b.SaveResponse(sv.ID, v, []string{sv.Options[1].ID}, "from b"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent write failed: %v", err)
	}

	if n := a.Count(sv.ID); n != 2*each {
		t.Errorf("respondents = %d, want %d — writes were lost", n, 2*each)
	}
	results, voters := b.Tally(sv.ID)
	if voters != 2*each {
		t.Errorf("tally sees %d respondents, want %d", voters, 2*each)
	}
	for _, r := range results {
		if r.Votes != each {
			t.Errorf("%s = %d votes, want %d", r.Option.Text, r.Votes, each)
		}
	}
	// And the database is intact for a third reader.
	if n := reopen(t, a).Count(sv.ID); n != 2*each {
		t.Errorf("after reopening, respondents = %d, want %d", n, 2*each)
	}
}

func TestBackupProducesAUsableDatabase(t *testing.T) {
	s := newStore(t)
	if _, err := s.AddUser("alice", RoleAdmin, "password123"); err != nil {
		t.Fatal(err)
	}
	sv := mustSurvey(t, s, "Pizza", "Tacos")
	for _, who := range []string{"a", "b", "c"} {
		if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, who), []string{sv.Options[0].ID}, "hi"); err != nil {
			t.Fatal(err)
		}
	}

	dst := filepath.Join(t.TempDir(), "backup", dbFile)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	// Taken while the store is open, which is the whole point.
	if err := s.BackupTo(dst); err != nil {
		t.Fatalf("BackupTo: %v", err)
	}
	if err := s.BackupTo(dst); err == nil {
		t.Error("backing up over an existing file should be refused")
	}

	// The backup opens as an ordinary data directory with everything in it.
	restored, err := Open(filepath.Dir(dst))
	if err != nil {
		t.Fatalf("opening the backup: %v", err)
	}
	defer restored.Close()

	if _, ok := restored.Authenticate("alice", "password123"); !ok {
		t.Error("the account did not survive the backup")
	}
	if n := restored.Count(sv.ID); n != 3 {
		t.Errorf("responses in the backup = %d, want 3", n)
	}
	results, voters := restored.Tally(sv.ID)
	if voters != 3 || results[0].Votes != 3 {
		t.Errorf("tally in the backup = %+v (%d voters)", results, voters)
	}
	// And the instance key came too, so voter identities still resolve.
	if restored.VoterID(sv.ID, "a") != s.VoterID(sv.ID, "a") {
		t.Error("the instance key did not survive the backup; returning respondents would look new")
	}
}

// The form's maxlength is a suggestion to a browser. An anonymous write-in
// arrives straight from a POST body.
func TestOptionTextIsCappedServerSide(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza")
	huge := strings.Repeat("x", MaxOptionText+1)

	if _, err := s.AddWriteIn(sv.ID, s.VoterID(sv.ID, "spammer"), huge); err == nil {
		t.Error("a write-in longer than the cap was accepted")
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		_, err := AddOption(d, huge, OptApproved, "editor")
		return err
	}); err == nil {
		t.Error("an editor option longer than the cap was accepted")
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionText(d, sv.Options[0].ID, huge)
	}); err == nil {
		t.Error("renaming an option past the cap was accepted")
	}
	// Nothing oversized reached the database.
	got, _ := s.Survey(sv.ID)
	for _, o := range got.Options {
		if len(o.Text) > MaxOptionText {
			t.Errorf("stored option of %d characters", len(o.Text))
		}
	}
	// Exactly at the cap is still fine.
	if _, err := s.AddWriteIn(sv.ID, s.VoterID(sv.ID, "ok"), strings.Repeat("y", MaxOptionText)); err != nil {
		t.Errorf("a write-in exactly at the cap was rejected: %v", err)
	}
}

func TestSignOutRevokesEverySession(t *testing.T) {
	s := newStore(t)
	u, err := s.AddUser("alice", RoleAdmin, "password123")
	if err != nil {
		t.Fatal(err)
	}
	before := u.SessionKey()
	if err := s.RevokeSessions("alice"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.User("alice")
	if after.SessionKey() == before {
		t.Error("the session key did not move, so existing cookies stay valid")
	}
	// The password still works — revoking sessions is not locking the account.
	if _, ok := s.Authenticate("alice", "password123"); !ok {
		t.Error("revoking sessions broke the password")
	}
	if reopen(t, s).SessionsFromOf(t, "alice").IsZero() {
		t.Error("the revocation did not survive a restart")
	}
}

// SessionsFromOf is a test helper.
func (s *Store) SessionsFromOf(t *testing.T, name string) time.Time {
	t.Helper()
	u, ok := s.User(name)
	if !ok {
		t.Fatalf("no user %q", name)
	}
	return u.SessionsFrom
}

func TestLastAdminCannotBeDemoted(t *testing.T) {
	s := newStore(t)
	if _, err := s.AddUser("root", RoleAdmin, "password123"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddUser("ed", RoleEditor, "password123"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRole("root", RoleViewer); err == nil {
		t.Fatal("demoting the only admin should be refused — nobody could administer the instance")
	}
	if _, err := s.AddUser("root2", RoleAdmin, "password123"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRole("root", RoleViewer); err != nil {
		t.Errorf("with a second admin present, demotion should work: %v", err)
	}
}

func TestUsernameValidation(t *testing.T) {
	s := newStore(t)
	// "|" is the separator in a session cookie: such an account could be
	// created and could then never sign in.
	for _, bad := range []string{"", " ", "has|pipe", "has space", "-leading", ".dot",
		"emoji😀", strings.Repeat("x", 65), "with/slash", "with\x00null"} {
		if _, err := s.AddUser(bad, RoleViewer, "password123"); err == nil {
			t.Errorf("username %q was accepted", bad)
		}
	}
	for _, good := range []string{"alice", "a", "A1", "first.last", "with-dash", "with_underscore"} {
		if _, err := s.AddUser(good, RoleViewer, "password123"); err != nil {
			t.Errorf("username %q was rejected: %v", good, err)
		}
	}
}

// options.id is a primary key across every survey, and anonymous write-ins
// mint them, so a collision used to rewrite another survey's option.
func TestOptionIDsAreWideAndScopedToTheirSurvey(t *testing.T) {
	s := newStore(t)
	a := mustSurvey(t, s, "Alpha")
	b := mustSurvey(t, s, "Beta")
	if len(a.Options[0].ID) < 12 {
		t.Errorf("option ID is %d characters; too narrow for a global primary key", len(a.Options[0].ID))
	}
	// Force the collision the width is meant to make improbable, and check it
	// cannot reach across surveys.
	if _, err := s.db.Exec(
		`INSERT INTO options (id, survey_id, position, text, status, source, merged_into, created)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET text=excluded.text
		 WHERE options.survey_id = excluded.survey_id`,
		a.Options[0].ID, b.ID, 99, "hijacked", OptApproved, "editor", "", dbTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Survey(a.ID)
	if got.Options[0].Text != "Alpha" {
		t.Errorf("another survey's write overwrote this option: %q", got.Options[0].Text)
	}
}
