# Rookery – Architecture proposal (v2)

Management software for 10 Compute Blades (Raspberry Pi CM4) in the BladeRunner rack.

**Requirements**

1. Blades receive their configuration over the network.
2. Different distributions per blade (e.g. DietPi, Debian 13 Trixie, Ubuntu 24.04).
3. A blade netboots into a mini OS that writes the assigned image to the NVMe.
4. Every blade gets a fixed IP address.
5. After that, the application continuously monitors the state of the blades.
6. Manage several racks with 4, 10 or 20 blades.

From this follows an architecture along the **lifecycle**, not along features:

```
   Provision  →  Configure  →  Operate & Monitor  →  Re-provision
   (Netboot)      (Agent)         (Health)            (Reimage)
```

---

## 1. The central decision

Version 1 of this design had "one golden image for all blades" as its guiding idea.
**That one falls.** In its place comes:

> The server owns an **image catalogue**. The distribution is an **attribute of the blade**,
> not a property of the installation.

`blade-03.image = dietpi-arm64` is thus an assignment in a database — no build process,
no second workflow, no special case. Multi-distro stops being an image problem and becomes
one line of configuration. That is exactly why requirement 2 and requirement 3 fit together so well:
without a netboot installer, multi-distro would be ten times the manual work; with it, it is a dropdown.

---

## 2. What the hardware gives us

| Feature | Detail | Consequence |
|---|---|---|
| CM4 serial | unique, from the device tree `serial-number` | **Primary key** of a blade — not MAC, not IP |
| PoE+ | 40–57 V via the switch port | Power cycle by switching the port off — remote reset without a BMC |
| NVMe M.2 | up to 22110 | Target of the installation; `BOOT_ORDER` 0x6 |
| Netboot | RPi bootloader: DHCP + TFTP, `BOOT_ORDER` 0x2 | Carries the mini OS |
| 2× RGB LED | NeoPixel on `GPIO18` | Health display on the faceplate + identify |
| Button | `GPIO20` | Slot assignment, acknowledging enrollment |
| Fan unit port | `GPIO12/13` = PWM0/1 or UART5 | Smart Fan Unit (RP2040 + EMC2101): RPM, exhaust temperature |
| PoE indicator | `GPIO23` | Part of the health model |
| EEPROM `BOOT_ORDER` | nibbles read from right to left | `0xf26` = NVMe → network → loop. **CM4 factory: `0xf641`** |

**Important:** `BOOT_ORDER=0xf26` means: try the NVMe first, fall through to netboot if the NVMe is
empty, otherwise start over. A fresh NVMe therefore ends up at the installer **by itself** —
without switching the EEPROM, without a button, without a cable. That is the pivot of the whole
provisioning.

---

## 3. Pillar 1 — Provisioning: netboot → mini OS → NVMe

### Sequence

1. **The blade boots.** NVMe empty or unreadable → the RPi bootloader falls through to netboot.
2. **DHCP.** The blade gets an address and the boot parameters (TFTP server).
3. **TFTP.** The bootloader loads from a directory named after the serial number:
   kernel, device tree, `cmdline.txt` and a **mini initramfs**.
4. **The mini OS runs.** It reads the CM4 serial number and asks the server:
   `POST /api/v1/provision/{serial}` → response: image URL, SHA-256, partition layout, seed data.
5. **Writing.** `curl … | zstd -d | dd of=/dev/nvme0n1`, then verify the checksum and
   expand the root partition to the size of the disk.
6. **Seeding (offline).** The installer mounts the freshly written root partition and puts the
   agent binary, systemd unit, server URL and blade token straight into it.
7. **Reboot.** Now the NVMe boots. The agent reports in to the server, the blade becomes `online`.

### The iron rule: nothing large over TFTP

That is not a matter of style; it has three documented reasons:

