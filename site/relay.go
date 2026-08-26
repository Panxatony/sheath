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

// The relay.
//
// Blades talk to their site, not to the centre. While the line is up that is
// a hop and nothing more. While it is down it is the whole point: a blade
// rebooting in a power cut must get its address, find its image and come back
// up without anyone far away answering the phone.
//
// What the site can answer alone is what the desired state contains: the
// configuration of its own blades, the job an installer is waiting for, the
// image bytes themselves. What it cannot invent is a decision — a command
// somebody typed, or an image nobody assigned. Those wait.

type relay struct {
	s        *site
	upstream string
	client   *http.Client

	mu      sync.Mutex
	pending []queued // blade reports waiting for the centre
}

// queued is a report a blade made while the centre was unreachable. Kept in
// order: a progress report that arrives after the "done" it precedes would
// tell the wrong story.
type queued struct {
	Method string
	Path   string
	Body   []byte
	Auth   string
	When   time.Time
}

const maxPending = 5000

func newRelay(s *site) *relay {
	return &relay{
		s:        s,
		upstream: s.cfg.Server,
		// Short: a blade waiting on us is a blade not booting. If the centre
		// does not answer in ten seconds it counts as away, and the site
		// answers by itself.
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (r *relay) routes() *http.ServeMux {
	mux := http.NewServeMux()

	// The installer's questions. Answerable from the cached state, which is
	// what makes an installation during an outage possible at all.
	mux.HandleFunc("POST /api/v1/provision/{serial}", r.provision)
	mux.HandleFunc("POST /api/v1/provision/{serial}/status", r.forwardOrQueue)

	// The agent's conversation.
	mux.HandleFunc("GET /api/v1/blades/{serial}/config", r.bladeConfig)
	mux.HandleFunc("POST /api/v1/blades/{serial}/status", r.forwardOrQueue)
	mux.HandleFunc("GET /api/v1/blades/{serial}/commands", r.commands)
	mux.HandleFunc("POST /api/v1/enroll", r.forwardOrQueue)

	// Bytes the site holds anyway.
	mux.HandleFunc("GET /images/", r.serveLocal(r.s.cfg.ImagesDir, "/images/"))
	mux.HandleFunc("GET /agent/", r.passThrough)
	mux.HandleFunc("GET /boot/", r.serveLocal(r.s.cfg.TFTPDir, "/boot/"))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, 200, map[string]any{
			"site":    r.s.cfg.SiteID,
			"online":  r.s.online,
			"applied": r.s.appliedVersion(),
			"queued":  r.queuedCount(),
		})
	})
	return mux
}

// provision answers the installer. Online this is the centre's call, because
// only the centre knows whether someone has just pressed the button. Offline
// the site answers from the state it holds — which is the last decision the
// centre made, and acting on it is exactly what it is for.
func (r *relay) provision(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if resp, err := r.forward(req, body); err == nil {
		defer resp.Body.Close()
		copyResponse(w, resp)
		return
	}

	serial := req.PathValue("serial")
	b := r.s.blade(serial)
	if b == nil {
		// An unknown blade is an enrolment, and enrolling is a decision the
		// centre makes. Told to wait rather than turned away: it will ask
		// again, and by then the line may be back.
		writeJSON(w, 200, map[string]any{
			"status": "waiting", "serial": serial, "retry_after": 30,
			"message": "site is offline — waiting for the central server",
		})
		return
	}
	// An erase is a job the site can hand out on its own: it needs no image
	// and no decision beyond the one already made.
	if b.InstallState == "wipe" {
		log.Printf("wipe %s answered from the cache (offline)", serial)
		r.s.note(serial, "warn", "erase started from the site cache — centre unreachable")
		writeJSON(w, 200, map[string]any{
			"status": "wipe", "serial": serial, "target": "/dev/nvme0n1",
			"token": b.Token, "message": "erasing the disk (answered by the site, offline)",
		})
		return
	}
	if b.Image == "" || !b.Netboot {
		writeJSON(w, 200, map[string]any{
			"status": "idle", "serial": serial, "image": b.Image, "retry_after": 30,
			"message": "no install requested (answered by the site, offline)",
		})
		return
	}
	im := r.s.image(b.Image)
	if im == nil {
		writeJSON(w, 200, map[string]any{
			"status": "waiting", "serial": serial, "retry_after": 60,
			"message": "image " + b.Image + " is not in the site cache",
		})
		return
	}
	name := im.Local
	if name == "" {
		name = filepath.Base(im.URL)
	}
	if _, err := os.Stat(filepath.Join(r.s.cfg.ImagesDir, name)); err != nil {
		writeJSON(w, 200, map[string]any{
			"status": "waiting", "serial": serial, "retry_after": 60,
			"message": "image " + b.Image + " has not arrived here yet",
		})
		return
	}
	log.Printf("provision %s answered from the cache (offline)", serial)
	r.s.note(serial, "warn", "install started from the site cache — centre unreachable")
	writeJSON(w, 200, map[string]any{
		"status": "go",
		"serial": serial,
		"image":  im.ID,
		"url":    r.selfURL(req) + "/images/" + name,
		"sha256": im.SHA256,
		"token":  b.Token,
		"target": "/dev/nvme0n1",
	})
}

