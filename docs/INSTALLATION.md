# Sheath — Installation

How to get from a blank Debian machine to a running installation: the central
server, one site that owns a blade segment, and three blades that netboot,
install themselves and report in. It ends with the traps this setup has
actually run into, as symptom → cause → fix.

Sheath is two programs, not one. **`sheathd`** holds the inventory, makes the
decisions and serves the web interface; it can stand anywhere. **`sheath-site`**
runs once per network segment, writes the DHCP reservations, watches the wire,
caches the images and relays what the blades ask. Everything below follows that
split — the parts a reader on a single machine can collapse are marked where
they occur.

## Conventions used in this guide

- The blade network is **`10.0.0.0/24`** throughout — an example. Any /24 works;
  substitute your own prefix everywhere `10.0.0` appears, including the site's
  `net_base`.
- The host is called **`sheath-server`** and holds **`10.0.0.10`**, the gateway
  `10.0.0.1`. In this guide it runs both programs: the centre and the one site.
- `d8:3a:dd:xx:xx:xx` stands for a blade's real MAC address, `10000000xxxxxxxx`
  for a CM4 serial number. Both are placeholders; nothing derives from them.
- `/srv/sheath` is the documented default for everything Sheath owns, with
  `/etc/sheath` for the dnsmasq reservations, `/etc/sheath-site` for the site's
  token and `/var/lib/sheath-site` for its cached state. They are paths, not
  requirements — but the flags, the units and `tools/*.sh` all assume them.
- Flags are written `--flag=value`. Go accepts a single dash just as well; the
  shipped unit files use two.

---

## 1. Requirements

### The two roles

| | `sheathd` | `sheath-site` |
|---|---|---|
| What it is | inventory, decisions, web interface, REST API | the network presence of one segment |
| Where it must sit | anywhere reachable from the sites | **in the blades' broadcast domain** |
| Runs as | the system user `sheath` | `root` — it writes `/etc/sheath/dhcp-hosts` and reloads dnsmasq |
| Decides | everything | nothing |
| Survives a link outage | — | yes: it works from its cached desired state |
| Talks to | nobody first; sites call it | the centre, outbound only |

The blades talk to **the site**, never to the centre. That is not a detail of
deployment, it is what lets a blade in an isolated segment boot at all.

### The central machine

| | |
|---|---|
| Machine | Any Linux host. A Raspberry Pi CM4 is enough; the binaries are static arm64 or amd64 |
| Architecture | **arm64 is strongly preferred** — see below |
| OS | Debian family (`apt`, systemd). Developed against Raspberry Pi OS on Debian 13 |
| Disk | Tens of GB for the image mirror. **Not the eMMC** — see below |
| Packages | `golang` (1.24 or newer), `dosfstools`, `mtools`, `cpio`, `zstd`, `xz-utils`, `python3`, `sqlite3`, `curl`, `e2fsprogs`, `fdisk` |
| Ports | 8080/tcp for the API, the interface, and the images and payload a site fetches |

> **Why arm64 for the centre.** Two of the tools only work on a machine of the
> blades' own architecture. `tools/prepare-image.sh` customises an image in a
> chroot without emulation, and `tools/build-bootimg.sh` builds the installer
> for the mini OS natively. On an amd64 centre both have to move to an arm64
> machine, and then the image catalogue and the prepared bytes live in two
> places. It is simpler to put the centre on arm64.

> **Do not install into the eMMC of a CM4.** An 8–32 GB eMMC carrying a full
> Raspberry Pi OS desktop image is already close to full before Sheath adds
> anything: Chromium and Firefox alone take ~740 MB, locales another 340 MB.
> Put `/srv/sheath` on the NVMe and leave the eMMC alone. The Go build caches
> in particular must be relocated (§3.2) — otherwise the first `go build` fills
> the eMMC. Check with `findmnt` where the disk is actually mounted before you
> create anything under a new path.

### The site machine

| | |
|---|---|
| Machine | Any Linux host **in the blade segment**. A second CM4, a small x86 box, or the central machine itself |
| Packages | `dnsmasq` |
| Ports | 67/udp DHCP, 69/udp TFTP, 53 DNS, and one TCP port for the relay |
| Privileges | root: it writes into `dhcp-hostsdir`, the TFTP root, and reloads dnsmasq |

The site needs no build tooling and no database. It fetches the netboot payload
and the images from the centre over HTTP and keeps them on its own disk.

### Network

Sheath needs a segment where **the site is the DHCP server**. That is not a
preference, it follows from the design:

- dnsmasq's proxy-DHCP mode supplies PXE information only and hands out no
  addresses at all — `dhcp-host` has no effect on allocation there. So "the
  router keeps doing DHCP, dnsmasq only does boot" is not available.
- TFTP is unauthenticated and unencrypted. It belongs in a management segment,
  not in a house network.

So: **blades get their own VLAN, and dnsmasq is authoritative there.** Check
before you start that no other DHCP server answers in that segment.

**Portfast (edge port) on every blade port.** A managed switch spends around 30
seconds on spanning-tree checks after link-up; the bootloader's DHCP attempt
runs out in the meantime. Without portfast the netboot takes minutes or fails
outright.

### Blades

- Compute Blade with a CM4 and an NVMe. The **CM4 serial number is the primary
  key** of a blade — not the MAC, not the IP.
- Bootloader from **2025-09-23 or newer**. Older ones overflow the TFTP block
  counter at 64 K blocks (~96 MB with a 1468-byte block size) and simply fail on
  a large initramfs.
- One pass of EEPROM configuration per blade over USB (§4). That is the only
  step that needs physical access, ever.

---

## 2. Address plan

Addresses belong to a **site**, not to the installation. Each site carries its
own network, gateway, DNS, domain and dynamic pool, and each BladeRunner in it
gets a block of 20 addresses reserved regardless of its actual size, so the
addresses stay stable if the rack is later replaced by a bigger one. Five racks
fit into a /24 that way.

```text
.1              gateway
.10             sheath-server (centre and site in this guide)
.101 – .110     rack 1  (10 slots, offset 100)
.121 – .124     rack 2  ( 4 slots, offset 120)
.141 – .160     rack 3  (20 slots, offset 140)
.210 – .240     dynamic pool for unknown blades
```

| Value | Rule | Example: rack 1, slot 3 |
|---|---|---|
| IP | `<net>.(offset + slot)` | `10.0.0.103` |
| MAC | `02:b1:ad:<rack>:00:<slot>` | `02:b1:ad:01:00:03` |
| Hostname | `blade-r<rack>s<slot>` | `blade-r1s03` |

The synthetic MAC is used only while the blade has not reported a real one. It
comes from the locally administered range, and it can be made true during
bring-up by writing exactly that value into the EEPROM property `MAC_ADDRESS` —
then MAC, IP, slot and name are all known before the blade has ever booted.

**The rack number follows the address block, not the database row.** The rack
holding `.101–.120` is rack 1, `.121–.140` is rack 2. Derive the names from row
IDs instead and the first blade in your only rack can end up called
`blade-r4s07`, because three test racks were created and deleted before it — a
leaked row number that nobody can explain.

| | Rule | When the blade is moved |
|---|---|---|
| Hostname | `blade-r<rack>s<slot>` | moves along — unless it was set by hand |
| MAC | `02:b1:ad:<rack>:00:<slot>` | moves along — unless the blade reported a real one |
| IP | `<net>.(offset + slot)` | always follows the position |

A blade sitting in slot 2 must not keep claiming to be `…s07`. A name assigned
by hand, such as `storage-01`, stays untouched, and so does a vendor MAC.

Two sites may legitimately use the same network. The address alone is then no
longer unique — a blade is `(site, BladeRunner, slot)`, and every netboot
session records which site saw it.

---

## 3. Install

The order below is the order to follow: directories, binaries, the central
server, its first start, a token for the site, dnsmasq, the site, the netboot
payload, images.

### 3.1 User and directories

```sh
sudo useradd --system --home /srv/sheath --shell /usr/sbin/nologin sheath
sudo install -d -o sheath -g sheath /srv/sheath/{data,images,agent,tftp,logs,build,go,tmp,tools}
sudo install -d -m 0755 /etc/sheath/dhcp-hosts
sudo install -d -m 0700 /etc/sheath-site
sudo install -d -m 0750 /var/lib/sheath-site
```

The layout that the flags, the units and `tools/*.sh` expect:

```text
/srv/sheath/
├── sheathd              the central server binary
├── data/                SQLite database + admin-token (0600, owned by sheath)
├── images/              OS images: mirrored by the centre, cached by the site
├── agent/               sheath-agent, copied into every image offline
├── tftp/                netboot root, ~36 MB
├── logs/                dnsmasq.log — the site follows this file
├── tools/               mirror-image.sh and prepare-image.sh, run by the server
├── build/               ramdisk and boot.img while they are being built
├── src-installer/       installer sources, if you build on the server
├── go/                  GOCACHE, GOMODCACHE, GOPATH
└── usbboot/             clone of raspberrypi/usbboot, for the bring-up

/etc/sheath/dhcp-hosts/  one reservation per blade — written by sheath-site
/etc/sheath-site/token   the site's credential (0600, root)
/var/lib/sheath-site/    desired.json, the last state the site received
/usr/local/bin/sheath-site
```

On a machine that runs both roles these directories are shared: the centre
writes `images/` and `tftp/`, the site reads them and would otherwise fetch
them over HTTP. That is the whole difference between the combined and the split
layout.

### 3.2 Relocate the Go caches, then build

On a CM4 this is not optional. Put it in the build shell's `~/.profile` as well:

```sh
export GOCACHE=/srv/sheath/go/cache GOMODCACHE=/srv/sheath/go/mod \
       GOPATH=/srv/sheath/go/path TMPDIR=/srv/sheath/tmp
```

```sh
cd server    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sheathd .
cd ../site   && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sheath-site .
cd ../agent  && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sheath-agent .
```

All static, no runtime dependencies. Sizes for orientation: server ~11.5 MB,
site ~7 MB, agent ~5.8 MB (about 1 MB of memory at runtime), installer ~6 MB.
The installer is not built here; `tools/build-bootimg.sh` builds it as part of
the payload (§3.9).

```sh
sudo install -o sheath -g sheath -m 0755 server/sheathd      /srv/sheath/sheathd
sudo install -o sheath -g sheath -m 0755 agent/sheath-agent  /srv/sheath/agent/sheath-agent
sudo install -o root  -g root  -m 0755 site/sheath-site      /usr/local/bin/sheath-site
sudo install -o root  -g root  -m 0755 tools/mirror-image.sh tools/prepare-image.sh \
                                                             /srv/sheath/tools/
```

