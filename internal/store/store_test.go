package store

import (
	"os"
	"path/filepath"
	"strings"
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
	if err := os.Remove(filepath.Join(s.Dir(), "secret.key")); err != nil {
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

	if _, err := s.SaveResponse(sv.ID, v, []string{sv.Options[0].ID}, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveResponse(sv.ID, v, []string{sv.Options[1].ID, sv.Options[2].ID}, "second"); err != nil {
		t.Fatal(err)
	}
	if n := s.Count(sv.ID); n != 1 {
		t.Fatalf("respondents = %d, want 1 — a second submission must replace the first", n)
	}
	got, _ := s.ResponseFor(sv.ID, v)
	if len(got.Choices) != 2 || !got.Chose(sv.Options[1].ID) || got.Chose(sv.Options[0].ID) {
		t.Errorf("choices = %v, want the second submission's", got.Choices)
	}
	if got.Comment != "second" {
		t.Errorf("comment = %q, want %q", got.Comment, "second")
	}

	// The log keeps both records; the replay picks the last.
	raw, err := os.ReadFile(filepath.Join(s.Dir(), "surveys", sv.ID, "responses.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(strings.TrimSpace(string(raw)), "\n") + 1; n != 2 {
		t.Errorf("log has %d lines, want 2 — the log is append-only", n)
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
	if _, err := os.Stat(filepath.Join(s.Dir(), "surveys", sv.ID)); !os.IsNotExist(err) {
		t.Error("survey directory still on disk after delete")
	}
	if _, ok := reopen(t, s).Survey(sv.ID); ok {
		t.Error("deleted survey came back after a restart")
	}
}

func TestTornFinalLineIsSkippedNotFatal(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza")
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "b1"), []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.Dir(), "surveys", sv.ID, "responses.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"id":"broken","voter":"b2","cho`) // simulates a kill mid-write
	f.Close()

	s2 := reopen(t, s) // must not return an error
	if n := s2.Count(sv.ID); n != 1 {
		t.Errorf("respondents = %d, want 1 — the torn line should be skipped and the rest kept", n)
	}
}
