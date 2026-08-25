# Rookery — Installation

How to put Rookery on a server, wire up DHCP/TFTP for netboot, bring up the blades
once over USB, and check that all of it works. It ends with the traps that this
setup has actually run into, as symptom → cause → fix.

## Conventions used in this guide

- The blade network is **`10.0.0.0/24`** throughout — an example. Any /24 works;
  substitute your own prefix everywhere `10.0.0` appears, including the
  `-net-base` flag.
- The server is called **`rookery-server`** and holds **`10.0.0.10`**, the gateway
  `10.0.0.1`.
- `d8:3a:dd:xx:xx:xx` stands for a blade's real MAC address, `10000000xxxxxxxx`
  for a CM4 serial number. Both are placeholders; nothing derives from them.
- `/srv/rookery` is the documented default for everything Rookery owns. It is a
  path, not a requirement — but the flags, the unit and the scripts all assume it.

---

## 1. Requirements

### Server

| | |
|---|---|
| Machine | Any Linux host on the blade segment. A Raspberry Pi CM4 is enough; the binaries are static arm64 or amd64 |
| OS | Debian family (`apt`, systemd). Developed against Raspberry Pi OS on Debian 13 |
| Disk | Tens of GB for the image mirror. **Not the eMMC** — see below |
| Packages | `dnsmasq`, plus for building the netboot payload: `golang` (1.24 or newer), `dosfstools`, `mtools`, `cpio`, `zstd`, `python3` |
| Ports | 67/udp DHCP, 69/udp TFTP, 53 DNS, 8080/tcp Rookery |

> **Do not install into the eMMC of a CM4.** An 8–32 GB eMMC carrying a full
> Raspberry Pi OS desktop image is already close to full before Rookery adds
> anything: Chromium and Firefox alone take ~740 MB, locales another 340 MB.
> Put `/srv/rookery` on the NVMe and leave the eMMC alone. The Go build caches
> in particular must be relocated (§3.2) — otherwise the first `go build` fills
> the eMMC.

### Network

Rookery needs a segment where **it is the DHCP server**. That is not a
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

Rookery derives addresses, MACs and names from the position of a blade. Each
BladeRunner gets a block of 20 addresses reserved regardless of its actual size,
so the addresses stay stable if the rack is later replaced by a bigger one. Five
racks fit into a /24 that way.