The two shell tools go somewhere root owns because the server runs them under
`sudo` when an image is added from the interface (§3.12). Leaving them writable
by the service user would turn one allowed command into any command.

The agent binary goes into `/srv/sheath/agent/` because the installer fetches it
from there and copies it straight into a freshly written root partition,
offline, before the blade has ever run its own userland.

### 3.3 The `sheathd` unit

`/etc/systemd/system/sheathd.service`:

```ini
[Unit]
Description=Sheath — central server
Documentation=https://github.com/Panxatony/sheath
Wants=network-online.target
After=network-online.target

[Service]
User=sheath
Group=sheath
WorkingDirectory=/srv/sheath
ExecStart=/srv/sheath/sheathd --net-base=10.0.0 --local-dhcp=false
Restart=on-failure
RestartSec=5
ProtectSystem=full
ReadWritePaths=/srv/sheath
# Deliberately NOT NoNewPrivileges: adding an image runs the two shell tools
# through sudo (§3.3.1). Set it where you never add images from the interface.

[Install]
WantedBy=multi-user.target
```

`--local-dhcp=false` is the line that matters. With it the server writes no
reservations and follows no dnsmasq log: a `sheath-site` owns the wire, and two
programs writing one directory would leave whichever wrote last in charge. The
server says so at startup — `local DHCP handling off — a sheath-site owns the
wire here` — and its `syncDHCP` becomes a no-op that reports `written by
sheath-site, not here`.

It then needs no privileged action on the network side at all. Leave
`--local-dhcp` at its default `true` only on a single machine that runs no site;
there the server needs write access to `/etc/sheath/dhcp-hosts` and the sudoers
line of §3.6.

`--net-base` is read on the first start only; afterwards the value lives on the
site row in the database.

| Flag | Default | Meaning |
|---|---|---|
| `--addr` | `:8080` | listen address |
| `--db` | `/srv/sheath/data/sheath.db` | SQLite file |
| `--images` | `/srv/sheath/images` | images, served under `/images/` |
| `--agent` | `/srv/sheath/agent` | agent binary, served under `/agent/` |
| `--base-url` | `http://<net-base>.10:8080` | the URL a **site** uses to reach the centre |
| `--local-dhcp` | `true` | write reservations and watch the log here |
| `--tftp` | `/srv/sheath/tftp` | payload, served under `/boot/` so a site can fetch it |
| `--dnsmasq-log` | `/srv/sheath/logs/dnsmasq.log` | only read when `--local-dhcp` is on |
| `--tools` | `/srv/sheath/tools` | the two shell tools the server runs to fetch and prepare images |
| `--root` | `/srv/sheath` | the directory those tools work in, passed to them as `--root=` |
| `--backup` | `/srv/sheath/backups` | where the database copies go; empty switches them off |
| `--backup-at` | `03:30` | time of day for the copy, local time |
| `--backup-keep` | `14` | how many copies to keep |

`--base-url` is not the address blades use. Blades use their site. This is the
address the site itself calls for the payload and the image bytes, and it is
what the centre puts in the `server_url` field of the desired state.

#### 3.3.1 The sudoers line for the image tools

Adding an image from the interface means unpacking a tarball, mounting a disk
image over a loop device and running a chroot. The server itself runs
unprivileged and should stay that way, so exactly those two scripts are
permitted and nothing else. `/etc/sudoers.d/sheath-images`, mode `0440`:

```text
sheath ALL=(root) NOPASSWD: /srv/sheath/tools/mirror-image.sh, /srv/sheath/tools/prepare-image.sh
```

```sh
sudo visudo -cf /etc/sudoers.d/sheath-images
```

The scripts must be owned by root and not writable by `sheath` — otherwise the
rule permits whatever the service user cares to write into them. Skip this file
entirely where images are only ever added by hand on the command line (§3.12).

### 3.4 First start, the admin token, and site 1

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now sheathd
```

On its first start the server creates the database, generates an admin token
into `/srv/sheath/data/admin-token`, mode `0600`, owned by `sheath`, and creates
**site 1** from `--net-base`: network `10.0.0`, gateway `.1`, DNS `.10`, domain
`blades.lan`, pool `.210–.240`, rack offset base 100, step 20. Without a site
there would be no network for a BladeRunner to belong to.

Reading the token needs `sudo` — a plain `cat` as an ordinary user only yields
`Permission denied`:

```sh
sudo cat /srv/sheath/data/admin-token
```

- Web interface: `http://10.0.0.10:8080/` — log in with that token
- API: `http://10.0.0.10:8080/api/v1/` — same token as a bearer token

A browser form cannot send an `Authorization` header, so `/login` exchanges the
token once for a session cookie: HttpOnly, SameSite=Lax, 12 hours, held in
memory. Restarting the service logs everyone out. A wrong token costs one second
of delay, which is enough to make brute force uninteresting.

To rotate the token, write a new value into the file and restart the service; if
the file is missing at start, a fresh token is generated.

Adjust the site now if the defaults do not fit — on `/sites/1`, or over the API
with `PUT /api/v1/sites/1`.

### 3.5 The site signs itself in

A site needs a credential to read its desired state, report what it sees and
relay for its blades. It asks for one rather than being handed one: the
interface issues a code, the site exchanges it for its permanent token, and the
token is written straight into a file the site machine owns.

On the site page in the interface, **Create an enrollment code**. The code is
short enough to read out loud, good **once**, and good for **an hour**:

```text
Z78E-KZQP-EN9Q
```

Over the API instead, if that is easier:

```sh
curl -sS -X POST -H "Authorization: Bearer $T" \
     http://10.0.0.10:8080/api/v1/sites/2/enroll-code
```

Then, once, on the site machine:

```sh
sudo /usr/local/bin/sheath-site --server http://10.0.0.10:8080 \
     --relay-url http://10.0.1.10:8081 --enroll Z78E-KZQP-EN9Q --once
```

It writes `/etc/sheath-site/token` with mode `0600` and, beside it,
`/etc/sheath-site/site-id`. From then on neither the id nor the token needs to
appear in the unit file, on a command line, or in anyone's clipboard. Lower
case, spaces instead of dashes and a missing dash are all accepted — the code
is normalised before it is compared.

A spent code is refused, and so is one over an hour old. Issuing a new code for
a site that already has a token is allowed and replaces it, which also means
the site holding the old token stops being able to report: the interface says
so before you do it.

### 3.6 dnsmasq

Three files are involved, and the split between them matters: **`sheath.conf`
is written by hand and never touched by the software; everything blade-specific
is generated into `dhcp-hosts/`.**

- `/etc/dnsmasq.d/sheath.conf` — DHCP, DNS, TFTP, netboot gating; maintained by hand
- `/etc/sheath/dhcp-hosts/` — one file per blade, **generated by `sheath-site`**
- `/etc/sudoers.d/sheath` — only needed where a non-root process reloads dnsmasq

`/etc/dnsmasq.d/sheath.conf`:

```ini
# Sheath — blade segment. Blade-specific entries live in
# /etc/sheath/dhcp-hosts/ and are generated, not edited.

interface=eth0                 # the blade segment only, never the uplink
bind-interfaces
domain=blades.lan
expand-hosts

# This is the only DHCP server in this segment. Without it dnsmasq stays
# polite and waits for a server that does not exist.
dhcp-authoritative

# Loan addresses for blades that are not in the inventory yet: they boot,
# enroll, and get a reservation afterwards.
dhcp-range=10.0.0.210,10.0.0.240,1h
dhcp-option=option:router,10.0.0.1
dhcp-option=option:dns-server,10.0.0.10

# Reservations: one file per blade, written by sheath-site from the desired
# state it received from the centre.
dhcp-hostsdir=/etc/sheath/dhcp-hosts

# Netboot chain. With TFTP enabled dnsmasq points the boot server at itself;
# the boot file name is irrelevant, because the CM4 bootloader sits in EEPROM
# and asks for start4.elf on its own.
enable-tftp
tftp-root=/srv/sheath/tftp

# The netboot switch (§3.8). Unknown blades may always netboot, so that they
# can enroll; known blades only when the site has tagged them.
pxe-service=tag:!known,0,"Raspberry Pi Boot"
pxe-service=tag:bootnet,0,"Raspberry Pi Boot"

# sheath-site reads this log — it is how a booting blade becomes visible
# before any operating system runs on it. log-dhcp is required: without it the
# vendor class is not logged, and that is what tells a netboot apart from a
# plain address lease.
log-dhcp
log-facility=/srv/sheath/logs/dnsmasq.log
```

`/etc/logrotate.d/sheath-dnsmasq`:

```text
/srv/sheath/logs/dnsmasq.log {
    daily
    rotate 7
    missingok
    notifempty
    compress
    copytruncate
}
```

`copytruncate` is deliberate: the watcher follows the file by offset and notices
a truncation, while a rename would leave it reading a file nobody writes to any
more.

```sh
sudo systemctl enable --now dnsmasq
```

#### The sudoers line, and when you need it

`sheath-site` reloads dnsmasq by running `sudo -n systemctl reload dnsmasq`.
Running as root, that succeeds without any rule — the shipped unit runs as root
because it writes into `/etc/sheath/dhcp-hosts` and the TFTP root anyway.

Only a process that is **not** root needs `/etc/sudoers.d/sheath`, mode `0440`:

```text
sheath ALL=(root) NOPASSWD: /usr/bin/systemctl reload dnsmasq
```

```sh
sudo visudo -cf /etc/sudoers.d/sheath
```

That is the single privileged action in the whole system. Without the reload,
dnsmasq still picks up *new* files by itself; what is lost is the removal of
entries — which is exactly the netboot switch, and exactly the thing that must
not be stale.

#### What a generated reservation looks like

One file per blade, named `blade-<serial>.conf`:

```text
# Sheath – generated by sheath-site, do not edit by hand
# Blade 10000000xxxxxxxx  BladeRunner rack-1  Slot 1
# boots from the NVMe
d8:3a:dd:xx:xx:xx,blade-r1s01,10.0.0.101,infinite
```

> **The trap that has already gone off:** a file in a `dhcp-hostsdir` contains
> **only** what would otherwise stand to the right of `dhcp-host=`. Write the
> prefix along with it and dnsmasq reports `bad hex constant` **in its own log
> only** and silently discards the line — the reservation has no effect and
> nothing anywhere reports an error. That is why every line is validated before
> it is written: MAC format, DNS label, IPv4.

