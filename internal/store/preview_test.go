package store

import (
	"testing"
	"time"
)

// draftSurvey creates a survey and leaves it unpublished.
func draftSurvey(t *testing.T, s *Store, opts ...string) *Survey {
	t.Helper()
	sv, err := s.CreateSurvey("Draft survey", "", opts)
	if err != nil {
		t.Fatal(err)
	}
	return sv
}

func TestDraftAcceptsResponsesOnlyFromAPreviewer(t *testing.T) {
	s := newStore(t)
	sv := draftSurvey(t, s, "Alpha", "Beta")
	v := s.VoterID(sv.ID, "editor-browser")

	if sv.Accepting(time.Now()) {
		t.Error("a draft must not accept public responses")
	}
	if !sv.AcceptingFrom(time.Now(), true) {
		t.Error("a draft must accept responses from a previewer")
	}
	if _, err := s.SaveResponse(sv.ID, v, []string{sv.Options[0].ID}, ""); err == nil {
		t.Error("SaveResponse should refuse a draft")
	}
	if _, err := s.PreviewResponse(sv.ID, v, []string{sv.Options[0].ID}, "trying it out"); err != nil {
		t.Fatalf("PreviewResponse on a draft: %v", err)
	}
	if n := s.Count(sv.ID); n != 1 {
		t.Fatalf("respondents = %d, want 1", n)
	}
}

func TestPreviewCannotBypassClosedOrScheduledClose(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha") // open
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error { d.State = StateClosed; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PreviewResponse(sv.ID, "v1", []string{sv.Options[0].ID}, ""); err == nil {
		t.Error("preview must not reopen a closed survey")
	}

	sv2 := mustSurvey(t, s, "Beta")
	if _, err := s.UpdateSurvey(sv2.ID, func(d *Survey) error {
		d.CloseAt = time.Now().Add(-time.Minute).UTC()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PreviewResponse(sv2.ID, "v1", []string{sv2.Options[0].ID}, ""); err == nil {
		t.Error("preview must not bypass a passed close time")
	}
}

func TestPublishDiscardsPreviewResponsesOnce(t *testing.T) {
	s := newStore(t)
	sv := draftSurvey(t, s, "Alpha", "Beta")

	for _, b := range []string{"editor-laptop", "editor-phone"} {
		if _, err := s.PreviewResponse(sv.ID, s.VoterID(sv.ID, b), []string{sv.Options[0].ID}, "test"); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.Count(sv.ID); n != 2 {
		t.Fatalf("test responses = %d, want 2", n)
	}

	discarded, err := s.Publish(sv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if discarded != 2 {
		t.Errorf("Publish discarded %d, want 2", discarded)
	}
	if n := s.Count(sv.ID); n != 0 {
		t.Errorf("respondents after publishing = %d, want 0 — draft test data must not survive", n)
	}
	sv, _ = s.Survey(sv.ID)
	if sv.State != StateOpen || sv.FirstOpenedAt.IsZero() {
		t.Fatalf("state=%q firstOpened=%v", sv.State, sv.FirstOpenedAt)
	}

	// Real responses arrive, and must survive a close/reopen cycle.
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "real"), []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error { d.State = StateClosed; return nil }); err != nil {
		t.Fatal(err)
	}
	discarded, err = s.Publish(sv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if discarded != 0 {
		t.Errorf("reopening discarded %d responses, want 0", discarded)
	}
	if n := s.Count(sv.ID); n != 1 {
		t.Errorf("respondents after reopening = %d, want 1 — real data must never be discarded", n)
	}
	if n := reopen(t, s).Count(sv.ID); n != 1 {
		t.Errorf("after restart respondents = %d, want 1", n)
	}
}

func TestPublishClearsAPassedCloseTime(t *testing.T) {
	s := newStore(t)
	sv := draftSurvey(t, s, "Alpha")
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		d.CloseAt = time.Now().Add(-time.Hour).UTC()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(sv.ID); err != nil {
		t.Fatal(err)
	}
	sv, _ = s.Survey(sv.ID)
	if !sv.CloseAt.IsZero() {
		t.Error("publishing with a close time already in the past should clear it, not open for zero seconds")
	}
	if !sv.Accepting(time.Now()) {
		t.Error("the survey should be accepting after publication")
	}
}

func TestPreviewWriteInIsAllowedOnADraft(t *testing.T) {
	s := newStore(t)
	sv := draftSurvey(t, s, "Alpha")
	v := s.VoterID(sv.ID, "editor")
	if _, err := s.AddWriteIn(sv.ID, v, "Gamma"); err == nil {
		t.Error("a public write-in should be refused on a draft")
	}
	if _, err := s.PreviewWriteIn(sv.ID, v, "Gamma"); err != nil {
		t.Fatalf("PreviewWriteIn: %v", err)
	}
	sv, _ = s.Survey(sv.ID)
	if len(sv.PendingOptions()) != 1 {
		t.Error("the previewed write-in did not reach the moderation queue")
	}
}

func TestClearResponses(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Alpha")
	for _, b := range []string{"a", "b", "c"} {
		if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, b), []string{sv.Options[0].ID}, ""); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.ClearResponses(sv.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("cleared %d, want 3", n)
	}
	if s.Count(sv.ID) != 0 || reopen(t, s).Count(sv.ID) != 0 {
		t.Error("responses came back")
	}
	// The survey itself is untouched.
	if _, ok := s.Survey(sv.ID); !ok {
		t.Error("clearing responses removed the survey")
	}
}
