# Changelog

Notable changes to Rookery. Newest first. Dates are the day the change landed
on `main`.

## Unreleased

### Added
- A page per site at `/sites/{id}`: what stands in it, how it is doing, and
  its images in full — which image, what state, how many bytes are here
  against how many the catalogue has, and how many blades at that site are
  waiting for it. An image assigned to a blade here but not yet fetched is
  listed too, because that row is the one that explains a waiting install.
- Sites report what they actually hold, per image, with state (absent,
  fetching, ready, error). The overview and the map show it in one line; the
  site page shows the detail.
- A netboot session records which site saw it. Two sites may hand out the same
  address, so a session without its site is one nobody can place.

### Fixed
- The site box on the map ran out of room: the text overflowed on the right
  and the state dot sat inside the word. Wider box, two lines, and the dot
  beside the text rather than in it.

### Added
- A map at `/map`: the central server, the sites hanging off it, and every
  slot as one square in the colour it has on its BladeRunner page. The line to
  a site carries that link's state — solid while the site reports, dashed once
  it goes quiet — because with several sites the interesting failure stops
  being a blade and becomes a stretch of network. Drawn server-side as plain
  SVG from the theme's own tokens; no library, and it follows dark mode like
  everything else.
- A BladeRunner can be assigned to a site when it is created, and moved to
  another one afterwards. Moving renumbers every blade in it — the addresses
  are derived from the site — and rewrites the reservations at once, which the
  form says before you do it.

### Fixed
- The site choice only appeared once a second site existed, which left no way
  to see where a new BladeRunner was going.

### Added
- The site relays what blades ask for, and answers it alone when the centre is
  unreachable. Configuration and provisioning come out of the state it holds;
  images are served from its own cache; reports a blade makes are buffered and
  handed over in order once someone is listening again. A command it cannot
  invent — a command is something a person asked for — so an offline site
  returns none rather than guessing.
- The desired state carries, for the site's own blades, what those blades
  would be told: their merged configuration and their token. That is the
  trade-off the site design states openly — end-to-end encryption would cost
  exactly the offline capability this exists for.
- The overview says how each site is doing: online, stale, offline, or "no
  site process" for a site that was never given a token — which is a state,
  not a fault.

### Fixed
- The site edit overlay was unusable: the panel lays its forms out as a row,
  which suits a single field and turns five into a line of thumbnails.

### Added
- `rookery-site`, the network presence of one site, as its own program. It
  holds no decisions — which image a blade gets and whether it may netboot is
  decided centrally — but it owns the wire: the DHCP reservations, the netboot
  switch per blade, the image cache, and the boot payload in the TFTP root.
  It watches the dnsmasq log and reports what it saw; observations are
  buffered when the centre is unreachable, and the last desired state is kept
  on disk so a site keeps working through an outage.
- The site interface on the server: `GET /api/v1/site/{id}/desired` with an
  ETag, `POST /api/v1/site/{id}/events` (batched) and
  `POST /api/v1/site/{id}/status`, authenticated by the site's own token —
  a site may act for itself and for nothing else. `POST /api/v1/sites/{id}/token`
  issues or rotates that token.
- `GET /boot/` serves the netboot payload, so a site can offer the same
  installer without holding any build tooling.

### Changed
- `-local-dhcp=false` hands the wire to a site process: the server then
  neither writes reservations nor tails the dnsmasq log. Two programs owning
  one directory would mean the loser is whoever wrote last.

### Added
- Sites are complete as a model and visible as a thing. The overview groups
  BladeRunners under their site with that site's network and pool; a site can
  be created, renamed, moved to another network or removed from the interface
  and the API (`POST/PUT/DELETE /api/v1/sites`); the BladeRunner page names
  the site it stands in; and creating a BladeRunner asks which site it belongs
  to once there is more than one.

### Changed
- A blade's address is derived from the site of its BladeRunner, everywhere —
  not only in `bladeIP` but in the blade decoration, the BladeRunner view and
  the messages. What remains global is what legitimately means "here": the
  local site's pool and the address the blades are told to reach.
- DHCP reservations are written for the local site only, and the sync reports
  how many blades it skipped for belonging elsewhere. This server serves one
  broadcast domain; a reservation for another site is one nobody can hand out.
- Moving a site's network rewrites the reservations in the same breath — the
  addresses are derived, so they move with it.

### Fixed
- An address block was unique across all sites, so the second site could not
  have a `.100` block because the first one already did. Uniqueness is now per
  site, migrated by rebuilding the table.

### Added
- The BladeRunner page shows what the compute-blade-agent knows. Three levels:
  the hardware of one slot in its action overlay (SoC, airflow, fan speed and
  target, fan unit type, module, blade state, stealth); the enclosure as a
  whole in the header (hottest slot, spread of temperatures and fan speeds,
  how many slots sit on a smart fan unit) — a BladeRunner shares its air, and
  the spread says more than any single reading; and the last 48 hours as a
  sparkline per slot.
- A `samples` table keeps one measurement per blade every five minutes for two
  days — about six hundred rows per blade. Written when a blade reports,
  pruned in the same moment, drawn as a bare SVG polyline with no library and
  no script.

### Added
- The agent reports what it changed. Until now the only record of a blade
  being reconfigured — or of the attempt failing — sat in the journal of that
  blade, which is exactly the place you cannot reach when the change that
  failed was the one that opens the door. Changes now travel with the status
  report and land in the activity log, failures as warnings.
- The installer grows the last partition to the end of the disk after writing.
  An image is built for the smallest disk it must fit; a 500 GB NVMe was
  left holding a 3.5 GB root. Debian grows the filesystem into it via
  `x-systemd.growfs`, Ubuntu via cloud-init.
