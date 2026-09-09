package web

import (
	"testing"
	"time"

	"github.com/woodysmith1912/quicksurvey/internal/store"
)

// A close time is entered as wall-clock text in the server's timezone and
// stored as a UTC instant, so the comparison that decides whether a survey is
// still open is absolute and DST cannot reach it. These tests say so out loud,
// and pin down the one place a clock change is genuinely visible: what a
// wall-clock time *means* on the two days a year when one does not exist, or
// happens twice.
func TestCloseTimeAcrossDaylightSaving(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata for America/New_York")
	}
	s := &Server{cfg: Config{Location: ny}}

	for _, c := range []struct {
		name, entered string
		wantUTC       string
	}{
		// Ordinary times either side of the spring transition.
		{"before spring forward", "2027-03-14T01:30", "2027-03-14T06:30:00Z"},
		{"after spring forward", "2027-03-14T03:30", "2027-03-14T07:30:00Z"},
		// Either side of the autumn transition.
		{"before fall back", "2027-11-07T00:30", "2027-11-07T04:30:00Z"},
		{"after fall back", "2027-11-07T03:30", "2027-11-07T08:30:00Z"},
		// Midsummer and midwinter, to show the offset is applied at all.
		{"summer", "2027-07-01T12:00", "2027-07-01T16:00:00Z"},
		{"winter", "2027-01-15T12:00", "2027-01-15T17:00:00Z"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.parseCloseAt(c.entered)
			if err != nil {
				t.Fatalf("parseCloseAt(%q): %v", c.entered, err)
			}
			if got.Format(time.RFC3339) != c.wantUTC {
				t.Errorf("%q -> %s, want %s", c.entered, got.Format(time.RFC3339), c.wantUTC)
			}
		})
	}
}

// The behaviour that actually matters: a survey stays open right up to its
// close instant and shuts immediately after, whether or not a clock change
// falls in between.
func TestSurveyClosesAtTheRightInstantAcrossAClockChange(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata for America/New_York")
	}
	s := &Server{cfg: Config{Location: ny}}

	// 09:00 on the morning after the clocks go forward.
	closeAt, err := s.parseCloseAt("2027-03-14T09:00")
	if err != nil {
		t.Fatal(err)
	}
	sv := &store.Survey{State: store.StateOpen, CloseAt: closeAt}

	for _, c := range []struct {
		name string
		now  time.Time
		open bool
	}{
		{"the evening before, EST", time.Date(2027, 3, 13, 22, 0, 0, 0, ny), true},
		{"an hour before, EDT", closeAt.Add(-time.Hour), true},
		{"a second before", closeAt.Add(-time.Second), true},
		{"exactly at the close instant", closeAt, false},
		{"a second after", closeAt.Add(time.Second), false},
	} {
		if got := sv.Accepting(c.now); got != c.open {
			t.Errorf("%s: Accepting = %v, want %v (close at %s)",
				c.name, got, c.open, closeAt.Format(time.RFC3339))
		}
	}
}

// The one real ambiguity, recorded rather than fixed: a wall-clock time that
// does not exist, and one that happens twice. Go resolves both without
// complaining, so an editor picking such a time gets an instant an hour from
// what they may have pictured. There is no correct answer available — the text
// they typed does not identify a single moment — and an hour on two days a year
// is not worth a timezone picker on the form.
func TestAmbiguousWallClockTimesResolveWithoutError(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata for America/New_York")
	}
	s := &Server{cfg: Config{Location: ny}}

	// 02:30 does not exist on the morning the clocks go forward.
	nonexistent, err := s.parseCloseAt("2027-03-14T02:30")
	if err != nil {
		t.Fatalf("a nonexistent wall-clock time should still parse: %v", err)
	}
	// 01:30 happens twice on the morning they go back.
	ambiguous, err := s.parseCloseAt("2027-11-07T01:30")
	if err != nil {
		t.Fatalf("an ambiguous wall-clock time should still parse: %v", err)
	}
	t.Logf("02:30 on the spring-forward day -> %s", nonexistent.Format(time.RFC3339))
	t.Logf("01:30 on the fall-back day      -> %s", ambiguous.Format(time.RFC3339))

	// Whatever instant each resolves to, it is a real instant and the open/
	// closed decision around it is still exact.
	for _, at := range []time.Time{nonexistent, ambiguous} {
		sv := &store.Survey{State: store.StateOpen, CloseAt: at}
		if !sv.Accepting(at.Add(-time.Second)) {
			t.Errorf("closed a second early at %s", at.Format(time.RFC3339))
		}
		if sv.Accepting(at.Add(time.Second)) {
			t.Errorf("still open a second late at %s", at.Format(time.RFC3339))
		}
	}
}
