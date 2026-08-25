// rookery-agent runs on every blade and keeps it in step with the server.
//
// The agent pulls its desired state instead of having it pushed: it asks
// every 60 seconds, applies changes idempotently and reports back what it
// actually finds. That works with DHCP, needs no SSH access from the server,
// and converges by itself after every reboot.
//
// Its credentials were placed by the installer during provisioning:
// /etc/rookery/agent.env
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const (
	defaultEnvFile = "/etc/rookery/agent.env"
	stateFile      = "/var/lib/rookery/applied"
	userAgent      = "rookery-agent/1"
)

// Before the rename the project was called Blademaster. A blade written with
// the old paths has to keep working — otherwise the rename would be an
// outage rather than a migration.
var legacyEnvFiles = []string{"/etc/blademaster/agent.env"}
var legacyStateFiles = []string{"/var/lib/blademaster/applied"}

const legacyPrefix = "BLADEMASTER_"

type Config struct {
	Server string
	Serial string
	Token  string
}

type agent struct {
	cfg     Config
	http    *http.Client
	applied string   // last applied config version
	pending []string // changes from the last apply, waiting to be reported
}

func main() {
	var (
		envFile  = flag.String("env", defaultEnvFile, "file holding server, serial and token")
		interval = flag.Duration("interval", 60*time.Second, "interval between two passes")
		once     = flag.Bool("once", false, "run a single pass and exit")
		show     = flag.Bool("show", false, "print facts and health only, send nothing")
	)
	flag.Parse()

	log.SetFlags(0) // systemd adds its own timestamps

	// Diagnostics: shows what the agent would see, without a server and
	// without credentials. Useful on a new blade to check whether the
	// readings come out at all.
	if *show {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"facts":  collectFacts(),
			"health": collectHealth(),
		})
		return
	}

	cfg, err := loadConfig(*envFile)
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}
	a := &agent{
		cfg:  cfg,
		http: &http.Client{Timeout: 30 * time.Second},
	}
	a.applied = readState()

	log.Printf("Rookery agent started — serial %s, server %s", cfg.Serial, cfg.Server)
	if a.applied != "" {
		log.Printf("last applied configuration: %s", a.applied)
	}

	if *once {
		if err := a.tick(); err != nil {
			log.Fatalf("pass failed: %v", err)
		}
		return
	}

	// A random offset stops twenty blades from hammering the server in
	// lockstep after a power cut.
	jitter := time.Duration(rand.Int63n(int64(*interval / 4)))
	time.Sleep(jitter)

	for {
		if err := a.tick(); err != nil {
			log.Printf("pass failed: %v", err)
		}
		time.Sleep(*interval)
	}
}

// tick is one complete pass. The order is deliberate: report first, then
// fetch commands, then apply configuration. That way the server is current
// even when applying fails afterwards.
func (a *agent) tick() error {
	if err := a.report(); err != nil {
		return fmt.Errorf("status report: %w", err)
	}
	if err := a.runCommands(); err != nil {
		log.Printf("commands: %v", err)
	}
	if err := a.syncConfig(); err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	return nil
}

// ── Credentials ──────────────────────────────────────────────────────

func loadConfig(path string) (Config, error) {
	var c Config
	// systemd passes the values through EnvironmentFile; when invoked by hand
	// the agent reads the file itself. Both should work.
	c.Server = firstEnv("ROOKERY_SERVER", legacyPrefix+"SERVER")
	c.Serial = firstEnv("ROOKERY_SERIAL", legacyPrefix+"SERIAL")
	c.Token = firstEnv("ROOKERY_TOKEN", legacyPrefix+"TOKEN")

	for _, p := range append([]string{path}, legacyEnvFiles...) {
		readEnvFile(p, &c)
	}
	// The serial number is also in the hardware — in case the seed is gone.
	if c.Serial == "" {
		c.Serial = readSerial()
	}
	if c.Server == "" || c.Serial == "" || c.Token == "" {
		return c, fmt.Errorf("incomplete (server=%q serial=%q token=%s) — check %s",
			c.Server, c.Serial, present(c.Token), path)
	}
	c.Server = strings.TrimRight(c.Server, "/")
	return c, nil
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}

// readEnvFile only fills what is still empty — first source wins. That way
// the new file beats the old one without the old one being ignored.
func readEnvFile(path string, c *Config) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		// Fold the old and new spellings onto the same name.
		k = strings.TrimPrefix(k, legacyPrefix)
		k = strings.TrimPrefix(k, "ROOKERY_")
		switch k {
		case "SERVER":
			if c.Server == "" {
				c.Server = v
			}
		case "SERIAL":
			if c.Serial == "" {
				c.Serial = v
			}
		case "TOKEN":
			if c.Token == "" {
				c.Token = v
			}
		}
	}
}

func present(s string) string {
	if s == "" {
		return "missing"
	}
	return "set"
}

// ── HTTP ─────────────────────────────────────────────────────────────

