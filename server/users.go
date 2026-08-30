package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Who may do what.
//
// Sheath had one credential: whoever held the admin token held the fleet, and
// nothing recorded who had used it. That is the wrong shape as soon as there
// is a second pair of hands — and it became the wrong shape for a different
// reason when the interface moved behind a reverse proxy and stopped being a
// page on the LAN.
//
// Two roles, because two is what the work divides into. An operator installs
// blades, reinstalls them, calls an installation off and takes a blade out of
// service: everything that happens at the rack. An admin does that and
// everything that decides what the fleet *is* — sites, BladeRunners, images,
// policy, notifications, backups, accounts, and removing a blade from the
// books.

type role string

const (
	roleAdmin    role = "admin"
	roleOperator role = "operator"
)

func validRole(r role) bool { return r == roleAdmin || r == roleOperator }

// atLeast answers the only question the guards ask: may this role do a thing
// that needs that one. Deliberately not a number — there are two roles and
// one of them contains the other, and a comparison that reads as English is
// worth more here than a scale nobody can extend without thinking.
func (r role) atLeast(need role) bool {
	if need == roleOperator {
		return r == roleOperator || r == roleAdmin
	}
	return r == roleAdmin
}

type User struct {
	Name      string `json:"name"`
	Role      role   `json:"role"`
	Created   string `json:"created"`
	LastLogin string `json:"last_login"`
	Disabled  bool   `json:"disabled"`
}

const userSchema = `
CREATE TABLE IF NOT EXISTS users (
    name       TEXT PRIMARY KEY,
    pass       TEXT NOT NULL,
    role       TEXT NOT NULL DEFAULT 'operator',
    created    TEXT NOT NULL,
    last_login TEXT NOT NULL DEFAULT '',
    disabled   INTEGER NOT NULL DEFAULT 0
);
`

// ── Passwords ────────────────────────────────────────────────────────
//
// PBKDF2-HMAC-SHA256 with a per-user salt. It is in the standard library as
// of Go 1.24, so the one thing here that must not be improvised — and the one
// thing a homelab tool is most tempted to improvise — is somebody else's
// well-read code.
//
// The stored form says how it was made, so the cost can be raised later
// without a flag day: an old hash still verifies with the numbers it carries.

const pbkdf2Iterations = 240_000

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return encodeHash(pw, salt, pbkdf2Iterations)
}

func encodeHash(pw string, salt []byte, iter int) (string, error) {
	key, err := pbkdf2.Key(sha256.New, pw, salt, iter, 32)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", iter,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// passwordMatches is constant time in the comparison, which is the part that
// matters: everything before it is public knowledge stored beside the hash.
func passwordMatches(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	again, err := encodeHash(pw, salt, iter)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(again), []byte(stored)) == 1
}

// ── The accounts themselves ──────────────────────────────────────────

// validUserName keeps the names to what can be typed, logged and read back
// without quoting: letters, digits, dot, dash, underscore.
func validUserName(name string) bool {
	if len(name) < 2 || len(name) > 32 {
		return false
	}
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '-' || c == '_':
		default:
			return false
		}
	}
	return true
}

