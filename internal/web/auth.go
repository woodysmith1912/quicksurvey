package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/woodysmith1912/quicksurvey/internal/store"
)

const (
	sessionCookie = "qs_session"
	voterCookie   = "qs_voter"
	flashCookie   = "qs_flash"
	sessionTTL    = 12 * time.Hour
	voterTTL      = 365 * 24 * time.Hour
)

type ctxKey int

const userKey ctxKey = 1

func userFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(userKey).(*store.User)
	return u
}

func (s *Server) cookie(name, value string, ttl time.Duration) *http.Cookie {
	c := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	}
	if ttl > 0 {
		c.MaxAge = int(ttl.Seconds())
		c.Expires = time.Now().Add(ttl)
	} else {
		c.MaxAge = -1
	}
	return c
}

// --- sessions -------------------------------------------------------------

// newSession mints a cookie value of the form name|expiry|mac. The MAC covers
// the user's password hash, so changing a password invalidates every session
// that user has open, on every device.
func (s *Server) newSession(u *store.User) string {
	exp := strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	body := u.Name + "|" + exp
	return body + "|" + s.store.MAC("session", body, u.SessionKey())
}

// sessionUser resolves the session cookie, or nil if there is no valid session.
func (s *Server) sessionUser(r *http.Request) *store.User {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	parts := strings.Split(c.Value, "|")
	if len(parts) != 3 {
		return nil
	}
	name, exp, mac := parts[0], parts[1], parts[2]
	ts, err := strconv.ParseInt(exp, 10, 64)
	if err != nil || time.Now().Unix() > ts {
		return nil
	}
	u, ok := s.store.User(name)
	if !ok {
		return nil
	}
	want := s.store.MAC("session", name+"|"+exp, u.SessionKey())
	if subtle.ConstantTimeCompare([]byte(mac), []byte(want)) != 1 {
		return nil
	}
	return u
}

// requireRole gates a handler on an authenticated account of at least the given
// role. An account that must change its password can reach only that form.
func (s *Server) requireRole(min store.Role, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := s.sessionUser(r)
		if u == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			s.fail(w, r, http.StatusUnauthorized, errors.New("please log in again"))
			return
		}
		// A pending account can reach nothing but the page explaining why.
		if u.Pending && r.URL.Path != "/account/pending" {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/account/pending", http.StatusSeeOther)
				return
			}
			s.fail(w, r, http.StatusForbidden, errors.New("your account is still waiting for approval"))
			return
		}
		if u.MustChangePassword && !strings.HasPrefix(r.URL.Path, "/account/password") {
			http.Redirect(w, r, "/account/password", http.StatusSeeOther)
			return
		}
		if !u.Role.AtLeast(min) {
			s.fail(w, r, http.StatusForbidden,
				errors.New("your account does not have permission to do that"))
			return
		}
		if r.Method != http.MethodGet && !s.checkCSRF(r) {
			s.fail(w, r, http.StatusForbidden, errors.New("your session expired; please retry"))
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	})
}

// --- CSRF -----------------------------------------------------------------

// csrfToken derives a token from whichever cookie identifies the caller: the
// session for accounts, the voter token for anonymous respondents. Both are
// HttpOnly, so a cross-site page cannot read the token it would need to forge.
func (s *Server) csrfToken(r *http.Request) string {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		return s.store.MAC("csrf", c.Value)
	}
	if c, err := r.Cookie(voterCookie); err == nil && c.Value != "" {
		return s.store.MAC("csrf", c.Value)
	}
	return ""
}

