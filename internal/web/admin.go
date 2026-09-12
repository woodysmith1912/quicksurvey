package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/woodysmith1912/quicksurvey/internal/export"
	"github.com/woodysmith1912/quicksurvey/internal/store"
)

type surveyRow struct {
	Survey    *store.Survey
	Responses int
	Pending   int
	URL       string
	Overdue   bool // open, but past its scheduled close time
}

func (s *Server) row(r *http.Request, sv *store.Survey) surveyRow {
	return surveyRow{
		Survey:    sv,
		Responses: s.store.Count(sv.ID),
		Pending:   len(sv.PendingOptions()),
		URL:       s.surveyURL(r, sv.ID),
		Overdue:   sv.ClosedByClock(time.Now()),
	}
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	all := s.store.Surveys()
	rows := make([]surveyRow, 0, len(all))
	for _, sv := range all {
		rows = append(rows, s.row(r, sv))
	}
	s.render(w, r, http.StatusOK, "dashboard.html", "Surveys", rows)
}

func (s *Server) handleCreateSurvey(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	sv, err := s.store.CreateSurvey(r.FormValue("title"), r.FormValue("description"),
		strings.Split(r.FormValue("options"), "\n"))
	if err != nil {
		s.setFlash(w, err.Error(), true)
		http.Redirect(w, r, "/admin/", http.StatusSeeOther)
		return
	}
	s.cfg.Logger.Info("survey created", "id", sv.ID, "by", userFrom(r.Context()).Name)
	s.setFlash(w, "Survey created as a draft. Publish it when you are ready to share the link.", false)
	http.Redirect(w, r, "/admin/s/"+sv.ID+"/edit", http.StatusSeeOther)
}

// adminSurveyData is the results view: tally, comments, moderation queue.
type adminSurveyData struct {
	surveyRow
	Results   []store.Result
	Voters    int
	Comments  []*store.Response
	Pending   []store.Option
	MergeInto []store.Option
	CanEdit   bool
	Removed   []store.Option
}

func (s *Server) loadAdminSurvey(w http.ResponseWriter, r *http.Request) (*adminSurveyData, bool) {
	sv, ok := s.store.Survey(r.PathValue("id"))
	if !ok {
		s.fail(w, r, http.StatusNotFound, errors.New("no such survey"))
		return nil, false
	}
	results, voters := s.store.Tally(sv.ID)
	d := &adminSurveyData{
		surveyRow: s.row(r, sv),
		Results:   results,
		Voters:    voters,
		Pending:   sv.PendingOptions(),
		CanEdit:   userFrom(r.Context()).Role.AtLeast(store.RoleEditor),
	}
	d.Comments = s.store.Comments(sv.ID)
	for _, o := range sv.Options {
		switch o.Status {
		case store.OptApproved:
			d.MergeInto = append(d.MergeInto, o)
		case store.OptRemoved, store.OptRejected:
			d.Removed = append(d.Removed, o)
		}
	}
	return d, true
}

func (s *Server) handleAdminSurvey(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadAdminSurvey(w, r)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, "admin_survey.html", d.Survey.Title, d)
}

func (s *Server) handleEditForm(w http.ResponseWriter, r *http.Request) {
	d, ok := s.loadAdminSurvey(w, r)
	if !ok {
		return
	}
	s.render(w, r, http.StatusOK, "edit.html", "Edit — "+d.Survey.Title, d)
}

