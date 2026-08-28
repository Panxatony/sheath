package main

import (
	"net"
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

// A site on a machine with no dnsmasq is not a site in trouble.
func TestNoDnsmasqUnitIsNotAFault(t *testing.T) {
	if unitExists("sheath-nothing-of-this-name") {
		t.Error("a unit that does not exist was reported as loaded")
	}
}