func (a *agent) do(method, path string, body any, out any, extra map[string]string) (int, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, a.cfg.Server+path, rdr)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return resp.StatusCode, nil
	}
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return resp.StatusCode, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("response unreadable: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// ── Reporting ────────────────────────────────────────────────────────

func (a *agent) report() error {
	payload := map[string]any{
		"facts":          collectFacts(),
		"health":         collectHealth(),
		"config_applied": a.applied,
	}
	// What the agent changed belongs where the fleet is watched, not only in
	// the journal of the machine it changed. Handed over once, then cleared:
	// a change reported every minute would read as a change happening every
	// minute.
	if len(a.pending) > 0 {
		payload["changes"] = a.pending
		a.pending = nil
	}
	_, err := a.do("POST", "/api/v1/blades/"+a.cfg.Serial+"/status", payload, nil, nil)
	return err
}

// ── Configuration ────────────────────────────────────────────────────

type configResp struct {
	Version string         `json:"version"`
	Config  map[string]any `json:"config"`
}

// syncConfig fetches the desired state and applies it — but only when it has
// changed. The server supplies an ETag for that; if nothing moved it answers
// 304 and sends no data at all.
func (a *agent) syncConfig() error {
	extra := map[string]string{}
	if a.applied != "" {
		extra["If-None-Match"] = `"` + a.applied + `"`
	}
	var cr configResp
	code, err := a.do("GET", "/api/v1/blades/"+a.cfg.Serial+"/config", nil, &cr, extra)
	if err != nil {
		return err
	}
	if code == http.StatusNotModified {
		return nil
	}
	if cr.Version == a.applied {
		return nil
	}

	log.Printf("new configuration %s — applying", cr.Version)
	changes, err := applyConfig(cr.Config)
	if err != nil {
		// Partially applied: do NOT record the version, so the next pass
		// tries again.
		log.Printf("applied incompletely: %v", err)
		for _, c := range changes {
			log.Printf("  %s", c)
		}
		a.pending = append(a.pending, changes...)
		a.pending = append(a.pending, "FAILED: "+err.Error())
		return err
	}
	if len(changes) == 0 {
		log.Printf("nothing to do — state already matches")
	}
	a.pending = append(a.pending, changes...)
	for _, c := range changes {
		log.Printf("  %s", c)
	}
	a.applied = cr.Version
	writeState(cr.Version)
	return nil
}

// ── Commands ─────────────────────────────────────────────────────────

type command struct {
	ID      int64           `json:"id"`
	Kind    string          `json:"kind"`
	Args    json.RawMessage `json:"args"`
	Created string          `json:"created"`
}

// commandTTL is the second safeguard. The server clears expired commands
// itself; should it ever fail to — an older build, a wrong clock — the agent
// still refuses to run them. On this agent's very first start, seven-hour-old
// test commands sat in the queue and triggered an immediate reboot.
const commandTTL = 15 * time.Minute

func (c command) stale() bool {
	if c.Created == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, c.Created)
	if err != nil {
		return false
	}
	return time.Since(t) > commandTTL
}

func (a *agent) runCommands() error {
	var cmds []command
	if _, err := a.do("GET", "/api/v1/blades/"+a.cfg.Serial+"/commands", nil, &cmds, nil); err != nil {
		return err
	}
	// Of commands of the same kind only the newest counts. Four "reimage" in
	// a row does not mean installing four times.
	newest := map[string]command{}
	var order []string
	for _, c := range cmds {
		if c.stale() {
			log.Printf("command %d (%s) is older than %s — skipped",
				c.ID, c.Kind, commandTTL)
			continue
		}
		if _, seen := newest[c.Kind]; !seen {
			order = append(order, c.Kind)
		}
		newest[c.Kind] = c
	}
	// A reboot makes everything after it moot — so it goes last.
	sort.SliceStable(order, func(i, j int) bool {
		return rank(order[i]) < rank(order[j])
	})

	for _, kind := range order {
		c := newest[kind]
		log.Printf("command %d: %s", c.ID, c.Kind)
		switch c.Kind {
		case "identify":
			if err := identify(true); err != nil {
				log.Printf("  identify not possible: %v", err)
			}
		case "identify_off":
			// The identify state is normally ended by someone pressing the
			// blade's button — that is what it is for. --confirm does the
			// same from here, for whoever is not standing at the rack.
			if err := identify(false); err != nil {
				log.Printf("  identify not stopped: %v", err)
			}
		case "stealth_on", "stealth_off":
			if err := stealth(c.Kind == "stealth_on"); err != nil {
				log.Printf("  stealth not switched: %v", err)
			}
		case "reboot":
			log.Printf("  rebooting in 5 s")
			go delayedReboot(5 * time.Second)
		case "reimage":
			// The server has already unlocked netboot; a reboot is enough for
			// the blade to land in the installer.
			log.Printf("  reinstall requested — rebooting in 10 s")
			go delayedReboot(10 * time.Second)
		default:
			log.Printf("  unknown, skipped")
		}
	}
	return nil
}

// ── Scratch note ─────────────────────────────────────────────────────

func readState() string {
	for _, p := range append([]string{stateFile}, legacyStateFiles...) {
		if b, err := os.ReadFile(p); err == nil {
			if v := strings.TrimSpace(string(b)); v != "" {
				return v
			}
		}
	}
	return ""
}

func writeState(v string) {
	if err := os.MkdirAll("/var/lib/rookery", 0o755); err != nil {
		return
	}
	if err := os.WriteFile(stateFile, []byte(v+"\n"), 0o644); err != nil {
		log.Printf("scratch note not writable: %v", err)
	}
}

// rank orders commands so reboots come last.
func rank(kind string) int {
	switch kind {
	case "identify", "identify_off", "stealth_on", "stealth_off":
		return 0
	case "reimage":
		return 8
	case "reboot":
		return 9
	}
	return 5
}

var errNoTool = errors.New("tool not present")
