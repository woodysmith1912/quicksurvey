package store

import (
	"bytes"
	"regexp"
	"strings"
	"testing"
)

// approvedTexts returns a survey's approved option texts, in ballot order.
func approvedTexts(sv *Survey) []string {
	var out []string
	for _, o := range sv.Options {
		if o.Status == OptApproved {
			out = append(out, o.Text)
		}
	}
	return out
}

func TestSurveyDocumentRoundTripsTheDefinition(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Tacos", "Ramen", "Pho")
	sv, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		d.Description = "Pick what you'd eat"
		d.ShowResults = true
		d.AllowComment = false
		d.NoRandomize = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := s.WriteSurveyDocument(&buf, sv.ID, false); err != nil {
		t.Fatalf("WriteSurveyDocument: %v", err)
	}
	got, err := s.ReadSurveyDocument(&buf, false)
	if err != nil {
		t.Fatalf("ReadSurveyDocument: %v", err)
	}

	if got.Title != sv.Title || got.Description != sv.Description {
		t.Errorf("title/description = %q/%q, want %q/%q",
			got.Title, got.Description, sv.Title, sv.Description)
	}
	if got.ShowResults != true || got.AllowComment != false || got.NoRandomize != true {
		t.Errorf("settings did not survive: show_results=%v allow_comment=%v no_randomize=%v",
			got.ShowResults, got.AllowComment, got.NoRandomize)
	}
	want := []string{"Tacos", "Ramen", "Pho"}
	if diff := approvedTexts(got); !equalStrings(diff, want) {
		t.Errorf("options = %v, want %v — order is part of the definition", diff, want)
	}
}