func (s *Server) checkCSRF(r *http.Request) bool {
	want := s.csrfToken(r)
	if want == "" {
		return false
	}
	got := r.FormValue("csrf")
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// --- voter identity -------------------------------------------------------

// voterToken returns the caller's random browser token, issuing one if this is
// their first visit. The token is meaningless on its own: it identifies a
// browser to itself, and only a keyed derivation of it is ever stored.
func (s *Server) voterToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(voterCookie); err == nil && len(c.Value) >= 16 {
		return c.Value
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	http.SetCookie(w, s.cookie(voterCookie, tok, voterTTL))
	// Make it visible to this request too, so the page can carry a CSRF token
	// derived from a cookie the browser has not sent back yet.
	r.AddCookie(&http.Cookie{Name: voterCookie, Value: tok})
	return tok
}

// --- flash ----------------------------------------------------------------

type flash struct {
	Message string
	Error   bool
}

const flashKey ctxKey = 2

// setFlash carries a message across a redirect, in a cookie. Use it only when
// the response is a redirect: the cookie is not visible to a page rendered by
// this same request, because that page reads the flash off the incoming
// request. For those, use flashNow.
func (s *Server) setFlash(w http.ResponseWriter, msg string, isErr bool) {
	kind := "i"
	if isErr {
		kind = "e"
	}
	v := base64.RawURLEncoding.EncodeToString([]byte(kind + msg))
	http.SetCookie(w, s.cookie(flashCookie, v, 5*time.Minute))
}

// flashNow attaches a message to a page this request is about to render, with
// no round trip and no cookie. Returns the request to render with.
func flashNow(r *http.Request, msg string, isErr bool) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), flashKey, flash{Message: msg, Error: isErr}))
}

// takeFlash returns the message to show, preferring one set for this very
// response and otherwise reading and clearing the redirect cookie.
func takeFlash(w http.ResponseWriter, r *http.Request) flash {
	if f, ok := r.Context().Value(flashKey).(flash); ok {
		return f
	}
	c, err := r.Cookie(flashCookie)
	if err != nil || c.Value == "" {
		return flash{}
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Path: "/", MaxAge: -1})
	b, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil || len(b) < 1 {
		return flash{}
	}
	return flash{Message: string(b[1:]), Error: b[0] == 'e'}
}

// --- handlers -------------------------------------------------------------

type loginData struct {
	Next string
	Name string
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if s.sessionUser(r) != nil {
		http.Redirect(w, r, "/admin/", http.StatusSeeOther)
		return
	}
	// Issue the cookie the CSRF token is derived from before rendering the form.
	s.voterToken(w, r)
	s.render(w, r, http.StatusOK, "login.html", "Sign in", loginData{Next: safeNext(r.FormValue("next"))})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.fail(w, r, http.StatusForbidden, errors.New("your session expired; please try again"))
		return
	}
	name, pw := strings.TrimSpace(r.FormValue("username")), r.FormValue("password")
	next := safeNext(r.FormValue("next"))
	u, ok := s.store.Authenticate(name, pw)
	if !ok {
		s.cfg.Logger.Warn("failed login", "user", name)
		r = flashNow(r, "Incorrect username or password.", true)
		s.render(w, r, http.StatusUnauthorized, "login.html", "Sign in", loginData{Next: next, Name: name})
		return
	}
	http.SetCookie(w, s.cookie(sessionCookie, s.newSession(u), sessionTTL))
	if u.Pending {
		http.Redirect(w, r, "/account/pending", http.StatusSeeOther)
		return
	}
	if u.MustChangePassword {
		http.Redirect(w, r, "/account/password", http.StatusSeeOther)
		return
	}
	if next == "" {
		next = "/admin/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(r) {
		s.fail(w, r, http.StatusForbidden, errors.New("your session expired"))
		return
	}
	http.SetCookie(w, s.cookie(sessionCookie, "", 0))
	s.setFlash(w, "Signed out.", false)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handlePasswordForm(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusOK, "password.html", "Change password", userFrom(r.Context()))
}

func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())
	current, next, confirm := r.FormValue("current"), r.FormValue("new"), r.FormValue("confirm")
	fail := func(msg string) {
		s.render(w, flashNow(r, msg, true), http.StatusBadRequest, "password.html", "Change password", u)
	}
	if _, ok := s.store.Authenticate(u.Name, current); !ok {
		fail("Your current password is not correct.")
		return
	}
	if next != confirm {
		fail("The two new passwords do not match.")
		return
	}
	if err := s.store.SetPassword(u.Name, next); err != nil {
		fail(err.Error())
		return
	}
	// The old session MAC covered the old password hash and is now invalid.
	u, _ = s.store.User(u.Name)
	http.SetCookie(w, s.cookie(sessionCookie, s.newSession(u), sessionTTL))
	s.setFlash(w, "Password changed.", false)
	http.Redirect(w, r, "/admin/", http.StatusSeeOther)
}

// safeNext keeps post-login redirects on this site: an absolute or
// protocol-relative URL from a query parameter is an open-redirect.
func safeNext(next string) string {
	if strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") {
		return next
	}
	return ""
}
