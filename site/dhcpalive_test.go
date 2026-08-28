package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// /proc/net/udp writes the local port in upper-case hexadecimal, and a check
// that gets that wrong reports either every site as broken or none. So it is
// asked about a port that really is open, and one that really is not.
func TestListeningOnUDPReadsTheKernelsTable(t *testing.T) {
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no UDP socket to test with: %v", err)
	}
	defer c.Close()
	port := c.LocalAddr().(*net.UDPAddr).Port

	if !listeningOnUDP(port) {
		t.Errorf("port %d is open and was not found", port)
	}
	c.Close()

	// The same port again, now that nothing holds it. A port can be taken by
	// something else in between, so a single retry keeps the test honest
	// without making it flaky.
	if listeningOnUDP(port) && listeningOnUDP(port) {
		t.Errorf("port %d is closed and was reported as listening", port)
	}
}

// A site that does not serve DHCP is not a site in trouble — and the signal
// for that is its own range file, not what systemd says about a unit. A
// masked unit reads as "not installed", which is precisely the state somebody
// would want to hear about.
func TestOnlyASiteThatServesDHCPIsJudgedOnIt(t *testing.T) {
	dir := t.TempDir()
	s := &site{cfg: config{}}
	if s.servesDHCP() {
		t.Error("a site with no range file was taken for a DHCP server")
	}
	s.cfg.RangeFile = filepath.Join(dir, "sheath-range.conf")
	if s.servesDHCP() {
		t.Error("a range file that is not there counted as one that is")
	}
	if err := os.WriteFile(s.cfg.RangeFile, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if s.servesDHCP() {
		t.Error("an empty range file counted as a range")
	}
	if err := os.WriteFile(s.cfg.RangeFile, []byte("dhcp-range=10.0.0.10,10.0.0.20,1h\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !s.servesDHCP() {
		t.Error("a site that wrote a range is a DHCP server and was not counted as one")
	}
}