func TestRestoredSurveyIsANewDraftWithNewIdentifiers(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Tacos", "Ramen")
	if sv.State != StateOpen {
		t.Fatalf("precondition: want an open survey, got %q", sv.State)
	}

	var buf bytes.Buffer
	if err := s.WriteSurveyDocument(&buf, sv.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadSurveyDocument(&buf, false)
	if err != nil {
		t.Fatal(err)
	}

	// A survey ID is the respondent URL. Reusing it would reopen links handed
	// out for the original.
	if got.ID == sv.ID {
		t.Error("restore reused the survey ID; respondent links to the original would resolve to the copy")
	}
	// Responses reference options by ID, so a reused option ID would let votes
	// on the original attach to the copy.
	for _, o := range got.Options {
		for _, orig := range sv.Options {
			if o.ID == orig.ID {
				t.Errorf("restore reused option ID %q", o.ID)
			}
		}
	}
	if got.State != StateDraft {
		t.Errorf("restored state = %q, want %q — restoring must never publish", got.State, StateDraft)
	}
	if !got.FirstOpenedAt.IsZero() {
		t.Error("restored survey carries the original's publication history")
	}
	// Both survive independently.
	if _, ok := s.Survey(sv.ID); !ok {
		t.Error("restoring destroyed the survey it came from")
	}
}

func TestSurveyDocumentCarriesNoResponses(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Tacos", "Ramen")
	voter := s.VoterID(sv.ID, "someone")
	if _, err := s.SaveResponse(sv.ID, voter, []string{sv.Options[0].ID}, "extra cheese"); err != nil {
		t.Fatal(err)
	}
	if n := len(s.Responses(sv.ID)); n != 1 {
		t.Fatalf("precondition: want 1 response, got %d", n)
	}

	var buf bytes.Buffer
	if err := s.WriteSurveyDocument(&buf, sv.ID, false); err != nil {
		t.Fatal(err)
	}
	// The comment is the most obviously respondent-authored thing there is.
	if strings.Contains(buf.String(), "extra cheese") {
		t.Error("a saved survey contains respondent text")
	}
	got, err := s.ReadSurveyDocument(bytes.NewReader(buf.Bytes()), false)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(s.Responses(got.ID)); n != 0 {
		t.Errorf("restored survey has %d responses, want 0", n)
	}
}

func TestSurveyDocumentCarriesApprovedWriteInsButNotUnmoderatedOnes(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Tacos")
	voter := s.VoterID(sv.ID, "someone")

	approvedID, err := s.AddWriteIn(sv.ID, voter, "Dumplings")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddWriteIn(sv.ID, voter, "Still deciding"); err != nil {
		t.Fatal(err)
	}
	rejectedID, err := s.AddWriteIn(sv.ID, voter, "Nonsense")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		for i := range d.Options {
			switch d.Options[i].ID {
			case approvedID:
				d.Options[i].Status = OptApproved
			case rejectedID:
				d.Options[i].Status = OptRejected
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := s.WriteSurveyDocument(&buf, sv.ID, false); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	// Pending and rejected write-ins are respondent-authored text that was
	// never on the ballot. A file described as carrying no responses must not
	// carry them.
	if strings.Contains(body, "Still deciding") {
		t.Error("a saved survey contains a write-in still awaiting moderation")
	}
	if strings.Contains(body, "Nonsense") {
		t.Error("a saved survey contains a rejected write-in")
	}

	got, err := s.ReadSurveyDocument(bytes.NewReader(buf.Bytes()), false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Tacos", "Dumplings"}
	if texts := approvedTexts(got); !equalStrings(texts, want) {
		t.Fatalf("options = %v, want %v — an approved write-in is on the ballot and part of the survey", texts, want)
	}
	for _, o := range got.Options {
		if o.Text == "Dumplings" && o.Source != "writein" {
			t.Errorf("restored option source = %q, want %q — provenance should survive", o.Source, "writein")
		}
	}
}

func TestReadSurveyDocumentRefusesWhatItCannotFaithfullyRestore(t *testing.T) {
	s := newStore(t)
	cases := []struct{ name, body, wantErr string }{
		{"wrong format", `{"format":"something-else","version":1,"survey":{"title":"T"}}`, "not a quicksurvey survey document"},
		{"newer version", `{"format":"quicksurvey.survey","version":99,"survey":{"title":"T"}}`, "understands up to"},
		{"no title", `{"format":"quicksurvey.survey","version":1,"survey":{"title":"   "}}`, "needs a title"},
		{"unknown type", `{"format":"quicksurvey.survey","version":1,"survey":{"title":"T","type":"ranked"}}`, "unknown survey type"},
		// A field this build does not know would be dropped in silence, which
		// is exactly what a restore exists to rule out.
		{"unknown field", `{"format":"quicksurvey.survey","version":1,"survey":{"title":"T","weighting":"borda"}}`, "unknown field"},
		{"not json", `nonsense`, "reading survey document"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.ReadSurveyDocument(strings.NewReader(c.body), false)
			if err == nil {
				t.Fatalf("restoring %s succeeded; it should be refused", c.name)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, c.wantErr)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
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

// A full backup exists to be put back as the survey it was. The property that
// matters is that a respondent who already answered is still recognised
// afterwards, which only holds if the survey ID survives: their stored
// identifier is derived from it.
func TestFullBackupRestoresUnderTheOriginalIDAndStillRejectsDuplicates(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Tacos", "Ramen")
	const token = "a-browser"
	voter := s.VoterID(sv.ID, token)
	if _, err := s.SaveResponse(sv.ID, voter, []string{sv.Options[0].ID}, "no onions"); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := s.WriteSurveyDocument(&buf, sv.ID, true); err != nil {
		t.Fatalf("full save: %v", err)
	}
	if err := s.DeleteSurvey(sv.ID); err != nil {
		t.Fatal(err)
	}

	got, err := s.ReadSurveyDocument(bytes.NewReader(buf.Bytes()), true)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got.ID != sv.ID {
		t.Fatalf("restored ID = %q, want the original %q", got.ID, sv.ID)
	}
	if got.State != sv.State {
		t.Errorf("restored state = %q, want %q — a backup of an open survey should come back open",
			got.State, sv.State)
	}
	if got.FirstOpenedAt.IsZero() {
		t.Error("restored survey lost FirstOpenedAt; publishing it would discard the restored responses")
	}
	rs := s.Responses(got.ID)
	if len(rs) != 1 {
		t.Fatalf("restored %d responses, want 1", len(rs))
	}
	if rs[0].Comment != "no onions" {
		t.Errorf("comment = %q, want %q", rs[0].Comment, "no onions")
	}

	// The point of all of it: the same browser is still the same respondent.
	if again := s.VoterID(got.ID, token); again != voter {
		t.Fatalf("voter ID after restore = %q, want %q", again, voter)
	}
	if _, err := s.SaveResponse(got.ID, s.VoterID(got.ID, token), []string{got.Options[1].ID}, ""); err != nil {
		t.Fatal(err)
	}
	if n := len(s.Responses(got.ID)); n != 1 {
		t.Errorf("after the same browser answered again there are %d responses, want 1 — "+
			"duplicate rejection did not survive the restore", n)
	}
}

func TestFullBackupKeepsOptionsModerationHid(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Tacos")
	voter := s.VoterID(sv.ID, "someone")
	hidden, err := s.AddWriteIn(sv.ID, voter, "Dumplings")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		for i := range d.Options {
			if d.Options[i].ID == hidden {
				d.Options[i].Status = OptApproved
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveResponse(sv.ID, voter, []string{hidden}, ""); err != nil {
		t.Fatal(err)
	}
	// Now remove it, the way an editor would after votes existed.
	if _, err := s.UpdateSurvey(sv.ID, func(d *Survey) error {
		for i := range d.Options {
			if d.Options[i].ID == hidden {
				d.Options[i].Status = OptRemoved
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := s.WriteSurveyDocument(&buf, sv.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSurvey(sv.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadSurveyDocument(bytes.NewReader(buf.Bytes()), true)
	if err != nil {
		t.Fatalf("a backup whose response votes for a removed option must still restore: %v", err)
	}
	var found bool
	for _, o := range got.Options {
		if o.ID == hidden {
			found = true
			if o.Status != OptRemoved {
				t.Errorf("option status = %q, want %q", o.Status, OptRemoved)
			}
		}
	}
	if !found {
		t.Error("the removed option did not survive the backup; the response that chose it would dangle")
	}
	if n := len(s.Responses(got.ID)); n != 1 {
		t.Errorf("restored %d responses, want 1", n)
	}
}

func TestRestoringResponsesRequiresTheOriginalID(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Tacos")
	voter := s.VoterID(sv.ID, "someone")
	if _, err := s.SaveResponse(sv.ID, voter, []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.WriteSurveyDocument(&buf, sv.ID, true); err != nil {
		t.Fatal(err)
	}
	_, err := s.ReadSurveyDocument(bytes.NewReader(buf.Bytes()), false)
	if err == nil {
		t.Fatal("restoring responses under a fresh ID succeeded; those responses could never match a respondent")
	}
	if !strings.Contains(err.Error(), "answered twice") {
		t.Errorf("error = %q, want it to explain the duplicate-answer consequence", err)
	}
}

func TestKeepIDRefusesToOverwriteALiveSurvey(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Tacos")
	var buf bytes.Buffer
	if err := s.WriteSurveyDocument(&buf, sv.ID, true); err != nil {
		t.Fatal(err)
	}
	// The survey is still there: a restore must not silently replace it.
	_, err := s.ReadSurveyDocument(bytes.NewReader(buf.Bytes()), true)
	if err == nil {
		t.Fatal("restoring over a live survey succeeded; it should be refused")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error = %q, want it to say the survey already exists", err)
	}
	if got, ok := s.Survey(sv.ID); !ok || got.Title != sv.Title {
		t.Error("the refused restore damaged the survey it refused to overwrite")
	}
}

func TestDefinitionDocumentCarriesNoIdentifiersAtAll(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Tacos", "Ramen")
	var buf bytes.Buffer
	if err := s.WriteSurveyDocument(&buf, sv.ID, false); err != nil {
		t.Fatal(err)
	}
	body := buf.String()
	for _, o := range sv.Options {
		if strings.Contains(body, o.ID) {
			t.Errorf("a definition document contains option ID %q", o.ID)
		}
	}
	// SourceID is provenance and is allowed, but restoring must not use it.
	got, err := s.ReadSurveyDocument(bytes.NewReader(buf.Bytes()), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == sv.ID {
		t.Error("a definition restore reused the original survey ID")
	}
}

// A full backup has to carry the seen set, not just the choices. Shown is
// derived from it, the tally asserts Votes <= Shown <= respondents, and
// backfillSeen cannot repair a restored survey because the meta guard is
// already set on any database old enough to restore into. Without this the
// round trip silently turns "shown to 3" into "shown to 0" beside unchanged
// vote counts.
func TestFullBackupCarriesTheSeenSetSoTheTallyInvariantSurvives(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Tacos", "Ramen")
	for _, who := range []string{"a", "b", "c"} {
		v := s.VoterID(sv.ID, who)
		if _, err := s.SaveResponse(sv.ID, v, []string{sv.Options[0].ID}, ""); err != nil {
			t.Fatal(err)
		}
	}
	before := shownByText(t, s, sv.ID)
	if before["Tacos"] != 3 || before["Ramen"] != 3 {
		t.Fatalf("precondition: shown = %v, want both options shown to 3", before)
	}
	checkTallyInvariant(t, s, sv.ID)

	var buf bytes.Buffer
	if err := s.WriteSurveyDocument(&buf, sv.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSurvey(sv.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadSurveyDocument(bytes.NewReader(buf.Bytes()), true)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	after := shownByText(t, s, got.ID)
	if after["Tacos"] != 3 || after["Ramen"] != 3 {
		t.Errorf("shown after restore = %v, want %v — the seen set did not survive", after, before)
	}
	checkTallyInvariant(t, s, got.ID)

	results, _ := s.Tally(got.ID)
	for _, r := range results {
		if r.Votes > 0 && r.Shown == 0 {
			t.Errorf("%s: %d votes but shown to nobody", r.Option.Text, r.Votes)
		}
	}
}

// A document written before seen existed carries choices and no seen. The
// restore seeds the seen set from the choices and nothing more: every chosen
// option was on that ballot by definition, which is the only thing the
// document actually says.
//
// This pins the known limitation as well as the behaviour. An in-place
// upgrade of the same data would mark every non-pending option as seen by
// everyone, so a restored pre-seen backup reports a smaller denominator and
// therefore optimistic interest. An earlier version of this code copied the
// backfill's rule to close that gap and could not tell a pre-seen document
// from one recording a respondent who genuinely saw nothing -- so it
// corrupted faithful backups, and materialised enough strings to exhaust the
// container from a small upload. The gap is the better trade.
func TestRestoringADocumentWithoutSeenSeedsOnlyFromTheChoices(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Tacos", "Ramen")
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "a"), []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.WriteSurveyDocument(&buf, sv.ID, true); err != nil {
		t.Fatal(err)
	}
	// Strip the seen arrays, as a pre-feature backup would have them.
	stripped := regexp.MustCompile(`"seen": \[[^]]*\],`).ReplaceAllString(buf.String(), "")
	if strings.Contains(stripped, `"seen"`) {
		t.Fatalf("test setup did not strip the seen arrays")
	}
	if err := s.DeleteSurvey(sv.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadSurveyDocument(strings.NewReader(stripped), true)
	if err != nil {
		t.Fatalf("a document without seen must still restore: %v", err)
	}

	checkTallyInvariant(t, s, got.ID)
	shown := shownByText(t, s, got.ID)
	if shown["Tacos"] != 1 {
		t.Errorf("the chosen option shows %d, want 1 — seen was not seeded from the choices",
			shown["Tacos"])
	}
	// The limitation, asserted rather than only described: an option nobody
	// picked comes back with no exposures, where an in-place upgrade would
	// have given it one. If this ever reads 1, the blanket rule is back and
	// the defects that came with it need re-checking.
	if shown["Ramen"] != 0 {
		t.Errorf("the unchosen option shows %d, want 0 — the restore should claim only what "+
			"the document says, not what an in-place backfill would have guessed", shown["Ramen"])
	}
}

// The choices union is what keeps Votes <= Shown true for a document whose
// seen set is incomplete. Every round-trip test uses documents where seen
// already contains the choices, so the union is a no-op in all of them and
// nothing noticed when it was absent.
func TestRestoringAPartialSeenSetStillCannotReportMoreVotesThanExposures(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Tacos", "Ramen")
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "a"),
		[]string{sv.Options[0].ID, sv.Options[1].ID}, ""); err != nil {
		t.Fatal(err)
	}
	doc, err := s.SurveyDocument(sv.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Responses) != 1 || len(doc.Responses[0].Seen) != 2 {
		t.Fatalf("precondition: want one response shown both options, got %+v", doc.Responses)
	}
	// A hand-edited or third-party document: seen omits an option that was
	// nonetheless chosen.
	doc.Responses[0].Seen = []string{doc.Responses[0].Seen[1]}
	if err := s.DeleteSurvey(sv.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.RestoreSurvey(doc, true)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	checkTallyInvariant(t, s, got.ID)
	if shown := shownByText(t, s, got.ID); shown["Tacos"] != 1 {
		t.Errorf("Tacos shown %d with a vote recorded; the choices union did not run: %v",
			shown["Tacos"], shown)
	}
}

// An option with no text cannot be restored, and restoring the rest would
// leave every response that names it pointing at nothing. Refusing the
// document is what lets both reference checks below be strict and their
// errors be accurate.
func TestADocumentWithAnUnrestorableOptionIsRefusedWholesale(t *testing.T) {
	s := newStore(t)
	sv := mustSurvey(t, s, "Tacos", "Ramen")
	if _, err := s.SaveResponse(sv.ID, s.VoterID(sv.ID, "a"), []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	doc, err := s.SurveyDocument(sv.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	doc.Survey.Options[1].Text = "   "
	if err := s.DeleteSurvey(sv.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.RestoreSurvey(doc, true)
	if err == nil {
		t.Fatal("a document carrying an option with no text was restored; the responses " +
			"naming that option would silently lose an exposure")
	}
	if !strings.Contains(err.Error(), "no text") {
		t.Errorf("error = %q, want it to name the option that cannot be restored", err)
	}
}

// A newer document is full of fields this build does not know, so the version
// has to be read before the strict decode or the error blames a field when
// the real problem is the version. This is the case that distinguishes them;
// a newer document with no unknown fields passes either way.
func TestANewerDocumentReportsItsVersionRatherThanItsFields(t *testing.T) {
	s := newStore(t)
	_, err := s.ReadSurveyDocument(strings.NewReader(
		`{"format":"quicksurvey.survey","version":99,"tenant":"acme","survey":{"title":"T"}}`), false)
	if err == nil {
		t.Fatal("a document from a newer quicksurvey was accepted")
	}
	if !strings.Contains(err.Error(), "understands up to") {
		t.Errorf("error = %q, want the version explained rather than a field named", err)
	}
}

// A document declaring no version would otherwise be treated as current.
func TestADocumentWithNoVersionIsRefused(t *testing.T) {
	s := newStore(t)
	_, err := s.ReadSurveyDocument(strings.NewReader(
		`{"format":"quicksurvey.survey","survey":{"title":"T"}}`), false)
	if err == nil {
		t.Fatal("a document declaring no version was accepted as current")
	}
	if !strings.Contains(err.Error(), "no version") {
		t.Errorf("error = %q, want it to say the version is missing", err)
	}
}
