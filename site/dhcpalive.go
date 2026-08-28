package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Whether the DHCP server is actually serving.
//
// dnsmasq died at one site during a reload, and for twenty-five minutes
// nothing said so: sheath-site went on writing reservations and sending
// reloads into a unit that was not there — "Unit cannot be reloaded because
// it is inactive", three times, into a log nobody reads — while the centre
// showed the site as online. A site whose whole purpose is the wire has to
// know whether the thing it configures is running.
//
// The question asked is not "is the unit active" but "is anyone listening on
// port 67", which is the same question a blade asks. A unit can be active and
// serve nothing.

const dhcpPort = 67

// dhcpTrouble returns "" when the DHCP server is serving, and otherwise says
// what is wrong — having first tried to put it right, because a site that can
// start its own DHCP server again should not wait for a person.
//
// A machine with no dnsmasq unit at all is a site that does not do DHCP here,
// and that is not a fault: the answer there is "".
func (s *site) dhcpTrouble() string {
	if listeningOnUDP(dhcpPort) {
		return ""
	}
	// Whether this site serves DHCP at all is answered by whether it wrote a
	// range: that file is this program's own statement of intent. Asking
	// systemd instead was wrong twice over — a masked unit reads as "not
	// installed", which is exactly the state somebody would want to hear
	// about, and a site can be configured for DHCP before the package is.
	if !s.servesDHCP() {
		return ""
	}
	out, err := exec.Command("sudo", "-n", "systemctl", "start", "dnsmasq").CombinedOutput()
	if err != nil {
		return fmt.Sprintf("no DHCP server on port %d, and it would not start: %v: %s",
			dhcpPort, err, strings.TrimSpace(string(out)))
	}
	// Started, but that is a claim; the port is the proof.
	if !listeningOnUDP(dhcpPort) {
		return fmt.Sprintf("no DHCP server on port %d — dnsmasq was started and is still not serving", dhcpPort)
	}
	return ""
}

// listeningOnUDP reads the kernel's own socket table, both families. A local
// port in there is the one thing that means a client on the wire will be
// answered.
func listeningOnUDP(port int) bool {
	want := fmt.Sprintf(":%04X", port)
	for _, f := range []string{"/proc/net/udp", "/proc/net/udp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n")[1:] {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			if strings.HasSuffix(fields[1], want) {
				return true
			}
		}
	}
	return false
}

// servesDHCP says whether this site is the DHCP server for its segment. The
// range file it writes itself is the honest signal: a site that hands out
// addresses has one, and a site that does not never writes it.
func (s *site) servesDHCP() bool {
	if s.cfg.RangeFile == "" {
		return false
	}
	st, err := os.Stat(s.cfg.RangeFile)
	return err == nil && st.Size() > 0
}