func (s *Server) handleEditSave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")
	closeAt, err := s.parseCloseAt(r.FormValue("close_at"))
	if err != nil {
		s.setFlash(w, err.Error(), true)
		http.Redirect(w, r, "/admin/s/"+id+"/edit", http.StatusSeeOther)
		return
	}

	_, err = s.store.UpdateSurvey(id, func(sv *store.Survey) error {
		title := strings.TrimSpace(r.FormValue("title"))
		if title == "" {
			return errors.New("a survey needs a title")
		}
		sv.Title = title
		sv.Description = strings.TrimSpace(r.FormValue("description"))
		sv.ShowResults = r.FormValue("show_results") != ""
		sv.AllowWriteIn = r.FormValue("allow_write_in") != ""
		sv.AllowComment = r.FormValue("allow_comment") != ""
		sv.NoRandomize = r.FormValue("randomize") == ""
		sv.CloseAt = closeAt

		// Existing options: rename, remove, or restore. Pending write-ins are
		// left to the moderation queue so the two forms cannot fight.
		for i := range sv.Options {
			o := &sv.Options[i]
			if o.Status == store.OptPending || o.Status == store.OptMerged || o.Status == store.OptRejected {
				continue
			}
			if text := strings.TrimSpace(r.FormValue("opt_" + o.ID)); text != "" {
				o.Text = text
			}
			if r.FormValue("remove_"+o.ID) != "" {
				o.Status = store.OptRemoved
			} else {
				o.Status = store.OptApproved
			}
		}
		for _, text := range r.Form["newopt"] {
			if strings.TrimSpace(text) == "" {
				continue
			}
			if _, err := store.AddOption(sv, text, store.OptApproved, "editor"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.setFlash(w, err.Error(), true)
	} else {
		s.setFlash(w, "Saved.", false)
	}
	http.Redirect(w, r, "/admin/s/"+id+"/edit", http.StatusSeeOther)
}

// parseCloseAt interprets the datetime-local value in the server's configured
// timezone and stores it as UTC.
func (s *Server) parseCloseAt(v string) (time.Time, error) {
	if strings.TrimSpace(v) == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{"2006-01-02T15:04", "2006-01-02T15:04:05"} {
		if t, err := time.ParseInLocation(layout, v, s.cfg.Location); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("could not understand the close time %q", v)
}

func (s *Server) handleSetState(w http.ResponseWriter, r *http.Request) {
	id, state := r.PathValue("id"), r.FormValue("state")
	switch state {
	case store.StateDraft, store.StateOpen, store.StateClosed:
	default:
		s.fail(w, r, http.StatusBadRequest, errors.New("unknown survey state"))
		return
	}
	if state == store.StateOpen {
		// Publishing is its own store operation: the first publication also
		// discards whatever the editor recorded while previewing the draft.
		discarded, err := s.store.Publish(id)
		switch {
		case err != nil:
			s.setFlash(w, err.Error(), true)
		case discarded > 0:
			s.setFlash(w, fmt.Sprintf("Survey is now open. %d test %s from the draft %s discarded.",
				discarded, plural(discarded, "response", "responses"), plural(discarded, "was", "were")), false)
		default:
			s.setFlash(w, "Survey is now open.", false)
		}
		http.Redirect(w, r, "/admin/s/"+id, http.StatusSeeOther)
		return
	}
	if _, err := s.store.UpdateSurvey(id, func(sv *store.Survey) error {
		sv.State = state
		return nil
	}); err != nil {
		s.setFlash(w, err.Error(), true)
	} else {
		s.setFlash(w, "Survey is now "+state+".", false)
	}
	http.Redirect(w, r, "/admin/s/"+id, http.StatusSeeOther)
}

// handleModerate approves, rewords, merges, or rejects a proposed write-in.
func (s *Server) handleModerate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	id, opt, action := r.PathValue("id"), r.FormValue("option"), r.FormValue("action")
	msg := map[string]string{
		"approve": "Suggestion approved; votes for it now count.",
		"reject":  "Suggestion rejected.",
		"merge":   "Suggestion merged; its votes moved to the option you chose.",
		"remove":  "Option removed from the survey.",
		"restore": "Option restored.",
	}[action]

	_, err := s.store.UpdateSurvey(id, func(sv *store.Survey) error {
		if text := strings.TrimSpace(r.FormValue("text")); text != "" && action != "merge" {
			if err := store.SetOptionText(sv, opt, text); err != nil {
				return err
			}
		}
		switch action {
		case "approve", "restore":
			return store.SetOptionStatus(sv, opt, store.OptApproved, "")
		case "reject":
			return store.SetOptionStatus(sv, opt, store.OptRejected, "")
		case "remove":
			return store.SetOptionStatus(sv, opt, store.OptRemoved, "")
		case "merge":
			return store.SetOptionStatus(sv, opt, store.OptMerged, r.FormValue("merge_into"))
		}
		return errors.New("unknown moderation action")
	})
	if err != nil {
		s.setFlash(w, err.Error(), true)
	} else {
		s.setFlash(w, msg, false)
	}
	http.Redirect(w, r, "/admin/s/"+id, http.StatusSeeOther)
}

func (s *Server) handleDeleteSurvey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sv, ok := s.store.Survey(id)
	// Deleting responses is irreversible, so require the title to be retyped.
	if !ok || strings.TrimSpace(r.FormValue("confirm")) != sv.Title {
		s.setFlash(w, "Not deleted: the confirmation text did not match the survey title.", true)
		http.Redirect(w, r, "/admin/s/"+id, http.StatusSeeOther)
		return
	}
	if err := s.store.DeleteSurvey(id); err != nil {
		s.setFlash(w, err.Error(), true)
		http.Redirect(w, r, "/admin/s/"+id, http.StatusSeeOther)
		return
	}
	s.cfg.Logger.Warn("survey deleted", "id", id, "by", userFrom(r.Context()).Name)
	s.setFlash(w, "Survey and all of its responses were deleted.", false)
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	sv, ok := s.store.Survey(r.PathValue("id"))
	if !ok {
		s.fail(w, r, http.StatusNotFound, errors.New("no such survey"))
		return
	}
	kind := strings.TrimSuffix(r.PathValue("kind"), ".tsv")
	var render func() error
	switch kind {
	case "responses":
		render = func() error {
			return export.Responses(w, sv, s.store.Responses(sv.ID), s.cfg.Location)
		}
	case "summary":
		render = func() error {
			results, voters := s.store.Tally(sv.ID)
			return export.Summary(w, sv, results, voters)
		}
	default:
		s.fail(w, r, http.StatusNotFound, errors.New("unknown export type"))
		return
	}
	w.Header().Set("Content-Type", "text/tab-separated-values; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", export.Filename(sv, kind, time.Now().In(s.cfg.Location))))
	if err := render(); err != nil {
		s.cfg.Logger.Error("export failed", "survey", sv.ID, "kind", kind, "err", err)
	}
}

