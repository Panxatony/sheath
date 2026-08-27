package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Address plan
// ------------
// A BladeRunner holds an address block (ip_offset); every blade in it gets
// <network>.(ip_offset + slot). Slots count from 1.
//
//	BladeRunner 1 (offset 100), slot 3  ->  10.0.0.103
//	BladeRunner 2 (offset 120), slot 3  ->  10.0.0.123
//
// The MAC is set during bring-up via the EEPROM property MAC_ADDRESS and
// derived here deterministically — so MAC, IP, slot and hostname are all
// known before a blade has seen power for the first time.
//
//	02:b1:ad:<runner>:00:<slot>
//
// 02: locally administered, so no OUI can ever collide.

// netBase returns the local site's network. Once there is more than one site
// this shortcut is only correct for the local one — everything else has to
// carry its site along.
// netBase is the local site's network. It survives as a convenience for the
// places that legitimately mean "here" — the DHCP pool, the address the
// blades are told to reach. Anything that speaks about a particular
// BladeRunner must use that BladeRunner's site instead.
func (a *App) netBase() string {
	if st, err := a.defaultSite(); err == nil {
		return st.NetBase
	}
	return a.setting("net_base", "10.0.0")
}

// bladeIP computes the address from site network, block offset and slot.
// Two sites may share a network; the address alone is then no longer unique,
// but the pair (site, address) is.
func (a *App) bladeIP(r Rack, slot int) string {
	if slot < 1 || slot > r.Size {
		return ""
	}
	return fmt.Sprintf("%s.%d", a.siteNetBase(r.SiteID), r.IPOffset+slot)
}

// siteNetBase memoises networks for one request — otherwise a twenty-slot
// view would read the same row twenty times.
func (a *App) siteNetBase(id int64) string {
	a.netCacheMu.Lock()
	defer a.netCacheMu.Unlock()
	if a.netCache == nil {
		a.netCache = map[int64]string{}
	}
	if v, ok := a.netCache[id]; ok {
		return v
	}
	v := ""
	if st, err := a.getSite(id); err == nil {
		v = st.NetBase
	} else {
		v = a.setting("net_base", "10.0.0")
	}
	a.netCache[id] = v
	return v
}

// siteName is the label a reader recognises, read through the same cache as
// the networks — a twenty-slot view would otherwise ask for it twenty times.
func (a *App) siteName(id int64) string {
	a.netCacheMu.Lock()
	defer a.netCacheMu.Unlock()
	if a.nameCache == nil {
		a.nameCache = map[int64]string{}
	}
	if v, ok := a.nameCache[id]; ok {
		return v
	}
	v := ""
	if st, err := a.getSite(id); err == nil {
		v = st.Name
	}
	a.nameCache[id] = v
	return v
}

// invalidateNetCache after every change to sites.
func (a *App) invalidateNetCache() {
	a.netCacheMu.Lock()
	a.netCache = nil
	a.nameCache = nil
	a.netCacheMu.Unlock()
}

// rackIndex is the human-visible number of a BladeRunner: it follows the
// address block, not the database id. The unit holding .101-.120 is number 1,
// .121-.140 is number 2 — regardless of which row id it happened to get.
// Otherwise a blade in the only unit present might be called "blade-r4s07",
// which nobody can explain.
func (a *App) rackIndex(rk Rack) int {
	base, step := 100, 20
	if st, err := a.getSite(rk.SiteID); err == nil {
		base, step = st.OffsetBase, st.OffsetStep
	}
	if step <= 0 || rk.IPOffset < base {
		return 1
	}
	return (rk.IPOffset-base)/step + 1
}

// bladeMAC is the address handed to a blade that has not shown its own yet.
// The site is in it, because a BladeRunner numbered 1 exists at every site and
// two blades with one address is a fault that takes a day to find.
func bladeMAC(siteID, rackIdx int64, slot int) string {
	return fmt.Sprintf("02:b1:ad:%02x:%02x:%02x", siteID&0xff, rackIdx&0xff, slot&0xff)
}

// bladeHostname is what a blade is called when nobody named it. The prefix
// belongs to the site: BladeRunner 1 exists at every site, so without it the
// first blade of the first unit is called blade-r1s01 in each of them — one
// name, two machines, and every tool that resolves names picks one at random.
//
// An empty prefix keeps the name a single-site installation has always had,
// which is why it is the default and why the interface says so when two sites
// would collide.
func bladeHostname(prefix string, rackIdx int64, slot int) string {
	if prefix != "" {
		return fmt.Sprintf("blade-%s-r%ds%02d", prefix, rackIdx, slot)
	}
	return fmt.Sprintf("blade-r%ds%02d", rackIdx, slot)
}

