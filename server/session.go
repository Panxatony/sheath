package main

import (
	"crypto/subtle"
	"net/http"
	"sync"
	"time"
)

// The web interface cannot send bearer headers — a browser form sends only
// cookies. So /login trades the admin token once for a session. Sessions live
// in memory: with ten to a hundred blades and a single server that is the
// right size, and a restart simply signs everyone out.

const sessionCookie = "rk_session"
const sessionTTL = 12 * time.Hour

type sessions struct {
	mu sync.Mutex
	m  map[string]time.Time
}

func newSessions() *sessions {
	s := &sessions{m: map[string]time.Time{}}
	go s.reap()
	return s
}

func (s *sessions) create() string {
	id := newToken()
	s.mu.Lock()
	s.m[id] = time.Now().Add(sessionTTL)
	s.mu.Unlock()
	return id
}

func (s *sessions) valid(id string) bool {
	if id == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.m[id]
	if !ok || time.Now().After(exp) {
		delete(s.m, id)
		return false
	}
	return true
}

func (s *sessions) drop(id string) {
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
}

func (s *sessions) reap() {
	for {
		time.Sleep(time.Hour)
		now := time.Now()
		s.mu.Lock()
		for id, exp := range s.m {
			if now.After(exp) {
				delete(s.m, id)
			}
		}
		s.mu.Unlock()
	}
}

// loggedIn reports whether the caller may operate the interface. With no
// admin token set there is no sign-in at all — everything is open, and
// startup says so loudly.
func (a *App) loggedIn(r *http.Request) bool {
	if a.adminToken == "" {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return a.sess.valid(c.Value)
}

// requireUI guards a page. Callers who are not signed in land on the sign-in
// page, carrying the destination so they return where they meant to go.
func (a *App) requireUI(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.loggedIn(r) {
			next(w, r)
			return
		}
		to := r.URL.Path
		if to == "" || to == "/login" {
			to = "/"
		}
		http.Redirect(w, r, "/login?next="+to, http.StatusSeeOther)
	}
}

func (a *App) hLogin(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if next == "" || next[0] != '/' {
		next = "/"
	}
	if a.loggedIn(r) {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	l := a.resolveLang(w, r)
	render(w, loginTmpl, map[string]any{"Next": next, "L": l, "Path": "/login"})
}

func (a *App) hLoginPost(w http.ResponseWriter, r *http.Request) {
	// resolveLang rather than langOf: if the choice arrives as a parameter it
	// must reach the cookie here too — otherwise you sign in on a German page
	// and land on an English one.
	l := a.resolveLang(w, r)
	if err := r.ParseForm(); err != nil {
		render(w, loginTmpl, map[string]any{
			"Next": "/", "L": l, "Path": "/login", "Error": T(l, "err.form")})
		return
	}
	next := r.FormValue("next")
	if next == "" || next[0] != '/' {
		next = "/"
	}
	given := r.FormValue("token")
	if subtle.ConstantTimeCompare([]byte(given), []byte(a.adminToken)) != 1 {
		// Without a delay the token could be brute-forced quickly; one second
		// makes that unattractive enough.
		time.Sleep(time.Second)
		render(w, loginTmpl, map[string]any{
			"Next": next, "L": l, "Path": "/login", "Error": T(l, "login.wrong")})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    a.sess.create(),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *App) hLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.sess.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
