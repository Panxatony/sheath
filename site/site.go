package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// desired is the document the centre hands over. It is written to disk after
// every change, because the whole point of the split is that this program can
// still act when the centre is unreachable.
// desiredBlade is one blade as the centre sees it, including what the blade
// itself would be told — see the note on tokens in the site design.
type desiredBlade struct {
	Serial   string `json:"serial"`
	MAC      string `json:"mac"`
	Hostname string `json:"hostname"`
	IP       string `json:"ip"`
	Rack     string `json:"rack"`
	Slot     int    `json:"slot"`
	Netboot  bool   `json:"netboot"`
	Image    string `json:"image"`

	Token         string         `json:"token"`
	Config        map[string]any `json:"config"`
	ConfigVersion string         `json:"config_version"`
	InstallState  string         `json:"install_state"`
}

// desiredImage is an image this site should hold, because a blade of its own
// is assigned to it.
type desiredImage struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Local  string `json:"local"`
}

// desired is the document the centre hands over. It is written to disk after
// every change, because the whole point of the split is that this program can
// still act when the centre is unreachable.
type desired struct {
	Site struct {
		ID       int64  `json:"id"`
		Name     string `json:"name"`
		NetBase  string `json:"net_base"`
		Gateway  string `json:"gateway"`
		DNS      string `json:"dns"`
		Domain   string `json:"domain"`
		PoolFrom int    `json:"pool_from"`
		PoolTo   int    `json:"pool_to"`
	} `json:"site"`
	Blades []desiredBlade `json:"blades"`
	Images []desiredImage `json:"images"`
	Boot   struct {
		BootImg    string `json:"boot_img"`
		SHA256     string `json:"sha256"`
		CmdlineURL string `json:"cmdline_url"`
		ServerURL  string `json:"server_url"`
	} `json:"boot"`
	Version  string `json:"version"`
	Produced string `json:"produced"`
}

type site struct {
	cfg    config
	dry    bool
	http   *http.Client
	dl     *http.Client
	etag   string
	state  *desired
	mu     sync.Mutex
	queue  []event
	online bool
	relay  *relay
}

// event is an observation waiting to be reported. They are buffered rather
// than dropped: what happened during an outage is exactly what someone will
// want to read afterwards.
type event struct {
	TS     string `json:"ts"`
	Serial string `json:"serial,omitempty"`
	Level  string `json:"level,omitempty"`
	Msg    string `json:"msg"`
	Stage  string `json:"stage,omitempty"`
	MAC    string `json:"mac,omitempty"`
	IP     string `json:"ip,omitempty"`
}

const maxQueued = 2000

func newSite(c config, dry bool) *site {
	return &site{
		cfg: c,
		dry: dry,
		// Two clients: the pull is a small document on a short leash, an
		// image is hundreds of megabytes over a line that may be slow.
		http: &http.Client{Timeout: 30 * time.Second},
		dl:   &http.Client{Timeout: 0},
	}
}

// pass is one turn of the loop: fetch what should be, make it so, report what
// happened. Every step is allowed to fail on its own — a site with a stale
// image list still has to write its reservations.
func (s *site) pass() error {
	d, changed, err := s.fetch()
	if err != nil {
		s.online = false
		// Fall back to what was last known. Without this the first pass after
		// a reboot during an outage would do nothing at all.
		if s.stateHeld() == nil {
			if cached := s.loadState(); cached != nil {
				s.setState(cached)
				log.Printf("centre unreachable (%v) — working from the cached state %s",
					err, cached.Version)
			} else {
				return err
			}
		}
		d = s.stateHeld()
	} else {
		s.online = true
		s.setState(d)
		if changed {
			s.saveState(d)
			log.Printf("new desired state %s: %d blades, %d images",
				d.Version, len(d.Blades), len(d.Images))
		}
	}

	if err := s.writeReservations(d); err != nil {
		log.Printf("reservations: %v", err)
		s.note("", "warn", "reservations: "+err.Error())
	}
	if err := s.ensureBoot(d); err != nil {
		log.Printf("boot payload: %v", err)
	}
	if err := s.ensureImages(d); err != nil {
		log.Printf("images: %v", err)
	}
	s.flush()
	if s.online && s.relay != nil {
		s.relay.drain()
	}
	s.report(d)
	return nil
}

