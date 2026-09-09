// Package web serves both halves of QuickSurvey: the anonymous respondent
// pages under /s/, and the authenticated administration pages under /admin/.
package web

import (
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/woodysmith1912/quicksurvey/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Config is the deployment-dependent behaviour of the server. Defaults assume
// the container sits behind a TLS-terminating reverse proxy.
type Config struct {
	// BaseURL is the externally visible origin, used to build the shareable
	// survey links shown to editors. Empty means derive it per request.
	BaseURL string
	// SecureCookies marks cookies Secure. Leave true unless serving plain
	// HTTP on a trusted network, where the browser would otherwise drop them.
	SecureCookies bool
	// TrustProxy takes the client address from X-Real-Ip / X-Forwarded-For.
	//
	// It governs rate limiting, so it matters which way it is wrong. Behind a
	// proxy with it off, every request shares one bucket and the limit falls
	// on everybody at once. Exposed directly with it on, a caller sets the
	// header themselves and gets a fresh bucket per request — a limit that
	// looks like it works and does not. Every documented deployment here is
	// behind a proxy, so it defaults on.
	TrustProxy bool
	// RedirectHTTPS sends plain-HTTP requests to the https:// equivalent,
	// based on X-Forwarded-Proto. Needed when the proxy in front serves both
	// :80 and :443 without redirecting itself: Secure cookies are not sent
	// over http://, so a visitor arriving there cannot sign in or vote and
	// gets no explanation.
	RedirectHTTPS bool
	// Location is the timezone used to display and enter times.
	Location *time.Location
	Logger   *slog.Logger
}

// Server is the HTTP handler for the whole application.
type Server struct {
	cfg   Config
	store *store.Store
	tmpl  map[string]*template.Template
	mux   *http.ServeMux

	// Two limiters, for two different scarce things.
	loginLimit *limiter
	voterLimit *limiter
}

// New builds a Server. It fails if the embedded templates do not parse, which
// is a build-time defect rather than a runtime condition.
func New(st *store.Store, cfg Config) (*Server, error) {
	if cfg.Location == nil {
		cfg.Location = time.Local
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	s := &Server{
		cfg:   cfg,
		store: st,
		// A sign-in attempt costs 600,000 PBKDF2 iterations whether or not
		// the password is right. Nobody legitimately fails ten times a
		// minute, so this can be tight.
		loginLimit: newLimiter(10, time.Minute),
		// Issuing a *new* voter identity is the thing worth limiting, not
		// voting. Limiting every write by address punishes the case this
		// application is for — a survey link shared inside one office, where
		// everyone shares a NAT address — while barely inconveniencing an
		// attacker, who only needs one identity per fake vote anyway.
		//
		// Limiting issuance instead bounds fake identities directly, and
		// costs a legitimate crowd nothing: they are issued one cookie each
		// and then vote and re-vote freely.
		voterLimit: newLimiter(300, time.Minute),
	}
	if err := s.parseTemplates(); err != nil {
		return nil, err
	}
	s.routes()
	return s, nil
}

// MaxBody bounds a request body. Every form here is a few hundred bytes; the
// cap exists so an anonymous caller cannot choose how much memory a POST costs.
const MaxBody = 64 << 10

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		r.Body = http.MaxBytesReader(w, r.Body, MaxBody)
	}
	if s.cfg.RedirectHTTPS && !s.isHTTPS(r) {
		// 308 rather than 302: the method and body must survive, or a POSTed
		// vote would silently become a GET.
		http.Redirect(w, r, "https://"+r.Host+r.URL.RequestURI(), http.StatusPermanentRedirect)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	if s.cfg.SecureCookies {
		// Deliberately no includeSubDomains and no preload. This runs as one
		// app on one hostname among several under a shared domain, and both of
		// those would commit every sibling — present and future — to HTTPS on
		// this application's say-so. Neither is ours to promise.
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
	}
	// No third-party assets are loaded, so the policy can be this tight.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'self'; img-src 'self' data:; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	s.mux.ServeHTTP(w, r)
}

// limitLogin refuses a caller who has already spent their budget of failures.
// The handler charges only when the attempt fails, so someone signing in
// successfully never consumes it.
func (s *Server) limitLogin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.loginLimit.ok(clientKey(r, s.cfg.TrustProxy)) {
			s.cfg.Logger.Warn("rate limited", "path", r.URL.Path)
			s.tooMany(w, r, "Too many failed attempts from your address. Wait a minute and try again.")
			return
		}
		h(w, r)
	}
}