> **Second quirk of `dhcp-hostsdir`:** dnsmasq only ever *adds* entries
> dynamically. Rewriting one makes it complain `duplicate dhcp-host IP address`
> until a `SIGHUP` clears the table. That is why the site runs
> `systemctl reload dnsmasq` after every change. Skip the reload and the old
> netboot state stays in effect.

**Two files, and who owns which.** The one the playbook lays down holds what
never changes: the interface, the netboot switch, the TFTP root, the logging.
The second — `/etc/dnsmasq.d/sheath-range.conf` — is written by `sheath-site`
from the site record on every pass, and holds what somebody may want to change
from the interface:

```ini
dhcp-range=10.0.0.210,10.0.0.240,255.255.255.0,1h
dhcp-option=option:router,10.0.0.1
dhcp-option=option:dns-server,10.0.0.10
dhcp-option=option:domain-name,blades.lan
domain=blades.lan
local=/blades.lan/
```

Pool, lease time, gateway, resolver and domain therefore live in one place —
the site's page — and reach the wire on the next pass. Before this they were
Ansible variables, and moving the pool in the interface moved nothing at all.

The site checks the file with `dnsmasq --test` before putting it in place and
then **restarts** dnsmasq. A reload would not do: SIGHUP makes dnsmasq re-read
its host records and never its configuration, so a changed range would look
applied and would not be.

### 3.7 The `sheath-site` unit

The unit ships in the repository at `site/sheath-site.service`. Install it as it
is, or adjust the flags to your paths:

```ini
[Unit]
Description=Sheath site — the network presence of one site
Documentation=https://github.com/Panxatony/sheath
After=network-online.target dnsmasq.service
Wants=network-online.target

[Service]
Type=simple
# Root, because it writes into the dnsmasq host directory and the TFTP root,
# and reloads dnsmasq. The blast radius is one site's network configuration,
# which is exactly what this program is for.
User=root
ExecStart=/usr/local/bin/sheath-site \
    --server=http://127.0.0.1:8080 \
    --site=1 \
    --token-file=/etc/sheath-site/token \
    --dhcp-hosts=/etc/sheath/dhcp-hosts \
    --dnsmasq-log=/srv/sheath/logs/dnsmasq.log \
    --images=/srv/sheath/images \
    --tftp=/srv/sheath/tftp \
    --state=/var/lib/sheath-site \
    --interval=30s
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

| Flag | Default | Meaning |
|---|---|---|
| `--server` | — | the centre, e.g. `https://sheath.example`. Required |
| `--site` | — | this site's id. Required; site 1 exists after the first start |
| `--token-file` | `/etc/sheath-site/token` | the credential from §3.5 |
| `--dhcp-hosts` | `/etc/sheath/dhcp-hosts` | dnsmasq's `dhcp-hostsdir` |
| `--dnsmasq-log` | `/srv/sheath/logs/dnsmasq.log` | the log to follow |
| `--images` | `/srv/sheath/images` | where images are cached |
| `--tftp` | `/srv/sheath/tftp` | TFTP root, where `boot.img` is published |
| `--state` | `/var/lib/sheath-site` | `desired.json`, the last state received |
| `--interval` | `30s` | one pass of fetch → apply → report |
| `--listen` | `:8081` | the blade relay; empty turns the relay off |

Two more flags exist for looking rather than doing: `--once` runs a single pass
and exits, `--dry-run` computes everything and writes nothing. Together they are
the right way to check a new site before it touches anything.

```sh
sudo cp site/sheath-site.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now sheath-site
journalctl -u sheath-site -n 30
```

#### What one pass does

1. `GET /api/v1/site/1/desired` with `If-None-Match`. Unchanged is the common
   case and costs a 304. The document carries the site's network, every blade
   that stands in it — MAC, IP, hostname, rack, slot, netboot flag, image,
   token, merged configuration and install state — the images those blades
   need, and where to get the boot payload.
2. Write the reservations, one file per blade, remove the files of blades that
   no longer stand here, and reload dnsmasq if anything changed.
3. Fetch the netboot payload into the TFTP root if the checksum differs.
4. Fetch every image a blade of this site is assigned to, resuming a partial
   download, verifying SHA-256, and reporting `absent`, `fetching`, `ready` or
   `error` per image.
5. Hand over the buffered observations to `POST /api/v1/site/1/events`, drain
   the relay's queue, and report a heartbeat to `POST /api/v1/site/1/status`
   with version, applied state version, clock and image stock.

If the centre does not answer, the site loads `desired.json` from its state
directory and carries on with it. That is the point of the split: a blade that
reboots during a power cut gets its address and boots locally without anyone far
away answering the phone.

#### The relay

`--listen` opens the endpoints the blades actually call — the same paths the
centre offers, so an agent notices nothing:

```text
POST /api/v1/provision/{serial}            the installer asking for its job
POST /api/v1/provision/{serial}/status     progress reports
GET  /api/v1/blades/{serial}/config        the agent's desired state
POST /api/v1/blades/{serial}/status        the agent's report
GET  /api/v1/blades/{serial}/commands      identify, reboot, reimage
POST /api/v1/enroll                        a blade introducing itself
GET  /images/, /boot/, /agent/             bytes
GET  /healthz                              site id, online, applied, queued
```

Two behaviours are worth knowing before you debug anything:

- **It answers from the cache when the centre is unreachable.** Configuration,
  a pending install, a pending erase and the image bytes are all in the desired
  state. What it cannot invent is a decision: `commands` returns an empty list
  and an unknown blade is told to wait.
- **It rewrites image URLs to itself.** The centre names itself in the job's
  `url`, which is correct and useless — the bytes are already at the site. If
  the file is in the local cache, the relay substitutes its own address. That
  also survives the centre moving to another machine mid-download, which has
  happened, and took two installations with it.

Reports made while the centre is away are queued in order and delivered when it
answers again, oldest first; the site answers the blade `202` in the meantime.

### 3.8 Netboot, switchable per blade

The apparent conflict: with `BOOT_ORDER=0xf26` (local storage first) an installed blade
boots locally, but then it never netboots again — a reimage would mean wiping
the NVMe on site. With the network first it lands in the installer on *every*
start.

**It is resolved one layer down, in DHCP.** The Raspberry Pi bootloader only
netboots if the DHCP reply carries option 43 with a menu entry named
`Raspberry Pi Boot`. If that entry is absent, the netboot fails immediately and
the bootloader falls through to the next device in `BOOT_ORDER` — the NVMe.
dnsmasq can offer the entry selectively:

```ini
pxe-service=tag:!known,0,"Raspberry Pi Boot"    # unknown -> always, so it can enroll
pxe-service=tag:bootnet,0,"Raspberry Pi Boot"   # known   -> only when requested
```

dnsmasq sets `known` itself as soon as a MAC appears in a reservation.
`bootnet` is set by the site, by writing the blade's reservation file as either

```text
d8:3a:dd:xx:xx:xx,set:bootnet,blade-r1s01,10.0.0.101,infinite   # install or erase
d8:3a:dd:xx:xx:xx,blade-r1s01,10.0.0.101,infinite               # boot locally
```

So the rule for the fleet is **`BOOT_ORDER=0xf162`** — network, then NVMe, then
the SD/eMMC device, then start over — and every blade still boots locally as
long as no job is requested. A reimage is one click plus a reboot, with no
access to the rack.

The digits are read from the right: `2` network, `6` NVMe, `1` SD **or** eMMC,
`f` back to the beginning. There is no separate digit for eMMC — on a Compute
Module the eMMC *is* the SD device, and a Lite has a card there instead, so one
digit covers whichever of the two the module physically has. Network stays
first because that is where the decision is made: DHCP either offers the boot
menu entry or it does not, and a refusal costs a moment rather than the
45 seconds an unanswered DHCP request costs.

**The tag comes from the desired state, and from nowhere else.** The centre sets
`netboot: true` for a blade whose install state is `pending` or `wipe`; the site
turns that into `set:bootnet`. On a host with `--local-dhcp=false` the server's
own reservation writer is inert, so anything that is supposed to arm a netboot
has to travel through the site. An erase that was requested and never happened
is what taught that lesson.

### 3.9 The netboot payload

TFTP root `/srv/sheath/tftp`, about 36 MB:

```text
start4.elf, fixup4.dat          firmware (from the machine's /boot/firmware)
bcm2711-rpi-cm4.dtb, -io.dtb    device tree
overlays/                       371 files
boot.img                        ~26 MB – kernel + initramfs in one FAT image
config.txt                      boot_ramdisk=1, arm_64bit=1, uart_2ndstage=1
cmdline.txt                     console=serial0,115200 ip=dhcp debugconsole
                                sheath_server=http://10.0.0.10:8080
```

**`sheath_server=` in `cmdline.txt` is the one line that has to match your
installation, and it must name the site, not the centre.** It is how the mini OS
learns where to ask, before it has networking of its own, and a blade in an
isolated segment has no route to the centre at all. The generic form is
`sheath_server=http://<site>:8080`, with the site's relay bound to `--listen=:8080`.

In the combined layout of this guide the centre already holds port 8080 on that
machine, so the relay keeps its default `:8081` and the line reads
`sheath_server=http://10.0.0.10:8081`. Whichever you choose, the port in
`cmdline.txt` and the port in `--listen` are the same number.

`bm_server=` is still read as a fallback — the spelling from before the rename —
so a blade that catches an older `boot.img` does not stall.

**The prefix fallback is what makes this generic.** With `TFTP_PREFIX=0` the
bootloader first looks in a directory named after the lower four bytes of the
serial number (eight hex digits, not the full 16) and falls back to the root if
no `start4.elf` is there. The root is therefore the *generic installer*: every
unknown blade ends up there by itself and enrolls. Per-blade directories are
only needed for exceptions — and note the other side of the fallback: an empty
per-blade directory does not produce an error, it produces the wrong boot.

**Only kernel, DTB and initramfs go over TFTP.** The OS image comes over HTTP,
from inside the already-running mini OS. TFTP here has no windowing (one block
per round trip), no retry worth the name, and on older bootloaders a hard size
ceiling — it is not a transport for gigabytes.

Measured on a CM4: TFTP delivered a 31 MB `boot.img` in about **4 seconds**,
byte-identical. That is the critical path confirmed, and it stays far below the
~96 MB block-counter limit of older bootloaders — the image is smaller still
after the thinning described below.

#### Building it