// validSlot checks whether a slot fits a unit of the given size.
func validSlot(size, slot int) error {
	if slot < 1 || slot > size {
		return me("err.slotrange", slot, size)
	}
	return nil
}

// placeBlade puts a blade into a position or takes it out again. Both the
// API and the interface go through here so the checks live in one place:
// the unit exists, the slot is inside it, the slot is free. Hostname and MAC
// are derived when still missing.
func (a *App) placeBlade(serial string, rackID *int64, slot *int) error {
	cur, err := a.getBlade(serial)
	if err != nil {
		return me("err.bladeunknown", serial)
	}

	// Taking it out: clear the position, keep the identity.
	if rackID == nil || slot == nil {
		_, err := a.db.Exec(`UPDATE blades SET rack_id=NULL,slot=NULL,
			state=CASE WHEN state IN ('online','enrolled') THEN 'new' ELSE state END
			WHERE serial=?`, serial)
		return err
	}

	rk, err := a.getRack(*rackID)
	if err != nil {
		return me("err.racknotfound", *rackID)
	}
	if err := validSlot(rk.Size, *slot); err != nil {
		return err
	}
	var occupant string
	err = a.db.QueryRow(`SELECT serial FROM blades WHERE rack_id=? AND slot=? AND serial<>?`,
		*rackID, *slot, serial).Scan(&occupant)
	if err == nil && occupant != "" {
		return me("err.slottaken", *slot, rk.Name, occupant)
	}

	// Derived values must travel with a move, or a blade in slot 2 keeps
	// claiming to be "blade-r4s07". Names set by hand and real MACs reported
	// by the blade stay untouched.
	idx := int64(a.rackIndex(*rk))
	host := cur.Hostname
	if host == "" || autoHostRe.MatchString(host) {
		host = bladeHostname(a.hostPrefix(rk.SiteID), idx, *slot)
	}
	mac := cur.MAC
	if mac == "" || strings.HasPrefix(strings.ToLower(mac), autoMACPrefix) {
		mac = bladeMAC(rk.SiteID, idx, *slot)
	}
	state := cur.State
	if state == "new" {
		state = "enrolled"
	}
	_, err = a.db.Exec(`UPDATE blades SET rack_id=?,slot=?,hostname=?,mac=?,state=?
		WHERE serial=?`, *rackID, *slot, host, mac, state, serial)
	if err != nil {
		return me("err.assignfail", err.Error())
	}
	return nil
}

// ── dnsmasq reservations ─────────────────────────────────────────────
//
// Sheath writes one file per blade into dhcp_hosts_dir. dnsmasq picks up new
// and changed files by itself; deleted entries only disappear after SIGHUP.
// So every sync also triggers a reload — cheap, and it covers both cases.

// autoHostRe recognises names Sheath generated itself. autoMACPrefix is the
// locally administered range we hand out — a vendor MAC reported by a blade
// never starts like that.
var autoHostRe = regexp.MustCompile(`^blade-([a-z0-9]{1,8}-)?r\d+s\d{2}$`)

const autoMACPrefix = "02:b1:ad:"

var macRe = regexp.MustCompile(`^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$`)
var hostRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

// validReservation checks the line before it is written. dnsmasq reports
// format errors only in its own log and otherwise carries on — a broken
// reservation would otherwise surface only when a blade gets the wrong
// address.
func validReservation(mac, host, ip string) error {
	if !macRe.MatchString(mac) {
		return me("err.macformat", mac)
	}
	if !hostRe.MatchString(host) {
		return me("err.hostlabel", host)
	}
	if p := net.ParseIP(ip); p == nil || p.To4() == nil {
		return me("err.notipv4", ip)
	}
	return nil
}

func (a *App) dhcpHostsDir() string {
	return a.setting("dhcp_hosts_dir", "/etc/sheath/dhcp-hosts")
}

type syncResult struct {
	Written  []string `json:"written"`
	Removed  []string `json:"removed"`
	Reloaded bool     `json:"reloaded"`
	Warning  string   `json:"warning,omitempty"`
	// Blades belonging to another site, skipped on purpose. Reported rather
	// than hidden: a reservation that was never written is exactly the kind
	// of silence that costs an evening.
	Foreign int `json:"foreign,omitempty"`
}

