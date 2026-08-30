package main

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The web interface cannot send bearer headers — a browser form sends only
// cookies. So /login trades a credential once for a session. Sessions live
// in memory: with ten to a hundred blades and a single server that is the
// right size, and a restart simply signs everyone out.
//
// A session carries who signed in and what they may do. Both are decided at
// sign-in and never re-read: a role taken away reaches someone at their next
// sign-in, which is soon enough at twelve hours and much easier to reason
// about than a permission that changes under a running page.

const sessionCookie = "rk_session"
const sessionTTL = 12 * time.Hour

// who is signed in on one session.
type who struct {
	Name string
	Role role
	exp  time.Time
}

type sessions struct {
	mu sync.Mutex
	m  map[string]who
}

func newSessions() *sessions {
	s := &sessions{m: map[string]who{}}
	go s.reap()
	return s
}

func (s *sessions) create(name string, r role) string {
	id := newToken()
	s.mu.Lock()
	s.m[id] = who{Name: name, Role: r, exp: time.Now().Add(sessionTTL)}
	s.mu.Unlock()
	return id
}

func (s *sessions) lookup(id string) (who, bool) {
	if id == "" {
		return who{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.m[id]
	if !ok || time.Now().After(w.exp) {
		delete(s.m, id)
		return who{}, false
	}
	return w, true
}

func (s *sessions) valid(id string) bool {
	_, ok := s.lookup(id)
	return ok
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
		for id, w := range s.m {
			if now.After(w.exp) {
				delete(s.m, id)
			}
		}
		s.mu.Unlock()
	}
}

// caller says who is at the other end of a request, and whether anybody is.
// With no admin token set there is no sign-in at all — everything is open,
// startup says so loudly, and everyone is an admin.
func (a *App) caller(r *http.Request) (who, bool) {
	if a.adminToken == "" {
		return who{Name: "", Role: roleAdmin}, true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return who{}, false
	}
	return a.sess.lookup(c.Value)
}

// loggedIn reports whether the caller may operate the interface at all.
func (a *App) loggedIn(r *http.Request) bool {
	_, ok := a.caller(r)
	return ok
}

// isAdmin is for the pages: what a role may do is enforced on the route, and
// this only decides what is worth drawing. A button an operator cannot use is
// a dead end, not a boundary.
func (a *App) isAdmin(r *http.Request) bool {
	c, ok := a.caller(r)
	return ok && c.Role.atLeast(roleAdmin)
}

// actor is the name to write in the log for what this request does. Empty
// where nobody signed in, which is the open installation and reads as "the
// server did it" — which it did.
func (a *App) actor(r *http.Request) string {
	w, ok := a.caller(r)
	if !ok {
		return ""
	}
	return w.Name
}

// requireUI guards a page that any signed-in person may see. Callers who are
// not signed in land on the sign-in page, carrying the destination so they
// return where they meant to go.
func (a *App) requireUI(next http.HandlerFunc) http.HandlerFunc {
	return a.requireRole(roleOperator, next)
}

// requireAdminUI guards what only an administrator may do: the pages that
// decide what the fleet is, rather than what happens at the rack.
func (a *App) requireAdminUI(next http.HandlerFunc) http.HandlerFunc {
	return a.requireRole(roleAdmin, next)
}

// requireRole is the one place a page's role is decided. Somebody signed in
// with too little is told so and sent back to the overview — not to the
// sign-in page, which would look like the password was wrong.
func (a *App) requireRole(need role, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := a.caller(r)
		if !ok {
			to := r.URL.Path
			if to == "" || to == "/login" {
				to = "/"
			}
			http.Redirect(w, r, "/login?next="+to, http.StatusSeeOther)
			return
		}
		if !c.Role.atLeast(need) {
			redirectMsg(w, r, "/", "err", T(a.langOf(r), "err.notallowed"))
			return
		}
		next(w, r)
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
	render(w, loginTmpl, a.loginPage(l, next, ""))
}

// loginPage is what the sign-in form needs to know. Whether any account
// exists decides what it asks for: until one does, the token is the only way
// in and a name field would be a puzzle.
func (a *App) loginPage(l Lang, next, errMsg string) map[string]any {
	return map[string]any{
		"Next": next, "L": l, "Path": "/login", "Error": errMsg,
		"HaveUsers": a.anyUsers(),
	}
}

// overTLS answers whether this request reached the server encrypted. Behind a
// reverse proxy the connection here is plain HTTP and the proxy says what the
// client used; X-Forwarded-Proto is only worth reading because nothing but
// the proxy can reach this port.
func overTLS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
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
	// Two ways in, and the second one is the recovery route rather than an
	// account: a name with a password, or the admin token by itself. The
	// token is how the first account gets created and how somebody gets back
	// in after locking themselves out; it is a file on the server, so anyone
	// who can read it could change the database anyway.
	name := strings.TrimSpace(r.FormValue("user"))
	secret := r.FormValue("token")

	signedIn := who{}
	switch {
	case name != "":
		u, ok := a.authenticate(name, secret)
		if !ok {
			// Without a delay a password could be guessed quickly; one second
			// makes that unattractive enough.
			time.Sleep(time.Second)
			render(w, loginTmpl, a.loginPage(l, next, T(l, "login.wrong")))
			return
		}
		signedIn = who{Name: u.Name, Role: u.Role}
	case subtle.ConstantTimeCompare([]byte(secret), []byte(a.adminToken)) == 1:
		signedIn = who{Name: "", Role: roleAdmin}
	default:
		time.Sleep(time.Second)
		render(w, loginTmpl, a.loginPage(l, next, T(l, "login.wrong")))
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    a.sess.create(signedIn.Name, signedIn.Role),
		Path:     "/",
		HttpOnly: true,
		// Only where the request came in over TLS. Set unconditionally it
		// would lock out the plain-HTTP address on the LAN, which is how the
		// server is reached from inside; not set at all it would travel in
		// clear the moment somebody uses the proxy.
		Secure:   overTLS(r),
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