`tools/build-bootimg.sh` does the whole chain — installer → ramdisk →
`boot.img` → TFTP root:

```sh
sudo -u sheath tools/build-bootimg.sh        # build and publish
BUILD_ONLY=1 tools/build-bootimg.sh          # build, do not publish
```

**It must run on a machine of the blades' architecture (arm64).** The installer
is built with `GOARCH=arm64` and there is no cross-compilation step for the
ramdisk that surrounds it, which is the second reason the centre belongs on
arm64.

Sources are expected in `/srv/sheath/src-installer`, the unpacked ramdisk in
`/srv/sheath/build/rootfs`, and `config.txt` and `cmdline.txt` in
`/srv/sheath/build/`. `SHEATH_ROOT` and `INSTALLER_SRC` override the first two.
Write your `cmdline.txt` there once; the script copies it into every image it
builds.

The script is strict on purpose (`set -euo pipefail`, build to `.new`, publish
last): a failed build must never leave a stale `boot.img` in the TFTP root. That
has happened once, and the blade then netbooted yesterday's installer while the
log claimed today's.

A site that is not the build machine never runs this. It fetches
`GET /boot/boot.img` from the centre, compares the checksum from the desired
state, and publishes the payload into its own TFTP root.

#### The mini OS

Raspberry Pi's own netinstall `boot.img` boots fine on a CM4, but it shows the
menu-driven Imager on HDMI and never asks any API. Sheath ships its own image
with `sheath-installer` instead.

Its layout makes that easy: `boot.img` is a bare **FAT16** (label `BOOT`, no
partition table) holding `Image.gz`, `rootfs.cpio.zst`, `start4.elf`,
`fixup4.dat`, the DTB and `overlays/`, driven by `config.txt` — `boot_ramdisk=1`
on the TFTP side, `kernel=` and `initramfs` inside.

The decisive detail is its `/init`:

```sh
/sbin/udevd -d ; udevadm trigger ; udevadm settle
dhcpcd -f /etc/dhcpcd.conf --noarp &
/etc/init.d/S99rpi-imager start      # ← only this line is replaced
```

"Boot on a CM4, start udev, get networking" is already solved there. **No
initramfs of your own is needed** — only the last line changes.

Thinned out further: Qt libraries, Mesa DRI drivers, `qmaps` and fonts are
dropped, because without a GUI nothing uses them. Measured: **87 MB → 31 MB
unpacked, 23 MB → 11 MB compressed**, and the whole `boot.img` from 31 down to
**26 MB**. The 140 CA certificates in `etc/ssl/certs` stay — the installer
downloads over HTTPS.

> **Worth knowing:** the initramfs contains **zero kernel modules**. NVMe, ext4
> and the rest are built into the kernel, so the installer has nothing to load.
> The same fact decides whether a distribution can be installed at all: an image
> whose kernel has no built-in NVMe driver and builds no initramfs (DietPi, for
> instance) will not boot from the NVMe afterwards. Test that before adding it
> to the catalogue.

Keep the original around as `/srv/sheath/boot.img.rpi-netinstall.bak`.

#### `sheath-installer`

Go, static, ~6 MB, sources under `installer/`. Deliberately self-contained: the
initramfs ships neither `curl` nor `zstd`, `xz` or `resize2fs`, so HTTP,
decompression (xz/zstd/gz), writing and mounting all happen inside the program.

1. Serial number from the device tree, MAC from `/sys/class/net/eth0/address`
2. Server address from `sheath_server=` in `/proc/cmdline` — the only way to
   hand the mini OS anything before it has networking; `cmdline.txt` comes over
   TFTP, and the address it names is the site's relay
3. Wait for that address to answer, up to 3 minutes (mind portfast)
4. `POST /api/v1/provision/{serial}` with the MAC, then keep asking. Four
   answers are possible: `waiting` (no image picked yet), `idle` (an image is
   assigned but nobody requested an install), `go`, and `wipe`
5. On `go`: fetch the image in a single pass → decompress → write to the target.
   Nothing is buffered; a 6 GB image fits neither in the memory of a CM4 nor on
   any scratch space it has
6. SHA-256 is computed along the compressed stream and verified
7. `BLKRRPART` + `partprobe`, grow the last partition into the disk unless told
   not to, mount the largest partition as the root, drop the seed
   `/etc/sheath/agent.env` (server, serial, token), fetch `sheath-agent` from
   `/agent/` and enable it with a symlink in `multi-user.target.wants` —
   enabling a unit without a running systemd is exactly that symlink
8. Report progress, then reboot, halt or drop to a console, as configured

**No `resize2fs`:** the images grow their own root on first boot — Debian's
fstab carries `x-systemd.growfs`, Ubuntu runs cloud-init's `growpart`.

Two behaviours that look like bugs and are not:

- On `idle` the installer keeps asking rather than stopping, so that requesting
  an install afterwards does not need a power cycle. Nothing is ever written in
  that state.
- If it has been `idle` for a minute and the disk already looks installed, it
  restarts into the installed system. That is the case of a finished blade that
  came back before the netboot tag was cleared.

On failure the blade stops with a message instead of dropping into a boot loop.
`debugconsole` in `cmdline.txt` keeps a getty alongside, so you can get a shell
without pulling the blade.

**Where it is built.** On the centre, and nowhere else. `tools/build-bootimg.sh`
writes into the centre's TFTP root, and the sites fetch from there, checksums
and all — so the machine that owns the payload is the machine that states what
it is. It ran on a site machine once, and the copy that reached the centre was
whatever somebody last carried over; the second site then served a payload
missing the one file the bootloader reads first.

The firmware inside it comes from a pinned revision of the Raspberry Pi
firmware repository rather than from a directory somebody populated once:

```sh
sudo tools/fetch-firmware.sh /srv/sheath/build     # start4.elf, fixup4.dat, the CM4 device trees
FIRMWARE_REF=1.20250430 sudo tools/fetch-firmware.sh
```

The revision used is recorded in `build/.firmware-ref`. Changing it changes
what a blade boots, so rebuild and then actually netboot one — the payload is
the one piece where "it built fine" says nothing.

**The mini OS has a recipe too.** It is the Raspberry Pi network installer with
the imager taken out and the Sheath installer put in its place — a good base
rather than a lazy one, because it already solves "boot a CM4 over the network
and have an address": udev, the drivers, dhcpcd, an init that works.

```sh
sudo tools/build-minios.sh          # into /srv/sheath/build
```

It fetches `net_install/boot.img` — pinned by checksum, because upstream
replaces that file in place and the payload every blade boots is not a thing to
change by accident — takes the kernel and the firmware out of it, unpacks the
ramdisk, removes the imager along with Qt, the QML plugins, the graphics
drivers, maps, icons and fonts (57 MB a headless blade has no use for), and
puts `installer/init` and the freshly built installer in.

**Out of service is not gone either.** Between "this blade is in service" and
"this hardware no longer exists" there was nothing, and people reached for
Forget — which deletes the record and with it the blade's token, so an already
installed system can never talk to Sheath again without being installed a
second time. **Reset** is the middle: the blade leaves its slot, and the
assignment, the name, the image and everything the installed system said about
itself go with it. What stays is what the module *is* — board, memory,
revision code, boot order — along with its serial number, its token and its
whole history. It is then **in storage**: expected nowhere, raising nothing.
Put it in a slot and it is in service again, recognised by its serial number.

The disk is deliberately not touched. Erasing needs the blade to run and boot
into the mini OS, which is exactly what the hardware being retired often
cannot do any more, and a reset that only works on healthy blades is no use
for the ones you are putting away. Where the disk should be emptied, **Erase**
is its own act, with its own confirmation.

**Off is not gone.** A blade shut down through Sheath is remembered as such:
`state='offline'` alone is judged critical, because a blade that stops
answering usually means something broke — but one that was told to stop is
doing as it was told. So the health watch raises nothing for it, the status
column says "switched off" rather than "offline", and the mark is cleared the
moment an agent reports again, so a blade that came back on its own is not
carried as off for the rest of its life. Only a shutdown through Sheath can be
known this way: a blade somebody unplugs is indistinguishable from one that
failed, and stays an alert.

**Reading the firmware of a blade that is already running.** The boot order
lives in the EEPROM, and a running system often cannot reach it: an upstream
kernel builds no mailbox device, so `/dev/vcio_gencmd` does not exist and
`vcgencmd` — where the distribution ships it at all — has nothing to talk to.
The mini OS can, on every netboot; but a blade in a slot is not offered netboot
once it is installed, and rightly so, or it would never start its own system.

So there is **Read the firmware** on the blade: it arms that one blade for one
netboot and takes the tag off again the moment the mini OS reports what it
found. The restart is not sent in the same breath — that is a race the arming
loses about half the time, because a site fetches every thirty seconds while
the agent takes its command within sixty, and whichever gets there first
decides. Instead the centre waits until the site has actually taken the new
state: a site fetches the whole desired state only when it changed, and arming
a blade changes it, so a fetch stamped after the arming is proof rather than a
guess. The mini OS then sees a system on the disk, nothing
to do, and restarts into it by itself after a minute — which is also long
enough for the tag to have gone. Two restarts, nothing written. An arming that
is not taken up within thirty minutes expires, so a blade that was switched off
does not land in the installer weeks later.

One thing goes the other way: `vcgencmd` from `raspi-utils-core`, pinned by
checksum like everything else here. The network installer does not carry it,
and it is the only way to read a module's `BOOT_ORDER` — that number lives in
the EEPROM, which is reachable only through the firmware. 68 KB is worth it,
because the mini OS is the one thing that sees a blade **before** it has an
operating system, and a blade whose boot order does not name the device its
image goes on is exactly the blade that will never get one.

Then build the payload with `build-bootimg.sh` and, before trusting it, boot a
blade on it.

### 3.10 The backup

The database holds the admin token, the token of every site, the token of
every blade, the inventory, the configuration and the policy. It is the one
file whose loss is not "install Sheath again" but "enroll every blade again,
by hand, while they are running".

`sheathd` writes a copy of it into `--backup` (default `/srv/sheath/backups`)
daily at `--backup-at` (default `03:30`, local time), keeps `--backup-keep`
of them (default 14) and links the newest as `sheath-latest.db`. One is also
taken at startup when the newest is older than a day, so a machine that is off
at night still gets one. An empty `--backup` switches the whole thing off.

```sh
sudo install -d -o sheath -g sheath -m 0700 /srv/sheath/backups
```