func (a *App) syncDHCP() (*syncResult, error) {
	// Where a sheath-site owns the wire, the reservations are its business.
	// Writing them from here as well would mean two programs owning one
	// directory, and the loser would be whichever wrote last.
	if !a.localDHCP {
		return &syncResult{Written: []string{}, Removed: []string{},
			Warning: "written by sheath-site, not here"}, nil
	}
	dir := a.dhcpHostsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("directory %s: %w", dir, err)
	}

	blades, err := a.listBlades()
	if err != nil {
		return nil, err
	}

	res := &syncResult{Written: []string{}, Removed: []string{}}
	want := map[string]bool{}

	// Reservations are written for the local site only. dnsmasq here serves
	// one broadcast domain; a blade at another site is served by the server
	// standing in that domain, and writing its address here would be a
	// reservation nobody can hand out.
	var localID int64
	if st, err := a.localSite(); err == nil {
		localID = st.ID
	}

	for _, b := range blades {
		// Without a position there is no fixed address — the dynamic pool
		// serves those blades.
		if b.RackID == nil || b.Slot == nil || b.IP == "" {
			continue
		}
		if localID != 0 && b.SiteID != localID {
			res.Foreign++
			continue
		}
		mac := b.MAC
		if mac == "" {
			mac = bladeMAC(b.SiteID, int64(b.RackIdx), *b.Slot)
		}
		host := b.Hostname
		if host == "" {
			host = bladeHostname(a.hostPrefix(b.SiteID), int64(b.RackIdx), *b.Slot)
		}
		name := "blade-" + b.Serial + ".conf"
		want[name] = true

		// CAREFUL: a dhcp-hostsdir file contains ONLY what would otherwise
		// stand to the right of "dhcp-host=". Include the prefix and dnsmasq
		// reports "bad hex constant" and silently drops the line — the
		// reservation would have no effect at all.
		if err := validReservation(mac, host, b.IP); err != nil {
			return nil, fmt.Errorf("blade %s: %w", b.Serial, err)
		}

		// This is the netboot switch.
		//
		// The Raspberry Pi bootloader needs DHCP option 43 carrying
		// "Raspberry Pi Boot" to netboot at all. dnsmasq offers it only to
		// hosts whose tag matches a pxe-service line. If Sheath does not set
		// "bootnet", the netboot fails immediately and the bootloader falls
		// through to the next device in BOOT_ORDER — the NVMe.
		//
		// That makes installation controllable per blade without touching the
		// hardware or the EEPROM. Unknown blades get netboot via tag:!known
		// regardless.
		tag, why := "", "boots from the NVMe"
		switch b.InstallState {
		case installPending:
			tag, why = "set:bootnet,", "install requested – boots over the network"
		case installWipe:
			tag, why = "set:bootnet,", "erase requested – boots over the network"
		}
		body := fmt.Sprintf(
			"# Sheath – generated, do not edit by hand\n"+
				"# Blade %s  Rack %s  Slot %d\n"+
				"# %s\n"+
				"%s,%s%s,%s,infinite\n",
			b.Serial, b.RackName, *b.Slot, why, mac, tag, host, b.IP)

		path := filepath.Join(dir, name)
		old, _ := os.ReadFile(path)
		if string(old) == body {
			continue
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return nil, fmt.Errorf("schreiben %s: %w", path, err)
		}
		res.Written = append(res.Written, name)
	}

	// Remove orphaned files (blade deleted or slot cleared)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, "blade-") || !strings.HasSuffix(n, ".conf") {
			continue
		}
		if !want[n] {
			if err := os.Remove(filepath.Join(dir, n)); err == nil {
				res.Removed = append(res.Removed, n)
			}
		}
	}
	sort.Strings(res.Written)
	sort.Strings(res.Removed)

	if len(res.Written) > 0 || len(res.Removed) > 0 {
		if err := reloadDnsmasq(); err != nil {
			res.Warning = "dnsmasq not reloaded: " + err.Error()
		} else {
			res.Reloaded = true
		}
	}
	return res, nil
}

