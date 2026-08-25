package main

import (
	"bufio"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"time"
)

// Watching the wire.
//
// dnsmasq writes what it does; that log is the only place where "a blade is
// booting right now" appears at all. Reading it is how the site knows a blade
// has asked for an address, has been offered a boot, and has pulled the
// payload — none of which any blade reports itself, because at that point it
// is a bootloader with no idea Rookery exists.
var (
	// "DHCPACK(eth0) 10.0.0.210 d8:3a:dd:11:22:33 name"
	reDHCPAck = regexp.MustCompile(
		`DHCP(?:ACK|OFFER)\([^)]*\)\s+(\d+\.\d+\.\d+\.\d+)\s+([0-9a-fA-F:]{17})`)
	// "DHCPDISCOVER(eth0) d8:3a:dd:11:22:33" — no address yet
	reDHCPDiscover = regexp.MustCompile(
		`DHCP(?:DISCOVER|REQUEST)\([^)]*\)\s+([0-9a-fA-F:]{17})`)
	// dnsmasq-tftp: "sent /srv/rookery/tftp/boot.img to 10.0.0.210"
	reTFTPSent = regexp.MustCompile(`sent\s+(\S+)\s+to\s+(\d+\.\d+\.\d+\.\d+)`)
	reTFTPFail = regexp.MustCompile(`failed sending\s+(\S+)\s+to\s+(\d+\.\d+\.\d+\.\d+)`)
	// "<xid> vendor class: PXEClient:Arch:00000:UNDI:002001"
	reVendor = regexp.MustCompile(`dnsmasq-dhcp\[\d+\]:\s+(\S+)\s+vendor class:\s*(.+)$`)
	// The transaction id prefixes every DHCP line of one request, which is
	// what ties a vendor class to the MAC it belongs to.
	reXID = regexp.MustCompile(`dnsmasq-dhcp\[\d+\]:\s+(\S+)\s+DHCP`)
)

// watchLog tails the dnsmasq log. It never rewinds: on start it seeks to the
// end, so a restart of this program does not report last week's boots as if
// they were happening now.
func (s *site) watchLog() {
	var (
		f      *os.File
		rd     *bufio.Reader
		offset int64
		warned bool
	)
	openLog := func() bool {
		var err error
		f, err = os.Open(s.cfg.LogFile)
		if err != nil {
			return false
		}
		if offset == 0 {
			if st, err := f.Stat(); err == nil {
				offset = st.Size()
			}
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			offset = 0
		}
		rd = bufio.NewReader(f)
		return true
	}

	byXID := map[string]string{} // transaction id -> MAC
	byIP := map[string]string{}  // address -> MAC, to read TFTP lines

	for {
		if f == nil {
			if !openLog() {
				if !warned {
					// "not there" and "not readable" lead to the same waiting
					// but to different fixes, so say which one it is.
					if _, err := os.Stat(s.cfg.LogFile); err == nil {
						log.Printf("log watch: %s exists but is not readable "+
							"— does the file belong to this service's group?", s.cfg.LogFile)
					} else {
						log.Printf("log watch: %s not present yet — waiting", s.cfg.LogFile)
					}
					warned = true
				}
				time.Sleep(5 * time.Second)
				continue
			}
			log.Printf("log watch: reading %s", s.cfg.LogFile)
			warned = false
		}

		line, err := rd.ReadString('\n')
		if err != nil {
			if len(line) > 0 {
				// A partial line: step back and read it again once it is
				// complete, rather than parsing half of it.
				continue
			}
			// Truncated by logrotate's copytruncate? Then start over.
			if st, serr := f.Stat(); serr == nil && st.Size() < offset {
				offset = 0
				f.Close()
				f = nil
				continue
			}
			time.Sleep(time.Second)
			continue
		}
		offset += int64(len(line))
		s.handleLine(strings.TrimRight(line, "\n"), byXID, byIP)
	}
}

func (s *site) handleLine(line string, byXID, byIP map[string]string) {
	if m := reXID.FindStringSubmatch(line); m != nil {
		if d := reDHCPDiscover.FindStringSubmatch(line); d != nil {
			byXID[m[1]] = d[1]
		}
	}
	if m := reDHCPAck.FindStringSubmatch(line); m != nil {
		ip, mac := m[1], strings.ToLower(m[2])
		byIP[ip] = mac
		s.stageIP(mac, ip, "dhcp", "address "+ip)
		return
	}
	if m := reDHCPDiscover.FindStringSubmatch(line); m != nil {
		s.stage(strings.ToLower(m[1]), "dhcp", "asking for an address")
		return
	}
	if m := reVendor.FindStringSubmatch(line); m != nil {
		// The vendor class is what distinguishes a bootloader from an
		// operating system asking for the same address.
		if mac, ok := byXID[m[1]]; ok && strings.Contains(m[2], "PXEClient") {
			s.stage(mac, "tftp", "bootloader is asking ("+strings.TrimSpace(m[2])+")")
		}
		return
	}
	if m := reTFTPSent.FindStringSubmatch(line); m != nil {
		if mac, ok := byIP[m[2]]; ok {
			s.stage(mac, "ramdisk", "delivered "+lastPath(m[1]))
		}
		return
	}
	if m := reTFTPFail.FindStringSubmatch(line); m != nil {
		if mac, ok := byIP[m[2]]; ok {
			s.stage(mac, "error", "TFTP failed for "+lastPath(m[1]))
		}
	}
}

func lastPath(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
