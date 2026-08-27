package main

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// The address range, owned by the site rather than by whoever deployed it.
//
// The pool and the lease time are properties of a site, and the interface has
// let people edit them for weeks — while the numbers that actually reached
// dnsmasq came from a variable in an Ansible inventory. Changing the pool in
// the interface changed nothing on the wire until somebody remembered to run
// the playbook, which is the sort of half-truth that is noticed the day a
// blade gets an address it should not have.
//
// So the site writes that one line itself, from the state the centre handed
// it, and the playbook lays down only what never changes.

// ensureRange writes the range file and restarts dnsmasq when it changed.
//
// A restart, not a reload: SIGHUP makes dnsmasq re-read its host records and
// nothing else — the configuration itself is read once, at startup. A reload
// after changing this file leaves the daemon running happily with the numbers
// it had before, which looks like success and is not.
func (s *site) ensureRange(d *desired) error {
	if s.cfg.RangeFile == "" || s.dry {
		return nil
	}
	want := s.rangeConfig(d)
	if want == "" {
		return nil
	}
	if old, err := os.ReadFile(s.cfg.RangeFile); err == nil && string(old) == want {
		return nil
	}
	tmp := s.cfg.RangeFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(want), 0o644); err != nil {
		return err
	}
	// Checked before it is put in place: a file dnsmasq refuses is a site
	// where nothing boots, and it would be refused at the worst moment —
	// during the restart, with the old configuration already gone.
	if out, err := exec.Command("dnsmasq", "--test", "--conf-file="+tmp).CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("dnsmasq refuses the range: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.Rename(tmp, s.cfg.RangeFile); err != nil {
		return err
	}
	log.Printf("dhcp range: %s", strings.TrimSpace(firstLineOf(want)))
	s.note("", "info", "DHCP range rewritten: "+strings.TrimSpace(firstLineOf(want)))
	return restartDnsmasq()
}

// rangeConfig is the part of the DHCP configuration that belongs to the site
// record: which addresses may be handed out, for how long, and what to tell a
// client about the way out of the network.
func (s *site) rangeConfig(d *desired) string {
	net := d.Site.NetBase
	if net == "" || d.Site.PoolFrom <= 0 || d.Site.PoolTo < d.Site.PoolFrom {
		return ""
	}
	lease := d.Site.Lease
	if lease == "" {
		lease = "1h"
	}
	gw := d.Site.Gateway
	if gw == "" {
		gw = net + ".1"
	}
	var b strings.Builder
	b.WriteString("# Written by sheath-site from the site's own record. Edit the site in\n")
	b.WriteString("# the interface, not this file: it is rewritten on the next pass.\n")
	fmt.Fprintf(&b, "dhcp-range=%s.%d,%s.%d,255.255.255.0,%s\n",
		net, d.Site.PoolFrom, net, d.Site.PoolTo, lease)
	fmt.Fprintf(&b, "dhcp-option=option:router,%s\n", gw)
	// The resolver a blade is given is this machine, not the upstream one:
	// dnsmasq here knows the site's own names, and the address it is reached
	// under is the one blades already use for everything else.
	if dns := s.myAddress(d); dns != "" {
		fmt.Fprintf(&b, "dhcp-option=option:dns-server,%s\n", dns)
	}
	if d.Site.Domain != "" {
		fmt.Fprintf(&b, "dhcp-option=option:domain-name,%s\n", d.Site.Domain)
		fmt.Fprintf(&b, "domain=%s\nlocal=/%s/\n", d.Site.Domain, d.Site.Domain)
	}
	return b.String()
}

// myAddress is this machine as the blades reach it — the host out of the
// relay URL, which is the one address here that is known to work from a
// blade's point of view. Without a relay URL there is nothing better than
// what the site record says.
func (s *site) myAddress(d *desired) string {
	if s.cfg.RelayURL != "" {
		if u, err := url.Parse(s.cfg.RelayURL); err == nil {
			if h := u.Hostname(); h != "" {
				return h
			}
		}
	}
	return d.Site.DNS
}

func firstLineOf(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "dhcp-range=") {
			return l
		}
	}
	return s
}

// restartDnsmasq is the heavier hammer, and the right one here: the leases
// survive in their file, and the configuration is only read at startup.
func restartDnsmasq() error {
	out, err := exec.Command("sudo", "-n", "systemctl", "restart", "dnsmasq").CombinedOutput()
	if err != nil {
		return fmt.Errorf("dnsmasq not restarted: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
