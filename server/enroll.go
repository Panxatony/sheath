package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Letting a site sign itself in.
//
// A site used to be enrolled by generating its token here, copying it into a
// file on the site machine and setting the mode by hand. That works, and it
// puts a permanent credential through a clipboard and a shell history on the
// way. The blades solved this long ago: they enroll and are handed a token
// they never have to be told.
//
// So a site gets the same: a code that is short enough to read out over the
// phone, good once, and good for an hour. What comes back is the permanent
// token, written straight into a file the site owns. If the code leaks, it
// leaks for one hour and buys one enrollment — and the interface shows when a
// site last signed in, so a stolen one is visible.

const enrollWindow = time.Hour

// enrollAlphabet leaves out the characters people misread aloud: no O and 0
// together, no I, l and 1 together.
const enrollAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

func newEnrollCode() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// The alternative is a predictable code, and there is no version of
		// that worth having.
		panic("no randomness available: " + err.Error())
	}
	out := make([]byte, 0, 14)
	for i, v := range b {
		if i > 0 && i%4 == 0 {
			out = append(out, '-')
		}
		out = append(out, enrollAlphabet[int(v)%len(enrollAlphabet)])
	}
	return string(out)
}

// hSiteEnrollCode hands out a fresh code for one site. It replaces whatever
// was there: two valid codes for one site is one more than anybody needs.
func (a *App) hSiteEnrollCode(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	code, until, err := a.makeEnrollCode(id)
	if err != nil {
		fail(w, 404, "%v", err)
		return
	}
	writeJSON(w, 201, map[string]any{
		"site_id": id, "code": code, "expires": until.UTC().Format(time.RFC3339),
	})
}

func (a *App) makeEnrollCode(id int64) (string, time.Time, error) {
	st, err := a.getSite(id)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("unknown site")
	}
	code := newEnrollCode()
	until := time.Now().UTC().Add(enrollWindow)
	if _, err := a.db.Exec(`UPDATE sites SET enroll_code=?, enroll_until=? WHERE id=?`,
		code, until.Format(time.RFC3339), id); err != nil {
		return "", time.Time{}, err
	}
	a.logEvent("", "warn", fmt.Sprintf("site %d (%s): enrollment code issued, valid for one hour",
		id, st.Name))
	return code, until, nil
}

// hSiteEnroll is the one endpoint here that carries no credential, because
// the code in the body is the credential. It is spent on use.
func (a *App) hSiteEnroll(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
		Host string `json:"hostname"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		fail(w, 400, "JSON invalid")
		return
	}
	code := normaliseCode(in.Code)
	if code == "" {
		fail(w, 400, "no code")
		return
	}
	sites, err := a.listSites()
	if err != nil {
		fail(w, 500, "%v", err)
		return
	}
	now := time.Now().UTC()
	for _, st := range sites {
		if st.EnrollCode == "" {
			continue
		}
		// Constant time, so a wrong code cannot be narrowed down by how long
		// it took to be refused.
		if subtle.ConstantTimeCompare([]byte(normaliseCode(st.EnrollCode)), []byte(code)) != 1 {
			continue
		}
		until, perr := time.Parse(time.RFC3339, st.EnrollUntil)
		if perr != nil || now.After(until) {
			a.clearEnrollCode(st.ID)
			fail(w, 403, "that code has expired")
			return
		}
		tok := newToken()
		if _, err := a.db.Exec(`UPDATE sites SET token=?, enroll_code='', enroll_until='' WHERE id=?`,
			tok, st.ID); err != nil {
			fail(w, 500, "%v", err)
			return
		}
		from := in.Host
		if from == "" {
			from = strings.SplitN(r.RemoteAddr, ":", 2)[0]
		}
		a.logEvent("", "warn", fmt.Sprintf("site %d (%s) enrolled from %s", st.ID, st.Name, from))
		writeJSON(w, 201, map[string]any{
			"site_id": st.ID, "name": st.Name, "token": tok,
			"net_base": st.NetBase, "server": a.baseURL,
		})
		return
	}
	// Nothing about which part was wrong: an attacker with a guess should
	// learn only that it was a guess.
	fail(w, 403, "that code is not valid")
}

func (a *App) clearEnrollCode(id int64) {
	_, _ = a.db.Exec(`UPDATE sites SET enroll_code='', enroll_until='' WHERE id=?`, id)
}

// normaliseCode forgives what a person does to a code on the way from a
// screen to a terminal: lower case, spaces, missing dashes.
func normaliseCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(s)) {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// enrollState describes a site's pending code for the interface.
func (a *App) enrollState(st Site) (code string, left time.Duration) {
	if st.EnrollCode == "" || st.EnrollUntil == "" {
		return "", 0
	}
	until, err := time.Parse(time.RFC3339, st.EnrollUntil)
	if err != nil {
		return "", 0
	}
	if d := time.Until(until); d > 0 {
		return st.EnrollCode, d.Round(time.Minute)
	}
	return "", 0
}
