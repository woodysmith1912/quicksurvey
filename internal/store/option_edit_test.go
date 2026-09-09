package store

import "testing"

// Editing a survey after people have answered it is where a tally can quietly
// go wrong: the response log references options by ID, and the ballot shows
// only some of them. These tests pin down what happens to a vote when an editor
// renames, removes, restores or merges the option it was cast for.

func tallyByText(t *testing.T, s *Store, surveyID string) map[string]int {
	t.Helper()
	results, _ := s.Tally(surveyID)
	out := map[string]int{}
	for _, r := range results {
		out[r.Option.Text] = r.Votes
	}
	return out
}

func TestRenamingAnOptionCarriesItsVotes(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha", "Bravo")
	for _, who := range []string{"a", "b"} {
		if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, who), []string{sv.Options[0].ID}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionText(d, sv.Options[0].ID, "Alpha, clarified")
	}); err != nil {
		t.Fatal(err)
	}
	got := tallyByText(t, s, sv.ID)
	if got["Alpha, clarified"] != 2 {
		t.Errorf("after renaming: %v, want the two votes to follow the option", got)
	}
	if _, stillOld := got["Alpha"]; stillOld {
		t.Error("the old text is still in the tally")
	}
}

func TestRemovingAnOptionHidesItsVotesWithoutDestroyingThem(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha", "Bravo")
	bravo := sv.Options[1].ID
	for _, who := range []string{"a", "b"} {
		if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, who),
			[]string{sv.Options[0].ID, bravo}, ""); err != nil {
			t.Fatal(err)
		}
	}
	remove := func(status string) {
		t.Helper()
		if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
			return SetOptionStatus(d, bravo, status, "")
		}); err != nil {
			t.Fatal(err)
		}
	}

	remove(OptRemoved)
	if got := tallyByText(t, s, sv.ID); got["Bravo"] != 0 {
		if _, present := got["Bravo"]; present {
			t.Errorf("a removed option is still counted: %v", got)
		}
	}
	// The responses still reference it, which is what makes the removal
	// reversible rather than destructive.
	r, _ := s.ResponseFor(sv.ID, s.VoterID(sv.ID, "a"))
	if !r.Chose(bravo) {
		t.Fatal("removing an option deleted the vote for it")
	}
	// An editor who changes their mind gets the votes back.
	remove(OptApproved)
	if got := tallyByText(t, s, sv.ID); got["Bravo"] != 2 {
		t.Errorf("after restoring: %v, want both votes back", got)
	}
}

// The bug this exists to prevent: a respondent who resubmits while an option is
// hidden used to destroy their vote for it, because the option is absent from
// the ballot and so absent from the form. Restoring the option then produced a
// quietly smaller count, with nothing to show what had happened.
func TestResubmittingDoesNotDropVotesTheRespondentCannotSee(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha", "Bravo", "Charlie")
	alpha, bravo, charlie := sv.Options[0].ID, sv.Options[1].ID, sv.Options[2].ID
	voter := s.VoterID(sv.ID, "a")

	if _, err := s.SaveResponse(sv.ID, voter, []string{alpha, bravo}, "first"); err != nil {
		t.Fatal(err)
	}
	// An editor removes Bravo. It vanishes from the ballot.
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, bravo, OptRemoved, "")
	}); err != nil {
		t.Fatal(err)
	}
	sv, _ = s.Survey(sv.ID)
	for _, o := range sv.Ballot(nil) {
		if o.ID == bravo {
			t.Fatal("a removed option is still on the ballot")
		}
	}

	// The respondent changes something unrelated — one more tick, a new
	// comment — and submits what their ballot showed. Bravo is not in it.
	if _, err := s.SaveResponse(sv.ID, voter, []string{alpha, charlie}, "second"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.ResponseFor(sv.ID, voter)
	if !after.Chose(bravo) {
		t.Error("resubmitting destroyed a vote for an option the respondent was never shown")
	}
	if !after.Chose(charlie) {
		t.Error("the new selection was not recorded")
	}
	if after.Comment != "second" {
		t.Errorf("comment = %q, want the new one", after.Comment)
	}

	// So restoring the option restores the true count.
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, bravo, OptApproved, "")
	}); err != nil {
		t.Fatal(err)
	}
	if got := tallyByText(t, s, sv.ID); got["Bravo"] != 1 {
		t.Errorf("after restoring: %v, want Bravo back at 1", got)
	}
}

// A merge moves votes through Resolve rather than by rewriting responses, so
// the merged option must survive a later resubmission or the transfer is undone.
func TestResubmittingDoesNotUndoAMerge(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza")
	pizza := sv.Options[0].ID
	voter := s.VoterID(sv.ID, "a")

	dupe, err := s.AddWriteIn(sv.ID, voter, "pizza!!")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, dupe, OptMerged, pizza)
	}); err != nil {
		t.Fatal(err)
	}
	if got := tallyByText(t, s, sv.ID); got["Pizza"] != 1 {
		t.Fatalf("after the merge: %v, want the vote moved to Pizza", got)
	}

	// The respondent comes back and submits an empty ballot — they had only
	// ever selected the write-in, which is no longer shown to them.
	if _, err := s.SaveResponse(sv.ID, voter, nil, ""); err != nil {
		t.Fatal(err)
	}
	if got := tallyByText(t, s, sv.ID); got["Pizza"] != 1 {
		t.Errorf("after resubmitting: %v, want the merged vote to survive", got)
	}

	// Deselecting something they *can* see still works, so this is not a
	// blanket refusal to remove selections.
	if _, err := s.SaveResponse(sv.ID, voter, []string{pizza}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveResponse(sv.ID, voter, nil, ""); err != nil {
		t.Fatal(err)
	}
	got := tallyByText(t, s, sv.ID)
	if got["Pizza"] != 1 {
		t.Errorf("after deselecting the visible option: %v — the merged vote should "+
			"still be there, and only that one", got)
	}
}

// Deselecting a visible option must still work; carrying hidden choices forward
// must not turn into "selections can never be removed".
func TestVisibleOptionsCanStillBeDeselected(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha", "Bravo")
	voter := s.VoterID(sv.ID, "a")
	if _, err := s.SaveResponse(sv.ID, voter, []string{sv.Options[0].ID, sv.Options[1].ID}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveResponse(sv.ID, voter, []string{sv.Options[1].ID}, ""); err != nil {
		t.Fatal(err)
	}
	got := tallyByText(t, s, sv.ID)
	if got["Alpha"] != 0 || got["Bravo"] != 1 {
		t.Errorf("tally = %v, want Alpha deselected and Bravo kept", got)
	}
}
