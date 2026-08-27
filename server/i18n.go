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
	"foot.api": {LangEN: "Sheath · API at /api/v1/", LangDE: "Sheath · API unter /api/v1/"},
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
		LangEN: "No netboot attempted — it booted from its own storage. <code>BOOT_ORDER</code> is probably still at the factory value (<code>0xf641</code>); set <code>0xf162</code> for netboot.",
		LangDE: "Kein Netzboot versucht — hat vom eigenen Speicher gebootet. <code>BOOT_ORDER</code> steht vermutlich noch auf Werk (<code>0xf641</code>); für Netzboot <code>0xf162</code> setzen."},
	"nb.hint": {
		LangEN: "Read from the dnsmasq log. The Raspberry Pi bootloader announces itself as <code>PXEClient:…</code>; an ordinary Linux client does not — that is what separates a netboot from a plain address lease. A device that only took an address booted from its own storage; its <code>BOOT_ORDER</code> is probably still at the factory value (<code>0xf641</code>), <code>0xf162</code> enables netboot.",
		LangDE: "Aus dem dnsmasq-Protokoll gelesen. Der RPi-Bootloader meldet sich mit <code>PXEClient:…</code>, ein gewöhnlicher Linux-Client nicht — daran hängt der Unterschied zwischen Netzboot und bloßem Adressbezug. Ein Gerät, das nur eine Adresse bezog, bootete vom eigenen Speicher; sein <code>BOOT_ORDER</code> steht vermutlich noch auf Werk (<code>0xf641</code>), <code>0xf162</code> erlaubt den Netzboot."},

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
	"err.sitegone":     {LangEN: "that site does not exist", LangDE: "Diesen Standort gibt es nicht"},
	"err.sitename":     {LangEN: "a site needs a name", LangDE: "Ein Standort braucht einen Namen"},
	"err.sitenet":      {LangEN: "network must be three octets, e.g. 10.0.0", LangDE: "Netz muss drei Oktette sein, z. B. 10.0.0"},
	"err.sitepool":     {LangEN: "pool range must lie inside 1–254 and ascend", LangDE: "Pool-Bereich muss in 1–254 liegen und aufsteigen"},
	"err.sitehasracks": {LangEN: "site still holds %d BladeRunner(s)", LangDE: "Standort enthält noch %d BladeRunner"},
	"err.sitelast":     {LangEN: "the last site cannot be removed", LangDE: "Der letzte Standort kann nicht entfernt werden"},

	"err.imgurl":  {LangEN: "an http or https address is needed", LangDE: "Es wird eine http- oder https-Adresse gebraucht"},
	"err.imgid":   {LangEN: "the name may not contain spaces or slashes", LangDE: "Der Name darf keine Leerzeichen oder Schrägstriche enthalten"},
	"err.imgbusy": {LangEN: "%s is already being worked on", LangDE: "%s wird gerade schon bearbeitet"},
	"err.imgused": {LangEN: "%d blade(s) are installed from this image", LangDE: "%d Blade(s) sind aus diesem Image installiert"},

	"img.title": {LangEN: "Images", LangDE: "Images"},
	"img.name":  {LangEN: "Image", LangDE: "Image"},
	"img.lead": {
		LangEN: "Which operating systems a blade can be installed from. An image added here is fetched, unpacked, prepared and checksummed on this server — from then on it is installed over the local network, not over the internet link.",
		LangDE: "Aus welchen Betriebssystemen ein Blade installiert werden kann. Ein hier hinzugefügtes Image wird auf diesem Server geholt, entpackt, angepasst und geprüft — installiert wird danach aus dem eigenen Netz, nicht über die Internetleitung."},
	"img.add":       {LangEN: "Add an image", LangDE: "Image hinzufügen"},
	"img.url":       {LangEN: "Download address", LangDE: "Download-Adresse"},
	"img.urlhint":   {LangEN: ".img.xz or .tar.xz — Ubuntu 24.04, DietPi v10 and Debian 13 are recognised by themselves", LangDE: ".img.xz oder .tar.xz — Ubuntu 24.04, DietPi v10 und Debian 13 werden von selbst erkannt"},
	"img.id":        {LangEN: "Name in the catalogue", LangDE: "Name im Katalog"},
	"img.idhint":    {LangEN: "empty: derived from the file name", LangDE: "leer: aus dem Dateinamen abgeleitet"},
	"img.pkgs":      {LangEN: "Install into the image", LangDE: "Ins Image installieren"},
	"img.pkgshint":  {LangEN: "empty: what the recipe says", LangDE: "leer: was das Rezept vorsieht"},
	"img.noprep":    {LangEN: "Take the image as it comes, change nothing", LangDE: "Image unverändert übernehmen"},
	"img.fetch":     {LangEN: "Fetch and prepare", LangDE: "Holen und anpassen"},
	"img.known":     {LangEN: "Recognised sources", LangDE: "Bekannte Quellen"},
	"img.recipe":    {LangEN: "Recipe", LangDE: "Rezept"},
	"img.state":     {LangEN: "State", LangDE: "Zustand"},
	"img.size":      {LangEN: "Size", LangDE: "Größe"},
	"img.inuse":     {LangEN: "in use", LangDE: "in Benutzung"},
	"img.blades":    {LangEN: "blade(s)", LangDE: "Blade(s)"},
	"img.none":      {LangEN: "No image in the catalogue yet.", LangDE: "Noch kein Image im Katalog."},
	"img.st.queued": {LangEN: "waiting", LangDE: "wartet"},
	"img.st.work":   {LangEN: "working", LangDE: "in Arbeit"},
	"img.st.ready":  {LangEN: "ready", LangDE: "bereit"},
	"img.st.error":  {LangEN: "failed", LangDE: "fehlgeschlagen"},
	"img.st.local":  {LangEN: "mirrored", LangDE: "gespiegelt"},
	"img.st.remote": {LangEN: "not mirrored", LangDE: "nicht gespiegelt"},
	"img.queued":    {LangEN: "%s is being fetched — this takes a while, the page keeps up.", LangDE: "%s wird geholt — das dauert, die Seite bleibt dran."},
	"img.removed":   {LangEN: "%s removed", LangDE: "%s entfernt"},
	"img.kernel":    {LangEN: "Kernel", LangDE: "Kernel"},
	"img.k.down":    {LangEN: "Raspberry Pi kernel — overlays work, fan reports", LangDE: "Raspberry-Pi-Kernel — Overlays wirken, Lüfter meldet sich"},
	"img.k.up":      {LangEN: "upstream kernel — no overlays, no fan telemetry", LangDE: "Upstream-Kernel — keine Overlays, keine Lüfterdaten"},
	"img.verified":  {LangEN: "booted on a blade", LangDE: "auf einem Blade gebootet"},
	"img.remove":    {LangEN: "Remove", LangDE: "Entfernen"},
	"img.rmask":     {LangEN: "Remove %s including the mirrored file?", LangDE: "%s samt gespiegelter Datei entfernen?"},

	"enr.title": {LangEN: "Enrollment", LangDE: "Anmeldung"},
	"enr.make":  {LangEN: "Create an enrollment code", LangDE: "Anmeldecode erzeugen"},
	"enr.code":  {LangEN: "Code", LangDE: "Code"},
	"enr.valid": {LangEN: "good once, expires in %s", LangDE: "einmal gültig, läuft in %s ab"},
	"enr.gone":  {LangEN: "no code outstanding", LangDE: "kein Code offen"},
	"enr.has":   {LangEN: "This site already has a token. A new code replaces it — the site that holds the old one stops being able to report.", LangDE: "Dieser Standort hat schon ein Token. Ein neuer Code ersetzt es — der Standort mit dem alten kann dann nichts mehr melden."},
	"enr.run":   {LangEN: "On the site machine:", LangDE: "Auf der Standort-Maschine:"},
	"enr.lead":  {LangEN: "A code the site signs itself in with, instead of a token carried there by hand. Good once, good for an hour.", LangDE: "Ein Code, mit dem sich der Standort selbst anmeldet, statt ein Token von Hand hinzutragen. Einmal gültig, eine Stunde lang."},
	"enr.made":  {LangEN: "Code created — it is shown once", LangDE: "Code erzeugt — er wird einmal angezeigt"},

	"site.prefix":   {LangEN: "Name prefix", LangDE: "Namenskürzel"},
	"site.prefixph": {LangEN: "none", LangDE: "keins"},
	"site.prefixhint": {
		LangEN: "Blades here are called %s. A BladeRunner numbered 1 exists at every site, so without a prefix the first blade of the first unit has the same name in each of them — one name for two machines, and every tool that resolves names picks one of them.",
		LangDE: "Blades hier heißen %s. Einen BladeRunner Nummer 1 gibt es an jedem Standort, also trägt ohne Kürzel das erste Blade der ersten Einheit überall denselben Namen — ein Name für zwei Maschinen, und jedes Werkzeug, das Namen auflöst, greift sich eine davon."},
	"warn.names":  {LangEN: "Names", LangDE: "Namen"},
	"msg.renamed": {LangEN: "%d blade(s) renamed", LangDE: "%d Blade(s) umbenannt"},
	"warn.dupehost": {
		LangEN: "%s is the name of %d blades. Give the sites a name prefix on their own page, or rename the blades by hand: a name that means two machines resolves to whichever answers first.",
		LangDE: "%s ist der Name von %d Blades. Gib den Standorten auf ihrer Seite ein Namenskürzel oder benenne die Blades von Hand um: ein Name für zwei Maschinen löst auf die auf, die zuerst antwortet."},

	"inv.title": {LangEN: "Inventory", LangDE: "Inventar"},
	"inv.lead": {
		LangEN: "What is screwed into the racks, across every site. This is what the blades reported about themselves, not a probe: a blade that has been away for a week still shows what it last said.",
		LangDE: "Was in den Racks steckt, über alle Standorte. Das ist, was die Blades über sich gemeldet haben, keine Abfrage: ein Blade, das seit einer Woche weg ist, zeigt weiterhin, was es zuletzt gesagt hat."},
	"inv.blade":       {LangEN: "Blade", LangDE: "Blade"},
	"inv.where":       {LangEN: "Where", LangDE: "Wo"},
	"inv.board":       {LangEN: "Module", LangDE: "Modul"},
	"inv.ram":         {LangEN: "Memory", LangDE: "Speicher"},
	"inv.cpu":         {LangEN: "Processor", LangDE: "Prozessor"},
	"inv.storage":     {LangEN: "Storage", LangDE: "Datenträger"},
	"inv.running":     {LangEN: "Running", LangDE: "Läuft"},
	"inv.emmc":        {LangEN: "eMMC", LangDE: "eMMC"},
	"inv.nvme":        {LangEN: "NVMe", LangDE: "NVMe"},
	"inv.lite":        {LangEN: "Lite (no eMMC)", LangDE: "Lite (kein eMMC)"},
	"inv.radio":       {LangEN: "wireless", LangDE: "Funk"},
	"inv.firmware":    {LangEN: "Firmware", LangDE: "Firmware"},
	"inv.boot":        {LangEN: "Bootloader", LangDE: "Bootloader"},
	"inv.via.nvme":    {LangEN: "came up from NVMe", LangDE: "kam von NVMe hoch"},
	"inv.via.network": {LangEN: "came up over the network", LangDE: "kam über das Netz hoch"},
	"inv.via.sd":      {LangEN: "came up from SD", LangDE: "kam von SD hoch"},
	"inv.via.usb-msd": {LangEN: "came up from USB", LangDE: "kam von USB hoch"},
	"inv.via.usb-bcm": {LangEN: "came up from USB", LangDE: "kam von USB hoch"},
	"inv.via.rpiboot": {LangEN: "came up over rpiboot", LangDE: "kam über rpiboot hoch"},
	"inv.via.http":    {LangEN: "came up over HTTP", LangDE: "kam über HTTP hoch"},
	"inv.fwhint": {
		LangEN: "The bootloader version and the day it was built come from the device tree, where the firmware writes them; so does how this blade came up, which is worth a second look when a blade you meant to reinstall says it came from its NVMe. The VideoCore line needs vcgencmd, which Debian does not ship.",
		LangDE: "Bootloader-Version und Baudatum stehen im Device Tree, wo die Firmware sie hinterlegt — ebenso, wie dieses Blade hochgekommen ist, was einen zweiten Blick wert ist, wenn ein Blade, das neu installiert werden sollte, von seiner NVMe kam. Die VideoCore-Zeile braucht vcgencmd, das Debian nicht mitliefert."},
	"inv.none":    {LangEN: "This blade has not said what it is yet.", LangDE: "Dieses Blade hat noch nicht gesagt, was es ist."},
	"inv.empty":   {LangEN: "No blade in the inventory yet.", LangDE: "Noch kein Blade im Inventar."},
	"inv.sum":     {LangEN: "Together", LangDE: "Zusammen"},
	"inv.unknown": {LangEN: "%d without hardware data", LangDE: "%d ohne Hardware-Angaben"},
	"inv.revhint": {
		LangEN: "The revision code is the module's true name: the firmware reads it out of the OTP and it carries the board type, the revision, the memory size, the chip and who built it. The device tree model says less.",
		LangDE: "Der Revisionscode ist der eigentliche Name des Moduls: die Firmware liest ihn aus dem OTP, und er trägt Bauform, Revision, Speichergröße, Chip und Hersteller. Das Device-Tree-Modell sagt weniger."},

	"pay.title":    {LangEN: "Netboot payload", LangDE: "Netboot-Payload"},
	"pay.same":     {LangEN: "installer as at the centre", LangDE: "Installer wie in der Zentrale"},
	"pay.differs":  {LangEN: "different installer than the centre", LangDE: "anderer Installer als die Zentrale"},
	"pay.unknown":  {LangEN: "installer unknown", LangDE: "Installer unbekannt"},
	"pay.nocentre": {LangEN: "the centre has no payload", LangDE: "die Zentrale hat kein Payload"},
	"pay.centre":   {LangEN: "Centre", LangDE: "Zentrale"},
	"pay.here":     {LangEN: "This site", LangDE: "Dieser Standort"},
	"pay.none":     {LangEN: "none", LangDE: "keins"},
	"pay.hint": {
		LangEN: "The centre builds the payload and every site fetches it. A site that serves a different one hands a blade a different installer than the one you last built — it will catch up on its next pass, unless it cannot reach the centre.",
		LangDE: "Die Zentrale baut das Payload, jeder Standort holt es sich. Ein Standort mit einem anderen gibt einem Blade einen anderen Installer als den zuletzt gebauten — er zieht beim nächsten Durchlauf nach, sofern er die Zentrale erreicht."},

	"nf.title": {LangEN: "Notification", LangDE: "Benachrichtigung"},
	"nf.lead": {
		LangEN: "Where to say it when a blade goes bad. Nothing is sent until a verdict has held for the hold time — a blade that reboots is briefly offline and briefly warm, and neither is news. What went bad is said once, and what recovered is said once.",
		LangDE: "Wohin gemeldet wird, wenn es einem Blade schlecht geht. Gesendet wird erst, wenn ein Urteil die Haltezeit über bestehen bleibt — ein Blade, das neu startet, ist kurz offline und kurz warm, und beides ist keine Nachricht. Was schlecht wurde, wird einmal gesagt, und was sich erholt hat, auch."},
	"nf.on":       {LangEN: "Send notifications", LangDE: "Benachrichtigungen senden"},
	"nf.host":     {LangEN: "SMTP server", LangDE: "SMTP-Server"},
	"nf.port":     {LangEN: "Port", LangDE: "Port"},
	"nf.sec":      {LangEN: "Security", LangDE: "Verschlüsselung"},
	"nf.starttls": {LangEN: "STARTTLS (587)", LangDE: "STARTTLS (587)"},
	"nf.tls":      {LangEN: "TLS (465)", LangDE: "TLS (465)"},
	"nf.none":     {LangEN: "none", LangDE: "keine"},
	"nf.user":     {LangEN: "User", LangDE: "Benutzer"},
	"nf.pass":     {LangEN: "Password", LangDE: "Passwort"},
	"nf.passkeep": {LangEN: "leave empty to keep", LangDE: "leer lassen, um es zu behalten"},
	"nf.passset":  {LangEN: "a password is stored", LangDE: "ein Passwort ist hinterlegt"},
	"nf.from":     {LangEN: "Sender", LangDE: "Absender"},
	"nf.to":       {LangEN: "Recipient", LangDE: "Empfänger"},
	"nf.tohint":   {LangEN: "several separated by commas", LangDE: "mehrere durch Komma getrennt"},
	"nf.min":      {LangEN: "Send from", LangDE: "Senden ab"},
	"nf.min.warn": {LangEN: "attention", LangDE: "Achtung"},
	"nf.min.crit": {LangEN: "trouble only", LangDE: "nur kritisch"},
	"nf.hold":     {LangEN: "Hold time min", LangDE: "Haltezeit Min"},
	"nf.test":     {LangEN: "Send a test", LangDE: "Test senden"},
	"nf.sent":     {LangEN: "Test sent to %s", LangDE: "Test an %s gesendet"},
	"nf.failed":   {LangEN: "Not sent: %s", LangDE: "Nicht gesendet: %s"},
	"nf.open":     {LangEN: "Currently unwell", LangDE: "Derzeit auffällig"},
	"nf.opennone": {LangEN: "every blade is well", LangDE: "allen Blades geht es gut"},
	"nf.secret": {
		LangEN: "The password is kept in the database and never shown again, and it is not part of what a blade receives — the desired state a blade pulls would carry it to every blade in the rack.",
		LangDE: "Das Passwort liegt in der Datenbank, wird nie wieder angezeigt und gehört nicht zu dem, was ein Blade bekommt — der Sollzustand, den ein Blade abholt, trüge es sonst zu jedem Blade im Rack."},

	"bk.title": {LangEN: "Backup", LangDE: "Sicherung"},
	"bk.lead": {
		LangEN: "A copy of the database, complete and consistent at the moment it was taken, for the backup that carries this machine away. Copying the live file instead would catch it mid-write: SQLite keeps a write-ahead log, and half of a transaction restores as a broken database.",
		LangDE: "Eine Kopie der Datenbank, vollständig und in sich stimmig zum Zeitpunkt der Aufnahme, für die Sicherung, die diese Maschine forträgt. Die laufende Datei direkt zu kopieren erwischt sie mitten im Schreiben: SQLite führt ein Write-Ahead-Log, und eine halbe Transaktion stellt sich als kaputte Datenbank wieder her."},
	"bk.dir":     {LangEN: "Directory", LangDE: "Verzeichnis"},
	"bk.at":      {LangEN: "Daily at", LangDE: "Täglich um"},
	"bk.keep":    {LangEN: "Copies kept", LangDE: "Aufbewahrte Kopien"},
	"bk.last":    {LangEN: "Newest copy", LangDE: "Neueste Kopie"},
	"bk.none":    {LangEN: "none yet", LangDE: "noch keine"},
	"bk.now":     {LangEN: "Back up now", LangDE: "Jetzt sichern"},
	"bk.done":    {LangEN: "%s written (%s)", LangDE: "%s geschrieben (%s)"},
	"bk.off":     {LangEN: "Switched off — the server was started without a backup directory.", LangDE: "Ausgeschaltet — der Server wurde ohne Sicherungsverzeichnis gestartet."},
	"bk.tokens":  {LangEN: "The copies carry every token in the system, so the directory is readable only by the server's own user. Point the outside backup at it; the newest is always sheath-latest.db.", LangDE: "Die Kopien enthalten sämtliche Tokens des Systems; das Verzeichnis ist deshalb nur für den Dienstbenutzer lesbar. Die äußere Sicherung zeigt darauf; die neueste heißt immer sheath-latest.db."},
	"bk.restore": {LangEN: "Restoring: stop the server, put the copy in place of the database, remove any -wal and -shm beside it, start the server.", LangDE: "Wiederherstellen: Dienst anhalten, die Kopie an die Stelle der Datenbank legen, danebenliegende -wal und -shm entfernen, Dienst starten."},

	"set.title": {LangEN: "Settings", LangDE: "Einstellungen"},
	"set.lead": {
		LangEN: "What the agent does on a blade, and how an installation is carried out. These apply to every blade; a BladeRunner or a single blade can be given its own values through the API.",
		LangDE: "Was der Agent auf einem Blade tut, und wie eine Installation abläuft. Das gilt für alle Blades; ein BladeRunner oder ein einzelnes Blade bekommt eigene Werte über die API."},
	"set.agent":     {LangEN: "Agent", LangDE: "Agent"},
	"set.install":   {LangEN: "Installation", LangDE: "Installation"},
	"set.scope":     {LangEN: "applies to every blade", LangDE: "gilt für alle Blades"},
	"set.interval":  {LangEN: "Interval s", LangDE: "Intervall s"},
	"set.jitter":    {LangEN: "Jitter s", LangDE: "Streuung s"},
	"set.allow":     {LangEN: "Allowed commands", LangDE: "Erlaubte Kommandos"},
	"set.allowhint": {LangEN: "empty: all — e.g. identify, reboot", LangDE: "leer: alle — z. B. identify, reboot"},
	"set.rebootcfg": {LangEN: "Restart by itself after a boot configuration change", LangDE: "Nach Änderung der Boot-Konfiguration selbst neu starten"},
	"set.window":    {LangEN: "Only within", LangDE: "Nur zwischen"},
	"set.reboothint": {
		LangEN: "Off by default: a setting the firmware reads is worth nothing until the firmware reads it, but restarting a machine that is doing work is a decision. Where this is on, the blade says it is going before it goes.",
		LangDE: "Standardmäßig aus: eine Einstellung, die nur die Firmware liest, wirkt erst nach einem Neustart — aber eine Maschine unter Last neu zu starten ist eine Entscheidung. Wo es an ist, meldet das Blade vorher, dass es weg ist."},
	"set.target":       {LangEN: "Target device", LangDE: "Zielgerät"},
	"set.after":        {LangEN: "When written", LangDE: "Nach dem Schreiben"},
	"set.after.reboot": {LangEN: "restart", LangDE: "neu starten"},
	"set.after.halt":   {LangEN: "stop and stay", LangDE: "anhalten"},
	"set.after.shell":  {LangEN: "drop to console", LangDE: "Konsole öffnen"},
	"set.rebootwait":   {LangEN: "Wait s", LangDE: "Wartezeit s"},
	"set.needsum":      {LangEN: "Refuse an image without a checksum", LangDE: "Image ohne Prüfsumme ablehnen"},
	"set.nogrow":       {LangEN: "Leave the partition as the image has it", LangDE: "Partition lassen wie im Image"},
	"set.nokeys":       {LangEN: "Do not place root SSH keys", LangDE: "Keine Root-SSH-Schlüssel ablegen"},
	"set.nocloud":      {LangEN: "No cloud-init seed", LangDE: "Kein cloud-init-Seed"},
	"set.noagent":      {LangEN: "Do not install the agent", LangDE: "Agent nicht installieren"},
	"set.seedhint": {
		LangEN: "The seeding steps are on unless switched off here. A blade without keys and without an agent can only be reached the way its image allows.",
		LangDE: "Die Seeding-Schritte sind an, solange sie hier nicht abgeschaltet werden. Ein Blade ohne Schlüssel und ohne Agent ist nur so erreichbar, wie sein Image es zulässt."},
	"err.window": {LangEN: "the window has to read HH:MM-HH:MM", LangDE: "Das Fenster muss HH:MM-HH:MM lauten"},

	"nav.label": {LangEN: "Sections", LangDE: "Bereiche"},
	"nav.map":   {LangEN: "Map", LangDE: "Karte"},
	"map.link":  {LangEN: "Map", LangDE: "Karte"},
	"map.title": {LangEN: "Installation map", LangDE: "Karte der Installation"},
	"map.lead": {
		LangEN: "The central server, the sites hanging off it, and every slot as one square.",
		LangDE: "Der zentrale Server, die Standorte daran, und jeder Slot als ein Quadrat."},
	"map.centre": {LangEN: "central server", LangDE: "Zentrale"},
	"map.counts": {LangEN: "%d BladeRunner(s) · %d blade(s)", LangDE: "%d BladeRunner · %d Blades"},
	"map.legend": {
		LangEN: "solid line: site reporting · dashed: stale or offline",
		LangDE: "durchgezogen: Standort meldet sich · gestrichelt: veraltet oder offline"},
	"map.alt": {
		LangEN: "Diagram of the central server and its sites",
		LangDE: "Schaubild des zentralen Servers und seiner Standorte"},

	"rk.movehint": {
		LangEN: "Moving this BladeRunner to another site renumbers every blade in it — the addresses are derived from the site, and the reservations are rewritten at once.",
		LangDE: "Ein Umzug an einen anderen Standort nummeriert alle Blades darin neu — die Adressen leiten sich vom Standort ab, und die Reservierungen werden sofort neu geschrieben."},
	"site.moved":  {LangEN: "%q moved to %q — new block %s–%s", LangDE: "%q nach %q verschoben — neuer Block %s–%s"},
	"site.assign": {LangEN: "Site", LangDE: "Standort"},

	"img.downstream": {
		LangEN: "Raspberry Pi kernel — device tree overlays apply, fan and LED telemetry work",
		LangDE: "Raspberry-Pi-Kernel — Device-Tree-Overlays greifen, Lüfter- und LED-Telemetrie funktioniert"},
	"img.upstream": {
		LangEN: "upstream kernel — the firmware applies no device tree directive, so no fan or LED telemetry",
		LangDE: "Upstream-Kernel — die Firmware wendet keine Device-Tree-Anweisung an, also keine Lüfter- oder LED-Telemetrie"},
	"img.unknownkernel": {
		LangEN: "kernel flavour not recorded",
		LangDE: "Kernel-Variante nicht hinterlegt"},

	"stock.detail": {LangEN: "Images at this site", LangDE: "Images an diesem Standort"},
	"stock.hint": {
		LangEN: "what this site holds, against what its blades were assigned",
		LangDE: "was dieser Standort hält, gegen das, was seinen Blades zugewiesen ist"},
	"stock.here":     {LangEN: "Here", LangDE: "Hier"},
	"stock.catalog":  {LangEN: "Catalogue", LangDE: "Katalog"},
	"stock.assigned": {LangEN: "Blades assigned", LangDE: "Blades zugewiesen"},
	"stock.waiting":  {LangEN: "%d still to install", LangDE: "%d noch zu installieren"},
	"stock.partial":  {LangEN: "differs from the catalogue", LangDE: "weicht vom Katalog ab"},
	"stock.absent":   {LangEN: "not here", LangDE: "nicht hier"},
	"stock.none": {
		LangEN: "No image is held here and none is assigned to a blade here.",
		LangDE: "Hier liegt kein Image, und keinem Blade hier ist eines zugewiesen."},
	"stock.state.ready":    {LangEN: "ready", LangDE: "vorrätig"},
	"stock.state.fetching": {LangEN: "fetching", LangDE: "wird geholt"},
	"stock.state.error":    {LangEN: "failed", LangDE: "fehlgeschlagen"},
	"pol.title":            {LangEN: "Thresholds at this site", LangDE: "Schwellen an diesem Standort"},
	"pol.hint":             {LangEN: "empty means: as the installation says", LangDE: "leer heißt: wie die Installation es sagt"},
	"pol.socwarn":          {LangEN: "SoC warn °C", LangDE: "SoC Warnung °C"},
	"pol.soccrit":          {LangEN: "SoC critical °C", LangDE: "SoC kritisch °C"},
	"pol.nvmewarn":         {LangEN: "NVMe warn °C", LangDE: "NVMe Warnung °C"},
	"pol.diskwarn":         {LangEN: "Disk warn %", LangDE: "Disk Warnung %"},
	"pol.diskcrit":         {LangEN: "Disk critical %", LangDE: "Disk kritisch %"},
	"pol.offline":          {LangEN: "Offline after min", LangDE: "Offline nach min"},
	"pol.inherit": {
		LangEN: "A blade in a ventilated rack and one in a warm office do not share the temperature at which someone should be woken.",
		LangDE: "Ein Blade im belüfteten Rack und eines im warmen Büro teilen nicht die Temperatur, bei der jemand geweckt werden sollte."},
	"pol.current": {
		LangEN: "In force here: SoC %.0f/%.0f °C · disk %.0f/%.0f %% · offline after %d min.",
		LangDE: "Hier gültig: SoC %.0f/%.0f °C · Disk %.0f/%.0f %% · offline nach %d min."},
	"err.policyorder": {
		LangEN: "the critical threshold has to be above the warning one",
		LangDE: "Die kritische Schwelle muss über der Warnschwelle liegen"},
	"msg.policysaved": {LangEN: "Thresholds saved.", LangDE: "Schwellen gespeichert."},

	"site.edit": {LangEN: "Edit site", LangDE: "Standort bearbeiten"},
	"site.notoken": {
		LangEN: "This site has no token, so no site process can speak for it. Issue one with <code>POST /api/v1/sites/{id}/token</code>.",
		LangDE: "Dieser Standort hat kein Token, also kann kein Standort-Dienst für ihn sprechen. Eines ausstellen mit <code>POST /api/v1/sites/{id}/token</code>."},

	"stock.title":    {LangEN: "Images", LangDE: "Images"},
	"stock.ready":    {LangEN: "%d image(s) ready", LangDE: "%d Image(s) vorrätig"},
	"stock.fetching": {LangEN: "%d of %d ready, %d being fetched", LangDE: "%d von %d vorrätig, %d wird geholt"},
	"stock.bad":      {LangEN: "%d of %d ready, %d failed", LangDE: "%d von %d vorrätig, %d fehlgeschlagen"},

	"site.online":  {LangEN: "online", LangDE: "online"},
	"site.stale":   {LangEN: "stale", LangDE: "veraltet"},
	"site.offline": {LangEN: "offline", LangDE: "offline"},
	"site.never":   {LangEN: "never reported", LangDE: "nie gemeldet"},
	"site.noagent": {LangEN: "no site process", LangDE: "kein Standort-Dienst"},

	"site.title":     {LangEN: "Sites", LangDE: "Standorte"},
	"site.one":       {LangEN: "Site", LangDE: "Standort"},
	"site.count":     {LangEN: "%d site(s)", LangDE: "%d Standort(e)"},
	"site.name":      {LangEN: "Name", LangDE: "Name"},
	"site.net":       {LangEN: "Network", LangDE: "Netz"},
	"site.pool":      {LangEN: "Pool", LangDE: "Pool"},
	"site.poolrange": {LangEN: "Pool from – to", LangDE: "Pool von – bis"},
	"site.poolfrom":  {LangEN: "Pool from", LangDE: "Pool ab"},
	"site.poolto":    {LangEN: "Pool to", LangDE: "Pool bis"},
	"site.here":      {LangEN: "here", LangDE: "hier"},
	"site.example":   {LangEN: "e.g. Hamburg office", LangDE: "z. B. Büro Hamburg"},
	"site.norack":    {LangEN: "No BladeRunner at this site yet.", LangDE: "Noch kein BladeRunner an diesem Standort."},
	"site.hint": {
		LangEN: "A site is one broadcast domain. Blades at another site are addressed from there — this server writes DHCP reservations only for the site it stands in.",
		LangDE: "Ein Standort ist eine Broadcast-Domäne. Blades an einem anderen Standort werden von dort adressiert — dieser Server schreibt DHCP-Reservierungen nur für den Standort, in dem er steht."},
	"site.movehint": {
		LangEN: "Moving the network moves every blade in this site; the reservations are rewritten at once.",
		LangDE: "Ein anderes Netz verschiebt alle Blades dieses Standorts; die Reservierungen werden sofort neu geschrieben."},
	"msg.sitecreated":   {LangEN: "Site %q created (%s.0/24).", LangDE: "Standort %q angelegt (%s.0/24)."},
	"msg.sitesaved":     {LangEN: "Site %q saved.", LangDE: "Standort %q gespeichert."},
	"msg.siteremoved":   {LangEN: "Site removed.", LangDE: "Standort entfernt."},
	"msg.dhcprewritten": {LangEN: "%d reservations rewritten", LangDE: "%d Reservierungen neu geschrieben"},

	"act.wipe":   {LangEN: "Erase NVMe — type the name", LangDE: "NVMe löschen — Namen eintippen"},
	"act.wipego": {LangEN: "Erase and remove", LangDE: "Löschen und entfernen"},
	"act.wipehint": {
		LangEN: "The blade netboots once more, the disk is erased there, and it leaves its slot only when that is done. It then stops, ready to be pulled.",
		LangDE: "Das Blade netbootet noch einmal, dort wird die Platte gelöscht, und es verlässt seinen Slot erst, wenn das erledigt ist. Danach hält es an und kann gezogen werden."},
	"err.wipeoff": {
		LangEN: "erasing is switched off at this site",
		LangDE: "Löschen ist an diesem Standort abgeschaltet"},
	"err.wipeconfirm": {
		LangEN: "type %q to confirm the erase",
		LangDE: "Zum Bestätigen %q eintippen"},
	"msg.wipearmed": {
		LangEN: "%s will erase its NVMe at the next netboot — it is restarting now.",
		LangDE: "%s löscht seine NVMe beim nächsten Netzboot — es startet gerade neu."},

	"act.identifyoff":    {LangEN: "Stop identify", LangDE: "Identify beenden"},
	"act.identifyofftip": {LangEN: "ends the identify state, as the blade's own button would", LangDE: "beendet den Identify-Zustand, wie es der Knopf am Blade täte"},
	"act.stealthon":      {LangEN: "Stealth on", LangDE: "Stealth an"},
	"act.stealthoff":     {LangEN: "Stealth off", LangDE: "Stealth aus"},
	"act.stealthtip":     {LangEN: "turns every LED on this blade off", LangDE: "schaltet alle LEDs dieses Blades aus"},

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
	"st.identify":     {LangEN: "identifying", LangDE: "Identify aktiv"},
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
