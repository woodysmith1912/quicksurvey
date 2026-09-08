package store

import "testing"

// votes returns the tally keyed by option text, which is what a reader of these
// tests actually wants to assert on.
func votes(t *testing.T, s *Store, id string) map[string]int {
	t.Helper()
	results, _ := s.Tally(id)
	out := map[string]int{}
	for _, r := range results {
		out[r.Option.Text] = r.Votes
	}
	return out
}

func TestWriteInIsInvisibleAndUncountedUntilApproved(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza", "Tacos")
	alice := s.VoterID(sv.ID, "alice-browser")
	bob := s.VoterID(sv.ID, "bob-browser")

	if _, err := s.SaveResponse(sv.ID, alice, []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	newOpt, err := s.AddWriteIn(sv.ID, alice, "Sushi")
	if err != nil {
		t.Fatalf("AddWriteIn: %v", err)
	}

	sv, _ = s.Survey(sv.ID)

	// Alice keeps her earlier choice and gains the pending one.
	got, _ := s.ResponseFor(sv.ID, alice)
	if !got.Chose(sv.Options[0].ID) {
		t.Error("proposing an option discarded the rest of the ballot")
	}
	if !got.Chose(newOpt) {
		t.Error("the submitter's vote for their own write-in was not recorded")
	}

	// Nobody else sees it yet, and it counts for nobody.
	for _, o := range sv.Ballot(nil) {
		if o.ID == newOpt {
			t.Error("a pending write-in appeared on everyone's ballot")
		}
	}
	if _, ok := votes(t, s, sv.ID)["Sushi"]; ok {
		t.Error("a pending write-in was counted in the tally")
	}

	// Bob cannot vote for it by guessing its ID.
	if _, err := s.SaveResponse(sv.ID, bob, []string{newOpt}, ""); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.ResponseFor(sv.ID, bob); r.Chose(newOpt) {
		t.Error("someone else was able to vote for another person's pending write-in")
	}

	// Approval makes it count, without the submitter having to come back.
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, newOpt, OptApproved, "")
	}); err != nil {
		t.Fatal(err)
	}
	if n := votes(t, s, sv.ID)["Sushi"]; n != 1 {
		t.Errorf("Sushi = %d votes after approval, want 1", n)
	}
}

func TestRejectedWriteInNeverCounts(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza")
	alice := s.VoterID(sv.ID, "alice")
	opt, err := s.AddWriteIn(sv.ID, alice, "Gruel")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, opt, OptRejected, "")
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := votes(t, s, sv.ID)["Gruel"]; ok {
		t.Error("a rejected write-in appears in the tally")
	}
}

func TestMergeTransfersVotesWithoutDoubleCounting(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza", "Tacos")
	pizza := sv.Options[0].ID

	// One person suggests a duplicate and also already voted for the original.
	both := s.VoterID(sv.ID, "both")
	if _, err := s.SaveResponse(sv.ID, both, []string{pizza}, ""); err != nil {
		t.Fatal(err)
	}
	dupe, err := s.AddWriteIn(sv.ID, both, "pizza!!")
	if err != nil {
		t.Fatal(err)
	}
	// A second person votes only for the duplicate.
	only := s.VoterID(sv.ID, "only")
	if _, err := s.SaveResponse(sv.ID, only, nil, ""); err != nil {
		t.Fatal(err)
	}
	dupe2, err := s.AddWriteIn(sv.ID, only, "PIZZA")
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{dupe, dupe2} {
		if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
			return SetOptionStatus(d, id, OptMerged, pizza)
		}); err != nil {
			t.Fatalf("merge: %v", err)
		}
	}

	v := votes(t, s, sv.ID)
	if v["Pizza"] != 2 {
		t.Errorf("Pizza = %d, want 2 — the person who voted for both must count once", v["Pizza"])
	}
	if _, ok := v["pizza!!"]; ok {
		t.Error("a merged option still appears in the tally")
	}
}

func TestMergeChainResolves(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza")
	pizza := sv.Options[0].ID
	a, _ := s.AddWriteIn(sv.ID, s.VoterID(sv.ID, "a"), "Peetza")
	b, _ := s.AddWriteIn(sv.ID, s.VoterID(sv.ID, "b"), "Pitsa")

	// b -> a -> pizza
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		if err := SetOptionStatus(d, a, OptApproved, ""); err != nil {
			return err
		}
		return SetOptionStatus(d, b, OptMerged, a)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, a, OptMerged, pizza)
	}); err != nil {
		t.Fatal(err)
	}
	if n := votes(t, s, sv.ID)["Pizza"]; n != 2 {
		t.Errorf("Pizza = %d, want 2 — both merged options should resolve through the chain", n)
	}
}

func TestMergeIntoSelfOrMergedIsRefused(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza")
	a, _ := s.AddWriteIn(sv.ID, s.VoterID(sv.ID, "a"), "Peetza")
	sv, _ = s.Survey(sv.ID)

	if err := SetOptionStatus(sv, a, OptMerged, a); err == nil {
		t.Error("merging an option into itself should be refused")
	}
	if err := SetOptionStatus(sv, a, OptMerged, "nope"); err == nil {
		t.Error("merging into a nonexistent option should be refused")
	}
	if err := SetOptionStatus(sv, sv.Options[0].ID, OptMerged, a); err != nil {
		t.Fatal(err)
	}
	b, _ := AddOption(sv, "Third", OptApproved, "editor")
	if err := SetOptionStatus(sv, b, OptMerged, sv.Options[0].ID); err == nil {
		t.Error("merging into an already-merged option should be refused, to keep chains shallow")
	}
}

func TestWriteInsAreCappedPerVoter(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza")
	v := s.VoterID(sv.ID, "spammer")
	for i := range maxWriteInsPerVoter {
		if _, err := s.AddWriteIn(sv.ID, v, string(rune('a'+i))); err != nil {
			t.Fatalf("write-in %d: %v", i, err)
		}
	}
	if _, err := s.AddWriteIn(sv.ID, v, "one too many"); err == nil {
		t.Error("the per-voter pending write-in cap was not enforced")
	}
}

func TestWriteInRefusedWhenDisabledOrClosed(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza")
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error { d.AllowWriteIn = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddWriteIn(sv.ID, s.VoterID(sv.ID, "a"), "Sushi"); err == nil {
		t.Error("write-ins should be refused when the survey disables them")
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		d.AllowWriteIn, d.State = true, StateClosed
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddWriteIn(sv.ID, s.VoterID(sv.ID, "a"), "Sushi"); err == nil {
		t.Error("write-ins should be refused on a closed survey")
	}
}

func TestRenamingAnOptionKeepsItsVotes(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza")
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "a"), []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionText(d, sv.Options[0].ID, "Pizza (any kind)")
	}); err != nil {
		t.Fatal(err)
	}
	if n := votes(t, s, sv.ID)["Pizza (any kind)"]; n != 1 {
		t.Errorf("renamed option = %d votes, want 1", n)
	}
}