**Why a copy at all, when the machine is already backed up.** Whatever carries
this machine away — Proxmox, Borgmatic, a tarball — copies files while the
server is writing to them. SQLite writes through a write-ahead log: the `.db`
alone is a stale state, the `.db` plus a half-written `-wal` is a torn one, and
a torn one restores as `database disk image is malformed`. `VACUUM INTO` folds
the log in and writes a complete database in one go, while everyone keeps
working. Point the outside backup at `/srv/sheath/backups` and it picks up a
file that was already consistent when it arrived.

**The copies are secrets.** Every token in the system is in them. The
directory is created `0700` and the files are written `0600`; they live
outside everything the server offers over HTTP.

**Restoring:**

```sh
sudo systemctl stop sheathd
sudo -u sheath cp /srv/sheath/backups/sheath-latest.db /srv/sheath/data/sheath.db
sudo rm -f /srv/sheath/data/sheath.db-wal /srv/sheath/data/sheath.db-shm
sudo systemctl start sheathd
```

Removing the `-wal` and `-shm` is not optional: they belong to the database
that was there before, and SQLite will try to replay them onto the one that
just arrived. That is the same trap the other way round — a database moved
without its `-wal` loses whatever had not been checkpointed yet.

Rehearse it before you need it. The copy can be opened without touching
anything that is running:

```sh
sudo -u sheath /srv/sheath/sheathd -db /tmp/drill.db -addr 127.0.0.1:8099 \
     --local-dhcp=false --backup=""
curl -s localhost:8099/api/v1/health
```

### 3.11 Notification

The health verdict is computed on every heartbeat, coloured on every page and
written into the log. Until this existed, that was all it did: a blade that
overheated at three in the morning was amber on a page nobody was looking at.

The **Notification** card on `/settings` holds an SMTP server, port, security
(`STARTTLS` on 587, `TLS` on 465, or none), user and password, sender and one
or more recipients, the level to send from, and the hold time. **Send a test**
sends one mail immediately, which is the only way to find out that a password
is right.

Two rules keep it from becoming noise, and noise is the only way a
notification stops being read:

- **A verdict has to hold.** Nothing is sent until a blade has been unwell for
  the hold time (default ten minutes). A blade that reboots is briefly offline
  and briefly warm; neither is news. The pass runs once a minute, so a spike
  is recorded, watched, and dropped again without a word.
- **Once each way.** What went bad is said once, and what recovered is said
  once. A blade that is warm for two days sends one mail, not two thousand.
  A verdict that gets *worse* — attention to trouble — starts the clock again
  and may be said a second time.

The state lives in the `alerts` table, one row per blade that is currently
unwell, and the settings page lists them under the card. A failed send is
logged and **not** marked as sent, so the next pass tries again.

**The password is not part of what a blade receives.** It is kept in the
`settings` table, never shown again once saved, and deliberately not in the
configuration document — the desired state a blade pulls would otherwise carry
the mail password to every blade in the rack. Leaving the password box empty
keeps what is stored.

Where mail is not wanted, leave the card switched off: the alert rows are still
kept and the events are still written, so the log tells the same story
afterwards.

### 3.12 Images

Images live in `/srv/sheath/images/` on the centre, are served under `/images/`,
and are copied to each site that needs them — once per site, not once per blade.

The interface adds one at `/images`: paste a download address, optionally a name
and a package list, and the server queues the work. Over the API that is

```sh
curl -sS -X POST -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
     -d '{"url":"https://…/debian-13-raspi-arm64-….tar.xz"}' \
     http://10.0.0.10:8080/api/v1/images/fetch
```

which answers `202` with the id it chose and the recipe it matched. The id is
derived from the file name where none is given: `ubuntu-24.04.3-arm64`,
`debian-13-arm64`, and for DietPi — whose file names carry the board rather
than the release — `dietpi-trixie-arm64`. The
catalogue entry exists before the download does, so the page can show `queued`,
`working`, `ready` or `failed` instead of nothing at all. **One job runs at a
time** — two chroots and two gigabyte downloads at once is how a Raspberry Pi
stops answering. `DELETE /api/v1/images/{id}` removes an entry and its file, and
refuses while a blade still uses it or while the job is running.

A **recipe** is what the server knows about a source before it has seen it.
Ubuntu 24.04, DietPi v10 and Debian 13 are recognised from the URL, and each
carries the `os_id`, the kernel flavour, a minimum disk and the packages that
image needs:

| Recipe | Kernel | Installs | Why |
|---|---|---|---|
| Ubuntu 24.04 (arm64) | downstream | — | brings cloud-init and SSH; overlays work, so the smart fan reports |
| DietPi v10 (arm64) | downstream | — | configures itself at first boot; apt in a chroot would run before that and confuse it |
| Debian 13 (arm64) | upstream | `openssh-server` | ships without SSH; the upstream kernel ignores overlays, so no fan telemetry |

An unknown source is still fetched — the entry then says so and derives no
attributes, and you set `kernel`, `min_disk` and `verified` yourself.

The work itself is not done in Go: the server runs the same two scripts you
would run by hand, through `sudo`, with a ninety-minute ceiling per step. They
remain the command-line route, and on a machine where the interface is not
reachable they are the only route:

```sh
tools/mirror-image.sh debian-13-arm64 https://…/debian-13-raspi-arm64-….tar.xz debian
tools/prepare-image.sh debian-13-arm64 openssh-server
```

`mirror-image.sh` fetches an image, unpacks a `.tar.xz` and mirrors the disk
image inside it (the installer writes disk images, not archives), recompresses
it with `xz -3`, takes the SHA-256 and enters it in the catalogue.

`prepare-image.sh` customises it before any blade sees it. It grows the image by
1 GB, mounts it over a loop device, installs the packages you name in a chroot,
then clears the identity the image was built with: an empty `/etc/machine-id`
(systemd derives the DHCP identity from it, and every blade would otherwise
fight over one lease), the SSH host keys removed, plus a small oneshot unit that
runs `ssh-keygen -A` at first boot — removing the keys is not enough, because
sshd refuses to start without them.

**The chroot has no emulation, so the host must be the same architecture as the
image.** That is the constraint that puts the centre on arm64. It matters
because the Debian raspi image ships neither `openssh-server` nor cloud-init: a
blade installed from it has exactly one door, the Sheath agent — and one door is
one too few when the question is why the agent did not start.

Two things follow from that, and both are in the installation path rather than
in a runbook. The installer **enables the ssh unit** while it still has the root
filesystem mounted — a symlink in `multi-user.target.wants`, which is all
`systemctl enable` writes — because a service nobody ever started is a door
that was fitted and never hung. And the agent **reports what it finds**: whether
`sshd` is installed, whether it runs, whether anything listens on 22, and how
many host keys there are. The last of those is the usual answer when a blade
answers every ping and refuses every connection: sshd will not start without
host keys, and the distribution makes them on the first boot — where that step
did not happen, nothing on the blade says so.

**An image with a GPT does not boot from a card.** Measured on this hardware,
not deduced: `blade-kb-r1s04` wrote Debian 13 to its eMMC without a single
error — checksum verified, partition grown, agent installed, `done 100%` — and
then never came up again. Not a ping, not a DHCP packet, for forty-five
minutes. The same blade, the same eMMC, the same bootloader build and the same
boot order (`0xf54162`, whose third step is SD/eMMC) booted DietPi five minutes
later. The only difference between the two images is in the first kilobyte:

| | Debian 13 (cloud.debian.org raspi) | DietPi |
|---|---|---|
| LBA 1 | `EFI PART` — a GPT | empty |
| MBR entry 1 | FAT32 `0x0c`, no boot flag | FAT32 `0x0c`, boot flag `0x80` |
| MBR entries 2–3 | `0xEE` protective | Linux `0x83` |
| from NVMe | boots | — |
| from eMMC | **does not boot** | boots |

The bootloader reads a GPT from an NVMe — it says which partition it took, in
`/proc/device-tree/chosen/bootloader/partition` — and does not from the card
interface. So the centre reads the first kilobyte of every image, through
whatever it is compressed with, and remembers what it found; `checkTarget`
refuses a GPT image on an eMMC or a card before the hour of writing rather than
after it. Debian itself is not the problem: the GPT belongs to the image
Debian's cloud builder produces, not to the distribution.

A catalogue entry carries more than bytes:

| Field | Meaning |
|---|---|
| `id`, `url`, `local`, `sha256`, `bytes` | what and where |
| `os_id` | which distribution the agent should expect |
| `kernel` | `downstream` or `upstream` — an upstream-kernel image gets no fan or LED telemetry (§7) |
| `min_disk` | bytes; an image cannot be assigned to a smaller disk |
| `part_table` | `gpt` or `mbr`, read off the image's own first sector; a GPT image is refused for an eMMC or a card |
| `verified` | this image has actually booted on a blade |
| `state`, `note` | `queued`, `working`, `ready` or `error`, with the last line of whatever failed |

Writing an entry only touches the fields the caller mentions. That is not
politeness: setting the kernel flavour by hand once overwrote the URL, checksum,
size and local file of all three entries, because the mirror script and a person
editing attributes were erasing each other.

A failed *preparation* leaves the entry `ready`, not `error`, with the reason in
the note. The bytes are on disk and a blade can be installed from them; only the
customisation did not happen, and calling that an error would suggest there is
nothing to install.

### 3.13 Settings and policy

Two places hold the numbers, and they are not the same thing.

**Settings** — `/settings` in the interface — owns the two sections a person
turns knobs in:

| Section | Keys |
|---|---|
| `agent` | `interval`, `jitter`, `allow`, `reboot_on_boot_config`, `maintenance` |
| `install` | `install_target`, `after`, `reboot_delay`, `no_grow`, `require_checksum`, `no_root_keys`, `no_cloud_init`, `no_agent` |

They live in the ordinary configuration under the `global` scope and are layered
global → group → blade like everything else a blade is told. The page merges
into what is there and deliberately never touches keys, files, units or
binaries — a form that has never seen them must not be able to remove them.

Over the API the same rule has to be kept by hand:

```sh
# Right: change one thing.
curl -sS -X PATCH -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
     -d '{"install":{"after":"reboot"}}' \
     http://10.0.0.10:8080/api/v1/config/global

# Wrong unless you mean it: PUT replaces the entire scope.
```

`PATCH /api/v1/config/{scope}` merges one level deep; a `null` value removes a
key. `PUT /api/v1/config/{scope}` replaces the whole scope, which is what PUT
means and exactly the wrong tool for changing one setting (§7).

