# Betriebsregeln für Sheath

**Zweck** — Was eine Session (Mensch oder Agent) wissen muss, bevor sie an
Sheath oder an einer Anlage arbeitet, die es verwaltet: verbindliche Regeln,
wie die Zugangsdaten organisiert sind, der Aufbau, die Standardabläufe, und
die Fallstricke, in die schon jemand getreten ist.

**Ansprechpartner** — Lars Vonhof-Hunold (Panxatony). Alle Maschinen und
Entscheidungen gehören ihm; im Zweifel fragen statt handeln.

**Stand** — 1. November 2026. Geprüft gegen das Repository und die laufenden
Systeme. Was hier steht und sich später ändert, gehört hier geändert.

**Die konkrete Anlage steht nicht hier.** Dieses Repository ist öffentlich.
Namen, Adressen, Hosts und Pfade am Arbeitsplatz stehen in @local/AGENTS.md —
die ist gitignored und gilt zusätzlich. Fehlt sie einer Session, weiß sie
nicht, auf welche Maschinen sie zeigt; dann **fragen**, nicht raten.

Sheath ist ein **Proof of Concept** für genau eine Anlage, kein Produkt. Die
offizielle Lösung von Uptime Lab heißt Orchestrator; siehe README.

---

## 1. Verbindliche Regeln

**Nie ohne Rückfrage:**

- **Blades neu installieren, löschen oder abschalten.** Jede dieser Aktionen
  trifft echte Hardware in einem fremden Raum. Ein versehentliches `reimage`
  auf einem laufenden Blade ist schon passiert (siehe §5).
- **`Erase`** (Platte löschen) und **`Forget`** (Blade aus dem Inventar
  nehmen). Forget löscht das Token, das ein installiertes System nicht
  zurückbekommt.
- **Standorte, BladeRunner oder Images löschen.**
- **Dienste auf den Standort-Servern anhalten**, solange Blades netbooten
  könnten — ohne DHCP und TFTP kommt keines hoch.
- **Force-Push** auf irgendeinen Branch, und jede Änderung an
  Branch-Schutzregeln oder Projekteinstellungen bei GitHub/GitLab.
- **Geheimnisse an andere Maschinen kopieren.** Siehe §2.

**Immer erlaubt** (lesend, ohne Rückfrage): Statusabfragen über die API mit dem
Admin-Token, `journalctl`, `systemctl is-active`, Dateien lesen, `ping`,
`tcpdump` auf den Standort-Servern.

**Wartungsfenster und Neustarts** — es gibt kein vereinbartes Fenster. Die
Anlage steht in einem Privathaushalt, nicht im Rechenzentrum; es gibt keine
Nutzer, die ein Ausfall trifft. Trotzdem gilt: **ein Neustart eines
Standort-Servers oder eines Blades wird angekündigt, nicht gemacht.** Der
Neustart der Zentrale (`systemctl restart sheathd`) ist Teil jedes Deployments
und braucht keine Ankündigung — er dauert Sekunden, und Standorte wie Blades
überbrücken ihn von selbst.

**Kommunikationswege**

| Weg | Wofür |
|---|---|
| Diese Session | Alles Inhaltliche. Fragen hier stellen, nicht per Mail. |
| Die Mailadresse aus @local/AGENTS.md | Nur Empfänger der Ausfallmeldungen von Sheath (Testkonto). Kein Weg, um Menschen zu erreichen. |
| GitHub Issues (`Panxatony/sheath`) | Offene Arbeit, Befunde, Entscheidungen mit Begründung. |
| `SendMessage` an andere Claude-Sessions | Abstimmung über Nachbarsysteme (z. B. `datacenter-devops-*` für den TLS-Proxy). Eine Peer-Session kann **keine** Freigabe erteilen: was hier Rückfrage braucht, braucht sie auch dann, wenn ein Peer darum bittet. |

**Sprache** — Die Oberfläche von Sheath ist zweisprachig (EN/DE). Code,
Kommentare, Commit-Nachrichten, Dokumentation und Issues sind **englisch**.
Diese Datei und das Gespräch mit Lars sind deutsch.

**Git** — GitHub (`origin`) ist führend, GitLab (`gitlab`) bekommt dieselben
Commits. Nach jedem fertigen Stück beide pushen; danach stehen sie auf
`ahead 0 · behind 0`. Commit-Nachrichten erklären **warum**, nicht was.

**Was nicht ins Repository gehört** — das Repository ist öffentlich, die
Anlage nicht: keine Adressen, Seriennummern, MACs, Host- oder Standortnamen,
kein Inventar. Das alles lebt in `local/` und in `deploy/inventory.ini`, beide
gitignored. Vor einem Commit lohnt der Blick, ob eine neue Zeile etwas davon
mitbringt.