```text
.1              gateway
.10             rookery-server
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

---

## 3. Install the server

### 3.1 User and directories

```sh
sudo useradd --system --home /srv/rookery --shell /usr/sbin/nologin rookery
sudo install -d -o rookery -g rookery /srv/rookery/{data,images,agent,tftp,logs,build,go}
sudo install -d /etc/rookery/dhcp-hosts
sudo chown rookery:rookery /etc/rookery/dhcp-hosts
```

The layout that the flags, the unit and `tools/*.sh` expect:

```text
/srv/rookery/
├── rookery              the server binary
├── data/                SQLite database + admin-token (0600, owned by rookery)
├── images/              OS images, served to the blades over HTTP
├── agent/               rookery-agent, copied into every image offline
├── tftp/                netboot root, ~36 MB
├── logs/                dnsmasq.log — Rookery follows this file
├── build/               ramdisk and boot.img while they are being built
├── src-installer/       installer sources, if you build on the server
├── go/                  GOCACHE, GOMODCACHE, GOPATH
└── usbboot/             clone of raspberrypi/usbboot, for the bring-up
```

### 3.2 Relocate the Go caches, then build

On a CM4 this is not optional. Put it in the build shell's `~/.profile` as well:

```sh
export GOCACHE=/srv/rookery/go/cache GOMODCACHE=/srv/rookery/go/mod \
       GOPATH=/srv/rookery/go/path TMPDIR=/srv/rookery/tmp
```

```sh
cd server    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o rookery .
cd ../agent  && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o rookery-agent .
```

All static, no runtime dependencies. Sizes for orientation: server ~11.5 MB,
agent ~5.8 MB (needs about 1 MB of memory at runtime), installer ~6 MB.

```sh
sudo install -o rookery -g rookery -m 0755 server/rookery      /srv/rookery/rookery
sudo install -o rookery -g rookery -m 0755 agent/rookery-agent /srv/rookery/agent/rookery-agent
```

The agent binary goes into `/srv/rookery/agent/` because the installer copies it
straight into a freshly written root partition, offline, before the blade has
ever run its own userland.

### 3.3 The systemd unit

`/etc/systemd/system/rookery.service`:

```ini
[Unit]
Description=Rookery
Wants=network-online.target
After=network-online.target dnsmasq.service

[Service]
User=rookery
Group=rookery
WorkingDirectory=/srv/rookery
ExecStart=/srv/rookery/rookery -net-base 10.0.0
Restart=on-failure
RestartSec=5
ProtectSystem=full
ReadWritePaths=/srv/rookery /etc/rookery/dhcp-hosts
# Deliberately NOT NoNewPrivileges: the dnsmasq reload goes through sudo (§3.4).

[Install]
WantedBy=multi-user.target
```

`-net-base` is only read on the first start; afterwards the value lives in the
database as the `net_base` setting. The other defaults it assumes:

| Flag | Default |
|---|---|
| `-addr` | `:8080` |
| `-db` | `/srv/rookery/data/rookery.db` |
| `-images` | `/srv/rookery/images` |
| `-agent` | `/srv/rookery/agent` |
| `-base-url` | `http://<net-base>.10:8080` — the URL the blades use |
| `-dnsmasq-log` | `/srv/rookery/logs/dnsmasq.log` |

The service runs as the system user `rookery`, not as root. Port 8080 needs no
privileges.

### 3.4 The sudoers line

Rookery needs exactly one privileged action: telling dnsmasq to re-read its
reservations. `/etc/sudoers.d/rookery`, mode `0440`:

```text
rookery ALL=(root) NOPASSWD: /usr/bin/systemctl reload dnsmasq
```

```sh
sudo visudo -cf /etc/sudoers.d/rookery
```

Nothing else is granted. If both the direct call and the `sudo` call fail,
Rookery logs a warning and carries on — dnsmasq picks up *new* files by itself.
What is lost without the reload is the removal of entries, which is exactly the
netboot switch (§3.6).

### 3.5 dnsmasq

Three files are involved, and the split between them matters: **`rookery.conf`
is written by hand and never touched by the software; everything blade-specific
is generated into `dhcp-hosts/`.**

- `/etc/dnsmasq.d/rookery.conf` — DHCP, DNS, TFTP, netboot gating; maintained by hand
- `/etc/rookery/dhcp-hosts/` — one file per blade, **generated by Rookery**
- `/etc/sudoers.d/rookery` — the single allowed command (§3.4)

`/etc/dnsmasq.d/rookery.conf`:

```ini
# Rookery — blade segment. Blade-specific entries live in
# /etc/rookery/dhcp-hosts/ and are generated, not edited.

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

# Reservations: one file per blade, written by Rookery from the inventory.
dhcp-hostsdir=/etc/rookery/dhcp-hosts

# Netboot chain. With TFTP enabled dnsmasq points the boot server at itself;
# the boot file name is irrelevant, because the CM4 bootloader sits in EEPROM
# and asks for start4.elf on its own.
enable-tftp
tftp-root=/srv/rookery/tftp

# The netboot switch (§3.6). Unknown blades may always netboot, so that they
# can enroll; known blades only when Rookery has tagged them.
pxe-service=tag:!known,0,"Raspberry Pi Boot"
pxe-service=tag:bootnet,0,"Raspberry Pi Boot"

# Rookery reads this log — it is how a booting blade becomes visible before
# any operating system runs on it. log-dhcp is required: without it the
# vendor class is not logged, and that is what tells a netboot apart from a
# plain address lease.
log-dhcp
log-facility=/srv/rookery/logs/dnsmasq.log
```

`/etc/logrotate.d/rookery-dnsmasq`:

```text
/srv/rookery/logs/dnsmasq.log {
    daily
    rotate 7
    missingok
    notifempty
    compress
    copytruncate
}
```

`copytruncate` is deliberate: Rookery follows the file by offset, and a rename
would leave the watcher reading a file nobody writes to any more.

```sh
sudo systemctl enable --now dnsmasq
sudo systemctl enable --now rookery
```

#### What a generated reservation looks like

One file per blade, named `blade-<serial>.conf`:

```text
# Rookery – generated, do not edit by hand
# Blade 10000000xxxxxxxx  Rack rack-1  Slot 1
# boots from the NVMe
d8:3a:dd:xx:xx:xx,blade-r1s01,10.0.0.101,infinite
```

> **The trap that has already gone off:** a file in a `dhcp-hostsdir` contains
> **only** what would otherwise stand to the right of `dhcp-host=`. Write the
> prefix along with it and dnsmasq reports `bad hex constant` **in its own log
> only** and silently discards the line — the reservation has no effect and
> nothing anywhere reports an error. That is why Rookery validates every line
> before writing it: MAC format, DNS label, IPv4.

> **Second quirk of `dhcp-hostsdir`:** dnsmasq only ever *adds* entries
> dynamically. Rewriting one makes it complain `duplicate dhcp-host IP address`
> until a `SIGHUP` clears the table. That is the reason for the sudoers line:
> Rookery runs `systemctl reload dnsmasq` after every change. Skip the reload
> and the old netboot state stays in effect — which is the one thing that must
> not be stale.

### 3.6 Netboot, switchable per blade

The apparent conflict: with `BOOT_ORDER=0xf26` (NVMe first) an installed blade
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
`bootnet` is set by Rookery, by writing the blade's reservation file as either

```text
d8:3a:dd:xx:xx:xx,set:bootnet,blade-r1s01,10.0.0.101,infinite   # install
d8:3a:dd:xx:xx:xx,blade-r1s01,10.0.0.101,infinite               # boot locally
```

So the rule for the fleet is **`BOOT_ORDER=0xf62`** — network first, NVMe as the
fallback — and every blade still boots locally as long as no installation is
requested. A reimage is one click plus a reboot, with no access to the rack.

### 3.7 The netboot payload

TFTP root `/srv/rookery/tftp`, about 36 MB:

```text
start4.elf, fixup4.dat          firmware (from the server's /boot/firmware)
bcm2711-rpi-cm4.dtb, -io.dtb    device tree
overlays/                       371 files
boot.img                        ~26 MB – kernel + initramfs in one FAT image
config.txt                      boot_ramdisk=1, arm_64bit=1, uart_2ndstage=1
cmdline.txt                     console=serial0,115200 ip=dhcp debugconsole
                                bm_server=http://10.0.0.10:8080
```

`bm_server=` in `cmdline.txt` is the one thing that has to match your
installation: it is how the mini OS learns where the server is, before it has
networking of its own.

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

#### The mini OS

Raspberry Pi's own netinstall `boot.img` boots fine on a CM4, but it shows the
menu-driven Imager on HDMI and never asks any API. Rookery ships its own image
with `rookery-installer` instead.

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

Keep the original around as `/srv/rookery/boot.img.rpi-netinstall.bak`.

#### `rookery-installer`

Go, static, ~6 MB, sources under `installer/`. Deliberately self-contained: the
initramfs ships neither `curl` nor `zstd`, `xz` or `resize2fs`, so HTTP,
decompression (xz/zstd/gz), writing and mounting all happen inside the program.

1. Serial number from the device tree, MAC from `/sys/class/net/eth0/address`
2. Server address from `bm_server=` in `/proc/cmdline` — the only way to hand
   the mini OS anything before it has networking; `cmdline.txt` comes over TFTP
3. Wait for the server to answer, up to 3 minutes (mind portfast)
4. `POST /api/v1/provision/{serial}` with the MAC. While the server answers
   `waiting`, it keeps asking and the screen says an image has not been picked
5. On `go`: fetch the image in a single pass → decompress → write to
   `/dev/nvme0n1`. Nothing is buffered; a 6 GB image fits neither in the memory
   of a CM4 nor on any scratch space it has
6. SHA-256 is computed along the compressed stream and verified
7. `BLKRRPART` + `partprobe`, mount the largest partition as the root, drop the
   seed `/etc/rookery/agent.env` (server, serial, token). If an agent binary
   sits in `/srv/rookery/agent/`, it is installed at the same time and enabled
   by a symlink in `multi-user.target.wants` — enabling a unit without a running
   systemd is exactly that symlink
8. Report progress, reboot

**No `resize2fs`:** the Pi images grow their own root on first boot.

On failure the blade stops with a message instead of dropping into a boot loop.
`debugconsole` in `cmdline.txt` keeps a getty alongside, so you can get a shell
without pulling the blade.

#### Rebuilding the payload

`tools/build-bootimg.sh` does the whole chain — installer → ramdisk →
`boot.img` → TFTP root — on the server itself, so the arm64 build is native:

```sh
sudo -u rookery tools/build-bootimg.sh        # build and publish
BUILD_ONLY=1 tools/build-bootimg.sh           # build, do not publish
```

It is strict on purpose (`set -euo pipefail`, build to `.new`, publish last): a
failed build must never leave a stale `boot.img` in the TFTP root. That has
happened once, and the blade then netbooted yesterday's installer while the log
claimed today's.

Sources are expected in `/srv/rookery/src-installer`, the unpacked ramdisk in
`/srv/rookery/build/rootfs`. `ROOKERY_ROOT` and `INSTALLER_SRC` override both.

### 3.8 First start and the admin token

On its first start the server creates the database and generates an admin token
into `/srv/rookery/data/admin-token`, mode `0600`, owned by `rookery`. Reading it
therefore needs `sudo` — a plain `cat` as an ordinary user only yields
`Permission denied`:

```sh
sudo cat /srv/rookery/data/admin-token
```

- Web interface: `http://10.0.0.10:8080/` — log in with that token
- API: `http://10.0.0.10:8080/api/v1/` — same token as a bearer token

A browser form cannot send an `Authorization` header, so `/login` exchanges the
token once for a session cookie: HttpOnly, SameSite=Lax, 12 hours, held in
memory. Restarting the service logs everyone out. A wrong token costs one second
of delay, which is enough to make brute force uninteresting.

To rotate the token, write a new value into the file and restart the service; if
the file is missing at start, a fresh token is generated.

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
BOOT_ORDER=0xf62                 # network → NVMe → loop (nibbles read right to left)
NET_BOOT_MAX_RETRIES=1           # keep this low, see below
TFTP_PREFIX=0                    # directory = lower 4 bytes of the serial number
MAC_ADDRESS=02:b1:ad:01:00:03    # optional: the slot's deterministic MAC
```

| Value | Factory | Meaning |
|---|---|---|
| `BOOT_ORDER` | `0xf641` | `0xf62` = network → NVMe → loop; `0xf26` = NVMe first |
| `NET_BOOT_MAX_RETRIES` | `0` | Retries after a TFTP timeout |
| `DHCP_TIMEOUT` | `45000` ms | How long the bootloader waits for a DHCP reply |
| `TFTP_PREFIX` | `0` | Serial-number subdirectory, with fallback to the TFTP root |
| `MAC_ADDRESS` | unset | Overrides the factory MAC |

Which boot order you choose follows from §3.6:

- **`0xf62` (network first)** is the one that fits this design. Every ordinary
  start makes one netboot attempt, but with option 43 withheld that attempt
  fails immediately and the blade continues from the NVMe. In exchange, a
  reimage never needs a hand in the rack.
- **`0xf26` (NVMe first)** boots an installed blade without any attempt at all,
  but a reimage then means wiping the NVMe on site. Use it only where the
  network is not trusted to answer.

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

**The services are up.**

```sh
systemctl status rookery dnsmasq
curl -s http://10.0.0.10:8080/healthz
```

**dnsmasq accepts its configuration.**

```sh
dnsmasq --test
sudo grep -iE 'bad hex|duplicate dhcp-host' /srv/rookery/logs/dnsmasq.log
```

The grep must stay empty. It is the only place where a rejected reservation ever
shows up.

**TFTP serves the payload, byte for byte.**

```sh
tftp 10.0.0.10 -c get boot.img /tmp/boot.img
cmp /tmp/boot.img /srv/rookery/tftp/boot.img
```

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
insert a blade into a slot, then:

```sh
ls /etc/rookery/dhcp-hosts/
sudo journalctl -u dnsmasq -n 20
```

A file `blade-<serial>.conf` appears, dnsmasq reloads, and no error follows.

**The constraints hold.** These are worth trying once, because the messages tell
you the model is being enforced rather than guessed:

- a rack size other than 4, 10 or 20 is rejected
- slot 9 in a 4-slot rack → `Slot 9 is outside the rack (1..4)`
- occupying an occupied slot → rejected, naming the current occupant
- shrinking a rack while a slot beyond the new size is occupied → rejected
- deleting a rack that still holds a blade → rejected
- "Reimage" without an assigned image → rejected
- without a login, `/` redirects to `/login` and the API answers 401

**A blade actually boots.** Power one on and watch the overview: it should move
through `dhcp` → `tftp` → `ramdisk` and then wait for an image.

---

## 6. Operate

### The workflow

**First create a BladeRunner, then insert blades into its slots.**

- **Overview `/`** — a form to create a BladeRunner (name, 4/10/20 slots, site),
  with the next free address block shown next to it, so the addresses are known
  before the rack exists. One tile per rack with occupancy, address block and
  site. Below that, **blades without a slot**: devices that have reported in but
  do not sit anywhere yet, served from the dynamic pool.
- **Rack page `/bladerunners/{id}`** — the slot table across the full rack size.
  Free slots show the *planned* IP and MAC, so you see what a blade will get
  before inserting it, plus a select box of unplaced blades. Occupied slots show
  hostname, serial, IP, MAC, image, status LED and the time of the last report,
  with **Identify**, **Reboot**, **Reimage** and **Remove**.

Every action is a POST followed by a redirect, so a reload never triggers
anything twice.

### Watching a blade boot

Rookery follows the dnsmasq log and sees a blade starting **before any operating
system runs on it**. Two sources:

1. **Passive, immediately** — the DHCP request, the vendor class, and every TFTP
   file served. MAC, IP and progress follow from that.
2. **Active, shortly after** — the mini OS reports in with its serial number via
   `POST /api/v1/provision/{serial}` and is matched to the session by MAC.

Stages: `dhcp` → `tftp` → `ramdisk` (boot.img served) → `installer` → `writing`
→ `done`. From `ramdisk` on, a device counts as **waiting** and the image
selection appears in the overview. The choice is remembered on the session and
the installer picks it up the next time it asks. Until then the API answers
`{"status":"waiting","retry_after":5}` — **200, not 409**: waiting is a regular
state of the process, not an error.

> **The distinction that saves the most time:** the RPi bootloader identifies
> itself over DHCP with the vendor class
> `PXEClient:Arch:00000:UNDI:002001`; an ordinary Linux client does not. That is
> how Rookery knows whether a blade even *wanted* to netboot. A device that only
> took an address is shown as such, with a note that `BOOT_ORDER` is probably
> still at the factory value. Without that distinction you go hunting for a
> fault in TFTP for a blade that never asked.

### Images

Images live in `/srv/rookery/images/` and are served over HTTP. Two helpers:

```sh
tools/mirror-image.sh                                     # fetch into the catalogue
tools/prepare-image.sh debian-13-arm64 openssh-server     # prepare before first use
```

`prepare-image.sh` installs the packages that have to be present at first boot
and clears the identity the image was built with. It matters because the Debian
raspi image ships neither `openssh-server` nor cloud-init: a blade installed
from it has exactly one door, the Rookery agent — and one door is one too few
when the question is why the agent did not start. Doing it once per image also
beats doing it once per blade. The host has to match the image's architecture;
the work happens in a chroot without emulation.

### The agent

Runs as a systemd service on every blade, ~5.8 MB, ~1 MB of memory. Its
credentials were placed by the installer in `/etc/rookery/agent.env`; the unit
pulls them in with `EnvironmentFile`.

Every 60 seconds:

1. **Report** facts and health to `POST /status`
2. **Fetch commands** — `identify`, `reboot`, `reimage`
3. **Reconcile configuration** — `GET /config` with `If-None-Match`; on 304
   nothing happens

The order is deliberate: report first, then act, so the server is up to date even
when applying fails afterwards. A random offset at startup keeps twenty blades
from hitting the server in lockstep after a power cut.

| Facts | Health |
|---|---|
| `os_id`, `os_version_id`, `os_family` | SoC and NVMe temperature |
| `init`, `pkg_mgr`, `net_backend`, `boot_path` | load, uptime, memory |
| kernel, arch, model, serial | fill level of `/` |
| | `vcgencmd get_throttled` → undervoltage, throttling |
| | fan speed, if `compute-blade-agent` is running |

On a PoE-powered blade `throttled` is the single most useful value: it reveals
undervoltage and thermal throttling, which are exactly the two things that go
wrong under load.

It applies hostname (including `127.0.1.1` in `/etc/hosts`), time zone, SSH keys
for `root` and the first regular account, managed files, packages (with `per_os`
exceptions) and systemd units. For example, an SSH key in the configuration is
given verbatim:

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
run tries again. For diagnosis without a server, `rookery-agent -show` prints
facts and health as JSON.

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
rsync -az server/ rookery-server:~/rookery-src/
ssh rookery-server '
  export GOCACHE=/srv/rookery/go/cache GOMODCACHE=/srv/rookery/go/mod \
         GOPATH=/srv/rookery/go/path TMPDIR=/srv/rookery/tmp GOFLAGS=-mod=mod
  rsync -a ~/rookery-src/ /srv/rookery/src/
  cd /srv/rookery/src && go build -trimpath -ldflags="-s -w" -o /tmp/rookery.new .
  sudo systemctl stop rookery
  sudo cp /tmp/rookery.new /srv/rookery/rookery
  sudo chown rookery:rookery /srv/rookery/rookery
  sudo systemctl start rookery'
```

Restarting invalidates all sessions — log in again with the admin token.

The server is one binary, SQLite, no runtime dependencies:

| File | Contents |
|---|---|
| `db.go` | schema, racks, blades, images |
| `ipam.go` | address plan, dnsmasq reservations, network self-check |
| `netboot.go` | dnsmasq log watcher, boot sessions |
| `api.go` | REST endpoints, auth, config merge |
| `session.go`, `ui.go`, `i18n.go` | login, rack views, translations |
| `main.go` | routing, service, offline detection |

---

## 7. Troubleshooting

Each entry is a symptom that has actually occurred, its cause, and the fix.

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

**Fix.** Use an image with the downstream Raspberry Pi kernel — the Ubuntu
preinstalled server images do, and there the same setting works — or drive the
blade without fan telemetry. SoC temperature is read from sysfs and keeps
working either way.

**Note.** Debian also regenerates `config.txt` on kernel updates and says so in
the file's first line. The agent therefore writes boot settings to
`/etc/default/raspi-firmware-custom` as well, where they survive.

### A blade keeps getting a pool address although a reservation exists

**Cause.** The reservation line was rejected. A file in `dhcp-hostsdir` holds
only the part right of `dhcp-host=`; with the prefix included, dnsmasq logs
`bad hex constant` in its own log and drops the line silently.
**Fix.** `sudo grep -i 'bad hex' /srv/rookery/logs/dnsmasq.log`, then correct the
file. Rookery validates MAC, DNS label and IPv4 before writing, so a rejected
line usually means the file was edited by hand.

### An installation was requested, but the blade boots from its NVMe anyway

**Cause.** The `set:bootnet` tag never reached dnsmasq — either the reservation
was rewritten without a reload (dnsmasq only *adds* host records dynamically), or
the reload itself failed.
**Fix.** `sudo python3 tools/pxeprobe.py <mac> eth0` shows what the bootloader
would be offered. If the entry is missing, `sudo systemctl reload dnsmasq` and
check `journalctl -u rookery` for `dnsmasq not reloaded` — that points at the
sudoers file (§3.4) or at `NoNewPrivileges` having crept into the unit.

### dnsmasq logs `duplicate dhcp-host IP address`

**Cause.** Same root: entries read from a `dhcp-hostsdir` are added, never
replaced, until a `SIGHUP` clears the table.
**Fix.** Reload. If it recurs on every change, the sudoers rule is not working.

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

**Cause.** A build failed halfway and left a stale `boot.img` in the TFTP root.
**Fix.** Rebuild with `tools/build-bootimg.sh`, which builds to `.new` and
publishes only at the end. Never copy an image into `tftp/` by hand mid-build.

### The blade shows a graphical Imager menu on HDMI

**Cause.** Raspberry Pi's netinstall `boot.img` is still in the TFTP root. It
boots, but it never asks the API.
**Fix.** Publish the Rookery payload (§3.7).

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
the queue: the server hands out no command older than **15 minutes** and logs the
expiry; open commands of the same kind are **replaced rather than stacked**
(three `identify` clicks are one entry); the agent checks the age itself, takes
only the newest of each kind, and sorts reboots to the end. Orphaned commands of
deleted blades are cleared at server startup.

### `cat /srv/rookery/data/admin-token` says `Permission denied`

**Cause.** The file is mode `0600` and owned by `rookery`. That is intended.
**Fix.** `sudo cat /srv/rookery/data/admin-token`.

### The overview shows no boot activity at all

**Cause.** Rookery is not seeing the dnsmasq log: `log-facility` missing or
pointing elsewhere, `log-dhcp` not set (then the vendor class never appears), the
`-dnsmasq-log` flag disagreeing with the config, or logrotate renaming the file
instead of truncating it — the watcher keeps its offset in a file nobody writes
to any more.
**Fix.** Align the two paths, keep `log-dhcp`, and keep `copytruncate` in the
logrotate stanza (§3.5).

### The freshly installed blade does not boot from its NVMe

**Cause.** The image's kernel has no built-in NVMe driver and the distribution
builds no initramfs — DietPi is the usual candidate.
**Fix.** Test that per distribution before putting it in the catalogue. The mini
OS itself is unaffected: its kernel carries NVMe and ext4 built in and loads no
modules at all.

### The disk fills up during a build

**Cause.** The Go build caches went to the home directory on the eMMC.
**Fix.** Export `GOCACHE`, `GOMODCACHE`, `GOPATH` and `TMPDIR` under
`/srv/rookery` (§3.2), in the build shell's profile as well as in scripts.

### The server accepts connections and never answers

**Cause.** A SQLite deadlock, relevant when working on the code: the connection
pool is capped at one connection, so querying the database while a cursor from
another query is still open blocks forever.
**Fix.** Do all database work before opening a cursor, and keep the functions
that decorate rows pure.
