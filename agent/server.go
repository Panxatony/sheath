package main

import (
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// Following the site a blade stands in.
//
// The address in the seed is the address the installer wrote at install time.
// It is right until the blade is carried to another site, or until the site
// it stands in gets a new machine — and it was wrong from the start for every
// blade that installed before the relay learned to name itself: those were
// pointed at the centre, so the machine next door answered nothing for them
// and an outage at the centre stopped them all.
//
// The configuration a blade pulls now carries the address of whoever handed
// it over. Where that differs from what the blade is using, the blade moves —
// but only after checking that the new address answers. An address that does
// not answer is a blade that goes silent, and a silent blade cannot be told
// to come back.

const serverKey = "SHEATH_SERVER"

// adoptServer takes the address out of the configuration, if it is a
// different one that works, and writes it into the seed for the next start.
// It reports what changed, so the move shows up in the log at the centre like
// any other applied change.
func (a *agent) adoptServer(cfg map[string]any, envFile string) string {
	want, _ := cfg["server_url"].(string)
	want = strings.TrimRight(strings.TrimSpace(want), "/")
	if want == "" || want == a.cfg.Server {
		return ""
	}
	if !serverAnswers(want) {
		log.Printf("server: %s was offered and does not answer — staying with %s", want, a.cfg.Server)
		return ""
	}
	if err := replaceEnvValue(envFile, serverKey, want); err != nil {
		log.Printf("server: %s answers but the seed could not be rewritten: %v", want, err)
		return ""
	}
	log.Printf("server: moving from %s to %s — that is the site this blade stands in",
		a.cfg.Server, want)
	change := "server: " + a.cfg.Server + " → " + want
	a.cfg.Server = want
	return change
}

// serverAnswers is the whole safety of the thing: a health endpoint, a short
// timeout, and no redirects followed.
func serverAnswers(base string) bool {
	cl := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := cl.Get(strings.TrimRight(base, "/") + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// replaceEnvValue rewrites one line of the seed and leaves the rest as it is —
// the token and the serial are in that file too, and this must not be the
// thing that loses them.
func replaceEnvValue(path, key, value string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	lines := strings.Split(string(raw), "\n")
	found := false
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), key+"=") {
			lines[i] = key + "=" + value
			found = true
		}
	}
	if !found {
		lines = append(lines, key+"="+value)
	}
	out := strings.Join(lines, "\n")
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(out), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}