// isHTTPS reports whether the request reached the proxy over TLS. The health
// check is exempt: kubelet probes the pod directly over plain HTTP, and
// redirecting them would fail every probe.
func (s *Server) isHTTPS(r *http.Request) bool {
	if r.TLS != nil || r.URL.Path == "/healthz" {
		return true
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	return strings.EqualFold(strings.TrimSpace(strings.Split(proto, ",")[0]), "https")
}

func (s *Server) routes() {
	m := http.NewServeMux()
	s.mux = m

	m.Handle("GET /static/", http.FileServerFS(assets))
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	m.HandleFunc("GET /{$}", s.handleIndex)

	// Respondent pages. No account required; a survey ID is the credential.
	m.HandleFunc("GET /s/{id}", s.handleBallot)
	m.HandleFunc("POST /s/{id}/vote", s.handleVote)
	m.HandleFunc("POST /s/{id}/writein", s.handleWriteIn)
	m.HandleFunc("GET /s/{id}/results", s.handlePublicResults)

	// Claiming an invitation is public: the token in the URL is the credential.
	m.HandleFunc("GET /invite/{token}", s.handleInviteForm)
	m.HandleFunc("POST /invite/{token}", s.limitLogin(s.handleInviteClaim))

	m.HandleFunc("GET /login", s.handleLoginForm)
	m.HandleFunc("POST /login", s.limitLogin(s.handleLogin))
	m.HandleFunc("POST /logout", s.handleLogout)
	m.Handle("GET /account/pending", s.requireRole(store.RoleViewer, s.handlePending))
	m.Handle("GET /account/password", s.requireRole(store.RoleViewer, s.handlePasswordForm))
	m.Handle("POST /account/password", s.requireRole(store.RoleViewer, s.handlePasswordChange))

	// Results and exports: any account.
	m.Handle("GET /admin/{$}", s.requireRole(store.RoleViewer, s.handleDashboard))
	m.Handle("GET /admin/s/{id}", s.requireRole(store.RoleViewer, s.handleAdminSurvey))
	m.Handle("GET /admin/s/{id}/export/{kind}", s.requireRole(store.RoleViewer, s.handleExport))

	// Authoring and moderation: editors and admins.
	m.Handle("POST /admin/surveys", s.requireRole(store.RoleEditor, s.handleCreateSurvey))
	m.Handle("GET /admin/s/{id}/edit", s.requireRole(store.RoleEditor, s.handleEditForm))
	m.Handle("POST /admin/s/{id}/edit", s.requireRole(store.RoleEditor, s.handleEditSave))
	m.Handle("POST /admin/s/{id}/state", s.requireRole(store.RoleEditor, s.handleSetState))
	m.Handle("POST /admin/s/{id}/moderate", s.requireRole(store.RoleEditor, s.handleModerate))
	m.Handle("POST /admin/s/{id}/delete", s.requireRole(store.RoleEditor, s.handleDeleteSurvey))

	// Accounts: admins only.
	m.Handle("GET /admin/users", s.requireRole(store.RoleAdmin, s.handleUsers))
	m.Handle("POST /admin/users", s.requireRole(store.RoleAdmin, s.handleUsersPost))
	m.Handle("POST /admin/invites", s.requireRole(store.RoleAdmin, s.handleInvitesPost))
}

func (s *Server) parseTemplates() error {
	pages, err := assets.ReadDir("templates")
	if err != nil {
		return err
	}
	funcs := template.FuncMap{
		"localtime": s.localtime,
		"datetimeLocal": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.In(s.cfg.Location).Format("2006-01-02T15:04")
		},
		"pct": func(f float64) string { return fmt.Sprintf("%.0f%%", f) },
		// Role checks live here rather than in the templates, because a
		// template cannot pass a plain string where a store.Role is wanted.
		"canEdit": func(u *store.User) bool { return u != nil && u.Role.AtLeast(store.RoleEditor) },
		"isAdmin": func(u *store.User) bool { return u != nil && u.Role == store.RoleAdmin },
		"bar":     func(f float64) template.CSS { return template.CSS(fmt.Sprintf("width:%.1f%%", f)) },
		"plural": func(n int, one, many string) string {
			if n == 1 {
				return one
			}
			return many
		},
	}
	// Files named with a leading underscore are shared fragments rather than
	// pages, and are parsed into every page.
	shared := []string{"templates/base.html"}
	for _, p := range pages {
		if strings.HasPrefix(p.Name(), "_") {
			shared = append(shared, "templates/"+p.Name())
		}
	}

	s.tmpl = map[string]*template.Template{}
	for _, p := range pages {
		name := p.Name()
		if name == "base.html" || strings.HasPrefix(name, "_") {
			continue
		}
		t, err := template.New("base.html").Funcs(funcs).
			ParseFS(assets, append(shared, "templates/"+name)...)
		if err != nil {
			return fmt.Errorf("template %s: %w", name, err)
		}
		s.tmpl[name] = t
	}
	return nil
}

func (s *Server) localtime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.In(s.cfg.Location).Format("2006-01-02 15:04 MST")
}

// page is the data every template receives.
type page struct {
	Title string
	User  *store.User
	CSRF  string
	Flash flash
	Data  any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name, title string, data any) {
	t, ok := s.tmpl[name]
	if !ok {
		s.fail(w, r, http.StatusInternalServerError, fmt.Errorf("no such template %q", name))
		return
	}
	p := page{Title: title, User: userFrom(r.Context()), CSRF: s.csrfToken(r), Flash: s.takeFlash(w, r), Data: data}

	// Render to memory first: a template error halfway through a streamed
	// response would leave the client with a broken page and a 200.
	var buf strings.Builder
	if err := t.Execute(&buf, p); err != nil {
		s.cfg.Logger.Error("render failed", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(buf.String()))
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, status int, err error) {
	if status >= 500 {
		s.cfg.Logger.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	msg := http.StatusText(status)
	if err != nil && status < 500 {
		msg = err.Error() // 4xx messages explain what the caller did wrong
	}
	s.render(w, r, status, "error.html", msg, msg)
}

// baseURL returns the externally visible origin, preferring the configured
// value and otherwise reconstructing it from proxy headers.
func (s *Server) baseURL(r *http.Request) string {
	if s.cfg.BaseURL != "" {
		return strings.TrimRight(s.cfg.BaseURL, "/")
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = strings.TrimSpace(strings.Split(p, ",")[0])
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = strings.TrimSpace(strings.Split(h, ",")[0])
	}
	return scheme + "://" + host
}

func (s *Server) surveyURL(r *http.Request, id string) string {
	return s.baseURL(r) + "/s/" + url.PathEscape(id)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if u := s.sessionUser(r); u != nil {
		http.Redirect(w, r, "/admin/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "index.html", "QuickSurvey", nil)
}
