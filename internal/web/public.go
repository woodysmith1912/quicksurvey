package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/woodysmith1912/quicksurvey/internal/store"
)

// ballotData drives the respondent-facing page.
type ballotData struct {
	Survey    *store.Survey
	Options   []store.Option
	Selected  map[string]bool
	Comment   string
	Responded bool
	Accepting bool
	Preview   bool // a signed-in editor looking at an unpublished survey
	Results   []store.Result
	Voters    int
}

// visibleSurvey loads a survey for a respondent. Draft surveys are treated as
// nonexistent for the public, but are previewable by an editor so they can
// check a survey before handing out its URL.
func (s *Server) visibleSurvey(w http.ResponseWriter, r *http.Request) (*store.Survey, bool, bool) {
	sv, ok := s.store.Survey(r.PathValue("id"))
	if !ok {
		s.fail(w, r, http.StatusNotFound, errors.New("no such survey"))
		return nil, false, false
	}
	if sv.State == store.StateDraft {
		u := s.sessionUser(r)
		if u == nil || !u.Role.AtLeast(store.RoleEditor) {
			s.fail(w, r, http.StatusNotFound, errors.New("no such survey"))
			return nil, false, false
		}
		return sv, true, true
	}
	return sv, true, false
}

func (s *Server) handleBallot(w http.ResponseWriter, r *http.Request) {
	sv, ok, preview := s.visibleSurvey(w, r)
	if !ok {
		return
	}
	token := s.voterToken(w, r)
	voter := s.store.VoterID(sv.ID, token)

	d := ballotData{Survey: sv, Selected: map[string]bool{},
		Accepting: sv.AcceptingFrom(time.Now(), preview), Preview: preview}
	if prev, ok := s.store.ResponseFor(sv.ID, voter); ok {
		d.Responded, d.Comment = true, prev.Comment
		for _, id := range prev.Choices {
			d.Selected[id] = true
		}
		d.Options = sv.Ballot(prev.Choices)
	} else {
		d.Options = sv.Ballot(nil)
	}
	if sv.ShowResults {
		d.Results, d.Voters = s.store.Tally(sv.ID)
	}
	s.render(w, r, http.StatusOK, "ballot.html", sv.Title, d)
}

func (s *Server) handleVote(w http.ResponseWriter, r *http.Request) {
	sv, ok, preview := s.visibleSurvey(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, errors.New("could not read the submitted form"))
		return
	}
	if !s.checkCSRF(r) {
		s.fail(w, r, http.StatusForbidden, errors.New("your session expired; please reload and try again"))
		return
	}
	voter := s.store.VoterID(sv.ID, s.voterToken(w, r))
	save := s.store.SaveResponse
	if preview {
		save = s.store.PreviewResponse
	}
	if _, err := save(sv.ID, voter, r.Form["choice"], r.FormValue("comment")); err != nil {
		s.setFlash(w, err.Error(), true)
		http.Redirect(w, r, "/s/"+sv.ID, http.StatusSeeOther)
		return
	}
	if preview {
		s.setFlash(w, "Recorded as a test response. It will be discarded when you publish the survey.", false)
		http.Redirect(w, r, "/s/"+sv.ID, http.StatusSeeOther)
		return
	}
	s.setFlash(w, "Your response has been recorded. You can change it any time from this page.", false)
	http.Redirect(w, r, "/s/"+sv.ID, http.StatusSeeOther)
}

func (s *Server) handleWriteIn(w http.ResponseWriter, r *http.Request) {
	sv, ok, preview := s.visibleSurvey(w, r)
	if !ok {
		return
	}
	if !s.checkCSRF(r) {
		s.fail(w, r, http.StatusForbidden, errors.New("your session expired; please reload and try again"))
		return
	}
	voter := s.store.VoterID(sv.ID, s.voterToken(w, r))
	add := s.store.AddWriteIn
	if preview {
		add = s.store.PreviewWriteIn
	}
	if _, err := add(sv.ID, voter, r.FormValue("text")); err != nil {
		s.setFlash(w, err.Error(), true)
	} else {
		s.setFlash(w, "Thanks — your suggestion is waiting for a moderator to review it. "+
			"Your vote for it is saved and will count once it is approved.", false)
	}
	http.Redirect(w, r, "/s/"+sv.ID, http.StatusSeeOther)
}

// handlePublicResults shows the tally to respondents, and only if the survey's
// editor turned that on. Comments are never shown here.
func (s *Server) handlePublicResults(w http.ResponseWriter, r *http.Request) {
	sv, ok, _ := s.visibleSurvey(w, r)
	if !ok {
		return
	}
	if !sv.ShowResults {
		s.fail(w, r, http.StatusNotFound, errors.New("results for this survey are not public"))
		return
	}
	results, voters := s.store.Tally(sv.ID)
	s.render(w, r, http.StatusOK, "results.html", sv.Title+" — results",
		ballotData{Survey: sv, Results: results, Voters: voters})
}
