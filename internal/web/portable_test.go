package web

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"

	"github.com/woodysmith1912/quicksurvey/internal/store"
)

// upload posts a file the way the restore form does.
func (b *browser) upload(path, csrf, filename, content string, fields map[string]string) result {
	b.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("csrf", csrf); err != nil {
		b.t.Fatal(err)
	}
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			b.t.Fatal(err)
		}
	}
	if filename != "" {
		part, err := mw.CreateFormFile("file", filename)
		if err != nil {
			b.t.Fatal(err)
		}
		if _, err := part.Write([]byte(content)); err != nil {
			b.t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		b.t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", b.base+path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return b.do(req)
}

// editor signs in as an account that can edit, and returns its browser.
func (h *harness) editor() *browser {
	h.t.Helper()
	pw := h.seedUser("edna", store.RoleEditor)
	b := h.browser()
	b.login("edna", pw)
	return b
}

func TestEditorSavesTheQuestionsWithoutAnythingRespondentsWrote(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Tacos", "Ramen")
	voter := h.st.VoterID(sv.ID, "a-browser")
	if _, err := h.st.SaveResponse(sv.ID, voter, []string{sv.Options[0].ID}, "extra salsa"); err != nil {
		t.Fatal(err)
	}
	b := h.editor()

	r := b.get("/admin/s/" + sv.ID + "/save/definition.json")
	if r.status != http.StatusOK {
		t.Fatalf("save definition = %d\n%s", r.status, r.body)
	}
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
	cd := r.header.Get("Content-Disposition")
	if !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, ".json") {
		t.Errorf("Content-Disposition = %q, want a .json attachment", cd)
	}
	if strings.Contains(r.body, "extra salsa") {
		t.Error("the questions file contains a respondent's comment")
	}
	if strings.Contains(r.body, voter) {
		t.Error("the questions file contains a respondent identifier")
	}
	for _, o := range sv.Options {
		if strings.Contains(r.body, o.ID) {
			t.Errorf("the questions file contains option ID %q", o.ID)
		}
	}
	if !strings.Contains(r.body, "Tacos") {
		t.Error("the questions file does not contain the questions")
	}
}

func TestEditorSavesEverythingAndRestoresItOntoTheOriginalLink(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Tacos", "Ramen")
	const token = "a-browser"
	voter := h.st.VoterID(sv.ID, token)
	if _, err := h.st.SaveResponse(sv.ID, voter, []string{sv.Options[0].ID}, "extra salsa"); err != nil {
		t.Fatal(err)
	}
	b := h.editor()

	saved := b.get("/admin/s/" + sv.ID + "/save/full.json")
	if saved.status != http.StatusOK {
		t.Fatalf("save full = %d\n%s", saved.status, saved.body)
	}
	if !strings.Contains(saved.body, "extra salsa") {
		t.Fatal("the full backup does not contain the responses")
	}

	// Lose the survey, the way someone would before wanting it back.
	if err := h.st.DeleteSurvey(sv.ID); err != nil {
		t.Fatal(err)
	}

	csrf := b.csrf("/admin/")
	r := b.upload(restorePath, csrf, "backup.json", saved.body, map[string]string{"keep_id": "1"})
	if r.status != http.StatusSeeOther {
		t.Fatalf("restore = %d\n%s", r.status, r.body)
	}
	if want := "/admin/s/" + sv.ID; r.location != want {
		t.Fatalf("redirected to %q, wanted the restored survey at %q", r.location, want)
	}
	page := b.get(r.location)
	if !strings.Contains(page.body, "extra salsa") {
		t.Error("the restored survey has lost its responses")
	}

	// The property the original link is for: the same respondent is still the
	// same respondent, so they replace their answer rather than adding one.
	if again := h.st.VoterID(sv.ID, token); again != voter {
		t.Fatalf("voter ID changed across the restore")
	}
	if _, err := h.st.SaveResponse(sv.ID, voter, []string{}, "changed my mind"); err != nil {
		t.Fatal(err)
	}
	if n := len(h.st.Responses(sv.ID)); n != 1 {
		t.Errorf("%d responses after the same person answered again, want 1", n)
	}
}

func TestRestoringTheQuestionsMakesACopyWithItsOwnLink(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Tacos", "Ramen")
	b := h.editor()

	saved := b.get("/admin/s/" + sv.ID + "/save/definition.json")
	csrf := b.csrf("/admin/")
	r := b.upload(restorePath, csrf, "questions.json", saved.body, nil)
	if r.status != http.StatusSeeOther {
		t.Fatalf("restore = %d\n%s", r.status, r.body)
	}
	if r.location == "/admin/s/"+sv.ID {
		t.Fatal("restoring the questions replaced the original instead of copying it")
	}
	if _, ok := h.st.Survey(sv.ID); !ok {
		t.Error("the original survey is gone")
	}
	page := b.follow(r)
	if !strings.Contains(page.body, "Tacos") {
		t.Error("the copy does not have the original's options")
	}
	if !strings.Contains(page.body, "draft") {
		t.Error("a restored copy should be a draft")
	}
}

