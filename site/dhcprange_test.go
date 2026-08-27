package main

import (
	"strings"
	"testing"
)

func TestRangeConfigSaysWhatTheSiteRecordSays(t *testing.T) {
	s := &site{cfg: config{RelayURL: "http://10.0.1.10:8081"}}
	var d desired
	d.Site.NetBase = "10.0.1"
	d.Site.PoolFrom, d.Site.PoolTo = 150, 200
	d.Site.Lease = "12h"
	d.Site.Gateway = "10.0.1.1"
	d.Site.Domain = "blades.lan"
	d.Site.DNS = "9.9.9.9" // upstream, and not what a blade should be told

	out := s.rangeConfig(&d)
	for _, want := range []string{
		"dhcp-range=10.0.1.150,10.0.1.200,255.255.255.0,12h",
		"dhcp-option=option:router,10.0.1.1",
		// The resolver handed to a blade is this machine: it knows the site's
		// own names, and the upstream does not.
		"dhcp-option=option:dns-server,10.0.1.10",
		"dhcp-option=option:domain-name,blades.lan",
		"local=/blades.lan/",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing: %s\n%s", want, out)
		}
	}
	if strings.Contains(out, "dns-server,9.9.9.9") {
		t.Error("blades were told to ask the upstream resolver directly")
	}

	// No lease said means the default, not an empty field dnsmasq refuses.
	d.Site.Lease = ""
	if !strings.Contains(s.rangeConfig(&d), ",1h\n") {
		t.Error("a site without a lease time produced no default")
	}

	// Nothing sensible to write is written as nothing, rather than as a range
	// that would hand out addresses nobody asked for.
	var empty desired
	if s.rangeConfig(&empty) != "" {
		t.Error("a site with no network produced a range anyway")
	}
}