- `boot_config` knows where Debian keeps boot settings. Debian's `config.txt`
  is generated by `raspi-firmware` and says so; the setting now also goes to
  `/etc/default/raspi-firmware-custom`, where it survives a kernel update.
- `tools/prepare-image.sh` puts a unit into the image that regenerates SSH
  host keys when they are missing, and makes sure `ssh.service` is enabled.
  Removing the image's host keys — which must happen, or every blade presents
  the same identity — otherwise leaves sshd refusing to start.

### Added
- Activity log on the BladeRunner page: what the blades in this enclosure have
  been doing, newest first, with slot, name and severity.
- The agent manages `config.txt`. Settings the firmware reads before Linux
  exists — `dtoverlay=uart5` for the smart fan unit, for instance — can now be
  rolled out centrally. It keeps a block of its own at the end of the file,
  leaves a setting that already stands elsewhere alone, and reports
  `reboot_required` until the blade has restarted. The interface shows that as
  "restart pending".
- The agent installs plain binaries from the Rookery server (`binaries` in the
  desired state), checked against a sha256 and only when the file on disk is
  not already the wanted one. That is how compute-blade-agent now reaches a
  blade: binary, config and unit come from the desired state, so fan and LED
  work without anyone logging in.
- `tools/build-bootimg.sh` builds installer, ramdisk and boot.img in one strict
  run and only publishes into the TFTP root when everything succeeded.
- `tools/prepare-image.sh` customises a mirrored image before any blade sees
  it: installs packages into it through a chroot (the server is arm64, so no
  emulation is needed), clears the machine id — systemd derives the DHCP
  identity from it, and every blade from one image would otherwise fight over
  the same lease — and drops the image's SSH host keys. The Debian raspi image
  ships neither openssh-server nor cloud-init; prepared this way it arrives on
  the blade with a door already in it.
- `tools/mirror-image.sh` mirrors an image locally, unpacking a `.tar.xz` —
  the Debian cloud images ship that way — and entering it in the catalogue.

### Changed
- Rack URLs became BladeRunner URLs: `/bladerunners/{id}`, and the API
  accordingly. The old paths answer with a permanent redirect.
- Provisioning no longer writes an event per percent. Live progress stays in
  the netboot session; the log keeps the phase changes and every 25 %.
- The installer keeps asking after "no install requested" instead of stopping.
  Requesting the installation afterwards no longer needs a power cycle.

### Fixed
- The overview threw the reader back to the start page: `<meta
  http-equiv="refresh">` kept its own navigation pending, and a click landing
  in the same moment lost the race. It is now a script timer that a click
  cancels.
- Actions on a blade without a slot returned to the overview instead of the
  page they were triggered from.
- Zero fan RPM is only critical on a smart fan unit. A standard unit has no
  tacho to report from, so 0 means "not measurable" there — and a healthy
  blade was being painted red for it.
- The installer sets its clock from the server before downloading. The mini OS
  has no RTC, so it starts at 1970 — and every valid TLS certificate is then
  "not yet valid", which failed the Debian download with what looked like a
  certificate problem.

### Changed
- The working language of the repository is English: code comments, log output,
  API error messages, event log entries, documentation and the issue script.
  The German user interface stays — it is a product feature and keeps working
  through the i18n catalogue.
- Health reasons ("no heartbeat", "fan stopped", "SoC 84 °C") moved into the
  i18n catalogue as keyed messages, so they are rendered in the reader's
  language instead of being assembled as fixed strings.
- The documents moved into `docs/`; `ARCHITEKTUR.md` became
  `docs/ARCHITECTURE.md` and `ARCHITEKTUR-STANDORTE.md` became
  `docs/ARCHITECTURE-SITES.md`.

### Fixed
- Rejecting a bad BladeRunner size named the wrong set: the message said
  "4, 10 or 20" while 2 has been valid for a while.

### Added
- `go.sum` for server and installer, so builds are reproducible.

## 2026-08-25 — first working chain on real hardware

### Added
- Site model: address management is per site rather than global, as the first
  step of the multi-site design in `docs/ARCHITECTURE-SITES.md`.
- BladeRunner cards on the overview show every slot as a coloured cell —
  green healthy, grey empty, red problem, yellow being deployed.
- Netboot detection from the dnsmasq log: a blade that has netbooted and is
  waiting shows up as "on the network right now", so an image can be picked
  for it at that moment.
- Own installer (`installer/`): a mini OS that writes the assigned image to the
  NVMe, seeds SSH keys, cloud-init and the agent, and reports progress.
- Agent (`agent/`): pulls its desired state every 60 s, applies it idempotently,
  reports facts and health, including fan and LED via compute-blade-agent.
- Netboot is switchable per blade, so a finished blade boots locally from its
  NVMe while a reimage stays remotely triggerable.
- German user interface alongside the English default.

### Fixed
- The image download survives a flapping link: HTTP range resume, up to 60
  reconnects, with a stall watchdog instead of one total timeout.
- Stale commands no longer fire when the agent first starts: commands expire
  after 15 minutes on both sides.
- dnsmasq reservations are written as values only — writing the `dhcp-host=`
  prefix into a `dhcp-hostsdir` file made dnsmasq discard the line while the
  API reported success.
- Hostnames are derived from the address block instead of the database row id,
  so a blade that moves gets the name of its new place.

### Changed
- The project was renamed from Blademaster to Rookery, and "Rack" to
  "BladeRunner" (2, 4, 10 and 20 nodes). BladeRunner and Compute Blade are
  trademarks of Uptime Lab.