**Policy** — the thresholds and timings that decide when Sheath calls something
a problem. `GET /api/v1/policy` reads the global set, `PUT /api/v1/policy`
writes it:

| Key | Default | |
|---|---|---|
| `soc_warn_c` / `soc_crit_c` | 70 / 80 | SoC temperature |
| `nvme_warn_c` | 70 | NVMe temperature |
| `disk_warn_pct` / `disk_crit_pct` | 85 / 95 | fill level of `/` |
| `offline_after_min` | 5 | no heartbeat for this long → offline |
| `no_wipe` | off | forbid erasing a disk at this site |
| `command_ttl_min` | 15 | commands older than this are not handed out |
| `sample_every_min` / `sample_keep_hours` | 5 / 48 | health history |

```sh
curl -sS -H "Authorization: Bearer $T" http://10.0.0.10:8080/api/v1/policy
curl -sS -X PUT -H "Authorization: Bearer $T" -H 'Content-Type: application/json' \
     -d '{"soc_warn_c":65}' http://10.0.0.10:8080/api/v1/policy
```

A site overrides what it reasonably can — the health thresholds and `no_wipe` —
on its own page, `/sites/{id}`, or with `PUT /api/v1/sites/{id}/policy`. An
empty field inherits; the last three keys are global only, because they are
properties of the centre's bookkeeping and not of a place. A blade in a
ventilated rack and one in a warm office do not share a temperature at which
someone should be woken.

---

## 4. Bring up the blades (once, over USB)

Everything else works without rack access. This one step does not, and it is
better planned than discovered: the CM4 leaves the factory with
`BOOT_ORDER=0xf641` (USB-MSD → NVMe → SD/eMMC), so an empty blade never falls
through to netboot.

