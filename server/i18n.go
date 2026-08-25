package main

import (
	"fmt"
	"net/http"
	"strings"
)

// Languages
// ---------
// English is the default, German an option. Text no longer lives in the
// templates but under keys in a catalogue — otherwise every string would need
// maintaining in several places and the translations would inevitably drift.
//
// Errors from the domain logic go through the catalogue too. They surface as
// messages in the interface, so leaving exactly the sentences you read when
// something breaks untranslated would be inconsistent.

type Lang string

const (
	LangEN Lang = "en"
	LangDE Lang = "de"
)

const langCookie = "rk_lang"

func (l Lang) Valid() bool { return l == LangEN || l == LangDE }

// langOf resolves the language: an explicit choice in the URL, else the
// cookie, else the server default, else English.
func (a *App) langOf(r *http.Request) Lang {
	if q := Lang(r.URL.Query().Get("lang")); q.Valid() {
		return q
	}
	if c, err := r.Cookie(langCookie); err == nil {
		if l := Lang(c.Value); l.Valid() {
			return l
		}
	}
	if l := Lang(a.setting("ui_lang", string(LangEN))); l.Valid() {
		return l
	}
	return LangEN
}

func setLangCookie(w http.ResponseWriter, l Lang) {
	http.SetCookie(w, &http.Cookie{
		Name: langCookie, Value: string(l), Path: "/",
		HttpOnly: false, SameSite: http.SameSiteLaxMode, MaxAge: 365 * 24 * 3600,
	})
}

// T translates a key. If it is missing the key itself is rendered — visible
// immediately in production rather than quietly wrong.
func T(l Lang, key string, args ...any) string {
	e, ok := catalog[key]
	if !ok {
		return key
	}
	s, ok := e[l]
	if !ok || s == "" {
		s = e[LangEN]
	}
	if len(args) == 0 {
		return s
	}
	return fmt.Sprintf(s, args...)
}

// ── Errors that carry a key ──────────────────────────────────────────

// msgErr carries its key and arguments so an error from the domain logic can
// appear in the reader's language. Error() returns English — that is the
// wording that belongs in the log.
type msgErr struct {
	key  string
	args []any
}

func (e *msgErr) Error() string { return T(LangEN, e.key, e.args...) }

func me(key string, args ...any) error { return &msgErr{key: key, args: args} }

// errText translates an error if it carries a key, and passes it through
// unchanged otherwise.
// joinErr renders a list of keyed messages in the reader's language.
func joinErr(l Lang, errs []error) string {
	parts := make([]string, 0, len(errs))
	for _, e := range errs {
		parts = append(parts, errText(l, e))
	}
	return strings.Join(parts, " · ")
}

func errText(l Lang, err error) string {
	if err == nil {
		return ""
	}
	if m, ok := err.(*msgErr); ok {
		return T(l, m.key, m.args...)
	}
	return err.Error()
}

// ── Katalog ──────────────────────────────────────────────────────────

