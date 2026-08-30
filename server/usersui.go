package main

import (
	"fmt"
	"net/http"
	"strings"
)

// The accounts page. Only an administrator sees it, and only an administrator
// can reach any of the routes behind it — the guard is on the route, not on
// the button, because a button that is merely hidden is not a boundary.

type userRow struct {
	User
	RoleName string
	Last     string
	Me       bool // the account this request is signed in as
}

func (a *App) hUsersPage(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	users, err := a.listUsers()
	if err != nil {
		redirectMsg(w, r, "/settings", "err", err.Error())
		return
	}
	me := a.actor(r)
	rows := make([]userRow, 0, len(users))
	for _, u := range users {
		row := userRow{User: u, RoleName: T(l, "role."+string(u.Role)), Me: u.Name == me}
		if u.LastLogin != "" {
			row.Last = ago(l, u.LastLogin)
		}
		rows = append(rows, row)
	}
	msg, errMsg := flash(r)
	render(w, usersTmpl, map[string]any{
		"Rows": rows, "L": l, "Path": "/users", "Msg": msg, "Err": errMsg, "Admin": true,
		"Me": me, "TokenUser": me == "",
	})
}

func (a *App) hUserCreate(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	if err := r.ParseForm(); err != nil {
		redirectMsg(w, r, "/users", "err", T(l, "err.form"))
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if err := a.createUser(name, r.FormValue("password"), role(r.FormValue("role"))); err != nil {
		redirectMsg(w, r, "/users", "err", errText(l, err))
		return
	}
	a.logActed(a.actor(r), "", "info", "account created: "+name+" ("+r.FormValue("role")+")")
	redirectMsg(w, r, "/users", "msg", fmt.Sprintf(T(l, "usr.created"), name))
}

func (a *App) hUserPassword(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	name := r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		redirectMsg(w, r, "/users", "err", T(l, "err.form"))
		return
	}
	if err := a.setUserPassword(name, r.FormValue("password")); err != nil {
		redirectMsg(w, r, "/users", "err", errText(l, err))
		return
	}
	a.logActed(a.actor(r), "", "warn", "password changed for "+name)
	redirectMsg(w, r, "/users", "msg", fmt.Sprintf(T(l, "usr.pwset"), name))
}

func (a *App) hUserRole(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	name := r.PathValue("name")
	if err := r.ParseForm(); err != nil {
		redirectMsg(w, r, "/users", "err", T(l, "err.form"))
		return
	}
	want := role(r.FormValue("role"))
	if err := a.setUserRole(name, want); err != nil {
		redirectMsg(w, r, "/users", "err", errText(l, err))
		return
	}
	a.logActed(a.actor(r), "", "warn", "role of "+name+" set to "+string(want))
	redirectMsg(w, r, "/users", "msg", fmt.Sprintf(T(l, "usr.roleset"), name, T(l, "role."+string(want))))
}

func (a *App) hUserDisable(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	name := r.PathValue("name")
	off := r.FormValue("off") != "0"
	if err := a.setUserDisabled(name, off); err != nil {
		redirectMsg(w, r, "/users", "err", errText(l, err))
		return
	}
	key, word := "usr.disabled", "disabled"
	if !off {
		key, word = "usr.enabled", "enabled"
	}
	a.logActed(a.actor(r), "", "warn", "account "+name+" "+word)
	redirectMsg(w, r, "/users", "msg", fmt.Sprintf(T(l, key), name))
}

func (a *App) hUserDelete(w http.ResponseWriter, r *http.Request) {
	l := a.langOf(r)
	name := r.PathValue("name")
	if err := a.deleteUser(name); err != nil {
		redirectMsg(w, r, "/users", "err", errText(l, err))
		return
	}
	a.logActed(a.actor(r), "", "warn", "account removed: "+name)
	redirectMsg(w, r, "/users", "msg", fmt.Sprintf(T(l, "usr.deleted"), name))
}

// ── What an operator may ask of a blade ──────────────────────────────

// actionNeeds is the security boundary of the operator role, in one place.
// Everything not named here needs an administrator.
//
// The line is: what happens at the rack, an operator does. What decides what
// the fleet is, an administrator does. So installing, reinstalling, calling an
// installation off and taking a blade out of service are here — and erasing a
// disk and forgetting a blade are not, because one destroys data and the other
// deletes the record along with the blade's credential.
var actionNeeds = map[string]role{
	"reimage":      roleOperator,
	"cancel":       roleOperator,
	"reset":        roleOperator,
	"shutdown":     roleOperator,
	"reboot":       roleOperator,
	"identify":     roleOperator,
	"identify_off": roleOperator,
	"stealth_on":   roleOperator,
	"stealth_off":  roleOperator,
	"probe":        roleOperator,
	"wipe":         roleAdmin,
}

// mayDo answers for one action, and answers "no" for anything it has never
// heard of — a new action is an administrator's until somebody decides
// otherwise, rather than everyone's by omission.
func mayDo(r role, kind string) bool {
	need, ok := actionNeeds[kind]
	if !ok {
		need = roleAdmin
	}
	return r.atLeast(need)
}

// requireAction is the guard on the blade action routes, in both interfaces.
func (a *App) mayAct(r *http.Request, kind string) bool {
	c, ok := a.caller(r)
	if !ok {
		return false
	}
	return mayDo(c.Role, kind)
}
