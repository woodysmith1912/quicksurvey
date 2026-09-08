package store

import (
	"encoding/json"
	"strings"
	"testing"
)

func jsonUnmarshalSurvey(doc string, sv *Survey) error { return json.Unmarshal([]byte(doc), sv) }

func optionTexts(opts []Option) []string {
	out := make([]string, len(opts))
	for i, o := range opts {
		out[i] = o.Text
	}
	return out
}

func TestRandomizeIsOnByDefault(t *testing.T) {
	s := newStore(t)
	sv, err := s.CreateSurvey("T", "", []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if !sv.Randomize() {
		t.Error("a new survey should randomise by default")
	}
	// A survey stored before the field existed has no "no_randomize" key, so
	// the zero value must mean "shuffle" rather than "do not".
	var old Survey
	if err := jsonUnmarshalSurvey(`{"id":"x","options":[],"state":"open"}`, &old); err != nil {
		t.Fatal(err)
	}
	if !old.Randomize() {
		t.Error("a survey stored before this feature must default to randomising")
	}
}

func TestShuffleIsStablePerRespondentAndDiffersBetweenThem(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot", "Golf", "Hotel")

	alice := s.VoterID(sv.ID, "alice")
	first := optionTexts(sv.BallotFor(nil, alice))

	// Same person, repeatedly: identical every time. Someone who reloads or
	// returns to edit must not watch their ticks move.
	for range 20 {
		if got := optionTexts(sv.BallotFor(nil, alice)); !equal(got, first) {
			t.Fatalf("order changed for the same respondent:\n%v\n%v", first, got)
		}
	}

	// Across people, the orders differ. With eight options a collision is
	// 1-in-40320, so a handful of voters settles it.
	distinct := map[string]bool{}
	for _, who := range []string{"alice", "bob", "carol", "dave", "erin"} {
		distinct[strings.Join(optionTexts(sv.BallotFor(nil, s.VoterID(sv.ID, who))), "|")] = true
	}
	if len(distinct) < 2 {
		t.Error("every respondent got the same order; the shuffle is not per-respondent")
	}
}

func TestShuffleKeepsEveryOptionExactlyOnce(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha", "Bravo", "Charlie", "Delta")
	plain := optionTexts(sv.Ballot(nil))

	for _, who := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		got := optionTexts(sv.BallotFor(nil, s.VoterID(sv.ID, who)))
		if len(got) != len(plain) {
			t.Fatalf("shuffle changed the option count: %v", got)
		}
		seen := map[string]int{}
		for _, o := range got {
			seen[o]++
		}
		for _, want := range plain {
			if seen[want] != 1 {
				t.Errorf("option %q appears %d times in %v", want, seen[want], got)
			}
		}
	}
}

func TestRandomizeOffGivesTheEditorsOrder(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha", "Bravo", "Charlie", "Delta", "Echo")
	sv, err := s.UpdateSurvey(sv.ID, func(d *Survey) error { d.NoRandomize = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo"}
	for _, who := range []string{"a", "b", "c"} {
		if got := optionTexts(sv.BallotFor(nil, s.VoterID(sv.ID, who))); !equal(got, want) {
			t.Errorf("order = %v, want the editor's order %v", got, want)
		}
	}
}

// A respondent's own pending write-in belongs at the end of their ballot,
// under the approved options, wherever the shuffle put those.
func TestPendingWriteInsStayAtTheEnd(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha", "Bravo", "Charlie")
	v := s.VoterID(sv.ID, "alice")
	opt, err := s.AddWriteIn(sv.ID, v, "Mine")
	if err != nil {
		t.Fatal(err)
	}
	sv, _ = s.Survey(sv.ID)
	got := optionTexts(sv.BallotFor([]string{opt}, v))
	if len(got) != 4 || got[3] != "Mine" {
		t.Errorf("ballot = %v, want the pending write-in last", got)
	}
}

// Shuffling is a presentation concern; nothing downstream may depend on it.
func TestShuffleDoesNotAffectTallyOrStoredOrder(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha", "Bravo", "Charlie")
	for _, who := range []string{"a", "b", "c"} {
		v := s.VoterID(sv.ID, who)
		_ = sv.BallotFor(nil, v) // whatever order they saw
		if _, err := s.SaveResponse(sv.ID, v, []string{sv.Options[1].ID}, ""); err != nil {
			t.Fatal(err)
		}
	}
	results, voters := s.Tally(sv.ID)
	if voters != 3 || results[0].Option.Text != "Bravo" || results[0].Votes != 3 {
		t.Errorf("tally = %+v (%d voters); the shuffle must not reach the results", results, voters)
	}
	// The definition keeps the editor's order, which is what exports use.
	if got := optionTexts(sv.Ballot(nil)); !equal(got, []string{"Alpha", "Bravo", "Charlie"}) {
		t.Errorf("stored order = %v, want the editor's order", got)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