Changing the EEPROM from the running system is not the intended route on a CM4:
`rpi-eeprom-update` is disabled there out of the box, and the update is **not
atomic** — a power cut during it damages the EEPROM. For a fleet that is not a
routine tool. So do it once per blade with `rpiboot` from
[raspberrypi/usbboot](https://github.com/raspberrypi/usbboot), before the blade
goes into the rack:

```ini
BOOT_ORDER=0xf162                # network → NVMe → SD/eMMC → loop (read right to left)
NET_BOOT_MAX_RETRIES=1           # keep this low, see below
TFTP_PREFIX=0                    # directory = lower 4 bytes of the serial number
MAC_ADDRESS=02:b1:ad:01:00:03    # optional: the slot's deterministic MAC
```

| Value | Factory | Meaning |
|---|---|---|
| `BOOT_ORDER` | `0xf641` | `0xf162` = network → NVMe → SD/eMMC → loop; `0xf26` = local storage first |
| `NET_BOOT_MAX_RETRIES` | `0` | Retries after a TFTP timeout |
| `DHCP_TIMEOUT` | `45000` ms | How long the bootloader waits for a DHCP reply |
| `TFTP_PREFIX` | `0` | Serial-number subdirectory, with fallback to the TFTP root |
| `MAC_ADDRESS` | unset | Overrides the factory MAC |

Which boot order you choose follows from §3.8:

- **`0xf162` (network first)** is the one that fits this design. Every ordinary
  start makes one netboot attempt, but with option 43 withheld that attempt
  fails immediately and the blade continues from the NVMe. In exchange, a
  reimage never needs a hand in the rack.
- The `1` after it covers a blade installed to its **eMMC or an SD card**
  rather than to NVMe: one digit for both, because on a Compute Module the
  eMMC is the SD device and a Lite has a card slot in its place. It costs
  nothing on a blade that boots from NVMe — that digit is only reached when the
  NVMe has nothing to offer.
- **`0xf26` (local storage first)** boots an installed blade without any
  attempt at all, but a reimage then means wiping the disk on site. Use it only
  where the network is not trusted to answer.

> **`NET_BOOT_MAX_RETRIES` belongs low (0 or 1)** with a network-first order.
> Every ordinary boot pays for the blocked netboot attempt, and it has to fail
> quickly; a high retry count delays every single start.

> **`DHCP_TIMEOUT`: leave it at 45 s and fix the switch instead.** The blocked
> netboot is only fast because DHCP *answers*. If DHCP stays silent — a port
> without portfast, a dead uplink — the blade waits out this timeout on every
> start. Lowering it below the switch's spanning-tree delay makes real netboots
> fail; raising it lengthens every boot.

While you are there, update the bootloader: anything before **2025-09-23** cannot
load an initramfs beyond roughly 96 MB.

---

## 5. Verify

Work down this list once. Each step has an observable result, and each one has
failed for somebody before.

**Both services are up.**

```sh
systemctl status sheathd sheath-site dnsmasq
curl -s http://10.0.0.10:8080/healthz
curl -s http://10.0.0.10:8081/healthz
```

The site's `/healthz` is the more informative of the two: it names the site id,
whether the centre is reachable, which desired-state version it has applied, and
how many blade reports are queued.

**The site is authenticated and pulling.**

```sh
journalctl -u sheath-site -n 20
```

You want a line like `new desired state sha256:… : 3 blades, 1 images`. `site 1
has no token yet` or `HTTP 401` means §3.5 has not happened, or the token was
rotated after the file was written.

**A dry run does what you expect.**

```sh
sudo systemctl stop sheath-site
sudo /usr/local/bin/sheath-site --server=http://127.0.0.1:8080 --site=1 \
     --token-file=/etc/sheath-site/token --once --dry-run
sudo systemctl start sheath-site
```

**dnsmasq accepts its configuration.**

```sh
dnsmasq --test
sudo grep -iE 'bad hex|duplicate dhcp-host' /srv/sheath/logs/dnsmasq.log
```

The grep must stay empty. It is the only place where a rejected reservation ever
shows up.

**TFTP serves the payload, byte for byte.**

```sh
tftp 10.0.0.10 -c get boot.img /tmp/boot.img
cmp /tmp/boot.img /srv/sheath/tftp/boot.img
```

**`cmdline.txt` names the site.**

```sh
mdir -i /srv/sheath/build/boot.img ::cmdline.txt
mtype -i /srv/sheath/build/boot.img ::cmdline.txt
```

The `sheath_server=` value must be an address the blades can reach — the site's
relay, on the port `--listen` binds.

**The netboot switch does what the interface claims.** `tools/pxeprobe.py` sends
a DHCPDISCOVER that poses as the RPi bootloader and shows whether the menu entry
comes back:

```sh
sudo python3 tools/pxeprobe.py d8:3a:dd:xx:xx:xx eth0
```

| Case | Option 43 | Netboot |
|---|---|---|
| unknown blade | `…09 14 …Raspberry Pi Boot` | allowed |
| known, nothing requested | `0601080a0400505845` | **blocked** |
| known, installation requested | `…09 14 …Raspberry Pi Boot` | allowed |
| after the installation finished | `0601080a0400505845` | **blocked** |

**The inventory produces reservations.** Create a BladeRunner in the interface,
insert a blade into a slot, then wait one site interval:

```sh
ls /etc/sheath/dhcp-hosts/
sudo journalctl -u sheath-site -n 20
sudo journalctl -u dnsmasq -n 20
```

A file `blade-<serial>.conf` appears, the site logs `reservations: 1 written, 0
removed`, dnsmasq reloads, and no error follows. Note the delay: the centre
decides immediately, the wire changes at the site's next pass, up to 30 seconds
later.

**The constraints hold.** These are worth trying once, because the messages tell
you the model is being enforced rather than guessed:

- a rack size other than 4, 10 or 20 is rejected
- slot 9 in a 4-slot rack → `Slot 9 is outside the rack (1..4)`
- occupying an occupied slot → rejected, naming the current occupant
- shrinking a rack while a slot beyond the new size is occupied → rejected
- deleting a rack that still holds a blade → rejected
- "Reimage" without an assigned image → rejected
- erasing without typing the blade's name → rejected
- without a login, `/` redirects to `/login` and the API answers 401

**A blade actually boots.** Power one on and watch the overview: it should move
through `dhcp` → `tftp` → `ramdisk` and then wait for an image.

---

## 6. Operate

### The workflow

**Site → BladeRunner → blade.** Site 1 already exists; create more only for more
segments.

- **Overview `/`** — a form to create a BladeRunner (name, 4/10/20 slots, site),
  with the next free address block shown next to it, so the addresses are known
  before the rack exists. One tile per rack with occupancy, address block and
  site. Below that, **blades without a slot**: devices that have reported in but
  do not sit anywhere yet, served from the dynamic pool.
- **Site page `/sites/{id}`** — what stands in this site, how it is doing, and
  its images in full: which image, what state, how many bytes are here against
  how many the catalogue has, and how many blades here are waiting for it. That
  last row is the one that explains a waiting install. The site's policy
  overrides live here too.
- **Map `/map`** — sites, racks and blades in one picture.
- **Rack page `/bladerunners/{id}`** — the slot table across the full rack size.
  Free slots show the *planned* IP and MAC, so you see what a blade will get
  before inserting it, plus a select box of unplaced blades. Occupied slots show
  hostname, serial, IP, MAC, image, status LED and the time of the last report,
  with the slot overlay's actions: **Identify** (and identify off), **Stealth**
  both ways, **Reboot**, **Reimage**, **Erase NVMe** and **Remove**.
- **Images `/images`** — the catalogue, what each entry can do, and the form that
  fetches and prepares a new one. An image in use cannot be removed.
- **Settings `/settings`** — the agent and installation sections (§3.13).

Every action is a POST followed by a redirect, so a reload never triggers
anything twice.

### Watching a blade boot

The site follows the dnsmasq log and sees a blade starting **before any
operating system runs on it**, then reports what it saw to the centre in
batches. Two sources:

1. **Passive, immediately** — the DHCP request, the vendor class, and every TFTP
   file served. MAC, IP and progress follow from that.
2. **Active, shortly after** — the mini OS reports in with its serial number via
   `POST /api/v1/provision/{serial}` through the relay and is matched to the
   session by MAC.

Stages: `dhcp` → `tftp` → `ramdisk` (boot.img served) → `installer` → `writing`
→ `done`. From `ramdisk` on, a device counts as **waiting** and the image
selection appears in the overview. The choice is remembered on the session and
the installer picks it up the next time it asks. Until then the API answers
`{"status":"waiting","retry_after":5}` — **200, not 409**: waiting is a regular
state of the process, not an error.

> **The distinction that saves the most time:** the RPi bootloader identifies
> itself over DHCP with the vendor class
> `PXEClient:Arch:00000:UNDI:002001`; an ordinary Linux client does not. That is
> how Sheath knows whether a blade even *wanted* to netboot. A device that only
> took an address is shown as such, with a note that `BOOT_ORDER` is probably
> still at the factory value. Without that distinction you go hunting for a
> fault in TFTP for a blade that never asked.

The site's log watcher never rewinds: it seeks to the end of the file at
startup, so restarting it does not report last week's boots as if they were
happening now.

### Installing and reimaging

1. Assign an image to the blade.
2. Press **Reimage**. The centre sets the install state to `pending`.
3. The site's next pass writes `set:bootnet` into the reservation and reloads
   dnsmasq. The image is fetched into the site cache if it is not there yet —
   the site page shows `fetching` while that happens.
4. Reboot the blade (the button queues a `reboot` command for the agent, so
   nobody has to walk to the rack).
5. The blade netboots, the installer asks the relay, gets `go`, writes the disk,
   seeds the agent and reboots.
6. On the installer's `done` report the centre clears the intent, and the relay
   fetches the desired state **immediately** rather than waiting for its next
   pass — see §7 for why that matters.

An assigned image never means "write it now". Otherwise a blade whose
`BOOT_ORDER` puts the network first would reinstall itself on every start.

### Erasing a blade

The slot overlay's **Erase NVMe** takes a blade out of service so it can be
pulled and put somewhere else. It works like an installation and ends
differently:

1. The site may forbid it: `no_wipe` in its policy, and the action is refused
   with `erasing is switched off at this site`.
2. Whoever asks has to type the blade's hostname. A slip of the mouse in a list
   of twenty rows should not empty a disk.
3. The install state becomes `wipe`, which arms the netboot exactly as an
   installation does, and a `reboot` command is queued.
4. The blade netboots once more. The installer gets `wipe`, discards the whole
   device where the drive accepts a discard, and overwrites the first and last
   64 MB either way — that is what actually removes the partition table, the
   boot sector, the filesystem superblocks and the backup GPT.
5. Only when the blade reports `wiped` does it leave its slot. Doing it at the
   click would have made a half-erased blade disappear from the interface, which
   is the worst moment to lose sight of one.
6. The blade then halts rather than rebooting, so it can be pulled. Where it is
   to stay put, `install.after: reboot` sends it back into the installer, where
   it waits and can be given a new image without anyone walking to the rack.

The work happens in the mini OS and not in the agent for one reason: the agent
runs from the disk it would have to erase, and a root filesystem cannot be
unmounted out from under itself.

### The agent

Runs as a systemd service on every blade, ~5.8 MB, ~1 MB of memory. Its
credentials were placed by the installer in `/etc/sheath/agent.env` (as
`SHEATH_SERVER`, `SHEATH_SERIAL`, `SHEATH_TOKEN`); the unit pulls them in with
`EnvironmentFile`. `SHEATH_SERVER` points at the site's relay.

Every 60 seconds, with a random offset at startup so twenty blades do not hit
the server in lockstep after a power cut:

1. **Report** facts and health to `POST /status`
2. **Fetch commands** — `identify`, `identify_off`, `stealth_on`, `stealth_off`,
   `reboot`, `reimage`
3. **Reconcile configuration** — `GET /config` with `If-None-Match`; on 304
   nothing happens

The order is deliberate: report first, then act, so the server is up to date even
when applying fails afterwards.

| Facts | Health |
|---|---|
| `os_id`, `os_version_id`, `os_name`, `os_base`, `os_family` | SoC and NVMe temperature |
| `init`, `pkg_mgr`, `net_backend`, `boot_path` | load, uptime, memory |
| kernel, arch, model, serial, `agent_version` | fill level of `/` |
| `reboot_required` | `vcgencmd get_throttled` → undervoltage, throttling |
| | fan speed, if `compute-blade-agent` is running |

On a PoE-powered blade `throttled` is the single most useful value: it reveals
undervoltage and thermal throttling, which are exactly the two things that go
wrong under load.

It applies hostname (including `127.0.1.1` in `/etc/hosts`), time zone, SSH keys
for `root` and the first regular account, managed files, packages and systemd
units (both with `per_os` exceptions), and single binaries such as `bladectl`,
fetched from the site rather than from the internet. An SSH key in the
configuration is given verbatim:

```text
ssh-ed25519 AAAA… your-key
```

Two rules run through all of it:

- **Idempotent.** Every change is checked first. The agent runs every minute —
  without this rule it would restart services once a minute. Measured: a second
  run reports no changes.
- **Restrained.** It **never** touches networking. The fixed address comes from
  the DHCP reservation, and on DietPi the network belongs to the distribution
  anyway.

If applying partly fails, the agent does not remember the version, so the next
run tries again — which also means a blade that can never finish a pass shows as
permanent drift (§7). For diagnosis without a server, `sheath-agent -show`
prints facts and health as JSON.

### Interface language

English is the default, German an option. Every page carries a switcher in the
top right that names the other language in its own spelling, so it is findable
by someone who cannot read the current one. The choice goes into the cookie
`rk_lang` (one year) and can be forced with `?lang=de` or `/lang/de?next=…`; the
server default is the `ui_lang` setting.

All text lives behind keys in `server/i18n.go`, including error messages from the
business logic: an error carries a key and its arguments rather than a finished
sentence, and is only translated when displayed — otherwise the very text you
read when something breaks would have stayed monolingual. The log always gets
the English version. A missing key shows the key itself, which is visible
immediately instead of quietly wrong.

### Redeploying after a code change

```sh
rsync -az server/ sheath-server:~/sheath-src/
ssh sheath-server '
  export GOCACHE=/srv/sheath/go/cache GOMODCACHE=/srv/sheath/go/mod \
         GOPATH=/srv/sheath/go/path TMPDIR=/srv/sheath/tmp GOFLAGS=-mod=mod
  rsync -a ~/sheath-src/ /srv/sheath/src/
  cd /srv/sheath/src && go build -trimpath -ldflags="-s -w" -o /tmp/sheathd.new .
  sudo systemctl stop sheathd
  sudo cp /tmp/sheathd.new /srv/sheath/sheathd
  sudo chown sheath:sheath /srv/sheath/sheathd
  sudo systemctl start sheathd'
```

Restarting invalidates all sessions — log in again with the admin token. The
site notices nothing: it retries, and works from its cached state meanwhile.

The two programs, one binary each, SQLite on the centre, no runtime dependencies:

| File | Contents |
|---|---|
| `server/db.go` | schema, sites, racks, blades, images |
| `server/ipam.go` | address plan per site, reservations, network self-check |
| `server/netboot.go` | boot sessions, dnsmasq log watcher (local DHCP only) |
| `server/api.go` | REST endpoints, site interface, auth, config merge |
| `server/policy.go` | thresholds and timings, global and per site |
| `server/session.go`, `ui.go`, `i18n.go` | login, pages, translations |
| `server/main.go` | routing, service, reaper |
| `site/site.go` | the pass: fetch, apply, report; the cached state |
| `site/dhcp.go` | reservations, the netboot tag, the dnsmasq reload |
| `site/netboot.go` | the dnsmasq log watcher |
| `site/images.go` | the image cache and the boot payload |
| `site/relay.go` | the endpoints the blades call |

---

## 7. Troubleshooting

Each entry is a symptom that has actually occurred, its cause, and the fix.

### A reservation or a netboot never reaches the wire

**Symptom.** The interface says an installation — or an erase — was requested.
`/etc/sheath/dhcp-hosts/` does not change. `POST /api/v1/dhcp/sync` answers
`{"written":[],"removed":[],"warning":"written by sheath-site, not here"}`.

**Cause.** That warning is the whole answer. On a host started with
`--local-dhcp=false` the server's own DHCP sync is a deliberate no-op: the site
owns that directory, and two programs writing it would leave whichever wrote
last in charge.

**Fix.** Look at the site, not the server. Anything that arms a netboot travels
through the site's desired state: the centre sets `netboot: true` for a blade
whose install state is `pending` or `wipe`, and the site turns that into
`set:bootnet` on its next pass. Check in this order — `journalctl -u
sheath-site` for `new desired state`, `curl -s http://10.0.0.10:8081/healthz`
for the applied version, and the reservation file itself. A site that is not
running, has no token, or cannot reach the centre and has no cached state is a
site that changes nothing.

### A blade netboots again right after an installation

**Symptom.** The installer finishes, the blade reboots — and lands in the
installer again instead of in the system it just wrote.

**Cause.** A race between two clocks. The site polls its desired state every
30 seconds; a finished installer restarts within seconds. If the reservation
still carries `set:bootnet` at that moment, the blade netboots once more.

**Fix.** Already handled in two places, and worth knowing when it still happens:
the relay fetches the desired state **immediately** when an installer reports
`done` or `wiped`, rather than waiting for its next pass; and an installer that
sees `idle` for a minute with a system already on the disk restarts into it.
So a blade that slips through waits one minute and then boots correctly. If it
loops instead, the tag really is still set — see the previous entry.

### The database jumped back in time

**Symptom.** After moving or renaming `sheath.db`, the inventory is missing the
last twenty minutes of work: blades back in old slots, an image entry gone, a
site's policy reverted. Nothing logged an error.

**Cause.** SQLite in WAL mode keeps recent transactions in `sheath.db-wal`, with
`sheath.db-shm` alongside. Moving only the `.db` file leaves them behind, and
SQLite then opens what it finds: a database as of the last checkpoint. It looked
like the database had jumped back a quarter of an hour, because it had.

**Fix.** Either move all three files together, or check the WAL back into the
main file first:

```sh
sudo systemctl stop sheathd
sudo sqlite3 /srv/sheath/data/sheath.db 'PRAGMA wal_checkpoint(TRUNCATE);'
sudo mv /srv/sheath/data/sheath.db /new/path/sheath.db
```

The same applies to backups: a copy of `sheath.db` alone is a copy of the
database as of its last checkpoint.

### An installation ran with no SSH keys and no agent

**Symptom.** Blades install and boot, but nobody can log in and none of them
report. The configuration looks almost right — one section is correct and
everything else is gone.

**Cause.** `PUT /api/v1/config/{scope}` replaces the whole scope. A request
carrying `{"install":{"after":"reboot"}}` and nothing else removed the SSH keys,
the boot configuration and the binaries of an entire installation. The blades
installed afterwards were installed exactly as told.

**Fix.** Use `PATCH`, which merges one level deep and treats `null` as "remove
this key". Use `PUT` only when you are sending the whole document, and fetch it
first if you are not sure:

```sh
curl -sS -H "Authorization: Bearer $T" \
     http://10.0.0.10:8080/api/v1/config/global > global.json
```

The settings page (§3.13) is safe by construction: it writes only the two
sections it owns and merges them.

### A DietPi blade shows permanent drift

**Symptom.** The blade reports, but the configuration version never settles.
Every pass reports changes; a rule written for DietPi seems to be ignored.

**Cause.** Two things that look like one. DietPi reports `ID=debian` in
`/etc/os-release`, which is true about its ancestry and wrong about everything
that matters here, so a rule written `per_os: {dietpi: …}` never matches unless
the agent's own detection is used — it reads `/boot/dietpi/.version` (or
`/var/lib/dietpi/.version`) and answers `dietpi`. And a pass that fails part way
does not record the version, so a blade whose units cannot be enabled reports
drift forever: OpenSSH is `ssh` on Debian and Ubuntu, while DietPi runs Dropbear
and has no `ssh.service` to enable.