// reloadDnsmasq triggers the SIGHUP dnsmasq needs to forget removed or
// changed reservations. When Sheath does not run as root it goes through a
// narrowly scoped sudoers rule. If both fail that is not fatal — dnsmasq
// picks up new entries by itself anyway.
func reloadDnsmasq() error {
	var last error
	for _, argv := range [][]string{
		{"systemctl", "reload", "dnsmasq"},
		{"sudo", "-n", "systemctl", "reload", "dnsmasq"},
	} {
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		if err == nil {
			return nil
		}
		last = fmt.Errorf("%s: %v: %s", strings.Join(argv, " "), err,
			strings.TrimSpace(string(out)))
	}
	return last
}

// ── Health evaluation ────────────────────────────────────────────────
//
// A blade's state is the worst of its individual checks. That same value is
// meant to light the edge LED later, so it is decided once here rather than
// rebuilt in the interface.

type healthLevel int

const (
	hOK healthLevel = iota
	hWarn
	hCrit
	hUnknown
)

func (l healthLevel) chip() string {
	switch l {
	case hOK:
		return "ok"
	case hWarn:
		return "warn"
	case hCrit:
		return "crit"
	}
	return "off"
}

// evalHealth judges the reported values. The thresholds come from the
// architecture proposal and sit together here so they can be adjusted in one
// place.
func (a *App) evalHealth(b *Blade) (healthLevel, []error) {
	return evalHealthWith(b, a.sitePolicy(b.SiteID))
}

// evalHealthWith judges the reported values against one policy. Split out so
// the judgement stays a pure function of (values, thresholds) — that is the
// part worth reading twice.
func evalHealthWith(b *Blade, p Policy) (healthLevel, []error) {
	var h map[string]any
	if len(b.Health) > 0 {
		_ = json.Unmarshal(b.Health, &h)
	}
	if b.State == "offline" {
		return hCrit, []error{me("health.nohb")}
	}
	if len(h) == 0 {
		return hUnknown, nil
	}

	level := hOK
	var reasons []error
	raise := func(l healthLevel, why error) {
		if l > level {
			level = l
		}
		reasons = append(reasons, why)
	}

	if v, ok := num(h["soc_temp_c"]); ok {
		switch {
		case v > p.SocCrit:
			raise(hCrit, me("health.soc", v))
		case v > p.SocWarn:
			raise(hWarn, me("health.soc", v))
		}
	}
	if v, ok := num(h["nvme_temp_c"]); ok && v > p.NVMeWarn {
		raise(hWarn, me("health.nvme", v))
	}
	if v, ok := num(h["disk_used_pct"]); ok {
		switch {
		case v > p.DiskCrit:
			raise(hCrit, me("health.disk", v))
		case v > p.DiskWarn:
			raise(hWarn, me("health.disk", v))
		}
	}
	if b, ok := h["undervoltage_now"].(bool); ok && b {
		raise(hCrit, me("health.undervolt"))
	}
	if b, ok := h["throttled_now"].(bool); ok && b {
		raise(hWarn, me("health.throttled"))
	}
	// A stopped fan means: it was asked to spin, has had time to, and is not
	// spinning. All three parts matter.
	//
	// Only a smart fan unit measures at all — a standard one has no tacho and
	// reports 0 always, which is "not measurable" and not a fault. A unit
	// asked for 0 per cent is idle by instruction. And in the first minutes
	// after a boot the unit has often not answered yet: a freshly started
	// blade briefly reports 0 RPM at 0 per cent, which once painted a
	// perfectly healthy blade critical while its fan was running at 3490.
	if v, ok := num(h["fan_rpm"]); ok && v == 0 {
		unit, _ := h["fan_unit"].(string)
		want, _ := num(h["fan_percent"])
		up, hasUp := num(h["uptime_s"])
		settled := !hasUp || up > 180
		if unit == "smart" && want > 0 && settled {
			raise(hCrit, me("health.fanstop"))
		}
	}
	return level, reasons
}

func num(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	}
	return 0, false
}

// ── Network self-check ───────────────────────────────────────────────

// checkNet reports problems that turn expensive during netboot: overlapping
// address blocks, addresses outside the network, collisions with the dynamic
// pool.
// checkNet validates per site. Blocks from different sites may overlap —
// they live in separate networks.
func (a *App) checkNet(l Lang) []string {
	var warn []string
	sites, err := a.listSites()
	if err != nil {
		return []string{T(l, "warn.racksread", err.Error())}
	}
	racksAll, err := a.listRacks()
	if err != nil {
		return append(warn, T(l, "warn.racksread", err.Error()))
	}
	for _, st := range sites {
		warn = append(warn, a.checkSite(l, st, racksAll)...)
	}
	return warn
}