- **Hard size limit.** The bootloader's TFTP block counter overflowed at 64 K blocks — with a
  block size of 1468 that is around **96–114 MB**. Only fixed with the bootloader from
  **2025-09-23** (rpi-eeprom issue #720). An initramfs that is too large simply fails on older bootloaders.
- **No retry.** `NET_BOOT_MAX_RETRIES` is **0** by factory default: after a TFTP timeout the
  netboot does not restart, the blade stays dark (issue #687). Set it to a value > 0.
- **No windowing.** The client does use `blksize` up to 1468, but no RFC 7440 `windowsize` —
  one block per round trip. That is the reason for the slowness.

> **Only** kernel, DTB and initramfs go over TFTP. The image comes **over HTTP**, from within the
> already running mini OS.

No NFS root, no image over TFTP. That is the difference between "works in the lab" and
"works on ten blades at the same time".

### TFTP directory per blade

`TFTP_PREFIX=0` (factory setting) makes the bootloader look in a subdirectory named after the
**lower four bytes of the serial number** — eight hex digits, not the full
16-digit serial:

```
/tftpboot/9ffefdef/{start4.elf, fixup4.dat, config.txt, cmdline.txt,
                    bcm2711-rpi-cm4.dtb, overlays/, kernel8.img, initramfs}
```

Watch out for the pitfalls: if the bootloader finds no `start4.elf` there, it **silently discards
the prefix** and loads from the TFTP root — an empty blade directory therefore does not lead to an
error, but to the wrong boot. `TFTP_PREFIX=2` would group by MAC address instead.

### Network pitfalls

- **STP on managed switches** is the classic netboot killer: after link-up the switch checks for
  loops for around 30 seconds, which makes the boot take minutes or fail (issue #480).
  **Set portfast / edge port on all blade ports.**
- TFTP is unsecured and belongs exclusively in the management VLAN.
- Netboot only works over the built-in Ethernet adapter.

### The mini OS: do not build your own

Raspberry Pi already ships exactly this component. The project `raspberrypi/usbboot` contains a
**`scriptexecute` initramfs**: a minimal Linux that starts, runs a script handed to it
and then exits. It is exactly the "utility operating system image (a Linux kernel and a
script-execute initramfs)" that Raspberry Pi describes in its own provisioning documentation —
and the basis that `cmprovision` works on as well.

**Recommendation:** adopt `scriptexecute` as the mini OS and only replace the script — instead of a
fixed provisioning run it calls our `rookery-installer` (a small static Go binary that
speaks the same REST API as the agent). That removes the entire effort of a custom
Buildroot or Alpine initramfs.

As a fallback for a truly dead blade the USB route remains: `mass-storage-gadget` from
the same project exposes NVMe/eMMC as USB mass storage and writes faster than
classic `rpiboot` while doing so. But that needs a cable and a human at the rack — for the normal case
the network route is there.

### Reinstalling a running blade

The CM4 does **not** know a one-time netboot: according to the documentation, `set_reboot_order` is
explicitly "Raspberry Pi 5 only". Two ways remain, both without touching the EEPROM:

- **`kexec`** — the agent fetches the installer kernel and initramfs over HTTP and jumps straight into it.
  Bypasses the bootloader and TFTP entirely. Prerequisite: `CONFIG_KEXEC` in the kernel of the respective
  distribution — so with three distributions, three things to check.
- **`tryboot`** — a real one-shot in the firmware: `reboot '0 tryboot'` loads `tryboot.txt` once
  instead of `config.txt`, and the flag is cleared before the start. A crash automatically leads back
  to the normal configuration. For that, however, the installer has to live on the NVMe boot partition,
  not on the network.

**Recommendation:** `kexec` as the primary route, `tryboot` as the fallback where `kexec` is not available.
From the rack's point of view nothing visible happens in either case — except that the LED switches to
*provisioning*.

---

## 4. Pillar 2 — Several distributions: exactly three extension points

Multi-distro gets expensive when distro knowledge seeps through the whole codebase. It stays cheap
if you lock it into named places. There are exactly three.

### (A) Image catalogue — data, not code

```yaml
images:
  ubuntu-24.04-arm64:
    url:    http://rookery.lan/images/ubuntu-24.04-cm4.img.zst
    sha256: 9f2c…
    boot_part: 1          # FAT
    root_part: 2
    seed:   generic
  debian-13-arm64:
    url:    http://rookery.lan/images/debian-13-cm4.img.zst
    seed:   generic
  dietpi-arm64:
    url:    http://rookery.lan/images/dietpi-cm4.img.zst
    seed:   generic
```

Adding a new distribution means: download the image, enter the checksum, done.

### (B) Seed adapter in the installer — ~20 lines per distribution

After writing, the installer puts down the initial configuration. This is where the actual trick lies:

> The installer has the root partition mounted and writable anyway.
> It can copy the agent **straight into it** — binary, systemd unit, token.

That means **no** cloud-init, **no** `dietpi.txt`, **no** `sysconf.txt` is needed. The entire
day-0 bootstrap, which in version 1 was still the most distro-dependent part, shrinks to a
file copy operation that works identically on every systemd distribution. That is the
`generic` adapter, and it is the recommended default route for all three distributions.

Concretely, these are four file operations in the mounted root partition:

```sh
install -m755 rookery-agent  $ROOT/usr/local/bin/
install -m644 rookery.service $ROOT/etc/systemd/system/
install -m600 blade-token.env     $ROOT/etc/rookery/
# Enable a unit without a running systemd = symlink by hand:
ln -s /etc/systemd/system/rookery.service \
      $ROOT/etc/systemd/system/multi-user.target.wants/rookery.service
```

The last step is the only one that is not obvious: `systemctl enable` creates nothing
more than exactly this symlink, and you can set it yourself offline. Because the agent binary is
statically linked, the same procedure works equally on glibc and musl systems.


The distro-native adapters (`cloud-init`, `dietpi`) remain as an option — only useful if you want to
take along distro-specific features, such as DietPi's software installer.

### (C) Platform adapter in the agent — one Go interface

```go
type Platform interface {
    Facts() Facts                       // what am I?
    SetHostname(string) error
    EnsureUser(User) error
    EnsurePackages([]string) error
    WriteNetwork(NetworkSpec) error
    RestartUnit(string) error
}
```

A base implementation `debianFamily` (apt, systemd, `useradd`, `/etc/sudoers.d/`) covers all
three distributions named. DietPi inherits from it and overrides only its peculiarities.

### Facts: the blade says what it is

On every heartbeat the agent reports from `/etc/os-release` plus a bit of probing:

```yaml
os_id:        dietpi | debian | ubuntu
os_version_id: "13" | "24.04"
os_family:    debian
init:         systemd
pkg_mgr:      apt
net_backend:  networkd | ifupdown | netplan | NetworkManager
boot_path:    /boot | /boot/firmware
```

With that the server can tailor configuration by distribution, and the rack view shows per
slot what is running there. Package names get an escape hatch in case they diverge:

```yaml
packages:
  - name: htop
  - name: linux-cpupower
    per_os: {ubuntu: linux-tools-common, debian: linux-cpupower}
```

### What the three images actually bring along

The following details come from a section of the real Ubuntu image (FAT32 parsed directly,
ext4 via `debugfs`, DTBs decompiled) as well as from the package and documentation sources of the other two.

| | Ubuntu 24.04 | Debian 13 | DietPi |
|---|---|---|---|
| Boot partition | FAT32, 512 MiB, label `system-boot` | FAT32 | FAT32 |
| mounted at | `/boot/firmware` | `/boot/firmware` | `/boot` |
| Root partition | ext4, label `writable` | ext4 | ext4 |
| Native seed on the boot partition | cloud-init NoCloud: `user-data`, `meta-data`, `network-config` | **none** (newer images) | `dietpi.txt` + `Automation_Custom_Script.sh` |
| Network backend | netplan → systemd-networkd (NetworkManager is not installed at all) | ifupdown / networkd | ifupdown, managed by DietPi |
| SSH password login out of the box | **off** (`ssh_pwauth: false`) | to be checked | — |

The conclusion of the research in one sentence: **a clean common denominator across all three does
not exist.** Ubuntu has a real, documented seed on the boot partition; newer Debian images
have none at all and require mounting the root partition anyway; DietPi has its own,
very powerful one — which, however, keeps writing back.

That is exactly the justification for the `generic` adapter. It sidesteps the problem instead of
solving it three times: whoever writes into the root partition anyway does not need the distribution's
seed mechanism at all.

### Two confirmations for the approach

**A simple `dd` is enough.** Ubuntu's `cmdline.txt` looks for the root via
`root=LABEL=writable … rootwait` — that is, medium-agnostic. The image can be written unchanged to the
NVMe and boots from there without anything having to be adjusted.

**The CM4 needs no `config.txt` change for NVMe.** In the decompiled
`bcm2711-rpi-cm4.dtb` the PCIe node has **no `status` property**, so it is `okay` by default.
The shipped `config.txt` consequently contains not a single NVMe or PCIe line.
`dtparam=nvme` and `PCIE_PROBE=1` are **Pi 5 topics** — on the Pi 5 the DTB says `status = "disabled"`.
For us that means: normally the installer does not have to touch the boot partition at all.

### Special case DietPi

DietPi manages a lot itself — `dietpi-config`, `dietpi-software`, its own network and
hostname handling, `dietpi-update`. An external configuration management that touches the same files
leads to a wrestling match that both sides lose.

> **Rule: the agent touches nothing that DietPi owns.**

**More important than that, however, is an open risk:** by default DietPi builds **no initramfs**
(`SKIP_INITRAMFS_GEN=yes`). Ubuntu loads the NVMe driver as a module from the initrd — if the initrd is
missing, NVMe has to be **compiled into the kernel**, otherwise DietPi does not find its root and does not
boot at all. Whether the DietPi RPi kernel does that is unverified.

> **This has to be tested before anything else:** write a DietPi image to an NVMe and see
> whether it boots. If the test comes out negative, DietPi either needs a forced initramfs or
> it drops out as an NVMe distribution. The rest of the architecture would be unaffected by that —
> it would be one line less in the image catalogue.

Concretely: leave the network on DietPi to DietPi (the DHCP default is enough, because the fixed address
comes from the reservation anyway — see pillar 3). The agent limits itself to hostname,
SSH keys, its own files and its own units.

### An honest assessment of the effort

All three distributions named are **Debian family**: `apt`, `dpkg`, systemd. The effort is
therefore considerably smaller than "multi-distro support" sounds — essentially a base class plus
a handful of DietPi exceptions. It would only get really expensive with Alpine (musl, OpenRC, apk), Fedora (dnf)
or Talos (no shell at all). The interface above keeps the door open for that without having to pay
for it today.

---

## 5. Pillar 3 — Fixed IP addresses and IPAM

### Correcting an obvious assumption

One assumes that the RPi bootloader can only do DHCP when netbooting. **That is not true.** Since the
bootloader from 2020-03-11 there has been a static configuration in the EEPROM:

```ini
CLIENT_IP=10.10.0.13
SUBNET=255.255.255.0
GATEWAY=10.10.0.1
TFTP_IP=10.10.0.1        # if these are set, DHCP is skipped
```

So there are three ways to a fixed address, not two. The decision still comes out clearly:

| Route | Centrally manageable | DHCP needed | Assessment |
|---|---|---|---|
| **DHCP reservation** | yes | yes | **Recommendation** |
| Static in the EEPROM | no — per device, only via `rpiboot` | no | Robust, but every change means: open the rack |
| Static in the OS | yes, but distro-dependent | no for the OS, yes for netboot | Worst route — see below |

The argument against the EEPROM is not a technical one but an operational one: the address would then live in
ten individually generated EEPROM images, and after enabling the write protection it would be nailed down.
A management software that is supposed to manage addresses can do nothing with that.

### The recommendation

> **Rookery owns the address management** and generates DHCP reservations from it.
> `slot 3 → 10.10.0.13 → dhcp-host=<mac>,10.10.0.13,blade-03`

The second, more important reason lies in the distribution topic:

> Network configuration is the most distro-divergent part of all — netplan on Ubuntu,
> `/etc/network/interfaces` on Debian and DietPi, plus systemd-networkd and NetworkManager. If
> the fixed address comes from the reservation, **every** distribution simply does DHCP, and this
> whole block of topics disappears from the agent.

The decision for reservations therefore not only buys clean addresses, it also takes
the most expensive chunk out of extension point C.

**Doing both — reservation *and* static in the OS — is the worst route.** The reservation
degenerates into documentation, the OS ignores it, and whoever changes only the reservation produces an
address that exists nowhere. On top of that, `ip=dhcp` in the `cmdline.txt` collides with a statically
configured OS network. One truth, not two.

### Proxy DHCP is not enough — the router has to give up the blade range

The obvious wish: dnsmasq only does the boot part, the existing router keeps handing out addresses.
**That does not work.** The dnsmasq documentation is unambiguous: in proxy mode there is "another DHCP server
on the network responsible for allocating IP addresses" — there dnsmasq supplies exclusively the
PXE information and hands out **no addresses at all**. `dhcp-host` has no effect whatsoever on
address allocation in proxy mode.

So there is no "dnsmasq does boot *and* fixed IPs, the router does the rest". It follows:

> **The blades go into their own VLAN, and there dnsmasq is the authoritative DHCP server.**
> The router in the house network stays untouched, because it is not responsible for that segment at all.

That is the recommendation anyway — TFTP is unsecured and does not belong in the house network. A
middle route would be `dhcp-range=<subnet>,static`: dnsmasq as a full DHCP server without a dynamic
pool, answering exclusively to known MAC addresses. But two DHCP servers in the same segment
remain a risk; a separate VLAN is cleaner.

### Changing reservations at runtime

Rookery writes the reservations from the inventory. dnsmasq can read them in without a restart:

```
dhcp-hostsdir=/etc/dnsmasq.d/blades/     # one file per blade
```

dnsmasq reads new or changed files in this directory **by itself**, without a signal. One
drawback: deleted or rewritten entries only disappear with a `SIGHUP`, because
host records are only *added* dynamically. Rookery therefore additionally sends a
`systemctl reload dnsmasq` after every removal or rewrite. DNS records for `blade-03.blades.lan` fall
out of the same instance as a by-product.

### Chicken and egg: the MAC is unknown beforehand

The obvious shortcut is **blocked**. On the Pi 3 and older the MAC could be derived from the
serial number; from BCM2711 on — that is, the CM4 — that has been dropped without replacement. The Raspberry Pi documentation
says so explicitly: "the MAC address is programmed at manufacture and there is **no link** between
the MAC address and serial number."

There are three better ways out:

1. **Assign the MAC yourself.** The EEPROM property `MAC_ADDRESS` overrides the factory-programmed
   address. During the one-time bring-up (see §6) Rookery can assign every slot a
   deterministic MAC — for instance `02:blade:00:00:00:03` for slot 3, from the
   locally administered range. That makes MAC, IP and slot known before the first boot, and the
   chicken-and-egg problem no longer exists. **That is the most elegant solution.**
2. **A small dynamic pool.** An unknown blade gets a loan address, netboots, enrolls
   itself with serial number and MAC — from the next boot on, the reservation applies. Works always
   and without preparation.
3. **Read the serial number from DHCP option 97.** The bootloader puts it in there: FourCC
   `RPi4`, board revision, lower four bytes of the MAC and the four bytes of the serial number. The documentation names
   exactly that as its purpose — being able to identify Pis "without relying upon the Ethernet MAC OUID".
   dnsmasq cannot derive an address from it, though (it matches on MAC or option 61); for that
   you would need Kea with `flex-id`. As a source of observation it is useful nonetheless.

**Recommendation:** route 1, with route 2 as a safety net for blades that end up in the rack without a bring-up.

---

## 6. The one-time bring-up — the one piece of manual work that remains

Everything described so far runs without rack access. Exactly **one** step does not, and it should
be planned deliberately instead of being discovered: the factory setting of the CM4 is `BOOT_ORDER=0xf641`
(USB-MSD → NVMe → SD/eMMC), not the desired `0xf26`. As long as that stays this way, an empty
blade does not fall through to netboot.

Changing the EEPROM from the running system is not the intended route on the CM4:
`rpi-eeprom-update` is **disabled there out of the box** (enabled via
`CM4_ENABLE_RPI_EEPROM_UPDATE=1`), and the documentation warns that the self-update is **not atomic** —
a power failure during it damages the EEPROM. For a fleet that is not a routine tool.

**So once per blade, via USB cable and `rpiboot`, before it goes into the rack:**

```ini
BOOT_ORDER=0xf26            # NVMe → network → loop
NET_BOOT_MAX_RETRIES=3      # factory is 0: no retry after a TFTP timeout
MAC_ADDRESS=02:b1:ad:00:00:03   # deterministic from the slot — solves chicken-and-egg (§5)
TFTP_PREFIX=0               # directory = lower 4 bytes of the serial number
```

That takes a few minutes per blade and is then never needed again. Rookery can generate the
EEPROM configuration from the inventory, so that the bring-up is one command
(`bmctl eeprom-config --slot 3`) and not hand editing.

After that, the following holds for the entire remaining lifecycle: plugging it in is enough.

**Mind the bootloader version:** for initramfs files beyond roughly 96 MB you need a
bootloader from **2025-09-23 or newer** (§3). Update it right away during the bring-up anyway.

---

## 7. Pillar 4 — Configuration during operation

Unchanged from version 1, because it fits well with the rest:

- **Pull instead of push.** The agent asks every 60 s for its desired state, applies it idempotently
  and reports back. No SSH access on the server, works with DHCP, converges by itself after every
  reboot.
- **Three levels**, merged: `global → group → blade`.
- **Hash comparison.** The server delivers the merged config with a hash; the agent reports the
  applied hash back. Drift detection falls out of that at no extra cost.
- **Hardware stays with the `compute-blade-agent`** (Go, Apache-2.0): LEDs, fan, button,
  critical mode on overtemperature, Prometheus on `:9666`, gRPC with mTLS. Rookery only writes
  its `config.yaml` and calls its API for *identify*. Its installer already detects
  dpkg/dnf/pacman and therefore fits all three distributions without modification.

---

## 8. Pillar 5 — Status monitoring

### Two independent observation paths

The decisive point: **a dead blade cannot report its own death.** That is why
two directions of view are needed.

**Inside view** — the agent reports every 60 s:
SoC temperature, fan speed, exhaust temperature, uptime, load, disk usage, NVMe SMART,
config hash, agent version, PoE indicator, optional service checks.

**Outside view** — the server observes from the outside:
age of the last heartbeat, ICMP/TCP probe, active DHCP lease and — if the switch
supports it — link status and PoE power of the port via SNMP.

### Health model: what "everything ok" means

| Check | Source | warn | crit |
|---|---|---|---|
| Heartbeat | server | > 90 s | > 5 min |
| Config sync | hash comparison | drift | apply error |
| SoC temperature | agent | > 70 °C | > 80 °C |
| Fan | agent | speed deviates | 0 rpm with target > 0 |
| NVMe SMART | agent | `percentage_used` > 80 % | media errors |
| Disk usage `/` | agent | > 85 % | > 95 % |
| PoE | agent / SNMP | — | port without power |
| Service | agent | — | e.g. k3s `NotReady` |

The blade status is the worst individual check — and exactly that lights up on the edge LED.
The rack view in the UI and the faceplate in the rack show the same information.

### Escalation

The heartbeat stays away → the server probes actively → the switch reports power at the port, but no
network traffic arrives → suspicion of a hung OS → suggestion of a power cycle via the PoE port.

**Default: only after confirmation**, with a rate limit (no more than one cycle per blade per hour).
Automatic repair can be switched on, but is not the default — a blade stuck in a
boot loop should not also be hard powered off every five minutes.

### Notification

ntfy, webhook or e-mail. For ten blades that is enough; Prometheus and Alertmanager are only worth it
if they are in the house anyway. The `compute-blade-agent` already delivers the metrics in
Prometheus format, Rookery provides the target list via HTTP-SD.

---

## 9. Data model

```yaml
Rack:                               # 4, 10 or 20 slots
  id, name, location
  size:      10
  ip_offset: 100                    # address block; every rack gets 20 reserved

Blade:
  serial:   "10000000abcdef12"      # primary key
  rack_id:  1                       # position = (rack, slot)
  slot:     3
  mac, hostname
  ip:       10.0.0.103         # derived: net.(ip_offset + slot)
  image:    dietpi-arm64            # ← the distribution as an assignment
  variant:  dev | tpm | basic
  state:    new | provisioning | enrolled | online | offline | critical
  groups:   [k3s-worker]
  facts:    {os_id, os_version_id, os_family, init, pkg_mgr, net_backend, boot_path}
  health:   {status, checks: [...], last_seen}
  config_version_applied: "sha256:…"

Config:                             # global → group → blade, merged
  hostname, timezone
  users, packages (with per_os overrides), files, units
  blade_agent: {fan: {...}, led: {...}}

Image:                              # the catalogue
  id, url, sha256, boot_part, root_part, seed
```

---

## 10. API

```
# Provisioning — called by the mini OS
POST /api/v1/provision/{serial}        → image URL, sha256, layout, seed, token
POST /api/v1/provision/{serial}/status → progress: writing 45 % / verifying / done

# Agent — one bearer token per blade
POST /api/v1/enroll
GET  /api/v1/blades/{serial}/config    → merged desired config + ETag
POST /api/v1/blades/{serial}/status    → heartbeat, facts, health values
GET  /api/v1/blades/{serial}/commands  → reboot | identify | reimage

# Management — UI and CLI
GET/PUT /api/v1/blades/{serial}        → slot, hostname, groups, image
PUT     /api/v1/blades/{serial}/image  → switch distribution (triggers a reimage)
POST    /api/v1/blades/{serial}/actions/{identify|reboot|reimage|power-cycle}
GET     /api/v1/images                 → catalogue
GET     /api/v1/health                 → overall state of the rack
GET     /api/v1/prometheus/targets     → HTTP Service Discovery
```

---

## 11. Technology

| Part | Recommendation | Why |
|---|---|---|
| Server | Go, SQLite (`modernc.org/sqlite`), templates + HTMX | One static binary, no runtime on the host |
| Agent | Go, static arm64, systemd timer 60 s | No dependencies on ten blades across three distributions |
| Installer | Go, static, runs in `scriptexecute` | The same API library as the agent |
| Mini OS | `scriptexecute` from `raspberrypi/usbboot` | Ready-made, official, proven — no custom initramfs |
| DHCP/TFTP/DNS | dnsmasq, configuration generated from the inventory | One service for boot, reservation and name resolution |
| Images | zstd-compressed, over HTTP, SHA-256 | Fast, verifiable, streamable |
| Monitoring | Prometheus + Grafana optional, health in the server itself | Metrics come from the compute-blade-agent |

### Deliberately not chosen

- **cmprovision as a basis.** It is a factory tool: "write this image onto fifty modules",
  then done. No ongoing desired state, no heartbeat, no roles. On top of that it targets eMMC
  instead of NVMe and requires an isolated network segment, because it provisions via broadcast.
  From this ecosystem we take the valuable part — the `scriptexecute` initramfs — and leave
  the Laravel application out.
- **MaaS / Tinkerbell.** Full-blown bare-metal provisioning including UEFI PXE, oversized for ten
  blades.
- **Ansible.** No self-enrollment, no heartbeat, needs SSH and known addresses.
  Can be added later; Rookery then supplies the dynamic inventory.
- **Network configuration in the agent.** Made superfluous by the DHCP reservation — and that
  was the most distro-dependent part.

---

## 12. Roadmap

| Phase | Content | Result |
|---|---|---|
| **0** | Bring-up: once per blade via `rpiboot`, set `BOOT_ORDER=0xf26`, `NET_BOOT_MAX_RETRIES`, `MAC_ADDRESS`. Switch ports to portfast. | Prerequisite for everything else |
| **1** | Server (inventory, image catalogue, config merge, REST), agent (enroll/pull/apply/status), `bmctl`. Image still written to the NVMe by hand. | Configuration runs over the network, three distributions in parallel |
| **2** | dnsmasq from the inventory: DHCP reservations, fixed IPs, DNS. | Addresses are fixed and in one place |
| **3** | Netboot chain: TFTP per serial number, `scriptexecute`, `rookery-installer`, image delivery. | Plugging it in is enough — distribution via dropdown |
| **4** | Health model, rack UI, alerting, reimage via `kexec`, optional PoE power cycle. | Monitoring and remote maintenance |
| **5** | Roles `k3s-server` / `k3s-worker` as group config. | Blades as Kubernetes nodes |

Phase 2 before phase 3, because netboot requires the DHCP service anyway — both fall to
the same dnsmasq instance.

---

## 13. Open points

**Answered** by the research and therefore no longer open: Rookery has to take over DHCP for the
blade VLAN (proxy DHCP cannot hand out addresses); on the CM4 the MAC *cannot* be derived
from the serial number, but it can be assigned by hand via `MAC_ADDRESS`; there is no one-shot netboot
on the CM4, `kexec` and `tryboot` take its place; the CM4 needs no
`config.txt` change for NVMe; and a plain `dd` of the Ubuntu image onto the NVMe boots
unchanged.

Sorted by urgency, what remains open:

1. **DietPi on NVMe — test before anything else.** DietPi builds no initramfs; without the NVMe driver
   compiled into the kernel it does not find its root. Write an image to an NVMe and let it boot,
   before anything is built. A negative outcome costs one line in the image catalogue, not the design.
2. **Check the Debian image:** is `root` locked, is `openssh-server` installed at all? For the
   `generic` adapter that does not matter — it does not depend on SSH — but it does for emergency access.
3. **`kexec` per distribution.** The reimage route stands and falls with `CONFIG_KEXEC` in the respective
   kernel. Where it is missing, `tryboot` takes over.
4. **Bootloader level of the ten blades.** For large initramfs files a level from 2025-09-23 on is
   needed; pull it along during the bring-up.
5. **Managed PoE switch with SNMP or an API?** Only then are power cycle and the outside view of the
   port available. Independently of that: **portfast on all blade ports**, otherwise netboot becomes unreliable.
6. **Check the NVMe models** against the compatibility list of the Compute Blade documentation.
7. **Observe, do not act:** Ubuntu's seed trick relies on `fs_label: system-boot` in
   `99-fake-cloud.cfg`; cloud-init has marked exactly that as deprecated since 24.3. Works today,
   keep an eye on it for 26.04. Only affects us if we do end up using the cloud-init adapter.

### Address plan with several racks

A rack has 4, 10 or 20 slots. Each one gets a **block of 20 addresses** reserved,
regardless of its actual size — that keeps addresses stable if a rack is later
replaced by a larger one. Five racks fit into a /24 that way.

| Value | Rule | Example: rack 1, slot 3 |
|---|---|---|
| IP | `<net>.(ip_offset + slot)` | `10.0.0.103` |
| MAC | `02:b1:ad:<rack>:00:<slot>` | `02:b1:ad:01:00:03` |
| Hostname | `blade-r<rack>s<slot>` | `blade-r1s03` |
| TFTP directory | lower 4 bytes of the serial number | `/tftpboot/9ffefdef/` |

Every one of these values can be derived from every other — except the serial number, which comes from the
hardware and is linked to the position in the inventory. The MAC is only derived if the blade
has not reported one yet; during the bring-up it can be set to exactly this value via `MAC_ADDRESS`.

**A blade is thus hung on two things:** the *serial number* is the identity and stays with the
device, the *position* (rack + slot) carries address, name and role. Replacing a blade means: a new
serial number in the same slot — address and role stay.

### Implementation status

The server is built and running; see `INSTALLATION.md` for the concrete setup on
`rookery-server` (dnsmasq with DHCP/DNS/TFTP, rack management, IPAM, reservation generator,
netboot root).