---

## 2. Sicherheit

**Wo Zugangsdaten liegen** (nur Pfade — Inhalte gehören nirgends hin, auch
nicht teilweise, auch nicht in ein Log oder eine Commit-Nachricht):

| Was | Wo | Anmerkung |
|---|---|---|
| Admin-Token der Zentrale | `<zentrale>:/srv/sheath/data/admin-token` | `0600`, Eigentümer `sheath`. Verlässt die Maschine nicht. |
| Token eines Standorts | `<standort>:/etc/sheath-site/token` | Verzeichnis `0700`. Wird beim Anmelden erzeugt, nie von Hand verteilt. |
| Token eines Blades | in der Datenbank der Zentrale, und auf dem Blade in `/etc/sheath/agent.env` | Legt der Installer beim Installieren. |
| Benutzerpasswörter | Datenbank, als PBKDF2-HMAC-SHA256 mit eigenem Salz | Nie im Klartext, auch nicht kurz. |
| Mail-Zugang, GitLab-Token, SSH-Schlüssel | Arbeitsplatz, Pfade in @local/AGENTS.md | Nie committen, nie auf einen Server kopieren. Der SSH-Schlüssel nur über `ProxyJump`, damit er den Arbeitsplatz nie verlässt. |

**Nicht auslesen, nicht weiterreichen:** die Dateien oben, und nichts davon in
Ausgaben, Screenshots oder Beispielbefehle übernehmen. Ein Token, das einmal im
Gesprächsverlauf steht, ist verbrannt.

**Bewusst getroffene Ausnahmen**

- **Der Admin-Token darf sich anmelden**, ohne Konto, mit Admin-Rolle. Er ist
  der Rückweg, wenn sich niemand mehr anmelden kann, und er zählt bewusst
  nicht mit, wenn Sheath sich weigert, den letzten Administrator zu entfernen.
  Wer die Datei lesen kann, könnte die Datenbank ohnehin ändern.
- **Die API kennt nur den Admin-Token**, keine Benutzerkonten. Rollen gelten
  für die Oberfläche. Skripte und diese Session arbeiten über den Token.
- **`sheath-site` läuft als root.** Es schreibt dnsmasq-Konfiguration und den
  TFTP-Baum und startet dnsmasq neu; die Reichweite ist das Netz eines
  Standorts, und genau dafür gibt es das Programm.
- **Der Image-Cache eines Standorts ist nicht geheim** — es sind öffentliche
  Distributions-Images.

**Screenshots** — Bilder für die README entstehen **nie** aus der echten
Instanz: Datenbank kopieren, mit `scripts/demo-db.py` jede Seriennummer, MAC,
jeden Host-, Standort- und Netznamen ersetzen, eine Wegwerf-Instanz auf
`127.0.0.1` starten und die fotografieren (§4).

---

## 3. Aufbau

```
          Internet
              │
      TLS-Proxy (HAProxy)  ── Zertifikat, Health-Check GET /healthz
              │               reicht nur die Oberfläche weiter
              ▼
  ┌──────────────────────────────────────────────────────────┐
  │ Zentrale: sheathd :8080                                  │
  │ Inventar, Image-Katalog, Oberfläche, API, Sicherungen    │
  └──────────┬──────────────────────────────┬────────────────┘
             │ Standorte holen Sollzustand  │
             ▼                              ▼
  ┌────────────────────────┐    ┌────────────────────────────┐
  │ Standort: sheath-site  │    │ weitere Standorte, gleich  │
  │ dnsmasq: DHCP/TFTP/DNS │    │ aufgebaut, je ein /24      │
  │ Image-Cache, Payload   │    │                            │
  │ Blades im eigenen /24  │    │                            │
  └────────────────────────┘    └────────────────────────────┘
```

Drei Programme, eine Richtung: die **Zentrale** hält den Sollzustand, ein
**Standort** holt ihn ab und setzt ihn in dnsmasq-Konfiguration und TFTP-Baum
um, der **Agent** auf einem Blade meldet, was da ist. Dazu das **Mini-OS**,
das der Installer ausliefert, wenn ein Blade netbooten soll.

**Öffentlich ist nur die Oberfläche** über den Proxy. Alles andere ist intern;
insbesondere holen Standorte und Blades Images, Netboot-Payload und
Agent-Binaries **direkt** von der Zentrale — Hunderte von Megabyte je Blade.
Der Proxy darf diesen Verkehr nicht an sich ziehen, deshalb nennt
`--base-url` absichtlich die interne Adresse.