// checkNames looks for one name meaning two machines. It can happen the moment
// a second site exists: BladeRunner 1 is numbered 1 at every site, so the
// first blade of the first unit is blade-r1s01 in each of them until the sites
// are given name prefixes. DNS resolves such a name to whichever answers
// first, which is not a thing anyone should be debugging at the rack.
func (a *App) checkNames(l Lang) []string {
	rows, err := a.db.Query(`SELECT hostname, COUNT(*) FROM blades
		WHERE hostname<>'' GROUP BY hostname HAVING COUNT(*) > 1 ORDER BY hostname`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var warn []string
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err == nil {
			warn = append(warn, T(l, "warn.dupehost", name, n))
		}
	}
	return warn
}

func (a *App) checkSite(l Lang, st Site, racksAll []Rack) []string {
	var warn []string
	base := st.NetBase
	if net.ParseIP(base+".1") == nil {
		return []string{T(l, "warn.netbase", base)}
	}
	poolFrom, poolTo := st.PoolFrom, st.PoolTo

	var racks []Rack
	for _, r := range racksAll {
		if r.SiteID == st.ID {
			racks = append(racks, r)
		}
	}
	type span struct {
		name   string
		lo, hi int
	}
	_ = base
	var spans []span
	for _, r := range racks {
		lo, hi := r.IPOffset+1, r.IPOffset+r.Size
		if hi > 254 {
			warn = append(warn, T(l, "warn.overflow", r.Name))
		}
		if lo <= poolTo && hi >= poolFrom {
			warn = append(warn, T(l, "warn.pool", r.Name, lo, hi, poolFrom, poolTo))
		}
		spans = append(spans, span{r.Name, lo, hi})
	}
	for i := 0; i < len(spans); i++ {
		for j := i + 1; j < len(spans); j++ {
			if spans[i].lo <= spans[j].hi && spans[j].lo <= spans[i].hi {
				warn = append(warn, T(l, "warn.blocks", spans[i].name, spans[j].name))
			}
		}
	}
	return warn
}

// hostPrefix is the site's own piece of a generated blade name, memoised for
// the duration of one request like the other site lookups.
func (a *App) hostPrefix(siteID int64) string {
	st, err := a.getSite(siteID)
	if err != nil {
		return ""
	}
	return st.HostPrefix
}

// renameSiteBlades re-derives the generated names in one site. Changing a
// site's prefix has to reach the blades standing in it, or the setting is a
// note in a form: the reservation, the DNS entry and the name the agent sets
// on the machine all come from here.
//
// Names somebody typed are left alone. That is the whole point of recognising
// our own: a blade called "buildbox" was called that on purpose.
func (a *App) renameSiteBlades(siteID int64) (int, error) {
	blades, err := a.listBlades()
	if err != nil {
		return 0, err
	}
	prefix := a.hostPrefix(siteID)
	changed := 0
	for i := range blades {
		b := &blades[i]
		if b.SiteID != siteID || b.RackID == nil || b.Slot == nil {
			continue
		}
		if b.Hostname != "" && !autoHostRe.MatchString(b.Hostname) {
			continue
		}
		want := bladeHostname(prefix, int64(b.RackIdx), *b.Slot)
		if want == b.Hostname {
			continue
		}
		if _, err := a.db.Exec(`UPDATE blades SET hostname=? WHERE serial=?`,
			want, b.Serial); err != nil {
			continue
		}
		a.logEvent(b.Serial, "info", "renamed "+b.Hostname+" → "+want)
		changed++
	}
	return changed, nil
}

// validHostPrefix keeps a site's prefix to what a hostname may contain and
// short enough that the name it produces still reads as a name.
var hostPrefixRe = regexp.MustCompile(`^[a-z0-9]{0,8}$`)

func validHostPrefix(p string) bool { return hostPrefixRe.MatchString(p) }

// siteRelayURL is where the blades of one site report. Empty until that site
// has said so, and empty is the right answer then: a blade keeps whatever it
// was told at installation rather than being sent somewhere unknown.
func (a *App) siteRelayURL(siteID int64) string {
	if siteID == 0 {
		return ""
	}
	st, err := a.getSite(siteID)
	if err != nil {
		return ""
	}
	return st.RelayURL
}