var catalog = map[string]map[Lang]string{
	// Chrome
	"nav.overview": {LangEN: "Overview", LangDE: "Übersicht"},
	"nav.rack":     {LangEN: "BladeRunner", LangDE: "BladeRunner"},
	"btn.signout":  {LangEN: "Sign out", LangDE: "Abmelden"},
	"lang.label":   {LangEN: "Language", LangDE: "Sprache"},
	"meta.racks":   {LangEN: "BladeRunners", LangDE: "BladeRunner"},
	"meta.blades":  {LangEN: "Blades", LangDE: "Blades"},
	"meta.used":    {LangEN: "Occupied", LangDE: "Belegt"},
	"meta.free":    {LangEN: "Free", LangDE: "Frei"},
	"sub.network": {
		LangEN: "Network %s.0/24 · dynamic pool .%d–.%d",
		LangDE: "Netz %s.0/24 · dynamischer Pool .%d–.%d"},
	"foot.api": {LangEN: "Rookery · API at /api/v1/", LangDE: "Rookery · API unter /api/v1/"},
	"foot.tm": {
		LangEN: "BladeRunner and Compute Blade are trademarks of Uptime Lab.",
		LangDE: "BladeRunner und Compute Blade sind Marken von Uptime Lab."},
	"warn.open": {
		LangEN: "<b>Careful:</b> no admin token is set — the interface and the API are unprotected.",
		LangDE: "<b>Achtung:</b> Kein Admin-Token gesetzt — Oberfläche und API sind ungeschützt."},
	"warn.net": {LangEN: "Network", LangDE: "Netz"},

	// Sign-in
	"login.lead":   {LangEN: "Sign in to manage.", LangDE: "Zum Verwalten anmelden."},
	"login.token":  {LangEN: "Admin token", LangDE: "Admin-Token"},
	"login.submit": {LangEN: "Sign in", LangDE: "Anmelden"},
	"login.wrong":  {LangEN: "That token doesn't match.", LangDE: "Das Token stimmt nicht."},
	"login.hint": {
		LangEN: "The token lives on the server. It belongs to the service user and only root can read it, so fetch it with <code>sudo</code>:",
		LangDE: "Das Token liegt auf dem Server. Es gehört dem Dienstbenutzer und ist nur für <code>root</code> lesbar — also mit <code>sudo</code> abrufen:"},

	// Overview
	"ov.newrack":   {LangEN: "Add a BladeRunner", LangDE: "BladeRunner hinzufügen"},
	"ov.nextblock": {LangEN: "next address block from %s", LangDE: "nächster Adressblock ab %s"},
	"ov.nospace": {
		LangEN: "No address block left in this network. Delete a rack or enlarge the network.",
		LangDE: "Im Netz ist kein Adressblock mehr frei. Ein bestehendes Rack löschen oder das Netz vergrößern."},
	"ov.blockhint": {
		LangEN: "Every BladeRunner reserves a fixed block of 20 addresses — even a 2-node one. That keeps the addresses the same if a larger unit later takes its place.",
		LangDE: "Jeder BladeRunner bekommt einen festen Block von 20 Adressen — auch ein 2-Node-Gerät. Damit bleiben die Adressen gleich, wenn später ein größeres an dieselbe Stelle kommt."},
	"ov.racks":    {LangEN: "BladeRunners", LangDE: "BladeRunner"},
	"ov.rackhint": {LangEN: "click one to fill its slots", LangDE: "zum Bestücken anklicken"},
	"ov.norack": {
		LangEN: "No BladeRunner yet. Add one above — then blades can go into its slots.",
		LangDE: "Noch kein BladeRunner vorhanden. Oben einen hinzufügen — danach lassen sich Blades in seine Slots einsetzen."},
	"ov.occupancy":  {LangEN: "%d occupied · %d free", LangDE: "%d belegt · %d frei"},
	"ov.slots":      {LangEN: "%d nodes", LangDE: "%d Nodes"},
	"ov.unassigned": {LangEN: "Blades without a slot", LangDE: "Blades ohne Slot"},
	"ov.unassignedtag": {
		LangEN: "%d device(s) · served from the dynamic pool",
		LangDE: "%d Stück · bedient vom dynamischen Pool"},
	"ov.unassignedhint": {
		LangEN: "These blades have checked in but sit in no slot. Put them into a rack — only then do they get a fixed address.",
		LangDE: "Diese Blades haben sich gemeldet, sitzen aber in keinem Slot. Im jeweiligen Rack lassen sie sich einsetzen — erst dann bekommen sie eine feste Adresse."},

	// Netboot
	"nb.title":   {LangEN: "On the network right now", LangDE: "Gerade am Netz"},
	"nb.count":   {LangEN: "%d device(s)", LangDE: "%d Gerät(e)"},
	"nb.refresh": {LangEN: " · refreshes every 5 s", LangDE: " · aktualisiert sich alle 5 s"},
	"nb.unknown": {LangEN: "unknown device", LangDE: "unbekanntes Gerät"},
	"nb.files":   {LangEN: "%d files · last %s", LangDE: "%d Dateien · zuletzt %s"},
	"nb.write":   {LangEN: "Write", LangDE: "Schreiben"},
	"nb.how":     {LangEN: "How this is detected", LangDE: "Wie das erkannt wird"},
	"nb.bootorder": {
		LangEN: "check BOOT_ORDER", LangDE: "BOOT_ORDER prüfen"},
	"nb.nocatalog": {
		LangEN: "catalog is empty", LangDE: "Katalog ist leer"},
	"nb.leasehint": {
		LangEN: "No netboot attempted — it booted from its own storage. <code>BOOT_ORDER</code> is probably still at the factory value (<code>0xf641</code>); set <code>0xf26</code> for netboot.",
		LangDE: "Kein Netzboot versucht — hat vom eigenen Speicher gebootet. <code>BOOT_ORDER</code> steht vermutlich noch auf Werk (<code>0xf641</code>); für Netzboot <code>0xf26</code> setzen."},
	"nb.hint": {
		LangEN: "Read from the dnsmasq log. The Raspberry Pi bootloader announces itself as <code>PXEClient:…</code>; an ordinary Linux client does not — that is what separates a netboot from a plain address lease. A device that only took an address booted from its own storage; its <code>BOOT_ORDER</code> is probably still at the factory value (<code>0xf641</code>), <code>0xf62</code> enables netboot.",
		LangDE: "Aus dem dnsmasq-Protokoll gelesen. Der RPi-Bootloader meldet sich mit <code>PXEClient:…</code>, ein gewöhnlicher Linux-Client nicht — daran hängt der Unterschied zwischen Netzboot und bloßem Adressbezug. Ein Gerät, das nur eine Adresse bezog, bootete vom eigenen Speicher; sein <code>BOOT_ORDER</code> steht vermutlich noch auf Werk (<code>0xf641</code>), <code>0xf62</code> erlaubt den Netzboot."},

	// Stages
	"stage.dhcp":      {LangEN: "address requested", LangDE: "Adresse angefragt"},
	"stage.tftp":      {LangEN: "loading firmware", LangDE: "lädt Firmware"},
	"stage.ramdisk":   {LangEN: "mini-OS loaded", LangDE: "Mini-OS geladen"},
	"stage.installer": {LangEN: "installer checking in", LangDE: "Installer meldet sich"},
	"stage.writing":   {LangEN: "writing image", LangDE: "schreibt Image"},
	"stage.done":      {LangEN: "done", LangDE: "fertig"},
	"stage.error":     {LangEN: "error", LangDE: "Fehler"},
	"stage.leaseonly": {LangEN: "address lease only", LangDE: "nur Adresse bezogen"},

	// Table headers
	"th.slot":     {LangEN: "Slot", LangDE: "Slot"},
	"th.hostname": {LangEN: "Hostname", LangDE: "Hostname"},
	"th.ip":       {LangEN: "IP", LangDE: "IP"},
	"th.mac":      {LangEN: "MAC", LangDE: "MAC"},
	"th.image":    {LangEN: "Image", LangDE: "Image"},
	"th.status":   {LangEN: "Status", LangDE: "Status"},
	"th.action":   {LangEN: "Action", LangDE: "Aktion"},
	"th.serial":   {LangEN: "Serial number", LangDE: "Seriennummer"},
	"th.lastseen": {LangEN: "Last seen", LangDE: "Zuletzt gemeldet"},
	"th.device":   {LangEN: "Device", LangDE: "Gerät"},
	"th.progress": {LangEN: "Progress", LangDE: "Fortschritt"},
	"th.last":     {LangEN: "Last", LangDE: "Zuletzt"},

	// Form
	"form.name":     {LangEN: "Name", LangDE: "Name"},
	"form.slots":    {LangEN: "Nodes", LangDE: "Nodes"},
	"form.location": {LangEN: "Location", LangDE: "Standort"},
	"form.optional": {LangEN: "optional", LangDE: "optional"},
	"form.create":   {LangEN: "Create", LangDE: "Anlegen"},
	"form.save":     {LangEN: "Save", LangDE: "Speichern"},
	"form.example":  {LangEN: "e.g. Basement", LangDE: "z. B. Keller"},

	// BladeRunner page
	"rk.slots":  {LangEN: "Slots", LangDE: "Slots"},
	"rk.nofree": {LangEN: "no blade without a slot available", LangDE: "kein Blade ohne Slot verfügbar"},
	"rk.free":   {LangEN: "free", LangDE: "frei"},
	"rk.insert": {LangEN: "Insert", LangDE: "Einsetzen"},
	"rk.set":    {LangEN: "Set", LangDE: "Setzen"},
	"rk.none":   {LangEN: "— none —", LangDE: "— keins —"},
	"rk.edit":   {LangEN: "Edit BladeRunner", LangDE: "BladeRunner bearbeiten"},
	"rk.edithint": {
		LangEN: "The address block survives a size change. Shrinking works only while no blade sits in a slot that would disappear.",
		LangDE: "Der Adressblock bleibt beim Ändern der Größe erhalten. Verkleinern geht nur, solange kein Blade in einem Slot sitzt, den es danach nicht mehr gäbe."},
	"rk.delete":     {LangEN: "Remove BladeRunner", LangDE: "BladeRunner entfernen"},
	"rk.deletehint": {LangEN: "The address block becomes free again.", LangDE: "Der Adressblock wird danach wieder frei."},
	"rk.hasblades": {
		LangEN: "%d blade(s) still sit in this BladeRunner. Remove them first.",
		LangDE: "Im BladeRunner sitzen noch %d Blade(s). Erst entnehmen."},
	"rk.confirm":   {LangEN: "Really remove this BladeRunner?", LangDE: "Diesen BladeRunner wirklich entfernen?"},
	"rk.block":     {LangEN: "address block %s – %s", LangDE: "Adressblock %s – %s"},
	"act.identify": {LangEN: "Identify", LangDE: "Identify"},
	"act.install":  {LangEN: "Install now", LangDE: "Jetzt installieren"},
	"act.installtip": {
		LangEN: "write the assigned image at the next netboot",
		LangDE: "das zugewiesene Image beim nächsten Netzboot schreiben"},
	"hw.soc":       {LangEN: "SoC", LangDE: "SoC"},
	"hw.airflow":   {LangEN: "Airflow", LangDE: "Luft"},
	"hw.fan":       {LangEN: "Fan", LangDE: "Lüfter"},
	"hw.fantarget": {LangEN: "Target", LangDE: "Sollwert"},
	"hw.fanunit":   {LangEN: "Fan unit", LangDE: "Fan Unit"},
	"hw.module":    {LangEN: "Module", LangDE: "Modul"},
	"hw.state":     {LangEN: "Blade state", LangDE: "Blade-Zustand"},
	"hw.stealth":   {LangEN: "Stealth", LangDE: "Stealth"},
	"hw.on":        {LangEN: "on", LangDE: "an"},
	"hw.off":       {LangEN: "off", LangDE: "aus"},
	"hw.soctrend":  {LangEN: "SoC 48 h", LangDE: "SoC 48 h"},
	"hw.fantrend":  {LangEN: "Fan 48 h", LangDE: "Lüfter 48 h"},
	"hw.window":    {LangEN: "%d samples, one every 5 min", LangDE: "%d Messwerte, alle 5 min einer"},
	"hw.hottest":   {LangEN: "Hottest slot", LangDE: "Wärmster Slot"},
	"hw.socspan":   {LangEN: "SoC spread", LangDE: "SoC-Spanne"},
	"hw.rpmspan":   {LangEN: "Fan spread", LangDE: "Lüfter-Spanne"},
	"hw.smartfans": {LangEN: "%d of %d on a smart fan unit", LangDE: "%d von %d an einer Smart Fan Unit"},

	"log.title": {LangEN: "Activity", LangDE: "Aktivität"},
	"log.count": {LangEN: "%d entries", LangDE: "%d Einträge"},
	"log.when":  {LangEN: "When", LangDE: "Wann"},
	"log.blade": {LangEN: "Blade", LangDE: "Blade"},
	"log.what":  {LangEN: "Event", LangDE: "Ereignis"},
	"log.empty": {
		LangEN: "Nothing logged for the blades in this BladeRunner yet.",
		LangDE: "Für die Blades in diesem BladeRunner ist noch nichts protokolliert."},

	"health.nohb":      {LangEN: "no heartbeat", LangDE: "kein Heartbeat"},
	"health.soc":       {LangEN: "SoC %.0f °C", LangDE: "SoC %.0f °C"},
	"health.nvme":      {LangEN: "NVMe %.0f °C", LangDE: "NVMe %.0f °C"},
	"health.disk":      {LangEN: "disk %.0f %%", LangDE: "Disk %.0f %%"},
	"health.undervolt": {LangEN: "undervoltage", LangDE: "Unterspannung"},
	"health.throttled": {LangEN: "throttled", LangDE: "gedrosselt"},
	"health.fanstop":   {LangEN: "fan stopped", LangDE: "Lüfter steht"},

	"inst.idle":    {LangEN: "not requested", LangDE: "nicht angefordert"},
	"inst.pending": {LangEN: "install pending", LangDE: "Installation angefordert"},
	"inst.done":    {LangEN: "installed", LangDE: "installiert"},
	"inst.error":   {LangEN: "install failed", LangDE: "Installation fehlgeschlagen"},
	"th.install":   {LangEN: "Installation", LangDE: "Installation"},
	"th.temp":      {LangEN: "SoC / NVMe", LangDE: "SoC / NVMe"},
	"th.distro":    {LangEN: "Distribution", LangDE: "Distribution"},
	"th.role":      {LangEN: "Role", LangDE: "Rolle"},
	"th.soc":       {LangEN: "SoC", LangDE: "SoC"},
	"th.fan":       {LangEN: "Fan", LangDE: "Lüfter"},
	"role.none":    {LangEN: "—", LangDE: "—"},
	"menu.open":    {LangEN: "Actions", LangDE: "Aktionen"},
	"menu.planned": {LangEN: "planned:", LangDE: "geplant:"},

	// Status shown in a slot row
	"st.free":         {LangEN: "free", LangDE: "frei"},
	"st.insync":       {LangEN: "in sync", LangDE: "in sync"},
	"st.rebootreq":    {LangEN: "restart pending", LangDE: "Neustart nötig"},
	"st.drift":        {LangEN: "drift", LangDE: "drift"},
	"st.critical":     {LangEN: "critical", LangDE: "kritisch"},
	"st.warn":         {LangEN: "attention", LangDE: "auffällig"},
	"st.offline":      {LangEN: "offline", LangDE: "offline"},
	"st.enrolled":     {LangEN: "no agent yet", LangDE: "noch kein Agent"},
	"st.provisioning": {LangEN: "provisioning", LangDE: "wird aufgesetzt"},
	"st.writing":      {LangEN: "writing %s %%", LangDE: "schreibt %s %%"},
	"msg.installrequested": {
		LangEN: "Install requested — %q is written at the next netboot of this blade.",
		LangDE: "Installation angefordert — %q wird beim nächsten Netzboot dieses Blades geschrieben."},
	"rk.installhint": {
		LangEN: "Assigning an image does not write anything. Only \"Install now\" arms it, and the blade must then netboot. That keeps a running system from being overwritten by an accidental netboot.",
		LangDE: "Ein Image zuzuweisen schreibt noch nichts. Erst „Jetzt installieren“ scharfschaltet, und das Blade muss danach netbooten. So überschreibt ein versehentlicher Netzboot kein laufendes System."},
	"act.reboot":  {LangEN: "Reboot", LangDE: "Reboot"},
	"act.reimage": {LangEN: "Reimage", LangDE: "Neu aufsetzen"},
	"act.remove":  {LangEN: "Remove", LangDE: "Entnehmen"},
	"act.identifytip": {
		LangEN: "make the edge LED blink", LangDE: "Kanten-LED blinken lassen"},
	"act.reimagetip": {
		LangEN: "write the image to the NVMe again", LangDE: "Image neu auf die NVMe schreiben"},

	// Messages
	"msg.rackcreated": {
		LangEN: "BladeRunner %q added — address block %s to %s.",
		LangDE: "BladeRunner %q hinzugefügt — Adressblock %s bis %s."},
	"msg.saved":   {LangEN: "Changes saved.", LangDE: "Änderungen gespeichert."},
	"msg.deleted": {LangEN: "BladeRunner %q removed.", LangDE: "BladeRunner %q entfernt."},
	"msg.removed": {
		LangEN: "Blade removed — its address is free again.",
		LangDE: "Blade entnommen — die Adresse ist wieder frei."},
	"msg.imageset": {
		LangEN: "Image %q assigned — it takes effect at the next netboot.",
		LangDE: "Image %q zugewiesen — es greift beim nächsten Netzboot."},
	"msg.imagecleared": {LangEN: "Image assignment removed.", LangDE: "Image-Zuweisung entfernt."},
	"msg.queued": {
		LangEN: "%q queued — the blade runs it the next time it checks in.",
		LangDE: "%q eingereiht — das Blade führt es beim nächsten Melden aus."},
	"msg.slotset": {LangEN: "Slot %d: %s → %s", LangDE: "Slot %d: %s → %s"},
	"msg.imagechosen": {
		LangEN: "Image %q chosen for %s — the installer picks it up the next time it asks.",
		LangDE: "Image %q für %s gewählt — der Installer holt es beim nächsten Nachfragen ab."},

	// Interface errors
	"err.form":        {LangEN: "Form could not be read.", LangDE: "Formular unlesbar."},
	"err.nameneeded":  {LangEN: "Please give it a name.", LangDE: "Bitte einen Namen angeben."},
	"err.rackexists":  {LangEN: "Could not be added — is the name already taken?", LangDE: "Konnte nicht angelegt werden — ist der Name schon vergeben?"},
	"err.rackgone":    {LangEN: "That BladeRunner no longer exists.", LangDE: "Diesen BladeRunner gibt es nicht mehr."},
	"err.bladegone":   {LangEN: "That blade does not exist.", LangDE: "Dieses Blade gibt es nicht."},
	"err.noblade":     {LangEN: "No blade selected.", LangDE: "Kein Blade ausgewählt."},
	"err.noimage":     {LangEN: "No image selected.", LangDE: "Kein Image ausgewählt."},
	"err.needimage":   {LangEN: "Assign an image first, then reimage.", LangDE: "Erst ein Image zuweisen, dann neu aufsetzen."},
	"err.unknownact":  {LangEN: "Unknown action.", LangDE: "Unbekannte Aktion."},
	"err.stillinrack": {LangEN: "%d blade(s) still sit in it. Remove them first.", LangDE: "Es sitzen noch %d Blade(s) darin. Erst entnehmen."},
	"err.deletefail":  {LangEN: "Delete failed: %s", LangDE: "Löschen fehlgeschlagen: %s"},
	"err.dhcpsync":    {LangEN: "Saved, but DHCP was not updated: %s", LangDE: "Gespeichert, aber DHCP nicht aktualisiert: %s"},
	"err.dhcpinsert":  {LangEN: "Inserted, but DHCP was not updated: %s", LangDE: "Eingesetzt, aber DHCP nicht aktualisiert: %s"},

	// Domain-logic errors
	"err.slotrange":    {LangEN: "Slot %d is outside this rack (1..%d)", LangDE: "Slot %d liegt außerhalb des Racks (1..%d)"},
	"err.slottaken":    {LangEN: "Slot %d in %q is already taken by %s", LangDE: "Slot %d in %q ist schon von %s belegt"},
	"err.racknotfound": {LangEN: "Rack %d does not exist", LangDE: "Rack %d gibt es nicht"},
	"err.bladeunknown": {LangEN: "Blade %s is unknown", LangDE: "Blade %s ist unbekannt"},
	"err.badsize":      {LangEN: "Size must be 2, 4, 10 or 20 nodes (was %d)", LangDE: "Größe muss 2, 4, 10 oder 20 Nodes sein (war %d)"},
	"err.shrink": {
		LangEN: "Cannot shrink to %d slots: slot %d is still occupied",
		LangDE: "Verkleinern auf %d Slots nicht möglich: Slot %d ist noch belegt"},
	"err.nooffset": {
		LangEN: "No free address block left in the configured network",
		LangDE: "Kein freier Adressblock mehr im konfigurierten Netz"},
	"err.assignfail": {LangEN: "Assignment failed: %s", LangDE: "Zuweisung fehlgeschlagen: %s"},
	"err.updatefail": {LangEN: "Update failed: %s", LangDE: "Änderung fehlgeschlagen: %s"},
	"err.macformat":  {LangEN: "MAC %q is not in the form aa:bb:cc:dd:ee:ff", LangDE: "MAC %q hat nicht das Format aa:bb:cc:dd:ee:ff"},
	"err.hostlabel":  {LangEN: "Hostname %q is not a valid DNS label", LangDE: "Hostname %q ist kein gültiges DNS-Label"},
	"err.notipv4":    {LangEN: "IP %q is not an IPv4 address", LangDE: "IP %q ist keine IPv4-Adresse"},

	// Network warnings
	"warn.netbase":   {LangEN: "net_base is not a valid IPv4 base: %s", LangDE: "net_base ist keine gültige IPv4-Basis: %s"},
	"warn.racksread": {LangEN: "Racks could not be read: %s", LangDE: "Racks nicht lesbar: %s"},
	"warn.overflow":  {LangEN: "Rack %q reaches beyond .254", LangDE: "Rack %q reicht über .254 hinaus"},
	"warn.pool": {
		LangEN: "Rack %q (.%d-.%d) overlaps the dynamic pool (.%d-.%d)",
		LangDE: "Rack %q (.%d-.%d) überlappt den dynamischen Pool (.%d-.%d)"},
	"warn.blocks": {
		LangEN: "Address blocks of %q and %q overlap",
		LangDE: "Adressblöcke von %q und %q überlappen"},
}

// langName names a language in its own language — so it is findable by
// someone who cannot read the one currently selected.
func langName(l Lang) string {
	switch l {
	case LangDE:
		return "Deutsch"
	default:
		return "English"
	}
}

func otherLang(l Lang) Lang {
	if l == LangDE {
		return LangEN
	}
	return LangDE
}

// withLang appends the language choice to a URL without losing existing
// parameters.
func withLang(path string, l Lang) string {
	if strings.Contains(path, "?") {
		return path + "&lang=" + string(l)
	}
	return path + "?lang=" + string(l)
}
