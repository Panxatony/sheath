// rookery-site is the network presence of one site.
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
//	rookery-site -server https://rookery.example -site 2 -token-file /etc/rookery-site/token
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const version = "rookery-site/1"

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
	Interval  time.Duration
}

func main() {
	var (
		server    = flag.String("server", "", "central Rookery server, e.g. https://rookery.example")
		siteID    = flag.Int64("site", 0, "id of this site")
		tokenFile = flag.String("token-file", "/etc/rookery-site/token", "file holding the site token")
		hostsDir  = flag.String("dhcp-hosts", "/etc/rookery/dhcp-hosts", "dnsmasq dhcp-hostsdir")
		logFile   = flag.String("dnsmasq-log", "/srv/rookery/logs/dnsmasq.log", "dnsmasq log to watch")
		imagesDir = flag.String("images", "/srv/rookery/images", "where images are cached")
		tftpDir   = flag.String("tftp", "/srv/rookery/tftp", "TFTP root")
		stateDir  = flag.String("state", "/var/lib/rookery-site", "where the last desired state is kept")
		interval  = flag.Duration("interval", 30*time.Second, "interval between two passes")
		once      = flag.Bool("once", false, "run a single pass and exit")
		dryRun    = flag.Bool("dry-run", false, "compute everything, write nothing")
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
		Interval:  *interval,
	}
	s := newSite(c, *dryRun)

	log.Printf("%s starting — site %d, server %s", version, c.SiteID, c.Server)
	if *dryRun {
		log.Printf("dry run: nothing is written")
	}

	if *once {
		if err := s.pass(); err != nil {
			log.Fatalf("pass: %v", err)
		}
		return
	}

	// The log watcher runs alongside the pull loop: a blade netbooting is an
	// event on the wire, not something a poll would catch in time.
	go s.watchLog()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
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
			return
		}
	}
}