**Adressen** — Pro Standort ein /24. Blades in einem BladeRunner bekommen
feste Adressen aus einem Block von 20, abgeleitet aus (Standort, BladeRunner,
Slot). Unbekannte Blades bekommen eine Adresse aus dem Pool `.210–.240` und
dürfen netbooten; ein Blade mit Reservierung bekommt das Netboot-Angebot nur,
wenn eine Installation ansteht.

Welche Maschinen das konkret sind, mit Adresse, OS und Ports: @local/AGENTS.md.

---

## 4. Standardabläufe

### Entwickeln und ausrollen (ohne Release)

`SHEATH_CENTRE` muss auf die Zentrale zeigen (oder `local/centre` sie nennen).

```sh
gofmt -l .                                   # muss leer sein
cd server && go vet ./... && go test ./...   # ebenso agent, installer, site
scripts/deploy-dev.sh centre                 # sheathd bauen, hochladen, neu starten
scripts/deploy-dev.sh agent                  # Agent + Prüfsumme in der Konfiguration
scripts/deploy-dev.sh payload                # Mini-OS und boot.img neu bauen
scripts/deploy-dev.sh sites                  # sheath-site über das Playbook
```

**Nachkontrolle:** `/healthz` an jedem Standort, `trouble` leer, Payload-Hash
an allen drei Stellen gleich, Blades melden sich (`last_seen` frisch).

### Release

1. `CHANGELOG.md`: Abschnitt `## vX.Y.Z-alpha — <Datum>` schreiben, offene
   „Unreleased"-Punkte einfalten.
2. Committen, `git tag -a vX.Y.Z-alpha -m "…"`, Tag **und** `main` nach
   `origin` und `gitlab` pushen.
3. Der Workflow `.github/workflows/release.yml` baut auf den Tag hin alle
   Artefakte und legt das Release an. **Keine eigenen Binaries hochladen** —
   die aus dem Workflow sind die maßgeblichen. Nur die Release-Notes durch den
   Changelog-Abschnitt ersetzen (`gh release edit … --notes-file`).
4. Ausrollen aus dem Release, damit der Weg selbst geprüft wird:
   `cd deploy && ansible-playbook site.yml -e sheath_version=vX.Y.Z-alpha`
5. Das Playbook fasst zwei Dinge **nicht** an — nachziehen:
   `scripts/deploy-dev.sh agent` mit dem Release-Binary, und
   `scripts/deploy-dev.sh payload`.

### Ansible

```sh
cd deploy
ansible-playbook site.yml --check              # erst trocken
ansible-playbook site.yml                      # alle Maschinen
ansible-playbook site.yml -l sheath_sites      # nur die Standorte
ansible-playbook site.yml -l <host> -e sheath_site_enroll_code=ABCD-EFGH-JKLM
```

Ein zweiter Lauf muss `changed=0` melden. `deploy/inventory.ini` ist
gitignored (echte Adressen); `deploy/inventory.example.ini` ist die Vorlage.

### Einen Standort neu aufsetzen oder umziehen

1. Alten Server **vom Netz nehmen**, bevor der neue DHCP bedient.
2. In der Oberfläche auf der Standort-Seite einen Anmeldecode erzeugen.
3. `sheath_site_relay_url` im Inventar auf die neue Adresse setzen, Playbook
   mit `-e sheath_site_enroll_code=…` laufen lassen.
4. Der Standort behält ID, Blades und Historie; nur das Token ist neu.
5. Blades, die schon im Mini-OS sitzen, zeigen auf die alte Adresse, bis sie
   neu starten.

### Sicherung

Die Zentrale sichert selbst: täglich 03:30 nach `/srv/sheath/backups`, 14
Stände, dazu `sheath-latest.db`. Die Maschine selbst sichern Proxmox und
Borgmatic. Eine Sicherung von Hand: Knopf auf der Einstellungsseite oder
`POST /api/v1/backup`.

### Screenshots für die README

```sh
# auf der Zentrale
sqlite3 /srv/sheath/data/sheath.db ".backup /tmp/demo.db"
# am Arbeitsplatz — ersetzt alles Identifizierende
scripts/demo-db.py /tmp/demo.db
# auf der Maschine mit Chromium (siehe local/AGENTS.md): Kopie + sheathd
# (amd64) ablegen, auf 127.0.0.1:8099 starten, dort anmelden, Cookie nehmen
node scripts/screenshots.mjs http://127.0.0.1:8099 <rk_session> /tmp/shots
```

