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

// shownByText returns Result.Shown keyed by option text.
func shownByText(t *testing.T, s *Store, surveyID string) map[string]int {
	t.Helper()
	results, _ := s.Tally(surveyID)
	out := map[string]int{}
	for _, r := range results {
		out[r.Option.Text] = r.Shown
	}
	return out
}

// checkTallyInvariant insists that for every option, votes <= shown <=
// respondents. A respondent can only pick what was on their ballot, and every
// seen row belongs to a respondent.
func checkTallyInvariant(t *testing.T, s *Store, surveyID string) {
	t.Helper()
	results, respondents := s.Tally(surveyID)
	for _, r := range results {
		if r.Votes > r.Shown || r.Shown > respondents {
			t.Errorf("%s: votes %d, shown %d, respondents %d — invariant broken",
				r.Option.Text, r.Votes, r.Shown, respondents)
		}
	}
}

func TestInterestIsTheShareOfThoseWhoWereShownAnOption(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha")
	a, b := s.VoterID(sv.ID, "a"), s.VoterID(sv.ID, "b")
	if _, err := s.SaveResponse(sv.ID, a, []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveResponse(sv.ID, b, nil, ""); err != nil {
		t.Fatal(err)
	}
	// a suggests Charlie, which is approved. b never comes back.
	charlie, err := s.AddWriteIn(sv.ID, a, "Charlie")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, charlie, OptApproved, "")
	}); err != nil {
		t.Fatal(err)
	}

	results, respondents := s.Tally(sv.ID)
	if respondents != 2 {
		t.Fatalf("respondents = %d, want 2", respondents)
	}
	byText := map[string]Result{}
	for _, r := range results {
		byText[r.Option.Text] = r
	}
	alpha, ch := byText["Alpha"], byText["Charlie"]
	if alpha.Shown != 2 || alpha.Votes != 1 || alpha.Percent != 50 || alpha.ShownPercent != 50 {
		t.Errorf("Alpha = %+v, want shown 2, votes 1, both shares 50", alpha)
	}
	if ch.Shown != 1 || ch.Votes != 1 || ch.Percent != 50 || ch.ShownPercent != 100 {
		t.Errorf("Charlie = %+v, want shown 1, votes 1, share 50 but interest 100", ch)
	}
	checkTallyInvariant(t, s, sv.ID)
}

func TestShownFollowsMergesWithoutDoubleCounting(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Pizza")
	pizza := sv.Options[0].ID
	both := s.VoterID(sv.ID, "both")
	if _, err := s.SaveResponse(sv.ID, both, []string{pizza}, ""); err != nil {
		t.Fatal(err)
	}
	dupe, err := s.AddWriteIn(sv.ID, both, "pizza!!")
	if err != nil {
		t.Fatal(err)
	}
	only := s.VoterID(sv.ID, "only")
	dupe2, err := s.AddWriteIn(sv.ID, only, "PIZZA")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{dupe, dupe2} {
		if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
			return SetOptionStatus(d, id, OptMerged, pizza)
		}); err != nil {
			t.Fatal(err)
		}
	}
	// "both" saw Pizza and its duplicate; "only" saw Pizza and theirs. Two
	// people, not four exposures.
	if got := shownByText(t, s, sv.ID)["Pizza"]; got != 2 {
		t.Errorf("Pizza shown = %d, want 2 — a person who saw an option and its duplicate counts once", got)
	}
	checkTallyInvariant(t, s, sv.ID)
}

func TestRemovingAndRestoringAnOptionKeepsItsShownCount(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha", "Bravo")
	bravo := sv.Options[1].ID
	for _, who := range []string{"a", "b"} {
		if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, who), []string{bravo}, ""); err != nil {
			t.Fatal(err)
		}
	}
	setStatus := func(status string) {
		t.Helper()
		if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
			return SetOptionStatus(d, bravo, status, "")
		}); err != nil {
			t.Fatal(err)
		}
	}
	setStatus(OptRemoved)
	// One person resubmits while it is off the ballot.
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "a"), []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	setStatus(OptApproved)
	results, _ := s.Tally(sv.ID)
	for _, r := range results {
		if r.Option.ID == bravo && (r.Shown != 2 || r.Votes != 2) {
			t.Errorf("restored Bravo: shown %d votes %d, want 2 and 2", r.Shown, r.Votes)
		}
	}
	checkTallyInvariant(t, s, sv.ID)
}

func TestCommentsReturnsOnlyResponsesWithACommentOldestFirst(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha", "Bravo")
	alpha := sv.Options[0].ID
	// No comment.
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "silent"), []string{alpha}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "first"), []string{alpha}, "first comment"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "second"), []string{alpha}, "second comment"); err != nil {
		t.Fatal(err)
	}

	got := s.Comments(sv.ID)
	if len(got) != 2 {
		t.Fatalf("Comments = %d responses, want 2 (the silent one excluded)", len(got))
	}
	if got[0].Comment != "first comment" || got[1].Comment != "second comment" {
		t.Errorf("Comments = %q, %q, want oldest first: %q, %q",
			got[0].Comment, got[1].Comment, "first comment", "second comment")
	}
}