**Fix.** Write `per_os` rules against the identity the agent reports — check it
with `sheath-agent -show` — and give units their DietPi names there too. Then
the pass completes, the version is recorded, and the drift stops.

### Adding an image from the interface fails at once

**Symptom.** The entry appears as `queued`, turns to `failed` within a second,
and the note reads `sudo: a password is required` or `command not found`.

**Cause.** The server runs `sudo -n /srv/sheath/tools/mirror-image.sh` and has
no rule permitting it, the scripts are not at `--tools`, or the unit carries
`NoNewPrivileges=true`, which makes `sudo` impossible whatever sudoers says.

**Fix.** Install both scripts under `/srv/sheath/tools`, owned by root, add
`/etc/sudoers.d/sheath-images` (§3.3.1), and check the unit for
`NoNewPrivileges`. `journalctl -u sheathd` carries the script's own last line,
which usually names the missing package — `mtools`, `xz-utils` and `e2fsprogs`
are the ones that go missing on a fresh machine.

### The disk filled up in minutes

**Symptom.** The root filesystem is full shortly after an image was mirrored.
`/srv/sheath/images` holds the file, and the NVMe is empty.

**Cause.** The directory was created before the data disk was mounted there, or
the NVMe was mounted somewhere else entirely. An image store written onto a 6 GB
SD card root fills it in minutes.

**Fix.** Check where the disk actually is before creating anything under a new
path, and check again after a reboot:

```sh
findmnt -no SOURCE,TARGET,SIZE /srv/sheath
df -h /srv/sheath /srv/sheath/images
lsblk -o NAME,SIZE,FSTYPE,MOUNTPOINT
```

A missing mount is not an error to any program that writes into the mount point
— it just writes underneath it.

### No fan speed or airflow temperature on a Debian blade

**Symptom.** `compute-blade-agent` fails to start, or reports
`fan_unit{type="standard"}` although the enclosure has a smart fan unit.
`/dev/ttyAMA5` does not exist.

**Cause.** The smart fan unit talks over UART5, which has to be enabled by the
firmware before Linux starts — `dtoverlay=uart5`. Debian's raspi images run
the *upstream* kernel (`upstream_kernel=1` in `config.txt`) and ship only two
overlays; the Raspberry Pi overlays are built against the downstream device
tree. Placing `uart5.dtbo` on the firmware partition and requesting it in
`config.txt` is not enough: the node stays `disabled` in the running tree,
which `/sys/firmware/devicetree/base/soc/serial@7e201a00/status` will confirm.

**Confirmed by test.** Put a harmless `dtparam=spi=on` in `config.txt` and
reboot: on a Debian raspi image `spi@7e204000` stays `disabled`. The firmware
applies no device-tree directive at all there — not an overlay, not even a
dtparam — so this is not about the overlay file. The firmware partition also
lacks `bcm2711-rpi-cm4.dtb` (only `-cm4-io` ships), while the blade reports
itself as `raspberrypi,4-compute-module`.

**Fix.** Use an image with the downstream Raspberry Pi kernel — the Ubuntu
preinstalled server images do, and there the same setting works — or drive the
blade without fan telemetry. SoC temperature is read from sysfs and keeps
working either way. Record the flavour in the catalogue entry's `kernel` field,
so the interface can say so before somebody spends an evening on it.

**Note.** Debian also regenerates `config.txt` on kernel updates and says so in
the file's first line. The agent therefore writes boot settings to
`/etc/default/raspi-firmware-custom` as well, where they survive.

### A blade keeps getting a pool address although a reservation exists

**Cause.** The reservation line was rejected. A file in `dhcp-hostsdir` holds
only the part right of `dhcp-host=`; with the prefix included, dnsmasq logs
`bad hex constant` in its own log and drops the line silently.
**Fix.** `sudo grep -i 'bad hex' /srv/sheath/logs/dnsmasq.log`, then correct the
file. MAC, DNS label and IPv4 are validated before writing, so a rejected line
usually means the file was edited by hand.

### An installation was requested, but the blade boots from its NVMe anyway

**Cause.** The `set:bootnet` tag never reached dnsmasq — either the site has not
made its pass yet, or the reservation was rewritten without a reload (dnsmasq
only *adds* host records dynamically), or the reload itself failed.
**Fix.** `sudo python3 tools/pxeprobe.py <mac> eth0` shows what the bootloader
would be offered. If the entry is missing, `sudo systemctl reload dnsmasq` and
check `journalctl -u sheath-site` for `dnsmasq not reloaded` — that points at a
site not running as root without the sudoers file (§3.6).

### dnsmasq logs `duplicate dhcp-host IP address`

**Cause.** Same root: entries read from a `dhcp-hostsdir` are added, never
replaced, until a `SIGHUP` clears the table.
**Fix.** Reload. If it recurs on every change, the reload is failing.

### The blade takes an address, but never asks for a TFTP file

**Cause.** It did not netboot at all. Its `BOOT_ORDER` is still the factory
`0xf641`, so it went to its own storage. The overview shows this explicitly,
because the RPi bootloader announces itself with a `PXEClient:…` vendor class and
an ordinary Linux client does not.
**Fix.** Bring the blade up over USB (§4). Do not go looking in TFTP — nothing
was ever requested there.

### Netboot takes minutes, or fails on some ports only

**Cause.** Spanning tree. After link-up a managed switch spends around 30 seconds
checking for loops, and the bootloader's DHCP attempt expires meanwhile.
**Fix.** Portfast / edge port on every blade port. Do not paper over it by
raising `DHCP_TIMEOUT`: that lengthens every boot instead.

### The blade netboots an old installer while the log claims the new one

**Cause.** A build failed halfway and left a stale `boot.img` in the TFTP root —
or the site has not fetched the new one yet.
**Fix.** Rebuild with `tools/build-bootimg.sh`, which builds to `.new` and
publishes only at the end. Never copy an image into `tftp/` by hand mid-build.
On a site that is not the build machine, check that the payload's checksum in
the desired state matches the file: the site compares them and re-fetches, but
only on a pass.

### The blade shows a graphical Imager menu on HDMI

**Cause.** Raspberry Pi's netinstall `boot.img` is still in the TFTP root. It
boots, but it never asks the API.
**Fix.** Publish the Sheath payload (§3.9).

### A blade boots something entirely wrong

**Cause.** With `TFTP_PREFIX=0` the bootloader looks in the serial-number
directory first and **silently** falls back to the TFTP root if no `start4.elf`
is there. A half-populated per-blade directory therefore does not produce an
error, it produces a different boot.
**Fix.** Either keep the directory complete, or do not create it at all and let
the generic root serve the blade.

### A large initramfs never finishes over TFTP

**Cause.** The bootloader's TFTP block counter overflows at 64 K blocks — around
96 MB at a 1468-byte block size — on bootloaders older than 2025-09-23. There is
also no retry by default and no windowing.
**Fix.** Update the bootloader during bring-up, and keep the payload small:
kernel, DTB and initramfs over TFTP, the OS image over HTTP.

### A blade reboots without warning shortly after the agent starts

**Cause.** Stale commands. On the first agent run this fetched **seven-hour-old**
commands from earlier test runs — four `reimage`, two `reboot` — and executed
them. Nothing was overwritten only because the netboot block held.
**Fix.** Already fixed in three places, and worth knowing when reasoning about
the queue: the server hands out no command older than `command_ttl_min`
(15 minutes by default) and logs the expiry; open commands of the same kind are
**replaced rather than stacked** (three `identify` clicks are one entry); the
agent checks the age itself, takes only the newest of each kind, and sorts
reboots to the end. Orphaned commands of deleted blades are cleared at server
startup. Where clocks disagree, commands expire wrongly — the site reports its
own clock in every heartbeat, and a skew above a minute is logged.

### `cat /srv/sheath/data/admin-token` says `Permission denied`

**Cause.** The file is mode `0600` and owned by `sheath`. That is intended.
**Fix.** `sudo cat /srv/sheath/data/admin-token`.

### The overview shows no boot activity at all

**Cause.** The site is not seeing the dnsmasq log: `log-facility` missing or
pointing elsewhere, `log-dhcp` not set (then the vendor class never appears),
the `--dnsmasq-log` flag disagreeing with the config, the file not readable by
the service, or logrotate renaming the file instead of truncating it.
**Fix.** Align the two paths, keep `log-dhcp`, and keep `copytruncate` in the
logrotate stanza (§3.6). The site says which of the two it is:
`log watch: … exists but is not readable` against `… not present yet — waiting`.

### The freshly installed blade does not boot from its NVMe

**Cause.** The image's kernel has no built-in NVMe driver and the distribution
builds no initramfs — DietPi is the usual candidate.
**Fix.** Test that per distribution before putting it in the catalogue, and set
`verified` on the entry only once an image has actually booted on a blade. The
mini OS itself is unaffected: its kernel carries NVMe and ext4 built in and
loads no modules at all.

### The disk fills up during a build

**Cause.** The Go build caches went to the home directory on the eMMC.
**Fix.** Export `GOCACHE`, `GOMODCACHE`, `GOPATH` and `TMPDIR` under
`/srv/sheath` (§3.2), in the build shell's profile as well as in scripts.

### The server accepts connections and never answers

**Cause.** A SQLite deadlock, relevant when working on the code: the connection
pool is capped at one connection, so querying the database while a cursor from
another query is still open blocks forever.
**Fix.** Do all database work before opening a cursor, and keep the functions
that decorate rows pure.