func (s *site) fetch() (*desired, bool, error) {
	req, err := http.NewRequest("GET",
		fmt.Sprintf("%s/api/v1/site/%d/desired", s.cfg.Server, s.cfg.SiteID), nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	req.Header.Set("User-Agent", version)
	if s.etag != "" {
		req.Header.Set("If-None-Match", s.etag)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return s.stateHeld(), false, nil
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var d desired
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, false, err
	}
	s.etag = d.Version
	return &d, true, nil
}

// note buffers an observation. Never blocks and never grows without bound —
// a site that has been alone for a week must not run out of memory over it.
func (s *site) note(serial, level, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) >= maxQueued {
		s.queue = s.queue[len(s.queue)-maxQueued/2:]
	}
	s.queue = append(s.queue, event{
		TS: time.Now().UTC().Format(time.RFC3339), Serial: serial, Level: level, Msg: msg,
	})
}

// stage reports what a MAC is doing on the wire, with the address if the line
// carried one — the netboot view is read by people looking for a blade, and a
// MAC alone is not something anyone recognises.
func (s *site) stage(mac, stage, msg string) { s.stageIP(mac, "", stage, msg) }

func (s *site) stageIP(mac, ip, stage, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = append(s.queue, event{
		TS: time.Now().UTC().Format(time.RFC3339), MAC: mac, IP: ip, Stage: stage, Msg: msg,
	})
}

// flush hands the buffer over. On failure the events stay queued — they are
// the record of an outage, and dropping them would erase exactly the part
// nobody else saw.
func (s *site) flush() {
	s.mu.Lock()
	pending := s.queue
	s.queue = nil
	s.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	body, _ := json.Marshal(map[string]any{"events": pending})
	req, err := http.NewRequest("POST",
		fmt.Sprintf("%s/api/v1/site/%d/events", s.cfg.Server, s.cfg.SiteID),
		bytes.NewReader(body))
	if err == nil {
		req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
		req.Header.Set("Content-Type", "application/json")
		resp, derr := s.http.Do(req)
		if derr == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			err = derr
		}
	}
	log.Printf("events not delivered (%v) — %d kept", err, len(pending))
	s.mu.Lock()
	s.queue = append(pending, s.queue...)
	s.mu.Unlock()
}

func (s *site) report(d *desired) {
	applied := ""
	if d != nil {
		applied = d.Version
	}
	body, _ := json.Marshal(map[string]any{
		"version":    version,
		"applied":    applied,
		"clock":      time.Now().UTC().Format(time.RFC3339),
		"blades":     len(d.Blades),
		"images":     len(d.Images),
		"dnsmasq_ok": true,
	})
	req, err := http.NewRequest("POST",
		fmt.Sprintf("%s/api/v1/site/%d/status", s.cfg.Server, s.cfg.SiteID),
		bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+s.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := s.http.Do(req); err == nil {
		resp.Body.Close()
	}
}

// setState and stateHeld guard the one piece of memory both loops touch: the
// pull loop replaces it, the relay reads it while answering a blade.
func (s *site) setState(d *desired) {
	s.mu.Lock()
	s.state = d
	s.mu.Unlock()
}

func (s *site) stateHeld() *desired {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// blade looks up one blade in the state currently held. Returns nil when the
// site has never heard of it — which is a real answer, not an error: an
// unknown blade is one the centre has to decide about.
func (s *site) blade(serial string) *desiredBlade {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil {
		return nil
	}
	for i := range s.state.Blades {
		if s.state.Blades[i].Serial == serial {
			return &s.state.Blades[i]
		}
	}
	return nil
}

func (s *site) image(id string) *desiredImage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil {
		return nil
	}
	for i := range s.state.Images {
		if s.state.Images[i].ID == id {
			return &s.state.Images[i]
		}
	}
	return nil
}

func (s *site) appliedVersion() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == nil {
		return ""
	}
	return s.state.Version
}

// ── The cached state ─────────────────────────────────────────────────

func (s *site) statePath() string { return filepath.Join(s.cfg.StateDir, "desired.json") }

func (s *site) saveState(d *desired) {
	if s.dry {
		return
	}
	if err := os.MkdirAll(s.cfg.StateDir, 0o750); err != nil {
		log.Printf("state directory: %v", err)
		return
	}
	raw, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return
	}
	tmp := s.statePath() + ".new"
	if err := os.WriteFile(tmp, raw, 0o640); err != nil {
		log.Printf("state not written: %v", err)
		return
	}
	_ = os.Rename(tmp, s.statePath())
}

func (s *site) loadState() *desired {
	raw, err := os.ReadFile(s.statePath())
	if err != nil {
		return nil
	}
	var d desired
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil
	}
	return &d
}
