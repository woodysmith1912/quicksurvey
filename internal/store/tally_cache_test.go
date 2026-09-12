package store

import (
	"fmt"
	"sync"
	"testing"
)

// A cache that can serve a stale count is worse than no cache: the number is
// the entire product. These tests exercise every write path that can change a
// tally and insist the next read reflects it.
func TestTallyCacheIsInvalidatedByEveryWritePath(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha", "Bravo", "Charlie")

	votes := func() map[string]int {
		results, _ := s.Tally(sv.ID)
		out := map[string]int{}
		for _, r := range results {
			out[r.Option.Text] = r.Votes
		}
		return out
	}
	shown := func() map[string]int {
		results, _ := s.Tally(sv.ID)
		out := map[string]int{}
		for _, r := range results {
			out[r.Option.Text] = r.Shown
		}
		return out
	}
	// Prime the cache.
	if got := votes()["Alpha"]; got != 0 {
		t.Fatalf("Alpha = %d, want 0", got)
	}

	// 1. A new response.
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "a"), []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	if got := votes()["Alpha"]; got != 1 {
		t.Errorf("after a vote, Alpha = %d, want 1 — the cache was not invalidated", got)
	}
	if got := shown()["Alpha"]; got != 1 {
		t.Errorf("after a vote, Alpha shown = %d, want 1 — the cache was not invalidated", got)
	}

	// 2. The same person changing their mind.
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "a"), []string{sv.Options[1].ID}, ""); err != nil {
		t.Fatal(err)
	}
	if v := votes(); v["Alpha"] != 0 || v["Bravo"] != 1 {
		t.Errorf("after a changed vote, got %v, want Alpha 0 and Bravo 1", v)
	}

	// 3. A write-in, and its approval.
	opt, err := s.AddWriteIn(sv.ID, s.VoterID(sv.ID, "b"), "Delta")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := votes()["Delta"]; ok {
		t.Error("a pending write-in appeared in the tally")
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, opt, OptApproved, "")
	}); err != nil {
		t.Fatal(err)
	}
	if got := votes()["Delta"]; got != 1 {
		t.Errorf("after approval, Delta = %d, want 1 — moderation did not invalidate", got)
	}
	if got := shown()["Delta"]; got != 1 {
		t.Errorf("after approval, Delta shown = %d, want 1 (its proposer)", got)
	}

	// 4. A merge, which moves votes between options.
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		return SetOptionStatus(d, opt, OptMerged, sv.Options[1].ID)
	}); err != nil {
		t.Fatal(err)
	}
	if v := votes(); v["Bravo"] != 2 {
		t.Errorf("after a merge, Bravo = %d, want 2 — got %v", v["Bravo"], v)
	}

	// 5. Clearing responses.
	if _, err := s.ClearResponses(sv.ID); err != nil {
		t.Fatal(err)
	}
	if v := votes(); v["Bravo"] != 0 {
		t.Errorf("after clearing, Bravo = %d, want 0", v["Bravo"])
	}
}

// Publishing discards a draft's preview responses; the tally must follow.
func TestTallyCacheFollowsPublish(t *testing.T) {
	s := newStore(t)
	sv := draftSurvey(t, s, "Alpha")
	if _, err := s.PreviewResponse(sv.ID, s.VoterID(sv.ID, "editor"), []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	if _, n := s.Tally(sv.ID); n != 1 {
		t.Fatalf("respondents = %d, want the preview response", n)
	}
	if _, err := s.Publish(sv.ID); err != nil {
		t.Fatal(err)
	}
	if _, n := s.Tally(sv.ID); n != 0 {
		t.Errorf("respondents = %d after publishing, want 0 — discarded test data was still cached", n)
	}
}

// One survey's writes must not serve another survey a stale count, and the
// cache must not hand out a slice that callers can mutate underneath others.
func TestTallyCacheIsolatesSurveysAndCopies(t *testing.T) {
	s := newStore(t)
	a := mustSurvey(t, s, "Alpha")
	b := mustSurvey(t, s, "Beta")
	if _, err := s.SaveResponse(a.ID, s.VoterID(a.ID, "x"), []string{a.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	ra, na := s.Tally(a.ID)
	if na != 1 || ra[0].Votes != 1 {
		t.Fatalf("survey a = %v (%d)", ra, na)
	}
	if _, nb := s.Tally(b.ID); nb != 0 {
		t.Errorf("survey b saw %d respondents from survey a's writes", nb)
	}
	// Mutating a returned slice must not affect the next caller.
	ra[0].Votes = 9999
	again, _ := s.Tally(a.ID)
	if again[0].Votes != 1 {
		t.Errorf("a caller mutated the cached result: %d", again[0].Votes)
	}
}

// Reads and writes race in production; the cache must not serve a count from
// before a write that has already committed.
func TestTallyCacheUnderConcurrentReadsAndWrites(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha")

	const writes = 50
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range writes {
			if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, fmt.Sprintf("v%d", i)),
				[]string{sv.Options[0].ID}, ""); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for range writes * 4 {
			s.Tally(sv.ID)
		}
	}()
	wg.Wait()

	// Whatever happened during the race, the settled answer must be right.
	results, n := s.Tally(sv.ID)
	if n != writes || results[0].Votes != writes {
		t.Errorf("after %d writes: %d respondents, %d votes — stale cache survived",
			writes, n, results[0].Votes)
	}
}