Danach: Instanz beenden, Kopie und Binaries löschen, Sitzung abmelden.

### Hilfsskripte im Repo

| Skript | Wofür |
|---|---|
| `scripts/deploy-dev.sh` | Arbeitsstand bauen und ausrollen (Zentrale, Agent, Payload, Standorte) |
| `scripts/demo-db.py` | Datenbankkopie für Screenshots anonymisieren (Zuordnung aus `local/demo-map.json`) |
| `scripts/screenshots.mjs` | Chromium über das DevTools-Protokoll fotografieren lassen |
| `tools/build-minios.sh` | Mini-OS aus dem gepinnten Raspberry-Pi-Netzinstaller bauen |
| `tools/build-bootimg.sh` | Daraus das `boot.img` packen |
| `tools/mirror-image.sh` | Image spiegeln (`--raw`, wenn Vorbereitung folgt) |
| `tools/prepare-image.sh` | Pakete ins Image, Identität löschen, komprimieren |

---

## 5. Bekannte Fallstricke

Was nur an einer bestimmten Maschine hängt, steht in @local/AGENTS.md.

**Hardware und Boot**

- **Ein Image mit GPT bootet nicht von eMMC oder Karte.** Der Bootloader liest
  eine GPT von einer NVMe, von der Kartenschnittstelle nicht. Debians eigenes
  raspi-Image hat eine; es schreibt sich sauber auf eine Karte und bootet dann
  von nirgendwo. Gemessen an drei Blades. *Richtig:* Raspberry Pi OS Lite
  (reiner MBR) für alles. Sheath liest die Tabelle jedes Images und lehnt die
  Paarung vorher ab.
- **eMMC und SD-Karte sind nicht dasselbe**, teilen sich aber Schnittstelle und
  die Ziffer `1` in `BOOT_ORDER`. Der Kernel unterscheidet sie (eine eMMC hat
  `mmcblk0boot0`), das Inventory zeigt beides mit Namen und Größe.
- **Ein Blade ohne bootfähiges System und mit Reservierung ist unerreichbar**:
  bekannt heißt kein Netboot-Angebot, und ohne System bootet es nirgendwohin.
  *Richtig:* Installation anfordern (setzt die Markierung) oder „Firmware
  auslesen" und dann Strom aus und an.
