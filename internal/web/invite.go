package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/woodysmith1912/quicksurvey/internal/store"
)

// inviteData drives the public account-creation page.
type inviteData struct {
	Token  string
	Invite *store.Invite
	Name   string
}

func (s *Server) handleInviteForm(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	iv, ok := s.store.InviteByToken(token)
	if !ok {
		s.fail(w, r, http.StatusNotFound,
			errors.New("that invitation link is not valid. It may have been used already, revoked, or expired."))
		return
	}
	// Issue the cookie the CSRF token is derived from before rendering the form.
	s.voterToken(w, r)
	s.render(w, r, http.StatusOK, "invite.html", "Create an account", inviteData{Token: token, Invite: iv})
}

func (s *Server) handleInviteClaim(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if !s.checkCSRF(r) {
		s.fail(w, r, http.StatusForbidden, errors.New("your session expired; please reload and try again"))
		return
	}
	iv, ok := s.store.InviteByToken(token)
	if !ok {
		s.fail(w, r, http.StatusNotFound, errors.New("that invitation link is no longer valid"))
		return
	}
	name, pw, confirm := strings.TrimSpace(r.FormValue("username")), r.FormValue("password"), r.FormValue("confirm")

	fail := func(msg string) {
		s.render(w, flashNow(r, msg, true), http.StatusBadRequest, "invite.html", "Create an account",
			inviteData{Token: token, Invite: iv, Name: name})
	}
	if pw != confirm {
		fail("The two passwords do not match.")
		return
	}
	u, err := s.store.ClaimInvite(token, name, pw)
	if err != nil {
		fail(err.Error())
		return
	}
	s.cfg.Logger.Info("invitation claimed", "user", u.Name, "invited_by", u.InvitedBy, "requested_role", u.Role)

	// Sign them in so they land on the waiting page rather than a login form
	// for an account that cannot yet do anything.
	http.SetCookie(w, s.cookie(sessionCookie, s.newSession(u), sessionTTL))
	http.Redirect(w, r, "/account/pending", http.StatusSeeOther)
}

func (s *Server) handlePending(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "pending.html", "Waiting for approval", userFrom(r.Context()))
}

// handleInvitesPost creates and revokes invitation links.
func (s *Server) handleInvitesPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, http.StatusBadRequest, err)
		return
	}
	me := userFrom(r.Context())

	switch r.FormValue("action") {
	case "create":
		days := 7
		if d := r.FormValue("days"); d != "" {
			if n, err := time.ParseDuration(d + "h"); err == nil && n > 0 {
				days = int(n.Hours()) / 24
			}
		}
		iv, token, err := s.store.CreateInvite(me.Name, store.Role(r.FormValue("role")),
			r.FormValue("note"), time.Duration(days)*24*time.Hour)
		if err != nil {
			s.setFlash(w, err.Error(), true)
			break
		}
		s.cfg.Logger.Info("invitation created", "id", iv.ID, "by", me.Name, "role", iv.Role)
		// The link is shown once, in a query parameter rather than a flash
		// cookie, so it is not left sitting in the browser's cookie jar.
		http.Redirect(w, r, "/admin/users?new_invite="+url.QueryEscape(token), http.StatusSeeOther)
		return

	case "revoke":
		if err := s.store.RevokeInvite(r.FormValue("id")); err != nil {
			s.setFlash(w, err.Error(), true)
		} else {
			s.setFlash(w, "Invitation link revoked.", false)
		}

	default:
		s.setFlash(w, "Unknown action.", true)
	}
	http.Redirect(w, r, "/admin/users", http.StatusSeeOther)
}
