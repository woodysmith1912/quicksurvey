package store

import "testing"

// seenByText returns the option texts a respondent has been shown, which is
// what a reader of these tests wants to assert on.
func seenByText(t *testing.T, s *Store, surveyID, voter string) map[string]bool {
	t.Helper()
	sv, ok := s.Survey(surveyID)
	if !ok {
		t.Fatal("survey vanished")
	}
	r, ok := s.ResponseFor(surveyID, voter)
	if !ok {
		t.Fatalf("no response from %s", voter)
	}
	out := map[string]bool{}
	for _, id := range r.Seen {
		o, ok := sv.Option(id)
		if !ok {
			t.Fatalf("seen references unknown option %s", id)
		}
		out[o.Text] = true
	}
	return out
}

func TestSubmittingRecordsTheApprovedOptionsAsSeen(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha", "Bravo", "Charlie")
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, sv.Options[2].ID, OptRemoved, "")
	}); err != nil {
		t.Fatal(err)
	}
	v := s.VoterID(sv.ID, "a")
	if _, err := s.SaveResponse(sv.ID, v, []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	got := seenByText(t, s, sv.ID, v)
	if !got["Alpha"] || !got["Bravo"] || got["Charlie"] {
		t.Errorf("seen = %v, want Alpha and Bravo but not the removed Charlie", got)
	}
	r, _ := s.ResponseFor(sv.ID, v)
	if !r.Saw(sv.Options[1].ID) || r.Saw(sv.Options[2].ID) {
		t.Error("Saw disagrees with Seen")
	}

	// Like everything else, it survives a restart.
	s2 := reopen(t, s)
	defer s2.Close()
	if r, _ := s2.ResponseFor(sv.ID, v); len(r.Seen) != 2 {
		t.Errorf("after reopening, seen = %v, want 2 options", r.Seen)
	}
}

func TestSeenGrowsAcrossSubmissionsAndNeverShrinks(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha", "Bravo")
	alice := s.VoterID(sv.ID, "alice")
	bob := s.VoterID(sv.ID, "bob")
	if _, err := s.SaveResponse(sv.ID, alice, []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}

	// Bob suggests Charlie. He has seen it; nobody else has.
	charlie, err := s.AddWriteIn(sv.ID, bob, "Charlie")
	if err != nil {
		t.Fatal(err)
	}
	if got := seenByText(t, s, sv.ID, bob); !got["Charlie"] {
		t.Error("the proposer of a write-in has been shown it")
	}
	if got := seenByText(t, s, sv.ID, alice); got["Charlie"] {
		t.Error("a pending write-in was recorded as shown to someone else")
	}

	// Approval alone changes nothing for Alice; coming back does.
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, charlie, OptApproved, "")
	}); err != nil {
		t.Fatal(err)
	}
	if got := seenByText(t, s, sv.ID, alice); got["Charlie"] {
		t.Error("approval marked an option as shown to someone who has not been back")
	}
	if _, err := s.SaveResponse(sv.ID, alice, []string{sv.Options[1].ID}, ""); err != nil {
		t.Fatal(err)
	}
	if got := seenByText(t, s, sv.ID, alice); !got["Charlie"] || !got["Alpha"] || !got["Bravo"] {
		t.Errorf("after resubmitting with Charlie on the ballot: %v", got)
	}

	// Bravo is removed and Alice resubmits. Once shown, always shown.
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, sv.Options[1].ID, OptRemoved, "")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveResponse(sv.ID, alice, []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	if got := seenByText(t, s, sv.ID, alice); !got["Bravo"] {
		t.Error("a removed option left the seen set on resubmission")
	}

	// A choice is always a subset of what was seen.
	for _, r := range s.Responses(sv.ID) {
		for _, id := range r.Choices {
			if !r.Saw(id) {
				t.Errorf("response %s chose %s without having seen it", r.ID, id)
			}
		}
	}
}
