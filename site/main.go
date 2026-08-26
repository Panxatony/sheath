// sheath-site is the network presence of one site.
//
// It holds no decisions. Which image a blade gets, whether it may netboot,
// what its address is — all of that is decided centrally and arrives here as
// a desired state. What this program owns is the wire: the DHCP reservations
// dnsmasq hands out, the netboot switch per blade, the images a blade
// downloads, and the boot payload offered over TFTP.
//
// The split exists for one reason: a site has to keep working while the line
// to the centre is down. A blade that reboots in a power cut must get its
// address and boot locally, without asking anyone far away.
//
//	sheath-site -server https://sheath.example -site 2 -token-file /etc/sheath-site/token
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// version is stamped at build time (-X main.version=v1.2.3).
var version = "sheath-site/dev"

type config struct {
	Server    string
	SiteID    int64
	Token     string
	HostsDir  string
	Dnsmasq   string
	LogFile   string
	ImagesDir string
	TFTPDir   string
	StateDir  string
	RelayURL  string
	Interval  time.Duration
}

func main() {
	var (
		server    = flag.String("server", "", "central Sheath server, e.g. https://sheath.example")
		siteID    = flag.Int64("site", 0, "id of this site")
		tokenFile = flag.String("token-file", "/etc/sheath-site/token", "file holding the site token")
		hostsDir  = flag.String("dhcp-hosts", "/etc/sheath/dhcp-hosts", "dnsmasq dhcp-hostsdir")
		logFile   = flag.String("dnsmasq-log", "/srv/sheath/logs/dnsmasq.log", "dnsmasq log to watch")
		imagesDir = flag.String("images", "/srv/sheath/images", "where images are cached")
		tftpDir   = flag.String("tftp", "/srv/sheath/tftp", "TFTP root")
		stateDir  = flag.String("state", "/var/lib/sheath-site", "where the last desired state is kept")
		interval  = flag.Duration("interval", 30*time.Second, "interval between two passes")
		listen    = flag.String("listen", ":8081",
			"address for the blade relay; empty turns the relay off")
		relayURL = flag.String("relay-url", "",
			"URL blades at this site should use, e.g. http://10.0.0.10:8081 — "+
				"written into the netboot payload so a blade here talks to this site")
		once   = flag.Bool("once", false, "run a single pass and exit")
		dryRun = flag.Bool("dry-run", false, "compute everything, write nothing")
	)
	flag.Parse()

	if *server == "" || *siteID == 0 {
		log.Fatal("-server and -site are required")
	}
	tok, err := os.ReadFile(*tokenFile)
	if err != nil {
		log.Fatalf("token: %v", err)
	}

	c := config{
		Server:    strings.TrimRight(*server, "/"),
		SiteID:    *siteID,
		Token:     strings.TrimSpace(string(tok)),
		HostsDir:  *hostsDir,
		LogFile:   *logFile,
		ImagesDir: *imagesDir,
		TFTPDir:   *tftpDir,
		StateDir:  *stateDir,
		RelayURL:  strings.TrimRight(*relayURL, "/"),
		Interval:  *interval,
	}
	s := newSite(c, *dryRun)

	log.Printf("%s starting — site %d, server %s", version, c.SiteID, c.Server)
	if *dryRun {
		log.Printf("dry run: nothing is written")
	}

	if *once {
		err := s.pass()
		s.writeSpools()
		if err != nil {
			log.Fatalf("pass: %v", err)
		}
		return
	}

	// The log watcher runs alongside the pull loop: a blade netbooting is an
	// event on the wire, not something a poll would catch in time.
	go s.watchLog()

	// The relay is what blades actually talk to. It answers from the cached
	// state when the centre is away, which is the reason this program exists
	// as a separate thing at all.
	if *listen != "" {
		s.relay = newRelay(s)
		go func() {
			log.Printf("relay listening on %s", *listen)
			srv := &http.Server{
				Addr:              *listen,
				Handler:           s.relay.routes(),
				ReadHeaderTimeout: 10 * time.Second,
			}
			if err := srv.ListenAndServe(); err != nil {
				log.Printf("relay stopped: %v", err)
			}
		}()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	// The buffers are written a second after they change, and once more on
	// the way out — a service that is asked to stop should not be the reason
	// an outage loses its record.
	spools := make(chan struct{})
	go s.spool.run(time.Second, spools)
	if s.relay != nil {
		go s.relay.spool.run(time.Second, spools)
	}
	t := time.NewTicker(c.Interval)
	defer t.Stop()

	if err := s.pass(); err != nil {
		log.Printf("first pass: %v", err)
	}
	for {
		select {
		case <-t.C:
			if err := s.pass(); err != nil {
				log.Printf("pass: %v", err)
			}
		case <-stop:
			log.Printf("stopping")
			close(spools)
			// Written here and not left to the goroutines: the process is
			// about to end, and a write that has not been scheduled yet is a
			// write that never happens.
			s.writeSpools()
			return
		}
	}
}