func (a *App) listUsers() ([]User, error) {
	rows, err := a.db.Query(`SELECT name,role,created,last_login,disabled FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		var disabled int
		if err := rows.Scan(&u.Name, &u.Role, &u.Created, &u.LastLogin, &disabled); err != nil {
			return nil, err
		}
		u.Disabled = disabled != 0
		out = append(out, u)
	}
	return out, rows.Err()
}

func (a *App) getUser(name string) (*User, error) {
	var u User
	var disabled int
	err := a.db.QueryRow(`SELECT name,role,created,last_login,disabled FROM users WHERE name=?`, name).
		Scan(&u.Name, &u.Role, &u.Created, &u.LastLogin, &disabled)
	if err != nil {
		return nil, err
	}
	u.Disabled = disabled != 0
	return &u, nil
}

func (a *App) createUser(name, pw string, r role) error {
	if !validUserName(name) {
		return me("err.username")
	}
	if !validRole(r) {
		return me("err.userrole")
	}
	if err := usablePassword(pw); err != nil {
		return err
	}
	hash, err := hashPassword(pw)
	if err != nil {
		return err
	}
	_, err = a.db.Exec(`INSERT INTO users(name,pass,role,created) VALUES(?,?,?,?)`,
		name, hash, string(r), now())
	if err != nil {
		return me("err.usertaken", name)
	}
	return nil
}

// usablePassword is the one rule worth having: length. Everything else is a
// composition rule, and composition rules are how people end up with
// Password1!.
func usablePassword(pw string) error {
	if len(pw) < 10 {
		return me("err.userpw")
	}
	return nil
}

func (a *App) setUserPassword(name, pw string) error {
	if err := usablePassword(pw); err != nil {
		return err
	}
	hash, err := hashPassword(pw)
	if err != nil {
		return err
	}
	res, err := a.db.Exec(`UPDATE users SET pass=? WHERE name=?`, hash, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return me("err.nosuchuser", name)
	}
	return nil
}

// setUserRole and setUserDisabled both refuse to leave the installation
// without a way in: the last admin who can still sign in is not something an
// interface should let somebody click away by accident.
func (a *App) setUserRole(name string, r role) error {
	if !validRole(r) {
		return me("err.userrole")
	}
	if r != roleAdmin {
		if err := a.wouldStrandUs(name); err != nil {
			return err
		}
	}
	_, err := a.db.Exec(`UPDATE users SET role=? WHERE name=?`, string(r), name)
	return err
}

func (a *App) setUserDisabled(name string, off bool) error {
	if off {
		if err := a.wouldStrandUs(name); err != nil {
			return err
		}
	}
	v := 0
	if off {
		v = 1
	}
	_, err := a.db.Exec(`UPDATE users SET disabled=? WHERE name=?`, v, name)
	return err
}

func (a *App) deleteUser(name string) error {
	if err := a.wouldStrandUs(name); err != nil {
		return err
	}
	_, err := a.db.Exec(`DELETE FROM users WHERE name=?`, name)
	return err
}

// wouldStrandUs refuses the change that removes the last admin who can sign
// in. The admin token is still a way in, and it is deliberately not counted:
// a file on the server is a recovery route, not an account, and an
// installation whose only administrator is a file is one nobody is
// responsible for.
func (a *App) wouldStrandUs(name string) error {
	var left int
	err := a.db.QueryRow(`SELECT COUNT(*) FROM users
		WHERE role='admin' AND disabled=0 AND name<>?`, name).Scan(&left)
	if err != nil {
		return err
	}
	if left == 0 {
		return me("err.lastadmin")
	}
	return nil
}

// authenticate answers the sign-in form. A wrong name and a wrong password
// take the same time and give the same answer, because the difference is
// worth nothing to the person signing in and everything to somebody guessing.
func (a *App) authenticate(name, pw string) (*User, bool) {
	var stored, r string
	var disabled int
	err := a.db.QueryRow(`SELECT pass,role,disabled FROM users WHERE name=?`, name).
		Scan(&stored, &r, &disabled)
	if err != nil {
		// Spend about as long as a real verification would, so the absence of
		// a name cannot be read off the clock.
		_, _ = hashPassword(pw)
		return nil, false
	}
	if !passwordMatches(stored, pw) || disabled != 0 {
		return nil, false
	}
	_, _ = a.db.Exec(`UPDATE users SET last_login=? WHERE name=?`, now(), name)
	return &User{Name: name, Role: role(r)}, true
}

// anyUsers says whether accounts have been created at all. Until they have,
// the admin token is the only way in and the interface says so.
func (a *App) anyUsers() bool {
	var n int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

var _ = time.Now
