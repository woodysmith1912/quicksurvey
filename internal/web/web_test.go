package web

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/woodysmith1912/quicksurvey/internal/store"
)

// harness is one server, its store, and a browser-like client per test.
type harness struct {
	t   *testing.T
	st  *store.Store
	srv *httptest.Server
	web *Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(st, Config{
		SecureCookies: false, // httptest serves plain HTTP
		Location:      time.UTC,
		Logger:        slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return &harness{t: t, st: st, srv: srv, web: s}
}

// browser is an HTTP client with its own cookie jar, which is what makes it a
// distinct "person" as far as the survey is concerned.
type browser struct {
	t    *testing.T
	base string
	c    *http.Client
}

func (h *harness) browser() *browser {
	h.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return &browser{t: h.t, base: h.srv.URL, c: &http.Client{
		Jar: jar,
		// Stop at the first redirect so tests can assert on Location.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

type result struct {
	status   int
	body     string
	location string
	header   http.Header
}

func (b *browser) do(req *http.Request) result {
	b.t.Helper()
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return result{resp.StatusCode, string(body), resp.Header.Get("Location"), resp.Header}
}

func (b *browser) get(path string) result {
	b.t.Helper()
	req, _ := http.NewRequest("GET", b.base+path, nil)
	return b.do(req)
}

func (b *browser) post(path string, form url.Values) result {
	b.t.Helper()
	req, _ := http.NewRequest("POST", b.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return b.do(req)
}

// follow chases redirects the way a browser would, up to a sane limit.
func (b *browser) follow(r result) result {
	b.t.Helper()
	for range 5 {
		if r.location == "" || r.status < 300 || r.status >= 400 {
			return r
		}
		r = b.get(r.location)
	}
	b.t.Fatal("too many redirects")
	return r
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// checked reports whether the ballot checkbox for an option is pre-selected.
// Attribute whitespace comes from the template, so match it loosely.
func checked(body, optionID string) bool {
	return regexp.MustCompile(`value="` + regexp.QuoteMeta(optionID) + `"\s+checked`).MatchString(body)
}

// csrf reads the token out of a rendered page, exactly as a browser would
// submit it. Tests that skip this are testing a request no browser makes.
func (b *browser) csrf(path string) string {
	b.t.Helper()
	m := csrfRe.FindStringSubmatch(b.get(path).body)
	if m == nil {
		b.t.Fatalf("no CSRF token on %s", path)
	}
	return m[1]
}

func (b *browser) login(name, password string) {
	b.t.Helper()
	token := b.csrf("/login")
	r := b.post("/login", url.Values{"csrf": {token}, "username": {name}, "password": {password}})
	if r.status != http.StatusSeeOther {
		b.t.Fatalf("login as %s: status %d\n%s", name, r.status, r.body)
	}
}

// seedUser adds an account directly, bypassing the UI.
func (h *harness) seedUser(name string, role store.Role) string {
	h.t.Helper()
	const pw = "password123"
	if _, err := h.st.AddUser(name, role, pw); err != nil {
		h.t.Fatal(err)
	}
	return pw
}

// seedSurvey creates an open survey directly.
func (h *harness) seedSurvey(opts ...string) *store.Survey {
	h.t.Helper()
	sv, err := h.st.CreateSurvey("Lunch", "Pick anything you'd eat", opts)
	if err != nil {
		h.t.Fatal(err)
	}
	sv, err = h.st.UpdateSurvey(sv.ID, func(d *store.Survey) error { d.State = store.StateOpen; return nil })
	if err != nil {
		h.t.Fatal(err)
	}
	return sv
}

func TestHealthAndSecurityHeaders(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	if r := b.get("/healthz"); r.status != 200 || !strings.Contains(r.body, "ok") {
		t.Errorf("healthz = %d %q", r.status, r.body)
	}
	r := b.get("/")
	if got := r.header.Get("Content-Security-Policy"); !strings.Contains(got, "default-src 'none'") {
		t.Errorf("CSP = %q", got)
	}
	if got := r.header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
}

func TestAdminRequiresLogin(t *testing.T) {
	h := newHarness(t)
	b := h.browser()
	r := b.get("/admin/")
	if r.status != http.StatusSeeOther || !strings.HasPrefix(r.location, "/login") {
		t.Fatalf("anonymous GET /admin/ = %d %q, want a redirect to /login", r.status, r.location)
	}
	if !strings.Contains(r.location, "next=") {
		t.Error("the redirect should remember where the user was going")
	}
}

func TestLoginRejectsBadPasswordAndCrossSiteNext(t *testing.T) {
	h := newHarness(t)
	pw := h.seedUser("alice", store.RoleAdmin)
	b := h.browser()

	token := b.csrf("/login")
	r := b.post("/login", url.Values{"csrf": {token}, "username": {"alice"}, "password": {"wrong"}})
	if r.status != http.StatusUnauthorized {
		t.Errorf("bad password = %d, want 401", r.status)
	}

	// An attacker-supplied absolute URL must not become a post-login redirect.
	token = b.csrf("/login")
	r = b.post("/login", url.Values{
		"csrf": {token}, "username": {"alice"}, "password": {pw}, "next": {"https://evil.example/"},
	})
	if r.location != "/admin/" {
		t.Errorf("post-login redirect = %q, want /admin/ — an off-site next is an open redirect", r.location)
	}
}

func TestPostWithoutCSRFTokenIsRefused(t *testing.T) {
	h := newHarness(t)
	pw := h.seedUser("alice", store.RoleAdmin)
	b := h.browser()
	b.login("alice", pw)

	r := b.post("/admin/surveys", url.Values{"title": {"Sneaky"}})
	if r.status != http.StatusForbidden {
		t.Fatalf("POST without a CSRF token = %d, want 403", r.status)
	}
	if len(h.st.Surveys()) != 0 {
		t.Error("the survey was created despite the missing token")
	}
}

func TestBootstrapAccountMustChangePasswordBeforeAnythingElse(t *testing.T) {
	h := newHarness(t)
	if _, err := h.st.AddUserMustChange("admin", store.RoleAdmin, "generated-one"); err != nil {
		t.Fatal(err)
	}
	b := h.browser()
	b.login("admin", "generated-one")

	if r := b.get("/admin/"); r.location != "/account/password" {
		t.Fatalf("a must-change account reached /admin/ (redirect %q)", r.location)
	}
	token := b.csrf("/account/password")
	r := b.post("/account/password", url.Values{
		"csrf": {token}, "current": {"generated-one"}, "new": {"a better one"}, "confirm": {"a better one"},
	})
	if r.status != http.StatusSeeOther {
		t.Fatalf("password change = %d\n%s", r.status, r.body)
	}
	if r := b.get("/admin/"); r.status != 200 {
		t.Errorf("after changing the password, /admin/ = %d %q", r.status, r.location)
	}
}

func TestPasswordChangeSignsOutOtherDevices(t *testing.T) {
	h := newHarness(t)
	pw := h.seedUser("alice", store.RoleAdmin)
	laptop, phone := h.browser(), h.browser()
	laptop.login("alice", pw)
	phone.login("alice", pw)

	token := laptop.csrf("/account/password")
	if r := laptop.post("/account/password", url.Values{
		"csrf": {token}, "current": {pw}, "new": {"brand new one"}, "confirm": {"brand new one"},
	}); r.status != http.StatusSeeOther {
		t.Fatalf("password change = %d\n%s", r.status, r.body)
	}
	if r := laptop.get("/admin/"); r.status != 200 {
		t.Error("the device that changed the password was signed out")
	}
	if r := phone.get("/admin/"); r.status != http.StatusSeeOther {
		t.Error("the other device kept its session after a password change")
	}
}

func TestViewerCannotEditAndEditorCannotManageAccounts(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Pizza")
	viewerPW := h.seedUser("val", store.RoleViewer)
	editorPW := h.seedUser("eve", store.RoleEditor)

	viewer := h.browser()
	viewer.login("val", viewerPW)
	if r := viewer.get("/admin/s/" + sv.ID); r.status != 200 {
		t.Errorf("a viewer should see results: %d", r.status)
	}
	if r := viewer.get("/admin/s/" + sv.ID + "/export/summary.tsv"); r.status != 200 {
		t.Errorf("a viewer should be able to export: %d", r.status)
	}
	if r := viewer.get("/admin/s/" + sv.ID + "/edit"); r.status != http.StatusForbidden {
		t.Errorf("a viewer reached the edit form: %d", r.status)
	}

	editor := h.browser()
	editor.login("eve", editorPW)
	if r := editor.get("/admin/s/" + sv.ID + "/edit"); r.status != 200 {
		t.Errorf("an editor should reach the edit form: %d", r.status)
	}
	if r := editor.get("/admin/users"); r.status != http.StatusForbidden {
		t.Errorf("an editor reached account management: %d", r.status)
	}
}

func TestDraftSurveyIsInvisibleToThePublicButPreviewableByAnEditor(t *testing.T) {
	h := newHarness(t)
	sv, err := h.st.CreateSurvey("Secret plans", "", []string{"A", "B"})
	if err != nil {
		t.Fatal(err)
	}
	if r := h.browser().get("/s/" + sv.ID); r.status != http.StatusNotFound {
		t.Errorf("anonymous GET of a draft = %d, want 404", r.status)
	}
	pw := h.seedUser("eve", store.RoleEditor)
	editor := h.browser()
	editor.login("eve", pw)
	r := editor.get("/s/" + sv.ID)
	if r.status != 200 || !strings.Contains(r.body, "Preview") {
		t.Errorf("an editor should get a marked preview: %d", r.status)
	}
}

func TestVotingAndCookieDeduplication(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Pizza", "Tacos", "Salad")
	path := "/s/" + sv.ID

	alice := h.browser()
	token := alice.csrf(path)
	alice.follow(alice.post(path+"/vote", url.Values{
		"csrf": {token}, "choice": {sv.Options[0].ID, sv.Options[2].ID}, "comment": {"nothing spicy"},
	}))

	// The same browser voting again replaces, it does not add.
	token = alice.csrf(path)
	alice.follow(alice.post(path+"/vote", url.Values{"csrf": {token}, "choice": {sv.Options[1].ID}}))
	if n := h.st.Count(sv.ID); n != 1 {
		t.Fatalf("respondents after one browser voted twice = %d, want 1", n)
	}

	// A different browser is a different person.
	bob := h.browser()
	token = bob.csrf(path)
	bob.follow(bob.post(path+"/vote", url.Values{"csrf": {token}, "choice": {sv.Options[1].ID}}))
	if n := h.st.Count(sv.ID); n != 2 {
		t.Fatalf("respondents after a second browser voted = %d, want 2", n)
	}

	// Alice's ballot comes back pre-filled with what she last chose.
	body := alice.get(path).body
	if !checked(body, sv.Options[1].ID) {
		t.Error("the respondent's previous answer is not pre-selected when they return")
	}
	if checked(body, sv.Options[0].ID) {
		t.Error("a deselected option is still checked")
	}
	if !strings.Contains(body, "Update my response") {
		t.Error("a returning respondent should be told they are updating")
	}
}

func TestVoteRequiresCSRFToken(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Pizza")
	b := h.browser()
	b.get("/s/" + sv.ID) // pick up the voter cookie
	if r := b.post("/s/"+sv.ID+"/vote", url.Values{"choice": {sv.Options[0].ID}}); r.status != http.StatusForbidden {
		t.Fatalf("vote without a CSRF token = %d, want 403", r.status)
	}
	if h.st.Count(sv.ID) != 0 {
		t.Error("the vote was recorded despite the missing token")
	}
}

func TestClosedSurveyShowsClosedAndRefusesVotes(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Pizza")
	b := h.browser()
	token := b.csrf("/s/" + sv.ID)
	if _, err := h.st.UpdateSurvey(sv.ID, func(d *store.Survey) error { d.State = store.StateClosed; return nil }); err != nil {
		t.Fatal(err)
	}
	if body := b.get("/s/" + sv.ID).body; !strings.Contains(body, "no longer accepting responses") {
		t.Error("a closed survey should say so")
	}
	b.follow(b.post("/s/"+sv.ID+"/vote", url.Values{"csrf": {token}, "choice": {sv.Options[0].ID}}))
	if h.st.Count(sv.ID) != 0 {
		t.Error("a vote landed on a closed survey")
	}
}

func TestResultsAreHiddenFromRespondentsUnlessEnabled(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Pizza")
	b := h.browser()
	if r := b.get("/s/" + sv.ID + "/results"); r.status != http.StatusNotFound {
		t.Errorf("public results = %d, want 404 while sharing is off", r.status)
	}
	if _, err := h.st.UpdateSurvey(sv.ID, func(d *store.Survey) error { d.ShowResults = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if r := b.get("/s/" + sv.ID + "/results"); r.status != 200 {
		t.Errorf("public results = %d once sharing is on, want 200", r.status)
	}
}

func TestCommentsAreNeverShownToRespondents(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Pizza")
	if _, err := h.st.UpdateSurvey(sv.ID, func(d *store.Survey) error { d.ShowResults = true; return nil }); err != nil {
		t.Fatal(err)
	}
	const secret = "my private aside"
	if _, err := h.st.SaveResponse(sv.ID, "someone", []string{sv.Options[0].ID}, secret); err != nil {
		t.Fatal(err)
	}
	b := h.browser()
	for _, p := range []string{"/s/" + sv.ID, "/s/" + sv.ID + "/results"} {
		if strings.Contains(b.get(p).body, secret) {
			t.Errorf("%s leaked a comment to respondents", p)
		}
	}
	pw := h.seedUser("val", store.RoleViewer)
	admin := h.browser()
	admin.login("val", pw)
	if !strings.Contains(admin.get("/admin/s/"+sv.ID).body, secret) {
		t.Error("the admin results page should show comments")
	}
}

func TestWriteInIsModeratedThroughTheUI(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Pizza")
	path := "/s/" + sv.ID

	alice := h.browser()
	token := alice.csrf(path)
	alice.follow(alice.post(path+"/writein", url.Values{"csrf": {token}, "text": {"Sushi"}}))

	// Only Alice can see it, and it is marked.
	if body := alice.get(path).body; !strings.Contains(body, "Sushi") || !strings.Contains(body, "awaiting review") {
		t.Error("the submitter should see their own pending suggestion, marked as such")
	}
	if strings.Contains(h.browser().get(path).body, "Sushi") {
		t.Error("a pending suggestion is visible to other respondents")
	}

	pw := h.seedUser("eve", store.RoleEditor)
	editor := h.browser()
	editor.login("eve", pw)
	adminPath := "/admin/s/" + sv.ID
	body := editor.get(adminPath).body
	if !strings.Contains(body, "Sushi") {
		t.Fatal("the suggestion is not in the moderation queue")
	}

	sv, _ = h.st.Survey(sv.ID)
	pending := sv.PendingOptions()
	if len(pending) != 1 {
		t.Fatalf("pending options = %d, want 1", len(pending))
	}
	token = editor.csrf(adminPath)
	editor.follow(editor.post(adminPath+"/moderate", url.Values{
		"csrf": {token}, "option": {pending[0].ID}, "action": {"approve"}, "text": {"Sushi (delivery)"},
	}))

	results, _ := h.st.Tally(sv.ID)
	var found bool
	for _, r := range results {
		if r.Option.Text == "Sushi (delivery)" {
			found = true
			if r.Votes != 1 {
				t.Errorf("approved suggestion = %d votes, want 1 — the submitter's vote should carry over", r.Votes)
			}
		}
	}
	if !found {
		t.Error("the reworded, approved option is not in the tally")
	}
	if strings.Contains(h.browser().get(path).body, "awaiting review") {
		t.Error("an approved option is still marked pending")
	}
}

func TestEditFormRenamesRemovesAndAddsOptions(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Pizza", "Tacos")
	pw := h.seedUser("eve", store.RoleEditor)
	editor := h.browser()
	editor.login("eve", pw)
	editPath := "/admin/s/" + sv.ID + "/edit"

	token := editor.csrf(editPath)
	editor.follow(editor.post(editPath, url.Values{
		"csrf":                       {token},
		"title":                      {"Lunch, revised"},
		"description":                {"now with fewer tacos"},
		"opt_" + sv.Options[0].ID:    {"Pizza (any kind)"},
		"opt_" + sv.Options[1].ID:    {"Tacos"},
		"remove_" + sv.Options[1].ID: {"1"},
		"newopt":                     {"Sushi", "", "Ramen"},
		"allow_comment":              {"1"},
		"show_results":               {"1"},
	}))

	got, _ := h.st.Survey(sv.ID)
	if got.Title != "Lunch, revised" {
		t.Errorf("title = %q", got.Title)
	}
	if !got.ShowResults || got.AllowWriteIn {
		t.Errorf("checkboxes: ShowResults=%v AllowWriteIn=%v — an unticked box must clear the setting",
			got.ShowResults, got.AllowWriteIn)
	}
	byText := map[string]string{}
	for _, o := range got.Options {
		byText[o.Text] = o.Status
	}
	if byText["Pizza (any kind)"] != store.OptApproved {
		t.Errorf("renamed option status = %q", byText["Pizza (any kind)"])
	}
	if byText["Tacos"] != store.OptRemoved {
		t.Errorf("removed option status = %q, want removed", byText["Tacos"])
	}
	if byText["Sushi"] != store.OptApproved || byText["Ramen"] != store.OptApproved {
		t.Errorf("new options were not added: %v", byText)
	}
	if len(got.Options) != 4 {
		t.Errorf("options = %d, want 4 (blank new-option rows are ignored)", len(got.Options))
	}
}

func TestDeleteRequiresTheTitleTyped(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Pizza")
	pw := h.seedUser("eve", store.RoleEditor)
	editor := h.browser()
	editor.login("eve", pw)
	p := "/admin/s/" + sv.ID

	token := editor.csrf(p)
	editor.follow(editor.post(p+"/delete", url.Values{"csrf": {token}, "confirm": {"wrong"}}))
	if _, ok := h.st.Survey(sv.ID); !ok {
		t.Fatal("the survey was deleted with the wrong confirmation text")
	}
	token = editor.csrf(p)
	editor.follow(editor.post(p+"/delete", url.Values{"csrf": {token}, "confirm": {sv.Title}}))
	if _, ok := h.st.Survey(sv.ID); ok {
		t.Error("the survey survived a correct confirmation")
	}
}

func TestExportHeadersAndContent(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Pizza", "Tacos")
	if _, err := h.st.SaveResponse(sv.ID, "v1", []string{sv.Options[0].ID}, "hi"); err != nil {
		t.Fatal(err)
	}
	pw := h.seedUser("val", store.RoleViewer)
	b := h.browser()
	b.login("val", pw)

	r := b.get("/admin/s/" + sv.ID + "/export/responses.tsv")
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/tab-separated-values") {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := r.header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, "lunch-responses-") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if !strings.HasPrefix(r.body, "response_id\tsubmitted\tupdated\tPizza\tTacos\t") {
		t.Errorf("body starts %q", strings.SplitN(r.body, "\n", 2)[0])
	}
	if r := b.get("/admin/s/" + sv.ID + "/export/nonsense.tsv"); r.status != http.StatusNotFound {
		t.Errorf("unknown export kind = %d, want 404", r.status)
	}
}

func TestUnknownSurveyIs404(t *testing.T) {
	h := newHarness(t)
	if r := h.browser().get("/s/does-not-exist"); r.status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", r.status)
	}
}

func TestErrorMessageAppearsOnThePageThatCausedIt(t *testing.T) {
	h := newHarness(t)
	pw := h.seedUser("alice", store.RoleAdmin)
	b := h.browser()

	// A failed sign-in must explain itself on the sign-in page, not silently
	// stash the message for whatever page the user visits next.
	token := b.csrf("/login")
	r := b.post("/login", url.Values{"csrf": {token}, "username": {"alice"}, "password": {"nope"}})
	if !strings.Contains(r.body, "Incorrect username or password") {
		t.Error("the sign-in failure message is missing from the response that failed")
	}
	if strings.Contains(b.get("/login").body, "Incorrect username or password") {
		t.Error("the message leaked onto a later page load")
	}

	// Same for a rejected password change.
	b.login("alice", pw)
	token = b.csrf("/account/password")
	r = b.post("/account/password", url.Values{
		"csrf": {token}, "current": {"wrong"}, "new": {"whatever12"}, "confirm": {"whatever12"},
	})
	if !strings.Contains(r.body, "current password is not correct") {
		t.Error("the password-change failure message is missing from the response that failed")
	}
	token = b.csrf("/account/password")
	r = b.post("/account/password", url.Values{
		"csrf": {token}, "current": {pw}, "new": {"onething12"}, "confirm": {"another12"},
	})
	if !strings.Contains(r.body, "do not match") {
		t.Error("the mismatched-password message is missing")
	}
}

// seedDraft creates a survey and leaves it unpublished.
func (h *harness) seedDraft(opts ...string) *store.Survey {
	h.t.Helper()
	sv, err := h.st.CreateSurvey("Lunch", "Pick anything you'd eat", opts)
	if err != nil {
		h.t.Fatal(err)
	}
	return sv
}

func TestFrontPageIsASplashWithAQuietSignInLink(t *testing.T) {
	h := newHarness(t)
	body := h.browser().get("/").body
	if !strings.Contains(body, "which of these are you interested in") {
		t.Error("the front page does not explain what the site is")
	}
	if !strings.Contains(body, `data-testid="signin-link"`) {
		t.Error("no sign-in link in the header for an anonymous visitor")
	}
	// The call to action is the explanation, not the login form.
	if strings.Contains(body, `name="password"`) {
		t.Error("the front page should not be a login form")
	}
}

func TestEditorCanTakeADraftAndPublishingDiscardsIt(t *testing.T) {
	h := newHarness(t)
	sv := h.seedDraft("Pizza", "Tacos")
	pw := h.seedUser("eve", store.RoleEditor)
	editor := h.browser()
	editor.login("eve", pw)
	path := "/s/" + sv.ID

	// The draft renders a usable ballot for the editor, not a "closed" notice.
	body := editor.get(path).body
	if !strings.Contains(body, `data-testid="preview-banner"`) {
		t.Fatal("no preview banner on a draft")
	}
	if !strings.Contains(body, `data-testid="submit-vote"`) {
		t.Fatal("a draft preview offers no way to take the survey")
	}
	if strings.Contains(body, `data-testid="closed"`) {
		t.Error("a draft preview claims the survey is closed")
	}

	token := editor.csrf(path)
	editor.follow(editor.post(path+"/vote", url.Values{"csrf": {token}, "choice": {sv.Options[0].ID}}))
	if n := h.st.Count(sv.ID); n != 1 {
		t.Fatalf("test response not recorded: respondents = %d", n)
	}

	// The public still cannot see it at all.
	if r := h.browser().get(path); r.status != http.StatusNotFound {
		t.Errorf("anonymous GET of a draft = %d, want 404", r.status)
	}

	// Publishing throws the test data away and says so.
	token = editor.csrf("/admin/s/" + sv.ID)
	r := editor.follow(editor.post("/admin/s/"+sv.ID+"/state", url.Values{"csrf": {token}, "state": {"open"}}))
	if !strings.Contains(r.body, "test response") || !strings.Contains(r.body, "discarded") {
		t.Error("publishing did not report discarding the draft's test responses")
	}
	if n := h.st.Count(sv.ID); n != 0 {
		t.Errorf("respondents after publishing = %d, want 0", n)
	}
}

func TestViewerCannotTakeADraft(t *testing.T) {
	h := newHarness(t)
	sv := h.seedDraft("Pizza")
	pw := h.seedUser("val", store.RoleViewer)
	viewer := h.browser()
	viewer.login("val", pw)
	if r := viewer.get("/s/" + sv.ID); r.status != http.StatusNotFound {
		t.Errorf("a viewer reached a draft ballot: %d", r.status)
	}
}

// inviteURL creates an invite as an admin through the UI and returns the link.
func (h *harness) inviteURL(b *browser, role store.Role) string {
	h.t.Helper()
	token := b.csrf("/admin/users")
	r := b.post("/admin/invites", url.Values{
		"csrf": {token}, "action": {"create"}, "role": {string(role)}, "note": {"come help"}, "days": {"168"},
	})
	if r.status != http.StatusSeeOther {
		h.t.Fatalf("create invite: %d\n%s", r.status, r.body)
	}
	loc := r.location
	i := strings.Index(loc, "new_invite=")
	if i < 0 {
		h.t.Fatalf("no invite token in redirect %q", loc)
	}
	tok, err := url.QueryUnescape(loc[i+len("new_invite="):])
	if err != nil {
		h.t.Fatal(err)
	}
	return "/invite/" + tok
}

func TestInviteFlowEndToEnd(t *testing.T) {
	h := newHarness(t)
	adminPW := h.seedUser("root", store.RoleAdmin)
	admin := h.browser()
	admin.login("root", adminPW)

	link := h.inviteURL(admin, store.RoleEditor)

	// The link is shown once, on the accounts page, and only there.
	if !strings.Contains(admin.get("/admin/users"+"?new_invite="+strings.TrimPrefix(link, "/invite/")).body,
		`data-testid="invite-url"`) {
		t.Error("the new invitation link is not displayed to its creator")
	}

	// Anyone with the link can open it without an account.
	guest := h.browser()
	if r := guest.get(link); r.status != 200 || !strings.Contains(r.body, "root") {
		t.Fatalf("invite page = %d; should name the inviter", r.status)
	}

	token := guest.csrf(link)
	r := guest.post(link, url.Values{
		"csrf": {token}, "username": {"newbie"}, "password": {"password123"}, "confirm": {"password123"},
	})
	if r.status != http.StatusSeeOther || r.location != "/account/pending" {
		t.Fatalf("claim = %d %q, want a redirect to /account/pending\n%s", r.status, r.location, r.body)
	}

	// They are signed in but can reach nothing except the waiting page.
	if !strings.Contains(guest.get("/account/pending").body, `data-testid="pending-notice"`) {
		t.Error("no waiting-for-approval page")
	}
	if got := guest.get("/admin/"); got.location != "/account/pending" {
		t.Errorf("a pending account reached /admin/ (redirect %q)", got.location)
	}
	if got := guest.get("/admin/users"); got.location != "/account/pending" {
		t.Errorf("a pending account reached account management (redirect %q)", got.location)
	}

	// The admin sees them in the queue and approves them as a viewer, not the
	// editor role the invite suggested.
	body := admin.get("/admin/users").body
	if !strings.Contains(body, `data-testid="pending-user"`) || !strings.Contains(body, "newbie") {
		t.Fatal("the request is not in the approval queue")
	}
	token = admin.csrf("/admin/users")
	admin.follow(admin.post("/admin/users", url.Values{
		"csrf": {token}, "action": {"approve"}, "username": {"newbie"}, "role": {"viewer"},
	}))
	u, ok := h.st.User("newbie")
	if !ok || u.Pending || u.Role != store.RoleViewer {
		t.Fatalf("after approval: %+v", u)
	}

	// And now the account works, with the role the approver chose.
	if r := guest.get("/admin/"); r.status != 200 {
		t.Errorf("approved account cannot reach /admin/: %d %q", r.status, r.location)
	}
	if r := guest.get("/admin/users"); r.status != http.StatusForbidden {
		t.Errorf("approved as viewer but reached account management: %d", r.status)
	}
}

func TestInviteLinkIsOneTimeOnly(t *testing.T) {
	h := newHarness(t)
	pw := h.seedUser("root", store.RoleAdmin)
	admin := h.browser()
	admin.login("root", pw)
	link := h.inviteURL(admin, store.RoleEditor)

	first := h.browser()
	token := first.csrf(link)
	if r := first.post(link, url.Values{
		"csrf": {token}, "username": {"alice"}, "password": {"password123"}, "confirm": {"password123"},
	}); r.status != http.StatusSeeOther {
		t.Fatalf("first claim = %d", r.status)
	}

	// The link is spent: it no longer even renders.
	second := h.browser()
	if r := second.get(link); r.status != http.StatusNotFound {
		t.Errorf("a used invitation link still renders: %d", r.status)
	}
	second.get("/login") // pick up a cookie so a CSRF token exists
	if r := second.post(link, url.Values{
		"csrf": {second.csrf("/login")}, "username": {"mallory"},
		"password": {"password123"}, "confirm": {"password123"},
	}); r.status == http.StatusSeeOther {
		t.Error("a used invitation link produced a second account")
	}
	if _, ok := h.st.User("mallory"); ok {
		t.Error("the second account exists")
	}
}

func TestInviteMismatchedPasswordsAndRevocation(t *testing.T) {
	h := newHarness(t)
	pw := h.seedUser("root", store.RoleAdmin)
	admin := h.browser()
	admin.login("root", pw)
	link := h.inviteURL(admin, store.RoleEditor)

	guest := h.browser()
	token := guest.csrf(link)
	r := guest.post(link, url.Values{
		"csrf": {token}, "username": {"alice"}, "password": {"password123"}, "confirm": {"different123"},
	})
	if r.status != http.StatusBadRequest || !strings.Contains(r.body, "do not match") {
		t.Errorf("mismatched passwords = %d, message shown: %v", r.status, strings.Contains(r.body, "do not match"))
	}
	if _, ok := h.st.User("alice"); ok {
		t.Fatal("the account was created despite the mismatch")
	}
	// A failed attempt must not spend the link.
	if r := guest.get(link); r.status != 200 {
		t.Error("a failed claim consumed the invitation")
	}

	// Revoking closes it.
	invites := h.st.Invites()
	if len(invites) != 1 {
		t.Fatalf("invites = %d", len(invites))
	}
	tok := admin.csrf("/admin/users")
	admin.follow(admin.post("/admin/invites", url.Values{
		"csrf": {tok}, "action": {"revoke"}, "id": {invites[0].ID},
	}))
	if r := guest.get(link); r.status != http.StatusNotFound {
		t.Errorf("a revoked link still renders: %d", r.status)
	}
}

func TestOnlyAdminsCanInvite(t *testing.T) {
	h := newHarness(t)
	pw := h.seedUser("eve", store.RoleEditor)
	editor := h.browser()
	editor.login("eve", pw)
	token := editor.csrf("/admin/")
	if r := editor.post("/admin/invites", url.Values{
		"csrf": {token}, "action": {"create"}, "role": {"admin"},
	}); r.status != http.StatusForbidden {
		t.Errorf("an editor created an invitation: %d", r.status)
	}
	if len(h.st.Invites()) != 0 {
		t.Error("the invitation was created anyway")
	}
}

var optionOrderRe = regexp.MustCompile(`id="opt-([a-z0-9]+)"`)

// ballotOrder reads the option IDs in the order the page presents them.
func ballotOrder(body string) []string {
	var out []string
	for _, m := range optionOrderRe.FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestBallotOrderIsShuffledPerRespondentButStableForEachOne(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Alpha", "Bravo", "Charlie", "Delta", "Echo", "Foxtrot", "Golf", "Hotel")
	path := "/s/" + sv.ID

	// One person's order does not move, across reloads or across submitting.
	alice := h.browser()
	first := ballotOrder(alice.get(path).body)
	if len(first) != 8 {
		t.Fatalf("got %d options on the ballot, want 8", len(first))
	}
	for range 5 {
		if got := ballotOrder(alice.get(path).body); strings.Join(got, ",") != strings.Join(first, ",") {
			t.Fatalf("a reload reshuffled the ballot:\n%v\n%v", first, got)
		}
	}
	token := alice.csrf(path)
	alice.follow(alice.post(path+"/vote", url.Values{"csrf": {token}, "choice": {first[0]}}))
	if got := ballotOrder(alice.get(path).body); strings.Join(got, ",") != strings.Join(first, ",") {
		t.Errorf("the order changed after voting; the respondent's ticks would appear to move")
	}

	// Different people get different orders.
	seen := map[string]bool{strings.Join(first, ","): true}
	for range 6 {
		seen[strings.Join(ballotOrder(h.browser().get(path).body), ",")] = true
	}
	if len(seen) < 2 {
		t.Error("every browser saw the same order; the ballot is not being randomised")
	}
}

func TestRandomizeCanBeTurnedOffPerSurvey(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Alpha", "Bravo", "Charlie", "Delta", "Echo")
	if _, err := h.st.UpdateSurvey(sv.ID, func(d *store.Survey) error { d.NoRandomize = true; return nil }); err != nil {
		t.Fatal(err)
	}
	want := make([]string, len(sv.Options))
	for i, o := range sv.Options {
		want[i] = o.ID
	}
	for range 5 {
		got := ballotOrder(h.browser().get("/s/" + sv.ID).body)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("with randomising off, order = %v, want the editor's order %v", got, want)
		}
	}
}

func TestEditFormTogglesRandomising(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Alpha", "Bravo")
	pw := h.seedUser("eve", store.RoleEditor)
	editor := h.browser()
	editor.login("eve", pw)
	editPath := "/admin/s/" + sv.ID + "/edit"

	// The box is ticked to begin with, because the default is on.
	if !regexp.MustCompile(`name="randomize" value="1"\s+checked`).MatchString(editor.get(editPath).body) {
		t.Error("the randomise box should start ticked, since randomising is the default")
	}

	// Saving without it turns randomising off.
	token := editor.csrf(editPath)
	editor.follow(editor.post(editPath, url.Values{
		"csrf": {token}, "title": {sv.Title}, "allow_comment": {"1"},
	}))
	got, _ := h.st.Survey(sv.ID)
	if got.Randomize() {
		t.Error("unticking the box did not turn randomising off")
	}

	// And saving with it turns it back on.
	token = editor.csrf(editPath)
	editor.follow(editor.post(editPath, url.Values{
		"csrf": {token}, "title": {sv.Title}, "randomize": {"1"},
	}))
	got, _ = h.st.Survey(sv.ID)
	if !got.Randomize() {
		t.Error("ticking the box did not turn randomising back on")
	}
}

func TestHTTPSRedirect(t *testing.T) {
	h := newHarness(t)
	h.web.cfg.RedirectHTTPS = true
	b := h.browser()

	// A plain-HTTP visit is sent to https, keeping path and query.
	r := b.get("/s/abc?x=1")
	if r.status != http.StatusPermanentRedirect {
		t.Fatalf("status = %d, want 308 — a 302 would turn a POSTed vote into a GET", r.status)
	}
	if !strings.HasPrefix(r.location, "https://") || !strings.HasSuffix(r.location, "/s/abc?x=1") {
		t.Errorf("Location = %q, want an https URL preserving path and query", r.location)
	}

	// A request the proxy already terminated is served normally.
	req, _ := http.NewRequest("GET", b.base+"/", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	if got := b.do(req); got.status != http.StatusOK {
		t.Errorf("X-Forwarded-Proto: https = %d, want 200", got.status)
	}

	// The health check must never redirect: kubelet probes the pod directly
	// over plain HTTP, and a redirect would fail every probe.
	if got := b.get("/healthz"); got.status != http.StatusOK {
		t.Errorf("/healthz = %d, want 200 — probes would fail", got.status)
	}
}

func TestNoRedirectWhenDisabled(t *testing.T) {
	h := newHarness(t)
	if r := h.browser().get("/"); r.status != http.StatusOK {
		t.Errorf("status = %d, want 200 when redirecting is off", r.status)
	}
}

// A browser follows WHATWG URL rules, where a backslash is a slash. "/\evil.com"
// looks same-origin to a naive prefix check and resolves to https://evil.com/.
func TestOpenRedirectVariants(t *testing.T) {
	for _, next := range []string{
		"//evil.com", `/\evil.com`, `/\/evil.com`, `/\\evil.com`,
		"https://evil.com", "http:evil.com", `\\evil.com`, "javascript:alert(1)",
	} {
		if got := safeNext(next); got != "" {
			t.Errorf("safeNext(%q) = %q, want it rejected", next, got)
		}
	}
	for _, next := range []string{"/admin/", "/admin/s/abc?x=1", "/s/abc"} {
		if got := safeNext(next); got != next {
			t.Errorf("safeNext(%q) = %q, want it kept", next, got)
		}
	}
}

func TestLoginDoesNotRedirectOffSite(t *testing.T) {
	h := newHarness(t)
	pw := h.seedUser("alice", store.RoleAdmin)
	b := h.browser()
	token := b.csrf("/login")
	r := b.post("/login", url.Values{
		"csrf": {token}, "username": {"alice"}, "password": {pw}, "next": {`/\evil.com`},
	})
	if r.location != "/admin/" {
		t.Errorf("post-login redirect = %q, want /admin/", r.location)
	}
}

// A pending account holds a valid session. Anything deciding access from that
// session has to consult the pending gate too, or it exempts itself from it.
func TestPendingAccountCannotReachOrVoteOnADraft(t *testing.T) {
	h := newHarness(t)
	if _, err := h.st.AddUser("root", store.RoleAdmin, "password123"); err != nil {
		t.Fatal(err)
	}
	_, token, err := h.st.CreateInvite("root", store.RoleEditor, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.ClaimInvite(token, "newbie", "password123"); err != nil {
		t.Fatal(err)
	}
	sv := h.seedDraft("Pizza")

	b := h.browser()
	b.login("newbie", "password123")
	if r := b.get("/s/" + sv.ID); r.status != http.StatusNotFound {
		t.Errorf("a pending account reached a draft ballot: %d", r.status)
	}
	if h.st.Count(sv.ID) != 0 {
		t.Error("a pending account recorded a response on a draft")
	}

	// The same must hold for an account that has not yet changed its
	// bootstrap password.
	if _, err := h.st.AddUserMustChange("boot", store.RoleAdmin, "generated-one"); err != nil {
		t.Fatal(err)
	}
	c := h.browser()
	c.login("boot", "generated-one")
	if r := c.get("/s/" + sv.ID); r.status != http.StatusNotFound {
		t.Errorf("a must-change-password account reached a draft ballot: %d", r.status)
	}
}

// The whole point of signing is that a client cannot mint identities offline.
func TestInventedVoterCookieIsRejected(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Pizza", "Tacos")
	path := "/s/" + sv.ID

	// Vote once legitimately.
	honest := h.browser()
	token := honest.csrf(path)
	honest.follow(honest.post(path+"/vote", url.Values{"csrf": {token}, "choice": {sv.Options[0].ID}}))
	if n := h.st.Count(sv.ID); n != 1 {
		t.Fatalf("respondents = %d, want 1", n)
	}

	// Now try what the review did: invent cookie values and vote repeatedly.
	// Each invented value must be rejected and replaced with a signed one, so
	// the ballot cannot be stuffed by editing a cookie.
	for i := range 10 {
		b := h.browser()
		forged := fmt.Sprintf("forged-voter-value-%d", i)
		u, _ := url.Parse(b.base)
		b.c.Jar.SetCookies(u, []*http.Cookie{{Name: voterCookie, Value: forged, Path: "/"}})
		page := b.get(path)
		if strings.Contains(page.body, forged) {
			t.Fatal("the forged token was echoed back, so it was accepted")
		}
	}

	// A signed cookie the server issued still works across requests.
	before := ballotOrder(honest.get(path).body)
	after := ballotOrder(honest.get(path).body)
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Error("a legitimately issued voter cookie stopped being recognised")
	}
	if n := h.st.Count(sv.ID); n != 1 {
		t.Errorf("respondents = %d, want 1 — forged cookies should not create voters", n)
	}
}

func TestVoterCookieSignatureIsVerified(t *testing.T) {
	h := newHarness(t)
	for _, bad := range []string{
		"", "short", "no-signature-at-all-here",
		"abcdefghijklmnopq.wrongmac",
		"abcdefghijklmnopq.",
		".onlymac",
	} {
		if _, ok := h.web.verifyVoterToken(bad); ok {
			t.Errorf("verifyVoterToken(%q) accepted an unsigned value", bad)
		}
	}
	// Round-trip a genuine one.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	tok := h.web.voterToken(w, r)
	signed := w.Result().Cookies()[0].Value
	got, ok := h.web.verifyVoterToken(signed)
	if !ok || got != tok {
		t.Errorf("round trip failed: got %q ok=%v, want %q", got, ok, tok)
	}
}

func TestLoginIsRateLimited(t *testing.T) {
	h := newHarness(t)
	pw := h.seedUser("alice", store.RoleAdmin)
	b := h.browser()

	// Only failures are billed, so a run of successful sign-ins must not
	// consume the budget.
	for range 20 {
		b2 := h.browser()
		b2.login("alice", pw)
	}
	// Each failed attempt costs a full PBKDF2 verification, so that is the
	// path that needs a ceiling.
	var limited bool
	for range 30 {
		token := b.csrf("/login")
		r := b.post("/login", url.Values{"csrf": {token}, "username": {"alice"}, "password": {"wrong"}})
		if r.status == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("30 failed sign-in attempts were never rate limited")
	}
	// The refusal tells a well-behaved client when to come back.
	token := b.csrf("/login")
	r := b.post("/login", url.Values{"csrf": {token}, "username": {"alice"}, "password": {pw}})
	if r.status == http.StatusTooManyRequests && r.header.Get("Retry-After") == "" {
		t.Error("429 without a Retry-After header")
	}
}

func TestNewVoterIdentitiesAreRateLimited(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Pizza")
	// A caller who already holds a cookie is never throttled, however often
	// they vote — that is the whole point of metering issuance instead.
	b := h.browser()
	for range 50 {
		if r := b.get("/s/" + sv.ID); r.status != http.StatusOK {
			t.Fatalf("an established voter was throttled: %d", r.status)
		}
	}
	// Fresh identities are bounded.
	h.web.voterLimit = newLimiter(5, time.Minute)
	var limited bool
	for range 20 {
		if h.browser().get("/s/"+sv.ID).status == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("unlimited new voter identities could be minted from one address")
	}
}

func TestOversizedBodyIsRefused(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Pizza")
	b := h.browser()
	token := b.csrf("/s/" + sv.ID)

	form := url.Values{"csrf": {token}, "comment": {strings.Repeat("x", MaxBody+1024)}}
	r := b.post("/s/"+sv.ID+"/vote", form)
	if r.status == http.StatusSeeOther {
		got, _ := h.st.ResponseFor(sv.ID, h.st.VoterID(sv.ID, "irrelevant"))
		_ = got
		t.Errorf("a body over the %d byte cap was accepted (status %d)", MaxBody, r.status)
	}
}

func TestCookiesUseHostPrefixWhenSecure(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	secure, err := New(st, Config{SecureCookies: true, Location: time.UTC, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if got := secure.cookieName(sessionCookie); got != "__Host-qs_session" {
		t.Errorf("secure cookie name = %q, want the __Host- prefix", got)
	}
	// The prefix is invalid without HTTPS, so it must not appear then.
	plain, err := New(st, Config{SecureCookies: false, Location: time.UTC, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if got := plain.cookieName(sessionCookie); got != "qs_session" {
		t.Errorf("plain cookie name = %q, want no prefix", got)
	}

	// A __Host- cookie the browser will actually accept: Secure, Path=/, no Domain.
	c := secure.cookie(sessionCookie, "v", time.Hour)
	if !c.Secure || c.Path != "/" || c.Domain != "" {
		t.Errorf("__Host- cookie violates its own requirements: %+v", c)
	}
}

func TestSignOutInvalidatesACapturedCookie(t *testing.T) {
	h := newHarness(t)
	pw := h.seedUser("alice", store.RoleAdmin)
	b := h.browser()
	b.login("alice", pw)

	// A copy of the cookie, as an attacker who captured it would hold.
	u, _ := url.Parse(b.base)
	var stolen *http.Cookie
	for _, c := range b.c.Jar.Cookies(u) {
		if c.Name == sessionCookie {
			stolen = c
		}
	}
	if stolen == nil {
		t.Fatal("no session cookie to capture")
	}
	if r := b.get("/admin/"); r.status != 200 {
		t.Fatalf("signed-in request = %d", r.status)
	}

	token := b.csrf("/admin/")
	b.follow(b.post("/logout", url.Values{"csrf": {token}}))

	thief := h.browser()
	thief.c.Jar.SetCookies(u, []*http.Cookie{stolen})
	if r := thief.get("/admin/"); r.status == 200 {
		t.Error("a cookie captured before sign-out still works; sign-out only cleared it locally")
	}
}

func TestHSTSOnlyWhenSecure(t *testing.T) {
	h := newHarness(t) // SecureCookies false
	if got := h.browser().get("/").header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS sent over plain HTTP: %q", got)
	}
	h.web.cfg.SecureCookies = true
	got := h.browser().get("/").header.Get("Strict-Transport-Security")
	if got == "" {
		t.Fatal("no HSTS header when serving over TLS")
	}
	// Neither is this application's to promise on behalf of sibling hostnames.
	if strings.Contains(got, "includeSubDomains") || strings.Contains(got, "preload") {
		t.Errorf("HSTS = %q; must not commit sibling hostnames", got)
	}
}

// People type passwords into the username box. Echoing the submitted string
// into a log puts those where they persist and are widely read.
func TestFailedLoginDoesNotLogWhatWasTyped(t *testing.T) {
	var buf strings.Builder
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddUser("alice", store.RoleAdmin, "password123"); err != nil {
		t.Fatal(err)
	}
	srv, err := New(st, Config{
		Location: time.UTC,
		Logger:   slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	h := &harness{t: t, st: st, srv: ts, web: srv}
	b := h.browser()

	// A password fat-fingered into the username field.
	const leaked = "hunter2-my-actual-password"
	token := b.csrf("/login")
	b.post("/login", url.Values{"csrf": {token}, "username": {leaked}, "password": {""}})
	if strings.Contains(buf.String(), leaked) {
		t.Errorf("the submitted username reached the log:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "<unknown>") {
		t.Error("a failed sign-in for a non-existent account should still be logged, anonymised")
	}

	// A real account is named, because knowing which account is under attack
	// is the part with operational value.
	token = b.csrf("/login")
	b.post("/login", url.Values{"csrf": {token}, "username": {"alice"}, "password": {"wrong"}})
	if !strings.Contains(buf.String(), "user=alice") {
		t.Error("a failed sign-in against a real account should name it")
	}
}

func TestAdminCannotSetAPasswordButCanHandOutAResetLink(t *testing.T) {
	h := newHarness(t)
	pw := h.seedUser("root", store.RoleAdmin)
	h.seedUser("alice", store.RoleEditor)
	admin := h.browser()
	admin.login("root", pw)

	// The old "set their password" action is gone, and asking for it does not
	// quietly succeed.
	token := admin.csrf("/admin/users")
	admin.follow(admin.post("/admin/users", url.Values{
		"csrf": {token}, "action": {"password"}, "username": {"alice"}, "password": {"chosen-by-admin"},
	}))
	if _, ok := h.st.Authenticate("alice", "chosen-by-admin"); ok {
		t.Fatal("an admin set another account's password")
	}
	if strings.Contains(admin.get("/admin/users").body, `name="password" type="password" placeholder="new password"`) {
		t.Error("the accounts page still offers a password field for other people")
	}

	// What they can do is hand over a link, rendered once, without the secret
	// passing through a query string.
	token = admin.csrf("/admin/users")
	r := admin.post("/admin/users", url.Values{
		"csrf": {token}, "action": {"reset-link"}, "username": {"alice"},
	})
	if r.status != http.StatusOK {
		t.Fatalf("reset-link = %d, want the page rendered directly", r.status)
	}
	m := regexp.MustCompile(`data-testid="reset-url">([^<]+)<`).FindStringSubmatch(r.body)
	if m == nil {
		t.Fatal("no reset link shown")
	}
	link := strings.TrimPrefix(strings.TrimSpace(m[1]), h.srv.URL)
	if strings.Contains(r.location, "new_reset") {
		t.Error("the reset token went through a query string")
	}

	// Alice, not the admin, chooses the password.
	alice := h.browser()
	page := alice.get(link)
	if page.status != 200 || !strings.Contains(page.body, "alice") {
		t.Fatalf("reset page = %d", page.status)
	}
	token = alice.csrf(link)
	if r := alice.post(link, url.Values{
		"csrf": {token}, "password": {"alice picks this"}, "confirm": {"alice picks this"},
	}); r.status != http.StatusSeeOther {
		t.Fatalf("setting the password = %d\n%s", r.status, r.body)
	}
	if _, ok := h.st.Authenticate("alice", "alice picks this"); !ok {
		t.Error("the password alice chose does not work")
	}
	// Spent.
	if r := alice.get(link); r.status != http.StatusNotFound {
		t.Errorf("a used reset link still renders: %d", r.status)
	}
}

func TestAccountDeleteRequiresTheUsernameTyped(t *testing.T) {
	h := newHarness(t)
	pw := h.seedUser("root", store.RoleAdmin)
	h.seedUser("alice", store.RoleEditor)
	admin := h.browser()
	admin.login("root", pw)

	token := admin.csrf("/admin/users")
	r := admin.follow(admin.post("/admin/users", url.Values{
		"csrf": {token}, "action": {"delete"}, "username": {"alice"}, "confirm": {"wrong"},
	}))
	if _, ok := h.st.User("alice"); !ok {
		t.Fatal("the account was deleted without a matching confirmation")
	}
	if !strings.Contains(r.body, "type the username exactly") {
		t.Error("no explanation of why the deletion did not happen")
	}
	// Blank confirmation must not pass either.
	token = admin.csrf("/admin/users")
	admin.follow(admin.post("/admin/users", url.Values{
		"csrf": {token}, "action": {"delete"}, "username": {"alice"},
	}))
	if _, ok := h.st.User("alice"); !ok {
		t.Fatal("an empty confirmation deleted the account")
	}

	token = admin.csrf("/admin/users")
	admin.follow(admin.post("/admin/users", url.Values{
		"csrf": {token}, "action": {"delete"}, "username": {"alice"}, "confirm": {"alice"},
	}))
	if _, ok := h.st.User("alice"); ok {
		t.Error("the account survived a correct confirmation")
	}
}

// Choosing someone's first password is the same power as resetting theirs: you
// can sign in as them. Invitations are the only web path in.
func TestAdminCannotCreateAnAccountWithAPassword(t *testing.T) {
	h := newHarness(t)
	pw := h.seedUser("root", store.RoleAdmin)
	admin := h.browser()
	admin.login("root", pw)

	token := admin.csrf("/admin/users")
	r := admin.follow(admin.post("/admin/users", url.Values{
		"csrf": {token}, "action": {"add"}, "username": {"planted"},
		"password": {"chosen-by-admin"}, "role": {"admin"},
	}))
	if _, ok := h.st.User("planted"); ok {
		t.Fatal("an admin created an account with a password they chose")
	}
	if !strings.Contains(r.body, "created by invitation") {
		t.Error("the refusal should say what to do instead")
	}
	// And the form is not offered.
	body := admin.get("/admin/users").body
	for _, id := range []string{`data-testid="add-user"`, `data-testid="new-password"`} {
		if strings.Contains(body, id) {
			t.Errorf("the accounts page still offers %s", id)
		}
	}
	// Invitations still work, and still produce an account nobody else has a
	// password for.
	link := h.inviteURL(admin, store.RoleEditor)
	guest := h.browser()
	token = guest.csrf(link)
	if r := guest.post(link, url.Values{
		"csrf": {token}, "username": {"invited"},
		"password": {"chosen-by-them"}, "confirm": {"chosen-by-them"},
	}); r.status != http.StatusSeeOther {
		t.Fatalf("claiming an invitation = %d", r.status)
	}
	if _, ok := h.st.User("invited"); !ok {
		t.Error("invitation flow broke")
	}
}

// tallyRow returns the rendered <tr> for one option, or "" if it is absent.
func tallyRow(body, option string) string {
	re := regexp.MustCompile(`(?s)<tr data-testid="tally-row" data-option="` + regexp.QuoteMeta(option) + `">.*?</tr>`)
	return re.FindString(body)
}

func TestTallyShowsHowManyWereShownEachOptionAndTheirInterest(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Pizza")
	path := "/s/" + sv.ID

	alice, bob := h.browser(), h.browser()
	for _, b := range []*browser{alice, bob} {
		token := b.csrf(path)
		b.follow(b.post(path+"/vote", url.Values{"csrf": {token}, "choice": {sv.Options[0].ID}}))
	}
	token := alice.csrf(path)
	alice.follow(alice.post(path+"/writein", url.Values{"csrf": {token}, "text": {"Sushi"}}))

	pw := h.seedUser("eve", store.RoleEditor)
	editor := h.browser()
	editor.login("eve", pw)
	adminPath := "/admin/s/" + sv.ID
	sv, _ = h.st.Survey(sv.ID)
	pending := sv.PendingOptions()
	token = editor.csrf(adminPath)
	editor.follow(editor.post(adminPath+"/moderate", url.Values{
		"csrf": {token}, "option": {pending[0].ID}, "action": {"approve"}, "text": {"Sushi"},
	}))

	body := editor.get(adminPath).body
	pizza, sushi := tallyRow(body, "Pizza"), tallyRow(body, "Sushi")
	if pizza == "" || sushi == "" {
		t.Fatalf("tally rows missing:\n%s", body)
	}
	if !strings.Contains(pizza, `data-testid="shown">2<`) || !strings.Contains(pizza, `data-testid="interest">100%<`) {
		t.Errorf("Pizza row should show shown 2 and interest 100%%:\n%s", pizza)
	}
	// Only Alice has been shown Sushi, so its share of respondents is 50% but
	// its interest is 100%.
	if !strings.Contains(sushi, `data-testid="shown">1<`) || !strings.Contains(sushi, `data-testid="interest">100%<`) {
		t.Errorf("Sushi row should show shown 1 and interest 100%%:\n%s", sushi)
	}
	if !strings.Contains(sushi, ">50%<") {
		t.Errorf("Sushi row should still show its 50%% share of all respondents:\n%s", sushi)
	}
}

func TestTallyShowsAnUnshownOptionsInterestAsNonNumeric(t *testing.T) {
	h := newHarness(t)
	sv := h.seedSurvey("Pizza")
	path := "/s/" + sv.ID

	alice, bob := h.browser(), h.browser()
	for _, b := range []*browser{alice, bob} {
		token := b.csrf(path)
		b.follow(b.post(path+"/vote", url.Values{"csrf": {token}, "choice": {sv.Options[0].ID}}))
	}

	// Both respondents have already submitted. An editor now adds a brand
	// new approved option directly through the edit form, so it lands on
	// nobody's ballot: Shown must be 0, not "0% interest".
	pw := h.seedUser("eve", store.RoleEditor)
	editor := h.browser()
	editor.login("eve", pw)
	editPath := "/admin/s/" + sv.ID + "/edit"
	token := editor.csrf(editPath)
	editor.follow(editor.post(editPath, url.Values{
		"csrf":   {token},
		"title":  {sv.Title},
		"newopt": {"Kebab"},
	}))

	body := editor.get("/admin/s/" + sv.ID).body
	kebab := tallyRow(body, "Kebab")
	if kebab == "" {
		t.Fatalf("Kebab row missing:\n%s", body)
	}
	if !strings.Contains(kebab, `data-testid="shown">0<`) {
		t.Errorf("Kebab should have been shown to nobody:\n%s", kebab)
	}
	if strings.Contains(kebab, `data-testid="interest">0%<`) {
		t.Errorf("Kebab's interest should not read as literal 0%%, which implies nobody wants it:\n%s", kebab)
	}
	if !regexp.MustCompile(`data-testid="interest">[^\d]`).MatchString(kebab) {
		t.Errorf("Kebab's interest cell should render a non-numeric placeholder, not a percentage:\n%s", kebab)
	}
}
