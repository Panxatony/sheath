package main

import (
	"database/sql"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type App struct {
	db         *sql.DB
	adminToken string
	baseURL    string
	localDHCP  bool
	imagesDir  string
	sess       *sessions

	// Site networks, memoised for the duration of one request
	netCacheMu sync.Mutex
	netCache   map[int64]string
	nameCache  map[int64]string
}

func main() {
	var (
		addr      = flag.String("addr", ":8080", "address the server listens on")
		dbPath    = flag.String("db", "/srv/rookery/data/rookery.db", "path to the SQLite file")
		imagesDir = flag.String("images", "/srv/rookery/images", "directory holding the OS images")
		agentDir  = flag.String("agent", "/srv/rookery/agent", "directory holding the agent binary")
		baseURL   = flag.String("base-url", "", "base URL reachable from the blades (default: http://<net_base>.10:8080)")
		netBase   = flag.String("net-base", "", "base of the blade network, e.g. 10.0.0 (needed on first start only)")
		localDHCP = flag.Bool("local-dhcp", true,
			"write dnsmasq reservations and watch the log here; turn off where a rookery-site does it")
		tftpDir = flag.String("tftp", "/srv/rookery/tftp",
			"TFTP root, served to sites over HTTP so they can offer the same payload")
		dnsmasqLog = flag.String("dnsmasq-log", "/srv/rookery/logs/dnsmasq.log",
			"dnsmasq log file; Rookery reads it to spot blades that are booting")
	)
	flag.Parse()

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		log.Fatalf("data directory: %v", err)
	}
	db, err := openDB(*dbPath)
	if err != nil {
		log.Fatalf("database: %v", err)
	}
	defer db.Close()

	app := &App{db: db, imagesDir: *imagesDir, sess: newSessions()}

	if *netBase != "" {
		if err := app.setSetting("net_base", *netBase); err != nil {
			log.Fatalf("net_base: %v", err)
		}
	}
	// Without a site, existing BladeRunners would have no network.
	if err := app.ensureDefaultSite(*netBase); err != nil {
		log.Fatalf("site: %v", err)
	}
	app.baseURL = *baseURL
	if app.baseURL == "" {
		app.baseURL = app.setting("base_url", "http://"+app.netBase()+".10:8080")
	}
	_ = app.setSetting("base_url", app.baseURL)

	// Admin token: read from file, otherwise generate once.
	tokPath := filepath.Join(filepath.Dir(*dbPath), "admin-token")
	if raw, err := os.ReadFile(tokPath); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		app.adminToken = strings.TrimSpace(string(raw))
	} else {
		app.adminToken = newToken()
		if err := os.WriteFile(tokPath, []byte(app.adminToken+"\n"), 0o600); err != nil {
			log.Printf("WARNING: admin token not stored (%v) — management stays open", err)
			app.adminToken = ""
		} else {
			log.Printf("admin token generated: %s", tokPath)
		}
	}

	mux := http.NewServeMux()

	// ── Management ──
	mux.HandleFunc("GET /api/v1/sites", app.requireAdmin(app.hSitesList))
	mux.HandleFunc("POST /api/v1/sites", app.requireAdmin(app.hSiteCreate))
	mux.HandleFunc("PUT /api/v1/sites/{id}", app.requireAdmin(app.hSiteUpdate))
	mux.HandleFunc("DELETE /api/v1/sites/{id}", app.requireAdmin(app.hSiteDelete))
	mux.HandleFunc("POST /api/v1/sites/{id}/token", app.requireAdmin(app.hSiteToken))

	// The site interface. Authenticated with the site's own token, not the
	// admin token: a site may act for itself and for nothing else.
	mux.HandleFunc("GET /api/v1/site/{id}/desired", app.hSiteDesired)
	mux.HandleFunc("POST /api/v1/site/{id}/events", app.hSiteEvents)
	mux.HandleFunc("POST /api/v1/site/{id}/status", app.hSiteStatus)
	mux.HandleFunc("GET /api/v1/bladerunners", app.requireAdmin(app.hRacksList))
	mux.HandleFunc("POST /api/v1/bladerunners", app.requireAdmin(app.hRacksCreate))
	mux.HandleFunc("PUT /api/v1/bladerunners/{id}", app.requireAdmin(app.hRackUpdate))
	mux.HandleFunc("DELETE /api/v1/bladerunners/{id}", app.requireAdmin(app.hRackDelete))

	mux.HandleFunc("GET /api/v1/blades", app.requireAdmin(app.hBladesList))
	mux.HandleFunc("GET /api/v1/blades/{serial}", app.requireAdmin(app.hBladeGet))
	mux.HandleFunc("PUT /api/v1/blades/{serial}", app.requireAdmin(app.hBladeUpdate))
	mux.HandleFunc("DELETE /api/v1/blades/{serial}", app.requireAdmin(app.hBladeDelete))
	mux.HandleFunc("POST /api/v1/blades/{serial}/actions/{kind}", app.requireAdmin(app.hBladeAction))

	mux.HandleFunc("GET /api/v1/images", app.requireAdmin(app.hImagesList))
	mux.HandleFunc("POST /api/v1/images", app.requireAdmin(app.hImagesCreate))

	mux.HandleFunc("GET /api/v1/config/{scope}", app.requireAdmin(app.hConfigGet))
	mux.HandleFunc("PUT /api/v1/config/{scope}", app.requireAdmin(app.hConfigPut))

	mux.HandleFunc("POST /api/v1/dhcp/sync", app.requireAdmin(app.hDHCPSync))
	mux.HandleFunc("GET /api/v1/netboot", app.requireAdmin(app.hNetbootList))
	mux.HandleFunc("POST /api/v1/netboot/{mac}/image", app.requireAdmin(app.hNetbootImage))
	mux.HandleFunc("GET /api/v1/health", app.hHealth)
	mux.HandleFunc("GET /api/v1/events", app.requireAdmin(app.hEvents))

	// ── Agent and mini-OS (own tokens, not the admin token) ──
	mux.HandleFunc("POST /api/v1/enroll", app.hEnroll)
	mux.HandleFunc("GET /api/v1/blades/{serial}/config", app.hBladeConfig)
	mux.HandleFunc("POST /api/v1/blades/{serial}/status", app.hBladeStatus)
	mux.HandleFunc("GET /api/v1/blades/{serial}/commands", app.hBladeCommands)
	mux.HandleFunc("POST /api/v1/provision/{serial}", app.hProvision)
	mux.HandleFunc("POST /api/v1/provision/{serial}/status", app.hProvisionStatus)

	// ── Serve images (streamed by the mini-OS over HTTP) ──
	mux.Handle("GET /images/", http.StripPrefix("/images/",
		http.FileServer(http.Dir(*imagesDir))))

	// ── Netboot payload, so a site can offer the same one ──
	// A site holds no build tooling; it fetches the payload the centre built
	// and puts it in its own TFTP root.
	mux.Handle("GET /boot/", http.StripPrefix("/boot/",
		http.FileServer(http.Dir(*tftpDir))))

	// ── Agent binary for offline seeding ──
	// The installer fetches it here and drops it into the freshly written
	// root. If it is missing that is not an error: the image simply boots
	// without an agent.
	mux.Handle("GET /agent/", http.StripPrefix("/agent/",
		http.FileServer(http.Dir(*agentDir))))

	// ── Web interface ──
	// These pages write, so they hang off a session. A browser form cannot
	// send a bearer header; /login trades the admin token once for a cookie.
	mux.HandleFunc("GET /lang/{code}", app.hLang)
	mux.HandleFunc("GET /login", app.hLogin)
	mux.HandleFunc("POST /login", app.hLoginPost)
	mux.HandleFunc("POST /logout", app.hLogout)

	mux.HandleFunc("GET /", app.requireUI(app.hUI))
	mux.HandleFunc("GET /map", app.requireUI(app.hTopology))
	mux.HandleFunc("GET /sites/{id}", app.requireUI(app.hSitePage))
	mux.HandleFunc("GET /bladerunners/{id}", app.requireUI(app.hRackPage))

	mux.HandleFunc("POST /sites", app.requireUI(app.hUISiteCreate))
	mux.HandleFunc("POST /sites/{id}", app.requireUI(app.hUISiteUpdate))
	mux.HandleFunc("POST /sites/{id}/delete", app.requireUI(app.hUISiteDelete))

	mux.HandleFunc("POST /bladerunners", app.requireUI(app.hUIRackCreate))
	mux.HandleFunc("POST /bladerunners/{id}", app.requireUI(app.hUIRackUpdate))
	mux.HandleFunc("POST /bladerunners/{id}/delete", app.requireUI(app.hUIRackDelete))
	mux.HandleFunc("POST /bladerunners/{id}/slots/{slot}/assign", app.requireUI(app.hUIAssign))

	// The rack became a BladeRunner. Old links keep working: everything under
	// /racks and /api/v1/racks is redirected once, permanently.
	// Registered per method: a pattern without one would be no more specific
	// than "GET /" for a GET, and ServeMux rejects that as a conflict.
	for _, p := range []string{
		"/racks", "/racks/{rest...}",
		"/api/v1/racks", "/api/v1/racks/{rest...}",
	} {
		for _, m := range []string{"GET", "POST", "PUT", "DELETE"} {
			mux.HandleFunc(m+" "+p, redirectPrefix)
		}
	}

	mux.HandleFunc("POST /blades/{serial}/unassign", app.requireUI(app.hUIUnassign))
	mux.HandleFunc("POST /blades/{serial}/image", app.requireUI(app.hUIBladeImage))
	mux.HandleFunc("POST /blades/{serial}/actions/{kind}", app.requireUI(app.hUIBladeAction))
	mux.HandleFunc("POST /netboot/{mac}/image", app.requireUI(app.hUINetbootImage))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})

	// Commands for blades that no longer exist are dead weight.
	if res, err := db.Exec(
		`UPDATE commands SET taken=? WHERE taken='' AND serial NOT IN (SELECT serial FROM blades)`,
		time.Now().UTC().Format(time.RFC3339)+" (verwaist)"); err == nil {
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("discarded %d orphaned command(s)", n)
		}
	}

	// Mark blades that have not checked in for a while as offline.
	go app.reaper()
	// Tail dnsmasq: that is how Rookery sees a blade netbooting, before any
	// operating system runs on it. Where a rookery-site owns the wire it does
	// the watching and reports what it saw, and doing it twice would mean two
	// programs writing the same records.
	app.localDHCP = *localDHCP
	if app.localDHCP {
		go app.watchDnsmasqLog(*dnsmasqLog)
	} else {
		log.Printf("local DHCP handling off — a rookery-site owns the wire here")
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           logging(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("Rookery listening on %s (network %s.0/24, base URL %s)", *addr, app.netBase(), app.baseURL)
	if warns := app.checkNet(LangEN); len(warns) > 0 {
		for _, w := range warns {
			log.Printf("WARNING: %s", w)
		}
	}
	if app.adminToken == "" {
		log.Printf("WARNING: no admin token — the management API is unprotected")
	}
	log.Fatal(srv.ListenAndServe())
}

// reaper marks blades without a heartbeat as offline. The threshold is
// deliberately generous: a reboot must not read as a failure.
func (a *App) reaper() {
	for {
		time.Sleep(30 * time.Second)
		cutoff := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)
		res, err := a.db.Exec(`UPDATE blades SET state='offline'
			WHERE state='online' AND last_seen<>'' AND last_seen < ?`, cutoff)
		if err == nil {
			if n, _ := res.RowsAffected(); n > 0 {
				a.logEvent("", "warn", fmt.Sprintf("%d blade(s) without heartbeat marked offline", n))
			}
		}
	}
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		// Do not log routine polling, or the log drowns in it.
		if r.URL.Path == "/healthz" || strings.HasSuffix(r.URL.Path, "/status") {
			return
		}
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.code, time.Since(start).Round(time.Millisecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int) {
	s.code = c
	s.ResponseWriter.WriteHeader(c)
}

// redirectPrefix answers the old rack paths with their BladeRunner
// equivalent. 308 rather than 301: the method and body of a POST must
// survive the hop, otherwise a form submitted against an old URL would
// silently turn into a GET.
func redirectPrefix(w http.ResponseWriter, r *http.Request) {
	u := *r.URL
	u.Path = strings.Replace(u.Path, "/racks", "/bladerunners", 1)
	http.Redirect(w, r, u.String(), http.StatusPermanentRedirect)
}