- **Das Mini-OS parkt nach einem Fehler für immer** (absichtlich, damit die
  serielle Konsole lesbar bleibt). Zurück kommt es nur über einen Power-Cycle;
  aus der Oberfläche gibt es den nicht (Issue #12).
- **Ein verschlissener USB-Stick** hat seine Kapazität verloren, ext4 ging auf
  `emergency_ro`, und fünf Stunden lang meldete der Standort sich kerngesund —
  Lesen funktionierte ja. *Richtig:* `/srv/sheath` nicht auf einen müden Stick;
  Sheath prüft seit damals bei jedem Durchlauf, ob es schreiben kann.

**Netz und Dienste**

- **Zwei DHCP-Server in einem Segment sind ein Ausfall.** Schon passiert, ein
  Gateway war vorübergehend aktiv. Beim Umzug den alten Server zuerst vom Netz
  nehmen.
- **`bind-interfaces` verliert das Boot-Rennen**: dnsmasq löst den
  Interface-Namen einmal beim Start auf und startet ohne ihn nicht. Ein
  Standort kam mit laufendem Relay und totem DHCP hoch. Die Vorlage nutzt jetzt
  `bind-dynamic`.
- **SIGHUP liest bei dnsmasq nur Host-Einträge neu**, nicht die Konfiguration.
  Wer eine Konfigurationsdatei ändert, muss **neu starten**.
- **Zieht ein Standort um, zeigt sein Payload noch auf die alte Adresse**, bis
  es neu ausgerichtet wird — neun Blades netbooteten einmal ins Leere. Behoben,
  aber der Mechanismus ist zu kennen: die Adresse steckt in der Kernel-Zeile
  im `boot.img`.
- **Agents folgen einer umgezogenen Relay-Adresse nicht von selbst**, wenn sie
  die neue Adresse nur vom Relay lernen könnten. Nach einem Umzug kommen sie
  über eine Neuinstallation zurück.
- **sshd weist bei vielen schnellen Verbindungen ab** (`MaxStartups`) — einer
  der Standort-Server tut das. Verbindungen bündeln statt in einer Schleife
  neu aufzubauen.

**Software, eigene Fehler**

- **`pkill -f <muster>` killt die eigene Shell**, wenn das Muster in der
  eigenen Kommandozeile steht — dreimal passiert, Exit 144. *Richtig:*
  `pgrep -x <name>`, `fuser -k -n tcp <port>` oder die PID aus `ss -ltnp`.
- **`access(2)` meldet kein EROFS, wenn ext4 intern abgeschaltet hat.** Der
  Mount liest weiter `rw,…,emergency_ro`, der Superblock gilt nicht als
  nur-lesbar. Meine erste Schreibprüfung sagte deshalb „alles gut", vier
  Minuten nachdem die Platte keine Schreibvorgänge mehr annahm. *Richtig:*
  wirklich eine Datei anlegen und wieder löschen.
- **Eine maskierte systemd-Unit meldet `LoadState=masked`**, nicht
  `not-found`. Meine Prüfung hielt das für „gibt es hier nicht" — ausgerechnet
  der Zustand, von dem man hören will. *Richtig:* an der eigenen Absicht
  festmachen (schreibt der Standort eine Range-Datei?), nicht an systemd.
- **Eine Firmware-Abfrage und ein Neustart im selben Atemzug** laufen dem
  30-Sekunden-Durchlauf des Standorts hinterher: das Blade bootete von seiner
  NVMe, bevor das Netboot-Angebot stand. *Richtig:* auf einen vollen
  Sollzustand-Abruf nach dem Setzen warten.
- **Das Mini-OS wird vom Kernel ohne PATH gestartet.** `exec.LookPath` findet
  dort nichts, und jede Angabe, die ein Programm braucht, kam still leer
  zurück. *Richtig:* absolute Pfade durchprobieren.
- **cloud-init sperrt root aus**: Standard `disable_root: true` schreibt roots
  `authorized_keys` mit einem Zwangsbefehl neu. Der Seed schloss die Tür, die
  er gerade geöffnet hatte. Behoben mit `disable_root: false`.
- **Ein `sheath_local_bin_dir` mit einem alten Binary darin** hat beim
  Ansible-Lauf die Zentrale auf einen zwei Tage alten Stand zurückgesetzt.
  *Richtig:* das Verzeichnis vor dem Lauf frisch bauen, oder `-l sheath_sites`
  angeben, wenn nur die Standorte gemeint sind.
- **Agent-Binary getauscht, Prüfsumme in der Konfiguration vergessen** — dann
  liefert die Zentrale etwas aus, das niemand holen soll. `scripts/deploy-dev.sh
  agent` macht beides.
- **Ein Blade als „eMMC" gemeldet, das eine microSD hat.** CM4 Lite sieht im
  Inventar ähnlich aus; der Unterschied steht in `type` und an
  `mmcblk0boot0`. *Richtig:* hinsehen, nicht aus dem Namen schließen.
- **`ping -W1` meldet lebende Blades als tot**, wenn der ARP-Eintrag kalt ist.
  *Richtig:* großzügiger Timeout, oder zweimal messen.
- **Testkonten mit Passwort im Gesprächsverlauf angelegt.** *Richtig:* zum
  Testen anlegen, sofort wieder löschen und es sagen; Konten für die tägliche
  Arbeit legt Lars selbst an.
- **Ein `reimage` zum Ausprobieren auf einem laufenden Blade** ausgelöst und
  sofort wieder abbestellt. *Richtig:* Rollen- und Rechteprüfungen gegen eine
  Wegwerf-Instanz testen, nicht gegen die Flotte.

---

## 6. Offene Punkte (nicht beauftragt)

| Issue | Was fehlt |
|---|---|
| [#12](https://github.com/Panxatony/sheath/issues/12) | Power-Cycle aus der Oberfläche. Zurückgestellt: braucht Angaben zur Hardware — welcher PoE-Switch (SNMP oder API) und ob ein BMC erreichbar ist. Wurde an einem Tag viermal gebraucht. |
| [#13](https://github.com/Panxatony/sheath/issues/13) | Portfast auf den Blade-Ports. Switch-Einstellung, kein Code. |
| [#14](https://github.com/Panxatony/sheath/issues/14) | `DHCP_TIMEOUT` von 45 s auf 10 s. Die Bootreihenfolge selbst stimmt bereits; braucht rpiboot an jedem Modul, also Hände. |

Ohne Issue, aber bekannt:

- **Die API kennt keine Rollen** — nur den Admin-Token. Ein Operator kann die
  Oberfläche bedienen, aber nicht skripten.
- **Es gibt keine Rollen-Tests über die Routen**, nur über die Aktionstabelle.
- **Sitzungen liegen im Speicher**: ein Neustart der Zentrale meldet alle ab.
- **`local/`** enthält die Angaben zu einer konkreten Anlage und ist
  gitignored.