func TestRestoreRefusesWhatWouldGoWrongAndSaysWhy(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Tacos")
	voter := h.st.VoterID(sv.ID, "a-browser")
	if _, err := h.st.SaveResponse(sv.ID, voter, []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}
	b := h.editor()
	full := b.get("/admin/s/" + sv.ID + "/save/full.json").body

	cases := []struct{ name, content, keepID, want string }{
		{"responses without the original link", full, "", "answered twice"},
		{"onto a link already in use", full, "1", "already exists"},
		{"a file that is not a survey", `{"hello":"world"}`, "", "not a quicksurvey survey document"},
		{"a survey document with a field we do not know", `{"format":"quicksurvey.survey","version":1,` +
			`"survey":{"title":"T","weighting":"borda"}}`, "", "unknown field"},
		{"a document from a newer quicksurvey", `{"format":"quicksurvey.survey","version":99,` +
			`"survey":{"title":"T"}}`, "", "understands up to"},
		{"a file that is not JSON", "not json at all", "", "reading survey document"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fields := map[string]string{}
			if c.keepID != "" {
				fields["keep_id"] = c.keepID
			}
			csrf := b.csrf("/admin/")
			r := b.upload(restorePath, csrf, "f.json", c.content, fields)
			if r.status != http.StatusSeeOther {
				t.Fatalf("status = %d, want a redirect back with a message\n%s", r.status, r.body)
			}
			page := b.follow(r)
			if !strings.Contains(page.body, c.want) {
				t.Errorf("the page does not explain the problem; wanted it to mention %q", c.want)
			}
		})
	}
}

func TestRestoreWithNoFileChosenSaysSo(t *testing.T) {
	h := newHarness(t)
	b := h.editor()
	csrf := b.csrf("/admin/")
	r := b.upload(restorePath, csrf, "", "", nil)
	if r.status != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect back with a message", r.status)
	}
	if page := b.follow(r); !strings.Contains(page.body, "Choose a saved survey file") {
		t.Error("no message telling the person to choose a file")
	}
}

func TestSavingAndRestoringAreClosedToViewers(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Tacos")
	pw := h.seedUser("vic", store.RoleViewer)
	b := h.browser()
	b.login("vic", pw)

	for _, path := range []string{
		"/admin/s/" + sv.ID + "/save/definition.json",
		"/admin/s/" + sv.ID + "/save/full.json",
	} {
		if r := b.get(path); r.status != http.StatusForbidden {
			t.Errorf("GET %s as a viewer = %d, want 403", path, r.status)
		}
	}
	// A viewer has no restore form to read a token from, so borrow a valid one
	// and confirm the route itself refuses rather than the CSRF check.
	if r := b.upload(restorePath, "", "f.json", "{}", nil); r.status != http.StatusForbidden {
		t.Errorf("restore as a viewer = %d, want 403", r.status)
	}
}

func TestUnknownSaveKindIsNotFound(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Tacos")
	b := h.editor()
	if r := b.get("/admin/s/" + sv.ID + "/save/everything.json"); r.status != http.StatusNotFound {
		t.Errorf("unknown save kind = %d, want 404", r.status)
	}
}

// Shown and Interest are cohort sizes. A cohort of one — which happens
// routinely the moment an editor approves a write-in — states a single
// respondent's ballot entry as a percentage, so an anonymous reader of the
// public results page must not see those columns. A signed-in account is a
// permissioned reader and does.
func TestExposureColumnsAreHiddenFromAnonymousReaders(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Tacos", "Ramen")
	if _, err := h.st.UpdateSurvey(sv.ID, func(d *store.Survey) error {
		d.ShowResults = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.SaveResponse(sv.ID, h.st.VoterID(sv.ID, "a"), []string{sv.Options[0].ID}, ""); err != nil {
		t.Fatal(err)
	}

	anon := h.browser()
	for _, path := range []string{"/s/" + sv.ID + "/results", "/s/" + sv.ID} {
		body := anon.get(path).body
		if !strings.Contains(body, `data-testid="tally"`) {
			t.Fatalf("%s: no tally rendered at all; the test is not exercising the page", path)
		}
		if strings.Contains(body, `data-testid="shown"`) || strings.Contains(body, `data-testid="interest"`) {
			t.Errorf("%s: an anonymous reader can see the exposure columns", path)
		}
		if strings.Contains(body, "Shown to") || strings.Contains(body, "Interest") {
			t.Errorf("%s: the exposure headings leak to an anonymous reader", path)
		}
	}

	ed := h.editor()
	body := ed.get("/admin/s/" + sv.ID).body
	if !strings.Contains(body, `data-testid="shown"`) || !strings.Contains(body, `data-testid="interest"`) {
		t.Error("a signed-in account should still see the exposure columns")
	}

	// The public pages must stay clean for a SIGNED-IN reader too. Today
	// .User is nil there because only requireRole populates it, so the gate
	// is really "an /admin/ route" rather than "a signed-in account" -- and
	// the obvious future change, resolving the session on every route so a
	// public page can show the nav bar, would silently re-expose cohort
	// sizes. This is the case that would catch it.
	for _, path := range []string{"/s/" + sv.ID + "/results", "/s/" + sv.ID} {
		body := ed.get(path).body
		if !strings.Contains(body, `data-testid="tally"`) {
			t.Fatalf("%s: no tally rendered; the test is not exercising the page", path)
		}
		if strings.Contains(body, `data-testid="shown"`) || strings.Contains(body, `data-testid="interest"`) {
			t.Errorf("%s: exposure columns render on a public page for a signed-in reader", path)
		}
	}
}