// bladeConfig hands the agent its desired state. The site holds it for its
// own blades, so a configuration pass survives an outage — which matters,
// because the alternative is a blade drifting for as long as the line is down.
func (r *relay) bladeConfig(w http.ResponseWriter, req *http.Request) {
	if resp, err := r.forward(req, nil); err == nil {
		defer resp.Body.Close()
		copyResponse(w, resp)
		return
	}
	serial := req.PathValue("serial")
	b := r.s.blade(serial)
	if b == nil || b.Config == nil {
		http.Error(w, `{"error":"not in the site cache"}`, http.StatusServiceUnavailable)
		return
	}
	if !r.authorised(req, b.Token) {
		http.Error(w, `{"error":"blade token missing or wrong"}`, http.StatusUnauthorized)
		return
	}
	w.Header().Set("ETag", `"`+b.ConfigVersion+`"`)
	if strings.Contains(req.Header.Get("If-None-Match"), b.ConfigVersion) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, 200, map[string]any{"version": b.ConfigVersion, "config": b.Config})
}

// commands cannot be invented. A command is something a person asked for, and
// while the centre is unreachable there is nothing to ask. An empty list is
// the honest answer; the agent will ask again.
func (r *relay) commands(w http.ResponseWriter, req *http.Request) {
	if resp, err := r.forward(req, nil); err == nil {
		defer resp.Body.Close()
		copyResponse(w, resp)
		return
	}
	writeJSON(w, 200, map[string]any{"commands": []any{}})
}

// forwardOrQueue passes a report on, or keeps it until someone is listening.
// Reports are facts about what happened; losing them would erase exactly the
// part of the story nobody else saw.
func (r *relay) forwardOrQueue(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(req.Body, 4<<20))
	if resp, err := r.forward(req, body); err == nil {
		defer resp.Body.Close()
		copyResponse(w, resp)
		return
	}
	r.mu.Lock()
	if len(r.pending) >= maxPending {
		r.pending = r.pending[len(r.pending)-maxPending/2:]
	}
	r.pending = append(r.pending, queued{
		Method: req.Method, Path: req.URL.RequestURI(), Body: body,
		Auth: req.Header.Get("Authorization"), When: time.Now(),
	})
	n := len(r.pending)
	r.mu.Unlock()
	log.Printf("queued %s %s (%d waiting)", req.Method, req.URL.Path, n)
	writeJSON(w, 202, map[string]any{"queued": true, "waiting": n})
}

// drain hands the queue over once the centre answers again, oldest first.
func (r *relay) drain() {
	r.mu.Lock()
	pending := r.pending
	r.pending = nil
	r.mu.Unlock()
	if len(pending) == 0 {
		return
	}
	sent := 0
	for i, q := range pending {
		req, err := http.NewRequest(q.Method, r.upstream+q.Path, bytes.NewReader(q.Body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		if q.Auth != "" {
			req.Header.Set("Authorization", q.Auth)
		}
		resp, err := r.client.Do(req)
		if err != nil {
			// Still away. Put back what is left, in order.
			r.mu.Lock()
			r.pending = append(pending[i:], r.pending...)
			r.mu.Unlock()
			break
		}
		resp.Body.Close()
		sent++
	}
	if sent > 0 {
		log.Printf("delivered %d queued report(s)", sent)
		r.s.note("", "info", fmt.Sprintf("%d buffered blade report(s) delivered", sent))
	}
}

func (r *relay) forward(req *http.Request, body []byte) (*http.Response, error) {
	out, err := http.NewRequest(req.Method, r.upstream+req.URL.RequestURI(),
		bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for _, h := range []string{"Authorization", "Content-Type", "If-None-Match", "Range"} {
		if v := req.Header.Get(h); v != "" {
			out.Header.Set(h, v)
		}
	}
	// Say who really asked, so the centre's log does not read as if every
	// blade in the field were this one machine.
	out.Header.Set("X-Forwarded-For", clientIP(req))
	out.Header.Set("X-Rookery-Site", fmt.Sprintf("%d", r.s.cfg.SiteID))
	return r.client.Do(out)
}

// passThrough is for bytes the site does not hold itself.
func (r *relay) passThrough(w http.ResponseWriter, req *http.Request) {
	resp, err := r.forward(req, nil)
	if err != nil {
		http.Error(w, `{"error":"central server unreachable"}`, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	copyResponse(w, resp)
}

// serveLocal hands out what is already here, and only falls back to the
// centre when it is not. An image is the one thing that must never travel the
// site link twice.
func (r *relay) serveLocal(dir, prefix string) http.HandlerFunc {
	fs := http.StripPrefix(prefix, http.FileServer(http.Dir(dir)))
	return func(w http.ResponseWriter, req *http.Request) {
		name := strings.TrimPrefix(req.URL.Path, prefix)
		if name != "" && !strings.Contains(name, "..") {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				fs.ServeHTTP(w, req)
				return
			}
		}
		r.passThrough(w, req)
	}
}

func (r *relay) authorised(req *http.Request, token string) bool {
	if token == "" {
		return false
	}
	given := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	return given == token
}

func (r *relay) selfURL(req *http.Request) string {
	host := req.Host
	if host == "" {
		host = fmt.Sprintf("%s:%d", "127.0.0.1", 8081)
	}
	return "http://" + host
}

func (r *relay) queuedCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

func copyResponse(w http.ResponseWriter, resp *http.Response) {
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func clientIP(req *http.Request) string {
	if i := strings.LastIndexByte(req.RemoteAddr, ':'); i > 0 {
		return req.RemoteAddr[:i]
	}
	return req.RemoteAddr
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
