package store

import "testing"

// Tests for orders of operations rather than for individual functions.
//
// Every one of these exercises code that was already covered; what was not
// covered was the sequence. Coverage cannot find these, because nothing here is
// an unexecuted line — it is an unconsidered interleaving of two features that
// are each correct alone.

// A merge points votes at a target through Resolve. Removing that target has to
// hide them, and restoring it has to bring them back, or a moderator tidying up
// silently changes a count.
func TestMergedVotesFollowTheirTargetBeingRemovedAndRestored(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza")
	pizza := sv.Options[0].ID
	dupe, err := s.AddWriteIn(sv.ID, s.VoterID(sv.ID, "a"), "pizza!!")
	if err != nil {
		t.Fatal(err)
	}
	set := func(id, status, into string) {
		t.Helper()
		if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
			return SetOptionStatus(d, id, status, into)
		}); err != nil {
			t.Fatal(err)
		}
	}
	set(dupe, OptMerged, pizza)
	if got := tallyByText(t, s, sv.ID)["Pizza"]; got != 1 {
		t.Fatalf("after the merge Pizza = %d, want 1", got)
	}
	set(pizza, OptRemoved, "")
	if _, present := tallyByText(t, s, sv.ID)["Pizza"]; present {
		t.Error("a removed merge target is still counted")
	}
	set(pizza, OptApproved, "")
	if got := tallyByText(t, s, sv.ID)["Pizza"]; got != 1 {
		t.Errorf("after restoring the target Pizza = %d, want the merged vote back", got)
	}
}

// Publishing a draft discards its preview responses. Sending a live survey back
// to draft and republishing it must not, or an editor tidying up destroys real
// answers.
func TestSendingALiveSurveyBackToDraftDoesNotDiscardRealResponses(t *testing.T) {
	s := newStore(t)
	sv, err := s.CreateSurvey("Real", "", []string{"Alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(sv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "real"), []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error { d.State = StateDraft; return nil }); err != nil {
		t.Fatal(err)
	}
	discarded, err := s.Publish(sv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if discarded != 0 || s.Count(sv.ID) != 1 {
		t.Errorf("republishing discarded %d responses leaving %d — real answers were destroyed",
			discarded, s.Count(sv.ID))
	}
}

// Publish decides whether to discard by asking whether the survey has ever been
// open. That is only safe if every route to the open state records it — not
// just Publish itself.
func TestOpeningASurveyByAnyRouteRecordsThatItWasOpened(t *testing.T) {
	s := newStore(t)
	sv, err := s.CreateSurvey("Sideways", "", []string{"Alpha"})
	if err != nil {
		t.Fatal(err)
	}
	// Opened without going through Publish, as a different code path or an
	// older version might.
	opened, err := s.UpdateSurvey(sv.ID, func(d *Survey) error { d.State = StateOpen; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if opened.FirstOpenedAt.IsZero() {
		t.Fatal("a survey reached the open state without recording that it had")
	}
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "real"), []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	discarded, err := s.Publish(sv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if discarded != 0 || s.Count(sv.ID) != 1 {
		t.Errorf("publishing discarded %d responses leaving %d from a survey that was "+
			"already open — the guard was bypassable", discarded, s.Count(sv.ID))
	}
}

// A moderator working through the queue after a survey has closed is ordinary;
// approving then has to count, or late moderation quietly loses answers.
func TestApprovingAWriteInAfterTheSurveyClosesStillCounts(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha")
	opt, err := s.AddWriteIn(sv.ID, s.VoterID(sv.ID, "a"), "Late idea")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error { d.State = StateClosed; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, opt, OptApproved, "")
	}); err != nil {
		t.Fatalf("moderating a closed survey was refused: %v", err)
	}
	if got := tallyByText(t, s, sv.ID)["Late idea"]; got != 1 {
		t.Errorf("Late idea = %d after approval on a closed survey, want 1", got)
	}
}

// A reset link is a credential. Once the password has been set by any other
// route the link's reason for existing is gone, and leaving it live means a
// second person can still choose the password.
func TestChangingAPasswordRetiresAnOutstandingResetLink(t *testing.T) {
	s := newStore(t)
	if _, err := s.AddUser("root", RoleAdmin, "password123"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddUser("bob", RoleEditor, "password123"); err != nil {
		t.Fatal(err)
	}
	_, token, err := s.CreateReset("bob", "root")
	if err != nil {
		t.Fatal(err)
	}
	// Bob changes it himself, or an admin uses the CLI.
	if err := s.SetPassword("bob", "chosen another way"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ResetByToken(token); ok {
		t.Error("the reset link is still live after the password was changed elsewhere")
	}
	if _, err := s.UseReset(token, "set by the stale link"); err == nil {
		t.Fatal("a stale reset link set a password")
	}
	if _, ok := s.Authenticate("bob", "chosen another way"); !ok {
		t.Error("the password set by the other route was overwritten")
	}
}

// A reset link outliving its account must fail clearly rather than half-apply.
func TestResetLinkForADeletedAccountIsRefused(t *testing.T) {
	s := newStore(t)
	if _, err := s.AddUser("root", RoleAdmin, "password123"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddUser("alice", RoleEditor, "password123"); err != nil {
		t.Fatal(err)
	}
	_, token, err := s.CreateReset("alice", "root")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseReset(token, "a new password"); err == nil {
		t.Error("a reset link for a deleted account was accepted")
	}
	if _, ok := s.User("alice"); ok {
		t.Error("using the link recreated the account")
	}
}

// Voting on a survey that has just been deleted must fail rather than write
// rows nothing owns.
func TestVotingOnADeletedSurveyIsRefused(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha")
	if err := s.DeleteSurvey(sv.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "a"), []string{sv.Options[0].ID}, ""); err == nil {
		t.Error("a vote was recorded against a deleted survey")
	}
	var orphans int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM responses WHERE survey_id = ?`, sv.ID).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("%d orphaned response rows", orphans)
	}
}
