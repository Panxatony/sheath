package main

import (
	"bufio"
	"io"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Netboot detection
// -----------------
// A blade that is netbooting leaves two traces:
//
//  1. Passive, immediately: dnsmasq logs DHCP and every TFTP file it serves.
//     Within seconds that tells you something is booting, its MAC and IP, and
//     how far it got — long before an operating system runs. It does not tell
//     you the serial number.
//
//  2. Active, shortly after: the mini-OS reports its serial number over the
//     API. Only then is the identity certain.
//
// Both traces land in the same table and are joined on the MAC. From that the
// interface shows "this blade is waiting for an image choice", and the choice
// waits in the row until the installer collects it.

// Stages, in order of progress.
const (
	stageDHCP      = "dhcp"      // address requested
	stageTFTP      = "tftp"      // loading firmware
	stageRamdisk   = "ramdisk"   // boot.img served, mini-OS starting
	stageInstaller = "installer" // mini-OS has checked in
	stageWriting   = "writing"   // writing right now
	stageDone      = "done"
	stageError     = "error"
)

var stageRank = map[string]int{
	stageDHCP: 1, stageTFTP: 2, stageRamdisk: 3,
	stageInstaller: 4, stageWriting: 5, stageDone: 6, stageError: 7,
}

const netbootSchema = `
CREATE TABLE IF NOT EXISTS netboot (
    mac        TEXT PRIMARY KEY,
    ip         TEXT NOT NULL DEFAULT '',
    serial     TEXT NOT NULL DEFAULT '',
    stage      TEXT NOT NULL DEFAULT 'dhcp',
    files      INTEGER NOT NULL DEFAULT 0,
    last_file  TEXT NOT NULL DEFAULT '',
    image      TEXT NOT NULL DEFAULT '',   -- chosen here, collected by the installer
    client     TEXT NOT NULL DEFAULT '',   -- 'netboot' (RPi bootloader) or 'os'
    note       TEXT NOT NULL DEFAULT '',
    site_id    INTEGER NOT NULL DEFAULT 0,  -- which site saw it
    first_seen TEXT NOT NULL,
    last_seen  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_netboot_seen ON netboot(last_seen DESC);
`

type NetbootSession struct {
	MAC       string `json:"mac"`
	IP        string `json:"ip"`
	Serial    string `json:"serial"`
	Stage     string `json:"stage"`
	Files     int    `json:"files"`
	LastFile  string `json:"last_file"`
	Image     string `json:"image"`
	Client    string `json:"client"`
	Note      string `json:"note"`
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
	SiteID    int64  `json:"site_id"`

	// derived
	Known    bool   `json:"known"`     // MAC is known to the inventory
	Hostname string `json:"hostname"`  // if known
	RackName string `json:"rack_name"` // if known
	Slot     int    `json:"slot"`
	Ago      string `json:"-"`
	Waiting  bool   `json:"waiting"` // waiting for an image choice
}

// ── Tailing the log ──────────────────────────────────────────────────

var (
	// dnsmasq-dhcp: "DHCPACK(eth0) 10.0.0.210 e4:5f:01:11:22:33 name"
	reDHCPAck = regexp.MustCompile(
		`DHCP(?:ACK|OFFER)\([^)]*\)\s+(\d+\.\d+\.\d+\.\d+)\s+([0-9a-fA-F:]{17})`)
	// "DHCPDISCOVER(eth0) e4:5f:01:11:22:33" — no address yet
	reDHCPDiscover = regexp.MustCompile(
		`DHCP(?:DISCOVER|REQUEST)\([^)]*\)\s+([0-9a-fA-F:]{17})`)
	// dnsmasq-tftp: "sent /srv/sheath/tftp/boot.img to 10.0.0.210"
	reTFTPSent = regexp.MustCompile(`sent\s+(\S+)\s+to\s+(\d+\.\d+\.\d+\.\d+)`)
	// "<xid> vendor class: PXEClient:Arch:00000:UNDI:002001"
	reVendor = regexp.MustCompile(`dnsmasq-dhcp\[\d+\]:\s+(\S+)\s+vendor class:\s*(.+)$`)
	// The transaction id prefixes every DHCP line of the same request.
	reXID      = regexp.MustCompile(`dnsmasq-dhcp\[\d+\]:\s+(\S+)\s+DHCP`)
	reTFTPFail = regexp.MustCompile(`failed sending\s+(\S+)\s+to\s+(\d+\.\d+\.\d+\.\d+)`)
)

// watchDnsmasqLog tails the dnsmasq log file. It never rewinds: on start it
// seeks to the end so a restart does not report old boots as new.
func (a *App) watchDnsmasqLog(path string) {
	var (
		f      *os.File
		rd     *bufio.Reader
		offset int64
	)
	open := func() bool {
		var err error
		f, err = os.Open(path)
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

	warned := false
	for {
		if f == nil {
			if !open() {
				if !warned {
					// Tell "does not exist" apart from "may not read": both
					// lead to the same waiting, but only the right message
					// leads to the right fix.
					if _, err := os.Stat(path); err == nil {
						log.Printf("netboot watch: %s exists but is not readable "+
							"— does the file belong to the service group?", path)
					} else if os.IsNotExist(err) {
						log.Printf("netboot watch: %s not present yet — waiting", path)
					} else {
						log.Printf("netboot watch: %s not accessible (%v)", path, err)
					}
					warned = true
				}
				time.Sleep(5 * time.Second)
				continue
			}
			log.Printf("netboot watch: reading %s", path)
			warned = false
		}

		line, err := rd.ReadString('\n')
		if len(line) > 0 {
			offset += int64(len(line))
			a.handleLogLine(strings.TrimRight(line, "\r\n"))
			continue
		}
		if err != nil && err != io.EOF {
			f.Close()
			f = nil
			continue
		}
		// At the end: check whether the file was rotated
		// (then it is smaller than our position) and reopen if so.
		if st, err := os.Stat(path); err == nil && st.Size() < offset {
			f.Close()
			f, offset = nil, 0
			continue
		}
		time.Sleep(700 * time.Millisecond)
	}
}

func (a *App) handleLogLine(line string) {
	if m := reVendor.FindStringSubmatch(line); m != nil {
		rememberXID(m[1], strings.TrimSpace(m[2]))
		return
	}
	// No vendor-class entry for this transaction means an ordinary client.
	// That is evaluated when the MAC is matched.
	kind := ""
	if m := reXID.FindStringSubmatch(line); m != nil {
		kind = clientKind(classForXID(m[1]))
		if kind == "" {
			kind = "os"
		}
	}
	if m := reDHCPAck.FindStringSubmatch(line); m != nil {
		// Create first, then label — an UPDATE on a row that does not exist
		// yet would go nowhere.
		a.touchNetboot(normMAC(m[2]), m[1], stageDHCP, "")
		a.setClient(normMAC(m[2]), kind)
		return
	}
	if m := reDHCPDiscover.FindStringSubmatch(line); m != nil {
		a.touchNetboot(normMAC(m[1]), "", stageDHCP, "")
		a.setClient(normMAC(m[1]), kind)
		return
	}
	if m := reTFTPSent.FindStringSubmatch(line); m != nil {
		file, ip := m[1], m[2]
		stage := stageTFTP
		// boot.img is the last and largest file: after it the kernel takes
		// over in RAM. From here the mini-OS is about to run.
		if strings.HasSuffix(file, "boot.img") {
			stage = stageRamdisk
		}
		a.touchNetbootByIP(ip, stage, file, "")
		return
	}
	if m := reTFTPFail.FindStringSubmatch(line); m != nil {
		a.touchNetbootByIP(m[2], "", "", "TFTP transfer aborted: "+shortFile(m[1]))
		return
	}
}

// The vendor-class line names only the transaction id; the MAC appears on
// the next line. So it is remembered briefly.
type xidNote struct {
	class string
	when  time.Time
}

var (
	xidMu  sync.Mutex
	xidMap = map[string]xidNote{}
)

func rememberXID(xid, class string) {
	xidMu.Lock()
	defer xidMu.Unlock()
	xidMap[xid] = xidNote{class: class, when: time.Now()}
	if len(xidMap) > 512 {
		for k, v := range xidMap {
			if time.Since(v.when) > 2*time.Minute {
				delete(xidMap, k)
			}
		}
	}
}

func classForXID(xid string) string {
	xidMu.Lock()
	defer xidMu.Unlock()
	n, ok := xidMap[xid]
	if !ok || time.Since(n.when) > 2*time.Minute {
		return ""
	}
	return n.class
}

// clientKind tells the RPi bootloader apart from the DHCP client of a
// running operating system. The bootloader announces itself with the PXE
// vendor class; a Linux client does not. That is the most reliable signal for
// whether a blade wanted to netboot at all — the mere absence of TFTP could
// just be timing.
func clientKind(class string) string {
	if strings.Contains(class, "PXEClient") {
		return "netboot"
	}
	if class != "" {
		return "os"
	}
	return ""
}

func normMAC(s string) string { return strings.ToLower(s) }

func shortFile(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// touchNetboot creates a session or carries it forward. The stage only moves
// forward — a DHCP packet arriving late must not push a running write back.
func (a *App) touchNetboot(mac, ip, stage, note string) {
	if mac == "" {
		return
	}
	ts := now()
	_, err := a.db.Exec(`INSERT INTO netboot(mac,ip,stage,first_seen,last_seen,note)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(mac) DO UPDATE SET
		  ip        = CASE WHEN excluded.ip <> '' THEN excluded.ip ELSE netboot.ip END,
		  last_seen = excluded.last_seen,
		  note      = CASE WHEN excluded.note <> '' THEN excluded.note ELSE netboot.note END`,
		mac, ip, stage, ts, ts, note)
	if err != nil {
		return
	}
	a.raiseStage(mac, stage)
}

func (a *App) touchNetbootByIP(ip, stage, file, note string) {
	if ip == "" {
		return
	}
	var mac string
	// TFTP lines name only the address; the MAC comes from the DHCP traffic
	// that preceded it in the same session.
	err := a.db.QueryRow(`SELECT mac FROM netboot WHERE ip=? ORDER BY last_seen DESC LIMIT 1`, ip).Scan(&mac)
	if err != nil {
		return
	}
	ts := now()
	if file != "" {
		_, _ = a.db.Exec(`UPDATE netboot SET files=files+1,last_file=?,last_seen=? WHERE mac=?`,
			shortFile(file), ts, mac)
	} else {
		_, _ = a.db.Exec(`UPDATE netboot SET last_seen=? WHERE mac=?`, ts, mac)
	}
	if note != "" {
		_, _ = a.db.Exec(`UPDATE netboot SET note=? WHERE mac=?`, note, mac)
	}
	a.raiseStage(mac, stage)
}

// setClient records the client kind. "netboot" overrides "os": if the same
// adapter ever announced itself as the bootloader, it was a netboot.
func (a *App) setClient(mac, kind string) {
	if mac == "" || kind == "" {
		return
	}
	if kind == "netboot" {
		_, _ = a.db.Exec(`UPDATE netboot SET client=? WHERE mac=?`, kind, mac)
		return
	}
	_, _ = a.db.Exec(`UPDATE netboot SET client=? WHERE mac=? AND client<>'netboot'`, kind, mac)
}

func (a *App) raiseStage(mac, stage string) {
	if stage == "" {
		return
	}
	var cur string
	if err := a.db.QueryRow(`SELECT stage FROM netboot WHERE mac=?`, mac).Scan(&cur); err != nil {
		return
	}
	if stageRank[stage] <= stageRank[cur] {
		return
	}
	_, _ = a.db.Exec(`UPDATE netboot SET stage=? WHERE mac=?`, stage, mac)
	if stage == stageRamdisk {
		a.logEvent("", "info", "netboot: "+mac+" loaded the mini OS")
	}
}

// ── Queries ──────────────────────────────────────────────────────────

// listNetboot returns the sessions of the last hour, newest first, enriched
// with what the inventory knows about the MAC.
func (a *App) listNetboot(l Lang) ([]NetbootSession, error) {
	cutoff := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	rows, err := a.db.Query(`SELECT mac,ip,serial,stage,files,last_file,image,client,note,
		first_seen,last_seen,site_id
		FROM netboot WHERE last_seen >= ? ORDER BY last_seen DESC`, cutoff)
	if err != nil {
		return nil, err
	}
	out := []NetbootSession{}
	for rows.Next() {
		var s NetbootSession
		if err := rows.Scan(&s.MAC, &s.IP, &s.Serial, &s.Stage, &s.Files,
			&s.LastFile, &s.Image, &s.Client, &s.Note, &s.FirstSeen, &s.LastSeen,
			&s.SiteID); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Enrich only after closing the cursor — otherwise this query holds one
	// connection while waiting for the next.
	blades, _ := a.listBlades()
	byMAC := map[string]Blade{}
	bySerial := map[string]Blade{}
	for _, b := range blades {
		if b.MAC != "" {
			byMAC[strings.ToLower(b.MAC)] = b
		}
		bySerial[b.Serial] = b
	}
	for i := range out {
		s := &out[i]
		b, ok := bySerial[s.Serial]
		if !ok {
			b, ok = byMAC[s.MAC]
		}
		if ok {
			s.Known = true
			s.Hostname = b.Hostname
			s.RackName = b.RackName
			if b.Slot != nil {
				s.Slot = *b.Slot
			}
			if s.Image == "" {
				s.Image = b.Image
			}
		}
		s.Ago = ago(l, s.LastSeen)
		s.Waiting = s.Image == "" &&
			stageRank[s.Stage] >= stageRank[stageRamdisk] &&
			stageRank[s.Stage] < stageRank[stageWriting]
	}
	return out, nil
}

// chooseImage records the image choice on the netboot session. If the blade
// is already in the inventory it is stored there too — otherwise the
// installer collects it from the session when it checks in.
func (a *App) chooseImage(mac, image string) error {
	mac = normMAC(mac)
	if _, err := a.db.Exec(`UPDATE netboot SET image=? WHERE mac=?`, image, mac); err != nil {
		return err
	}
	var serial string
	_ = a.db.QueryRow(`SELECT serial FROM netboot WHERE mac=?`, mac).Scan(&serial)
	if serial == "" {
		_ = a.db.QueryRow(`SELECT serial FROM blades WHERE lower(mac)=?`, mac).Scan(&serial)
	}
	if serial != "" {
		// A choice in the netboot panel explicitly means "write this now" —
		// unlike a mere assignment in the slot view.
		_, _ = a.db.Exec(`UPDATE blades SET image=?,install_state=? WHERE serial=?`,
			image, installPending, serial)
		_, _ = a.syncDHCP()
	}
	a.logEvent(serial, "info", "image chosen for netboot: "+image+" ("+mac+")")
	return nil
}

// linkNetboot ties a session to the serial number once the mini-OS has
// checked in, and returns any image pre-selected there.
func (a *App) linkNetboot(serial, mac string) string {
	mac = normMAC(mac)
	if mac == "" {
		return ""
	}
	_, _ = a.db.Exec(`UPDATE netboot SET serial=?,last_seen=? WHERE mac=?`, serial, now(), mac)
	a.raiseStage(mac, stageInstaller)
	var img string
	_ = a.db.QueryRow(`SELECT image FROM netboot WHERE mac=?`, mac).Scan(&img)
	return img
}

// resetStage sets the stage hard, including backwards. Needed when a new
// installation begins: raiseStage only moves forward — sensible so a late
// DHCP packet cannot push back a running write, but it left a second run
// stuck on "done" while it was actually writing.
func (a *App) resetStage(serial, stage, note string) {
	if serial == "" {
		return
	}
	_, _ = a.db.Exec(`UPDATE netboot SET stage=?,note=?,last_seen=? WHERE serial=?`,
		stage, note, now(), serial)
}

func (a *App) netbootStage(serial, stage, note string) {
	if serial == "" {
		return
	}
	var mac string
	if err := a.db.QueryRow(`SELECT mac FROM netboot WHERE serial=? ORDER BY last_seen DESC LIMIT 1`,
		serial).Scan(&mac); err != nil {
		return
	}
	_, _ = a.db.Exec(`UPDATE netboot SET last_seen=?,note=? WHERE mac=?`, now(), note, mac)
	a.raiseStage(mac, stage)
}

// siteLastSaw answers the question a blade without a slot raises: which
// building is it standing in? It has no BladeRunner, so it has no site of its
// own — but a site watched it come up on the wire, and that site wrote it
// down. Without this the list of unplaced blades says "somewhere".
func (a *App) siteLastSaw(serial string) (int64, string) {
	var id int64
	if err := a.db.QueryRow(`SELECT site_id FROM netboot WHERE serial=? AND site_id<>0
		ORDER BY last_seen DESC LIMIT 1`, serial).Scan(&id); err != nil || id == 0 {
		return 0, ""
	}
	st, err := a.getSite(id)
	if err != nil {
		return id, ""
	}
	return id, st.Name
}

// addressOnTheWire is the address a blade was last seen holding, as the site
// read it out of the DHCP log. Not the same question as "which address does
// it have" in the inventory: that one is derived from where the blade stands,
// and a blade standing nowhere still holds something.
func (a *App) addressOnTheWire(serial string) string {
	var ip string
	if err := a.db.QueryRow(`SELECT ip FROM netboot WHERE serial=? AND ip<>''
		ORDER BY last_seen DESC LIMIT 1`, serial).Scan(&ip); err != nil {
		return ""
	}
	return ip
}

// stageOnTheWire is where a blade stands on the wire, and whether that is news
// or history. Ten minutes: a mini OS asks the site every thirty seconds, so a
// session older than that belongs to a blade that has moved on — usually into
// the system it just installed.
func (a *App) stageOnTheWire(serial string) (string, bool) {
	var stage, last string
	if err := a.db.QueryRow(`SELECT stage,last_seen FROM netboot WHERE serial=?
		ORDER BY last_seen DESC LIMIT 1`, serial).Scan(&stage, &last); err != nil {
		return "", false
	}
	t, err := time.Parse(time.RFC3339, last)
	if err != nil {
		return stage, false
	}
	return stage, time.Since(t) < 10*time.Minute
}