// usersData drives the accounts page: approved accounts, the approval queue,
// and outstanding invitation links.
type usersData struct {
	Users   []*store.User
	Pending []*store.User
	Invites []*store.Invite
	Now     time.Time
	// NewInvite is a freshly minted link, shown exactly once immediately after
	// it is created. It is never stored anywhere it could be read again.
	NewInvite string
	// Resets holds outstanding reset links by username, so the page can say
	// one is already out there rather than quietly replacing it.
	Resets map[string]*store.Reset
	// NewReset is a freshly minted reset link and the account it belongs to,
	// rendered once, directly from the request that created it.
	NewReset     string
	NewResetUser string
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	all := s.store.Users()
	d := usersData{Pending: s.store.PendingUsers(), Invites: s.store.Invites(), Now: time.Now()}
	for _, u := range all {
		if !u.Pending {
			d.Users = append(d.Users, u)
		}
	}
	if tok := r.FormValue("new_invite"); tok != "" {
		d.NewInvite = s.baseURL(r) + "/invite/" + url.PathEscape(tok)
	}
	d.Resets = map[string]*store.Reset{}
	for _, u := range d.Users {
		if rp, ok := s.store.OutstandingReset(u.Name); ok {
			d.Resets[u.Name] = rp
		}
	}
	s.render(w, r, http.StatusOK, "users.html", "Accounts", d)
}

// renderUsersWithReset shows a freshly minted reset link exactly once, from the
// request that created it. Unlike the invitation flow it does not redirect with
// the token in a query string, which would put the secret into browser history
// and the proxy's access log.
func (s *Server) renderUsersWithReset(w http.ResponseWriter, r *http.Request, user, token string) {
	all := s.store.Users()
	d := usersData{Pending: s.store.PendingUsers(), Invites: s.store.Invites(), Now: time.Now()}
	for _, u := range all {
		if !u.Pending {
			d.Users = append(d.Users, u)
		}
	}
	d.Resets = map[string]*store.Reset{}
	for _, u := range d.Users {
		if rp, ok := s.store.OutstandingReset(u.Name); ok {
			d.Resets[u.Name] = rp
		}
	}
	d.NewReset = s.baseURL(r) + "/reset/" + url.PathEscape(token)
	d.NewResetUser = user
	s.render(w, r, http.StatusOK, "users.html", "Accounts", d)
}

func (s *Server) handleUsersPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	me := userFrom(r.Context())
	name := strings.TrimSpace(r.FormValue("username"))
	var err error
	var msg string

	switch r.FormValue("action") {
	case "add":
		// Deliberately absent. Choosing someone's first password means being
		// able to sign in as them, which is exactly the power that handing out
		// a reset link avoids. Invitations are the only way in from the web;
		// the CLI can still do it, and needs a shell in the container.
		err = errors.New("accounts are created by invitation, so that nobody else " +
			"ever knows the password. Use \"Invite someone by link\" below.")
	case "reset-link":
		_, token, err := s.store.CreateReset(name, me.Name)
		if err != nil {
			s.setFlash(w, err.Error(), true)
			break
		}
		s.cfg.Logger.Info("password reset link created", "for", name, "by", me.Name)
		s.renderUsersWithReset(w, r, name, token)
		return
	case "approve":
		err = s.store.ApproveUser(name, store.Role(r.FormValue("role")))
		msg = name + " approved as " + r.FormValue("role") + "."
	case "reject":
		if name == me.Name {
			err = errors.New("you cannot reject your own account")
			break
		}
		err = s.store.DeleteUser(name)
		msg = "Request from " + name + " rejected and the account removed."
	case "role":
		if name == me.Name {
			err = errors.New("you cannot change your own role")
			break
		}
		err = s.store.SetRole(name, store.Role(r.FormValue("role")))
		msg = "Role updated for " + name + "."
	case "delete":
		if name == me.Name {
			err = errors.New("you cannot delete your own account")
			break
		}
		// Deleting an account cannot be undone, so make it deliberate the
		// same way deleting a survey is: retype the name.
		if strings.TrimSpace(r.FormValue("confirm")) != name {
			err = errors.New("not deleted: type the username exactly to confirm")
			break
		}
		err = s.store.DeleteUser(name)
		msg = "Account " + name + " deleted."
	default:
		err = errors.New("unknown action")
	}
	if err != nil {
		s.setFlash(w, err.Error(), true)
	} else {
		s.cfg.Logger.Info("account change", "action", r.FormValue("action"), "target", name, "by", me.Name)
		s.setFlash(w, msg, false)
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}

// plural picks a word form. Duplicated from the template helpers because a
// flash message is assembled in Go rather than in a template.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
