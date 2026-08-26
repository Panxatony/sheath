# Sheath – Architecture

Management software for Compute Blades (Raspberry Pi CM4) in BladeRunner
chassis. This describes the system as it is built and why it is built that way.
The split across several network segments has its own document,
`ARCHITECTURE-SITES.md`; putting the thing on metal is `INSTALLATION.md`.

**Requirements**

1. Blades receive their configuration over the network.
2. Different distributions per blade (e.g. DietPi, Debian 13 Trixie, Ubuntu 24.04).
3. A blade netboots into a mini OS that writes the assigned image to the NVMe.
4. Every blade gets a fixed IP address.
5. After that, the application continuously monitors the state of the blades.
6. Manage several BladeRunners with 2, 4, 10 or 20 blades.
7. Manage BladeRunners standing in more than one network segment.

From this follows an architecture along the **lifecycle**, not along features:

```text
   Provision  →  Configure  →  Operate & Monitor  →  Re-provision
   (Netboot)      (Agent)         (Health)            (Reimage)
```

## Two programs, not one

Requirement 7 is what split the server. DHCP does not cross a router, TFTP does
not want WAN latency, and pulling a 1.2 GB image once per blade over a site link
is a bad idea. Only part of the server is tied to the blades' broadcast domain,
so that part is now its own program.

| | `sheathd` — one, central | `sheath-site` — one per segment |
|---|---|---|
| Owns | inventory, interface, image catalogue, configuration merge, policy, netboot state machine, audit trail | the wire: DHCP reservations, the netboot switch per blade, the dnsmasq log, the image cache, the boot payload in the TFTP root, and the relay the blades talk to |
| Decides | everything | nothing |
| Needs | to be reachable by its sites | to sit in the blades' broadcast domain |

The site holds a cache, not a share in the truth. It fetches a desired state,
turns it into dnsmasq host records and cached bytes, and keeps working from what
it last received when the link goes down — which is the whole reason it exists.
Blades talk to their site and never need WAN access; the site relays. The
reasoning, the interface between the two, and what survives an outage are in
`ARCHITECTURE-SITES.md`.

One machine serving one segment is still a legitimate arrangement: `sheathd
-local-dhcp=true` writes the reservations and tails the log itself, and no
`sheath-site` is needed. With a site present, `-local-dhcp=false` makes the
server keep its hands off the directory — two programs owning one directory
would mean the loser is whoever wrote last.

---

## 1. The central decision

An early version of this design had "one golden image for all blades" as its guiding idea. It was
dropped, and what replaced it is the decision the rest of the system hangs off:

> The server owns an **image catalogue**. The distribution is an **attribute of the blade**,
> not a property of the installation.

`blade.image = dietpi-arm64` is thus an assignment in a database — no build process,
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
2. **DHCP.** The blade gets an address and the boot parameters (TFTP server). It only gets
   the netboot offer at all if its reservation carries the `bootnet` tag — see §3.6 of
   `INSTALLATION.md`. That tag is set exactly while an installation or an erase is requested.
3. **TFTP.** The bootloader loads kernel, device tree, `cmdline.txt` and a **mini initramfs**,
   from a directory named after the serial number if there is one, otherwise from the root.
4. **The mini OS runs.** It reads the CM4 serial number, takes the server address from
   `sheath_server=` in `/proc/cmdline`, waits for that server to answer `/healthz`, and asks:
   `POST /api/v1/provision/{serial}` → response: `go`, `wipe`, `idle` or `waiting`, and on `go`
   the image URL, SHA-256, target device, install options, hostname, SSH keys and the blade token.
   Where a site is in front, that request goes to the site's relay, which answers it from its own
   cache when the centre is unreachable.
5. **Writing.** The image is streamed in one pass — fetch, decompress, write to the target —
   because a 6 GB image fits neither in the memory of a CM4 nor on any scratch space it has.
   The SHA-256 is computed along the compressed stream and verified afterwards. Then the last
   partition is grown to the end of the disk, unless the configuration says otherwise.
6. **Seeding (offline).** The installer mounts the freshly written root partition and puts the
   agent binary, systemd unit, server URL and blade token straight into it, plus root's SSH keys
   and a cloud-init seed on the boot partition. Each of those steps can be switched off.
7. **Reboot.** Now the NVMe boots. The agent reports in to the server, the blade becomes `online`.

The installer reports its phase every five seconds — `writing` with a real percentage, then
`done`. Live progress belongs in the netboot session, which is overwritten each time; the event
log keeps the phase changes and every 25 %, or one installation buries an hour of real events.

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

```text
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

Raspberry Pi already ships exactly this component, and Sheath uses it rather than building an
initramfs of its own. The payload is Raspberry Pi's own netinstall image with a single line of
its `/init` replaced: where it used to start the menu-driven Imager on HDMI, it now starts
`sheath-installer`, a small static Go binary that speaks the same REST API as the agent. "Boot
on a CM4, start udev, get networking" was already solved there, and that is the expensive part.

Thinned out afterwards — Qt, Mesa, fonts, none of which a program without a GUI uses — the whole
`boot.img` is about 26 MB, well under the block-counter ceiling of older bootloaders. The 140 CA
certificates stay: the installer downloads over HTTPS. `tools/build-bootimg.sh` does the chain
(installer → ramdisk → `boot.img` → TFTP root) and publishes only when every step succeeded; a
failed build must never leave a stale `boot.img` behind, which happened once and had a blade
netbooting yesterday's installer while the log claimed today's.

The initramfs contains **zero kernel modules** — NVMe, ext4 and the rest are built into its
kernel. The same fact decides whether a distribution can be installed at all: an image whose
kernel has no built-in NVMe driver and builds no initramfs will not boot from the NVMe
afterwards.

As a fallback for a truly dead blade the USB route remains: `mass-storage-gadget` from
the same project exposes NVMe/eMMC as USB mass storage and writes faster than
classic `rpiboot` while doing so. But that needs a cable and a human at the rack — for the normal case
the network route is there.

### Reinstalling a running blade

The CM4 does **not** know a one-time netboot: according to the documentation, `set_reboot_order` is
explicitly "Raspberry Pi 5 only". `kexec` (jump straight into the installer kernel, needs
`CONFIG_KEXEC` in each distribution's kernel) and `tryboot` (`reboot '0 tryboot'`, needs the
installer on the NVMe boot partition) were both considered, and neither turned out to be
necessary.

**What is built is simpler.** A reimage is two ordinary things at once: the blade's install state
becomes `pending`, which puts the `bootnet` tag into its reservation, and a `reboot` command is
queued. The agent reboots; the netboot offer is now there; the blade lands in the installer. No
firmware trickery, nothing distribution-specific, and the same mechanism works for a blade that
has to be erased rather than rewritten.

The two states are separate on purpose. Assigning an image to a blade triggers nothing —
`install_state` is what says "write this now". Without that split every netboot would trigger a
full reinstall, endlessly, on a blade whose `BOOT_ORDER` puts the network ahead of the NVMe.

### Erasing a blade

Erasing a blade's NVMe so it can be pulled and put in another BladeRunner happens in the netboot
mini OS, **not in the agent**. The reason is not preference: the agent runs from the disk it would
have to erase, and a root filesystem cannot be unmounted out from under itself. The mini OS lives
entirely in RAM with the NVMe untouched in front of it — the same position from which it writes
images. That is also why the erase is an install *state* (`wipe`) and not a command: a command is
something the agent carries out.

Two steps, and the second is not optional. A discard over the whole device tells the drive to
forget the blocks, which on an NVMe takes seconds — but a drive is allowed to ignore it, and
nothing in the protocol promises the bytes are gone. Overwriting the first and last 64 MB is what
actually removes the partition table, the boot sector, the filesystem superblocks and the backup
GPT: the things that decide what this disk claims to be.

The blade leaves its slot only when the disk is **reported empty**. Freeing it at the click would
have made a half-erased blade disappear from the interface, which is the worst possible moment to
lose sight of one. The record survives — the serial number is the same piece of hardware, and its
history is worth more than a tidy table. Afterwards the blade halts rather than rebooting, so it
can be pulled; a blade that stays where it is can be told to reboot instead
(`install.after: reboot`) and comes back up in the installer ready for a new image.

Two guards, because this is the one action a reinstall cannot undo: a site may forbid it entirely
(`no_wipe`, see §8), and whoever asks has to type the blade's name — a slip of the mouse in a list
of twenty rows should not empty a disk.

---

## 4. Pillar 2 — Several distributions: exactly three extension points

Multi-distro gets expensive when distro knowledge seeps through the whole codebase. It stays cheap
if you lock it into named places. There are exactly three, and they have stayed three.

### (A) Image catalogue — data, not code

```yaml
images:
  ubuntu-24.04-arm64:
    url:      http://sheath.lan/images/ubuntu-24.04-cm4.img.zst
    sha256:   9f2c…
    local:    ubuntu-24.04-cm4.img.zst   # mirrored file under images/
    bytes:    1203892112
    os_id:    ubuntu
    seed:     generic
    kernel:   downstream                 # downstream | upstream | unrecorded
    min_disk: 8589934592                 # bytes, 0 = unknown
    verified: true                       # has actually been booted on a blade
```

Adding a new distribution means: mirror the image, enter the checksum, done.
`tools/mirror-image.sh` fetches it into the catalogue; `tools/prepare-image.sh` customises it
once — installing the packages that have to be present at first boot, clearing the machine id
(systemd derives the DHCP identity from it, and every blade from one image would otherwise fight
over the same lease) and dropping the image's SSH host keys. Doing that once per image beats doing
it once per blade.

Nothing is written to an image entry that the caller did not mention. That sounds obvious and was
not: setting the kernel flavour by hand once wiped the URL, checksum, size and local file of every
entry in the catalogue, because the mirror script and a person editing attributes were writing the
whole row each time and erasing each other.

**Why the kernel flavour is an attribute.** The smart fan unit hangs off UART5, which the firmware
has to enable through a device-tree directive before Linux exists. On an image running the
**upstream** kernel the firmware applies no device-tree directive at all — not `dtoverlay`, not
even `dtparam` — so the fan unit stays invisible and the blade reports no fan and no LED
telemetry. Nothing about that is discoverable from a file name, and it cost an evening at the rack
to learn. It therefore belongs in the catalogue, where the interface can say it out loud next to
the image you are about to pick:

| `kernel` | What the interface says |
|---|---|
| `downstream` | Raspberry Pi kernel — device tree overlays apply, fan and LED telemetry work |
| `upstream` | upstream kernel — the firmware applies no device tree directive, so no fan or LED telemetry |
| unrecorded | kernel flavour not recorded |

`min_disk` and `verified` are recorded and shown in the same spirit: the size an image needs, and
whether anyone has actually watched it boot on a blade. Neither currently gates an installation —
they inform the person choosing, which is the point at which the information is worth something.

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
install -m755 sheath-agent          $ROOT/usr/local/bin/
install -m644 sheath-agent.service  $ROOT/etc/systemd/system/
install -m600 agent.env             $ROOT/etc/sheath/
# Enable a unit without a running systemd = symlink by hand:
ln -s /etc/systemd/system/sheath-agent.service \
      $ROOT/etc/systemd/system/multi-user.target.wants/sheath-agent.service
```

The last step is the only one that is not obvious: `systemctl enable` creates nothing
more than exactly this symlink, and you can set it yourself offline. Because the agent binary is
statically linked, the same procedure works equally on glibc and musl systems.

Alongside it the installer places root's SSH keys (root deliberately: which user accounts exist is
decided by the distribution's first boot) and a cloud-init `user-data` / `meta-data` pair on the
boot partition, found by trying to mount each partition read-only as vfat — the position says
nothing on a Debian image, which numbers its firmware partition 15 and puts it first.

### How an installation is carried out is the server's business

The installer used to decide all of this by itself, which meant a change needed a new `boot.img`
at every site. It is the server that knows which blade this is and what the operator asked for, so
the choices live in the ordinary configuration under `install` — same `global → group → blade`
layering as everything else a blade is told, because a choice about a blade belongs where its
other properties are and not in a second mechanism.

```yaml
install:
  install_target: /dev/nvme0n1   # where the image goes
  after: halt                    # when written: reboot (default) | halt | shell
  reboot_delay: 5                # seconds before restarting, so a console can be read
  require_checksum: true         # refuse an image the catalogue has no checksum for
  no_grow: true                  # leave the image's own partition layout alone
  no_root_keys: true             # each seeding step can be switched off on its own
  no_cloud_init: true
  no_agent: true
```

Two conventions carry the compatibility. Every option is phrased as a prohibition or as
"0 means the default", so **zero values mean exactly what the installer did before** and an older
server talking to a newer installer still agrees with it. And the checksum policy is a choice
rather than a constant: with a checksum the write is verified and a mismatch is fatal; without
one, `require_checksum` decides whether that is a refusal or a logged "content unverified".

The distro-native adapters (`cloud-init`, `dietpi`) remain as an option — only useful if you want to
take along distro-specific features, such as DietPi's software installer.

### (C) Distribution differences in the agent — named, not scattered

All three distributions are Debian family: `apt`, `dpkg`, systemd. The agent therefore has one
code path and a short list of named exceptions, rather than an adapter hierarchy that would cost
more than it saves at this size. What it applies:

```yaml
hostname, timezone
ssh_authorized_keys       # root, plus the uid-1000 account if there is one
files:    [{path, content, mode}]
binaries: [{path, url, sha256}]        # fetched, hashed, renamed into place
boot_config: [...]                     # lines for config.txt — see below
packages: [{name, per_os}]
units:    [{name, enabled, restart, per_os}]
```

The exceptions are three, and each one is a fact about a distribution rather than a branch in a
class:

- **DietPi has no dbus**, so `hostnamectl` and `timedatectl` fail; the agent writes `/etc/hostname`
  and symlinks `/etc/localtime` itself when the bus is missing.
- **Unit names differ.** OpenSSH is `ssh` on Debian and Ubuntu; DietPi runs Dropbear. A `per_os`
  entry replaces the name — and an empty value means "this distribution does not have this unit",
  so the entry is skipped rather than failing.
- **Package names differ**, with the same `per_os` escape hatch.

`boot_config` is the one that reaches below Linux. Settings the firmware reads before Linux exists
— `dtoverlay=uart5` for the smart fan unit, for instance — are rolled out centrally. The agent
keeps a marked block of its own at the end of `config.txt`, leaves a setting that already stands
elsewhere alone, and closes any section it opens with `[all]` so that anything appended later is
not silently filtered by a stray `[cm4]`. On Debian the same block also goes to
`/etc/default/raspi-firmware-custom`, because `raspi-firmware` generates `config.txt` and says so
in its own header — written anywhere else, the setting would not survive a kernel update.

A change there sets `reboot_required`, which the interface shows as "restart pending". A setting
the firmware reads is worth nothing until the firmware reads it, so the agent can also do the
restart itself — see §7.

### Facts: the blade says what it is

On every heartbeat the agent reports from `/etc/os-release` plus a bit of probing:

```yaml
os_id:          dietpi | debian | ubuntu
os_version_id:  "13" | "24.04"
os_name:        DietPi 9.7
os_base:        Debian GNU/Linux 13 (trixie)   # what DietPi is built on
os_family:      debian
init:           systemd
pkg_mgr:        apt
net_backend:    networkd | ifupdown | netplan | NetworkManager
boot_path:      /boot | /boot/firmware
kernel, arch, model, serial, agent_version
reboot_required: true                          # a boot setting is waiting for a restart
```

DietPi is reported as DietPi with its own version rather than as the Debian it is built on, and
the base name is kept alongside as `os_base` — both answers are true and both get asked.

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

### The route taken: DHCP reservations

> **Sheath owns the address management** and generates DHCP reservations from it.
> `slot 3 → 10.0.0.103 → dhcp-host=<mac>,blade-r1s03,10.0.0.103,infinite`

Address management is **per site**, not global. `net_base`, the pool for unknown blades and the
size of a BladeRunner's address block are properties of the site; a blade's address is derived
from `(site, BladeRunner, slot)`. Two sites may legitimately use the same network, so the block
uniqueness is `UNIQUE(site_id, ip_offset)` — with a global constraint the second site could not
have a `.100` block because the first one already did.

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

Sheath writes the reservations from the inventory — `sheath-site` where a site owns the wire,
`sheathd` itself where it does not. dnsmasq can read them in without a restart:

```ini
dhcp-hostsdir=/etc/sheath/dhcp-hosts/     # one file per blade
```

dnsmasq reads new or changed files in this directory **by itself**, without a signal. One
drawback: deleted or rewritten entries only disappear with a `SIGHUP`, because
host records are only *added* dynamically. Sheath therefore additionally sends a
`systemctl reload dnsmasq` after every removal or rewrite — and only then, because a reload for
nothing is noise in a log somebody has to read. DNS records for `blade-r1s03.blades.lan` fall
out of the same instance as a by-product.

Such a file holds **only** what would otherwise stand to the right of `dhcp-host=`. Write the
prefix into it and dnsmasq reports "bad hex constant" and drops the line silently: the reservation
then has no effect at all, while everything above it reports success.

### Chicken and egg: the MAC is unknown beforehand

The obvious shortcut is **blocked**. On the Pi 3 and older the MAC could be derived from the
serial number; from BCM2711 on — that is, the CM4 — that has been dropped without replacement. The Raspberry Pi documentation
says so explicitly: "the MAC address is programmed at manufacture and there is **no link** between
the MAC address and serial number."

There are three better ways out:

1. **Assign the MAC yourself.** The EEPROM property `MAC_ADDRESS` overrides the factory-programmed
   address. During the one-time bring-up (see §6) Sheath can assign every slot a
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

That takes a few minutes per blade and is then never needed again. Sheath can generate the
EEPROM configuration from the inventory, so that the bring-up is one command
(`bmctl eeprom-config --slot 3`) and not hand editing.

After that, the following holds for the entire remaining lifecycle: plugging it in is enough.

**Mind the bootloader version:** for initramfs files beyond roughly 96 MB you need a
bootloader from **2025-09-23 or newer** (§3). Update it right away during the bring-up anyway.

---

## 7. Pillar 4 — Configuration during operation

- **Pull instead of push.** The agent asks every 60 s for its desired state, applies it idempotently
  and reports back. No SSH access on the server, works with DHCP, converges by itself after every
  reboot.
- **Three levels**, merged: `global → group → blade`. Groups are merged in alphabetical order so
  the result is deterministic; after the merge the position-derived values (hostname, expected IP)
  win, because they are not opinions.
- **Hash comparison.** The server delivers the merged config with a hash and the same value as an
  ETag; the agent sends it back as `If-None-Match` and reports the applied hash in every status.
  Drift detection falls out of that at no extra cost. On a partially failed pass the agent
  deliberately does **not** record the version, so the next pass retries.
- **Order within a pass:** report, then commands, then configuration. Report first, so the server
  is up to date even when applying then fails.
- **Hardware stays with the `compute-blade-agent`** (Go, Apache-2.0): LEDs, fan, button,
  critical mode on overtemperature, Prometheus on `:9666`, gRPC with mTLS. Sheath delivers its
  binary and its `config.yaml` through the ordinary `binaries` and `files` sections and calls
  `bladectl` for identify and stealth. Both directions exist — identify can be switched off again,
  not only on — because an overlay that only offers the direction you are already in is a dead
  button.

### `PATCH`, not `PUT`, for one setting

`PUT /api/v1/config/{scope}` replaces, which is what PUT means and exactly the wrong tool for
changing one setting. A request carrying only `{"install":{"after":"reboot"}}` once emptied an
installation's SSH keys, boot configuration and binaries. `PATCH /api/v1/config/{scope}` merges one
level deep — a top-level `null` deletes a key, an object merges its sub-keys, anything else
replaces. The settings page uses it, and merges only the two sections it knows: a form that has
never seen keys, files, units or binaries must not be able to remove them.

### The `agent` section: what a blade does about itself

Layered like everything else, `global → group → blade`:

```yaml
agent:
  interval: 60                  # seconds between passes
  jitter: 15                    # random spread, so a rack does not ask in
                                # lockstep after a power cut
  allow: [identify, identify_off, reboot]
  reboot_on_boot_config: true
  maintenance: "02:00-04:00"    # and only inside this window
```

- **`interval` and `jitter`.** Ten blades that lose power together come back together; a random
  offset per pass is what keeps them from arriving at the server as one wave. The agent also waits
  a random fraction of an interval before its very first pass, for the same reason.
- **`allow`** is which commands this blade accepts **at all**, checked on the blade and not only
  at the server. Empty means every command — it is a restriction to opt into, for the machine that
  must never be reimaged by accident, not a default posture. A refused command is reported as
  refused rather than silently dropped.
- **`reboot_on_boot_config`** with an optional **`maintenance`** window. A setting the firmware
  reads is worth nothing until the firmware reads it; where this is on, the blade restarts itself
  after a boot-configuration change, reports that it is about to, and otherwise waits for an hour
  somebody chose. A window that ends before it starts wraps around midnight, which is what a night
  window usually does. **Off by default** — restarting a machine that is doing work is a decision,
  not a tidying step.

Commands are de-duplicated by kind (only the newest survives), ordered so that reboots come last,
and expire after fifteen minutes on both sides — a stale command that fires when an agent first
starts is a reboot nobody asked for today.

---

## 8. Pillar 5 — Status monitoring

### Two independent observation paths

The decisive point: **a dead blade cannot report its own death.** That is why
two directions of view are needed.

**Inside view** — the agent reports every 60 s: uptime, load, disk total/free/used, memory,
SoC temperature, NVMe temperature, throttling and undervoltage, plus whatever the
`compute-blade-agent` exposes on `:9666` — fan rpm and target, airflow temperature, blade state,
fan unit type, stealth mode, edge button count. The absence of that last group is silent: a blade
without the hardware agent is a blade with fewer readings, not a fault.

**Outside view** — the server observes what no blade can report about itself: the age of the last
heartbeat, and the wire. A blade netbooting appears in the dnsmasq log before any operating system
runs on it, so DHCP requests, the bootloader's vendor class and every TFTP file served are visible
from outside — that is a whole class of failure a heartbeat could never show.

The decisive distinction there: the RPi bootloader identifies itself over DHCP with the vendor
class `PXEClient:Arch:00000:UNDI:002001` and an ordinary Linux client does not. That is how Sheath
knows whether a blade even *wanted* to netboot. Without it, you go hunting for a fault in TFTP for
a blade that never asked.

An active probe of the blade (ICMP/TCP), and link state and PoE power of the switch port over
SNMP, are **not built**. They would need a managed switch to be worth anything, and the two views
above already separate "not answering" from "not booting", which is the distinction that
matters most.

### Health model: what "everything ok" means

| Check | Source | warn | crit |
|---|---|---|---|
| Heartbeat | server | — | older than `offline_after_min` |
| Config sync | hash comparison | drift | apply error |
| SoC temperature | agent | `soc_warn_c` | `soc_crit_c` |
| NVMe temperature | agent | `nvme_warn_c` | — |
| Fan | agent | — | 0 rpm with a target above 0, on a smart fan unit, after warm-up |
| Disk usage `/` | agent | `disk_warn_pct` | `disk_crit_pct` |
| Power | agent | throttled now | undervoltage now |

The blade status is the worst individual check — and exactly that lights up on the edge LED.
The rack view in the UI and the faceplate in the rack show the same information.

Two of those rows are worth their own sentence, because both were once wrong in a way that
painted healthy blades red:

- **Zero fan rpm is only critical on a smart fan unit.** A standard unit has no tacho, so 0 there
  means "not measurable", not "stopped". And "stopped" needs three things to be true, not one: the
  unit has to measure at all, the fan has to have been *asked* to spin, and it has to have had
  time to answer. A blade three minutes into its first boot reported 0 rpm at 0 per cent while its
  fan ran at 3490.
- **A value that is `+Inf` is dropped, not reported.** That is what a fan unit without a tacho
  actually sends, it is not JSON, and it once broke the whole status report rather than one field
  of it.

### Thresholds and timings are policy, not constants

They used to stand in the code, which is the right place for a constant and the wrong place for a
judgement. A blade in a ventilated rack and one in a warm office do not share the temperature at
which someone should be woken; nor does a fleet of three share a heartbeat timeout with a fleet of
two hundred.

So: defaults that match what the code did before, one global setting, and a **per-site override**
for the ones a site can reasonably differ on. An empty field inherits — it means "unchanged", not
"zero", because nobody means a critical temperature of 0 °C. The site form shows empty inputs with
the global value as a placeholder, so it is visible at a glance which numbers this site has
actually chosen.

| Setting | Default | Scope |
|---|---|---|
| `soc_warn_c` / `soc_crit_c` | 70 / 80 °C | global, overridable per site |
| `nvme_warn_c` | 70 °C | global, overridable per site |
| `disk_warn_pct` / `disk_crit_pct` | 85 / 95 % | global, overridable per site |
| `offline_after_min` | 5 min | global, overridable per site |
| `no_wipe` | off | global, overridable per site — a prohibition only, so a site inherits it and cannot switch it off |
| `command_ttl_min` | 15 min | global only |
| `sample_every_min` / `sample_keep_hours` | 5 min / 48 h | global only |

The last three are properties of the central server's bookkeeping, not of a place, so they have no
per-site form.

### A short history, kept cheaply

A blade reports every minute; keeping every report would be a database of weather, not of blades.
A `samples` table keeps **one measurement per blade every five minutes for 48 hours** — SoC
temperature, airflow temperature and fan rpm, about six hundred rows per blade. It is written when
a blade reports and pruned in the same moment, so the only thing that grows the table is the thing
that also trims it.

That is enough to see a fan ramping or a slot running hot, which is the question a number on its
own cannot answer. They are drawn as **sparklines**: a bare SVG polyline, no library and no
script, per slot on the BladeRunner page. A series of fewer than two points draws nothing rather
than a flat line at the bottom, because a flat line would be a lie about a missing series.

### What the interface shows

| Page | What is on it |
|---|---|
| **Overview** `/` | BladeRunners grouped under **their site**, with that site's network, pool and state; every slot as one coloured cell; live netboot sessions with the image choice for a blade that is on the wire right now; blades that have reported in but sit in no slot yet |
| **BladeRunner** `/bladerunners/{id}` | The slot table across the full size — free slots show the *planned* IP and MAC, so you see what a blade will get before inserting it. Per slot: hardware readouts, the two sparklines, and the actions. The enclosure as a whole in the header, because a BladeRunner shares its air and the *spread* of temperatures says more than any single reading. Below it an **activity log**: what the blades in this enclosure have been doing, newest first, with slot, name and severity |
| **Site** `/sites/{id}` | What stands in this site and how it is doing, its per-site thresholds, and its **image stock** in full — which image, what state (`absent`, `fetching`, `ready`, `error`), how many bytes are here against how many the catalogue has, and how many blades here are still waiting for it. An image assigned to a blade here but not yet fetched is listed too, because that row is the one that explains a waiting install |
| **Map** `/map` | The central server, the sites hanging off it, and every slot as one square in the colour it has on its BladeRunner page. The line to a site carries that link's state — solid while the site reports, dashed once it goes quiet — because with several sites the interesting failure stops being a blade and becomes a stretch of network. Server-rendered SVG from the theme's own tokens; no library, and it follows dark mode like everything else |
| **Settings** `/settings` | The two sections a person actually turns knobs in: what the agent does on a blade, and how an installation is carried out. It merges into the global desired state and deliberately does not touch keys, files, units or binaries |

The agent reports **what it changed**, and that lands in the activity log. Until it did, the only
record of a blade being reconfigured — or of the attempt failing — sat in the journal of that
blade, which is exactly the place you cannot reach when the change that failed was the one that
opens the door.

Every page carries the same header: mark, name, controls, menu. A header that moves is a header
nobody can aim at. Every action is a POST followed by a redirect, so a reload never triggers
anything twice.

### Escalation and notification — not built

Nothing here notifies anybody. A blade that goes quiet turns red on a page somebody has to be
looking at, and that is all. The intended shape is unchanged and written down so it is not
re-invented: heartbeat stays away → probe actively → the switch reports power at the port but no
traffic arrives → suspicion of a hung OS → *suggest* a power cycle over the PoE port, **only after
confirmation**, rate-limited to one cycle per blade per hour. Automatic repair could be switched
on, but should not be the default — a blade stuck in a boot loop should not also be hard powered
off every five minutes.

For notification, ntfy, a webhook or e-mail would be enough at this size; Prometheus and
Alertmanager are only worth it if they are in the house anyway. The `compute-blade-agent` already
delivers metrics in Prometheus format, so a target list over HTTP-SD is the cheap way in.

---

## 9. Data model

SQLite, one file. The schema in full is `server/db.go`; this is its shape.

```yaml
Site:                               # a network segment, not a building
  id, name, location
  net_base:    10.0.0               # addresses are derived within this
  gateway, dns, domain
  pool_from: 210, pool_to: 240      # loan addresses for unknown blades
  offset_base: 100, offset_step: 20 # address block per BladeRunner
  local:       false                # this process serves the wire here
  token                             # the site's own credential
  policy_json                       # per-site threshold overrides; empty inherits
  last_seen

BladeRunner:                        # 2, 4, 10 or 20 slots
  id, site_id, name, location
  size:      10
  ip_offset: 100
  UNIQUE (site_id, ip_offset)       # per site: .100 in one network is not
                                    # the same address as .100 in another

Blade:
  serial:        "10000000abcdef12" # primary key — the identity
  short_serial:  "9ffefdef"         # low 4 bytes = TFTP directory
  rack_id, slot                     # the position; both NULL until placed
  mac, hostname, token
  ip:            10.0.0.103         # derived: <site.net_base>.(ip_offset + slot)
  image:         dietpi-arm64       # ← the distribution as an assignment
  install_state: idle | pending | done | error | wipe
  installed_at
  state:         new | provisioning | enrolled | online | offline | critical
  groups, facts, health
  config_applied: "sha256:…"
  UNIQUE (rack_id, slot)

Config:                             # scope = global | group:<name> | blade:<serial>
  hostname, timezone                # merged in that order
  ssh_authorized_keys, packages (with per_os), files, units, binaries
  boot_config                       # lines for config.txt
  agent:   {interval, jitter, allow, reboot_on_boot_config, maintenance}
  install: {install_target, after, reboot_delay, require_checksum, no_*}

Image:                              # the catalogue — see §4(A)
  id, url, sha256, seed, os_id, local, bytes, notes
  kernel, min_disk, verified
  state: queued | working | ready | error

SiteImage:                          # what a site says it actually holds
  site_id, image_id
  state: absent | fetching | ready | error
  bytes, note, ts

Netboot:                            # one live session per MAC
  mac, ip, serial, site_id
  stage: dhcp | tftp | ramdisk | installer | writing | done | error
  files, last_file, image, client, note

Sample:                             # one per blade per 5 min, kept 48 h
  serial, ts, soc, airflow, rpm
```

Two things in there are separations that were learned rather than designed.
`install_state` is separate from `image` because assigning an image must not
mean writing it. And `netboot.site_id` exists because two sites may hand out
the same address, so a session without its site is one nobody can place.

---

## 10. API

Three audiences, three credentials: the mini OS has none yet, a blade has its own token, a site
has its own token, and a person has the admin token (traded once at `/login` for a session
cookie, because a browser form cannot send an `Authorization` header).

```http
# Provisioning — called by the mini OS, which has no credential yet
POST /api/v1/provision/{serial}          → go | wipe | idle | waiting, and on go:
                                           image URL, sha256, target, options,
                                           hostname, ssh_keys, token
POST /api/v1/provision/{serial}/status   → phase: writing 45 % | wiping | done | wiped | error

# Agent — one bearer token per blade
POST /api/v1/enroll
GET  /api/v1/blades/{serial}/config      → merged desired config + ETag
POST /api/v1/blades/{serial}/status      → facts, health, applied version, what changed
GET  /api/v1/blades/{serial}/commands    → identify | identify_off | stealth_on |
                                           stealth_off | reboot | reimage

# Site — one bearer token per site; a site may act for itself and nothing else
GET  /api/v1/site/{id}/desired           → its blades, their images, the boot payload + ETag
POST /api/v1/site/{id}/events            → batched observations from the wire
POST /api/v1/site/{id}/status            → heartbeat, applied version, clock, image stock

# Management — admin token
GET/POST/PUT/DELETE /api/v1/sites[/{id}]
POST    /api/v1/sites/{id}/token         → issue or rotate a site credential
PUT     /api/v1/sites/{id}/policy        → per-site thresholds
GET/PUT /api/v1/policy                   → the global ones
GET/POST/PUT/DELETE /api/v1/bladerunners[/{id}]
GET/PUT/DELETE      /api/v1/blades[/{serial}]
POST    /api/v1/blades/{serial}/actions/{kind}
GET/POST            /api/v1/images
GET/PUT/PATCH       /api/v1/config/{scope}   → PATCH merges, PUT replaces (§7)
POST    /api/v1/dhcp/sync                → rewrite the reservations now
GET     /api/v1/netboot                  → who is on the wire
POST    /api/v1/netboot/{mac}/image      → pick an image for a blade that is booting
GET     /api/v1/events                   → the audit trail
GET     /api/v1/health                   → overall state

# Bytes, unauthenticated
GET /images/…    the OS images
GET /boot/…      the netboot payload, so a site needs no build tooling
GET /agent/…     the agent binary, for offline seeding
```

The site offers the four blade-facing paths under the same names, so the agent notices nothing of
the split; it just has a different address in its seed.

---

## 11. Technology

| Part | Choice | Why |
|---|---|---|
| `sheathd` | Go, SQLite (`modernc.org/sqlite`), server-rendered templates | One static binary, no runtime on the host |
| `sheath-site` | Go, static, no external dependencies at all | It has to keep working when nothing else does |
| `sheath-agent` | Go, static arm64, systemd service, 60 s pass | ~1 MB of memory, no dependencies across three distributions |
| `sheath-installer` | Go, static, runs inside the mini OS | The initramfs ships neither `curl` nor `zstd`, `xz` or `resize2fs`, so HTTP, decompression, writing and mounting all happen inside the program |
| Mini OS | Raspberry Pi's netinstall initramfs, thinned, one line of `/init` replaced | Ready-made, official, proven — no custom initramfs |
| DHCP/TFTP/DNS | dnsmasq, configuration generated from the inventory | One service for boot, reservation and name resolution |
| Images | zstd/xz/gz-compressed, over HTTP, SHA-256 | Fast, verifiable, streamable |
| Drawing | inline SVG from the theme's own tokens | No library, no script, follows dark mode, and it prints |
| Monitoring | health in the server itself, Prometheus optional | Metrics come from the compute-blade-agent |

Four binaries, all static, all built from one repository, all `CGO_ENABLED=0`. The server binary
is `sheathd`, not `sheath` — the plain name is kept free for a command-line client.

### Deliberately not chosen

- **cmprovision as a basis.** It is a factory tool: "write this image onto fifty modules",
  then done. No ongoing desired state, no heartbeat, no roles. On top of that it targets eMMC
  instead of NVMe and requires an isolated network segment, because it provisions via broadcast.
  From this ecosystem we take the valuable part — a ready-made initramfs that boots on a CM4 and
  brings up networking — and leave the Laravel application out.
- **MaaS / Tinkerbell.** Full-blown bare-metal provisioning including UEFI PXE, oversized for a
  handful of blades.
- **Ansible.** No self-enrollment, no heartbeat, needs SSH and known addresses.
  Can be added later; Sheath then supplies the dynamic inventory.
- **Network configuration in the agent.** Made superfluous by the DHCP reservation — and that
  was the most distro-dependent part.
- **A site scope in the configuration merge.** Configuration is layered `global → group → blade`.
  What differs per site is *policy* — thresholds and timings — and that has its own mechanism,
  because a threshold is a property of a place and a configuration is a property of a machine.
- **End-to-end encryption between server and blade.** It would cost exactly the offline capability
  a site exists for. The trade-off is stated openly rather than hidden: see
  `ARCHITECTURE-SITES.md` §3.5.

---

## 12. Where it stands

| Phase | Content | State |
|---|---|---|
| **0** | Bring-up: once per blade via `rpiboot`, set `BOOT_ORDER=0xf26`, `NET_BOOT_MAX_RETRIES`, `MAC_ADDRESS`. Switch ports to portfast. | done — the one manual step, §6 |
| **1** | Server (inventory, image catalogue, config merge, REST), agent (enroll/pull/apply/status). | done |
| **2** | dnsmasq from the inventory: DHCP reservations, fixed IPs, DNS. | done |
| **3** | Netboot chain: TFTP, mini OS, `sheath-installer`, image delivery. | done — plugging it in is enough, distribution via dropdown |
| **4** | Health model, BladeRunner page, samples and sparklines, reimage, erase. | done |
| **5** | Sites: `sheath-site`, per-site addressing, image cache, relay, per-site policy, map. | done — see `ARCHITECTURE-SITES.md` |
| **6** | Alerting, PoE power cycle, an outside probe. | not built — §8 |
| **7** | Roles `k3s-server` / `k3s-worker` as group config. | not built |

Phase 2 came before phase 3 because netboot requires the DHCP service anyway — both fall to the
same dnsmasq instance. Phase 5 came last and had to: separate first, then distribute. Designing
the distributed system before the seam is right leads to an interface that can no longer be
changed later.

---

## 13. Open points

**Settled**, and recorded so nobody re-derives them: Sheath has to take over DHCP for the blade
VLAN (proxy DHCP cannot hand out addresses); on the CM4 the MAC *cannot* be derived from the
serial number, but it can be assigned by hand via `MAC_ADDRESS`; there is no one-shot netboot on
the CM4, and arming the reservation plus a reboot turned out to be enough, so neither `kexec` nor
`tryboot` was needed; the CM4 needs no `config.txt` change for NVMe; a plain write of the Ubuntu
image onto the NVMe boots unchanged; and an image whose kernel is the upstream one gets no
device-tree directive from the firmware, which is why the catalogue records the flavour.

What remains open:

1. **DietPi on NVMe.** DietPi builds no initramfs; without the NVMe driver compiled into the
   kernel it does not find its root. It is in the catalogue and the flow does not depend on it —
   a negative outcome costs one line there, not the design — but it has not been proven on metal.
2. **Emergency access to a Debian blade.** The Debian raspi image ships neither `openssh-server`
   nor cloud-init. `tools/prepare-image.sh` puts a door in it before any blade sees it, which
   works — but a blade prepared without that step has exactly one way in, the agent, and one door
   is one too few when the question is why the agent did not start.
3. **Bootloader level across the blades.** For large initramfs files a level from 2025-09-23 on is
   needed; pull it along during the bring-up. The current payload is well under the old ceiling,
   so this is a constraint to remember rather than a problem today.
4. **A managed PoE switch with SNMP or an API.** Only with one are a power cycle and the outside
   view of the port available at all (§8). Independently of that: **portfast on all blade ports**,
   otherwise netboot becomes unreliable.
5. **The NVMe models**, against the compatibility list in the Compute Blade documentation.
6. **`no_clock_sync` is accepted and ignored.** The installer sets its clock from the server
   before anything else — the mini OS has no RTC, starts at 1970, and every valid TLS certificate
   is then "not yet valid". Because that happens before the job is fetched, the option that would
   switch it off arrives too late to be read. Either move the option somewhere it can be read, or
   drop it.
7. **A blade's health is judged per site, but marked offline globally.** `offline_after_min` can
   be overridden per site and is honoured when the health verdict is computed — but the sweep that
   flips a blade's state to `offline` uses the global value. Two numbers that are meant to be one.
8. **Observe, do not act:** Ubuntu's seed trick relies on `fs_label: system-boot` in
   `99-fake-cloud.cfg`; cloud-init has marked exactly that as deprecated since 24.3. Works today,
   keep an eye on it for 26.04. Only affects the cloud-init seeding step.

The open points that belong to the site split — enrolling a new site, clock skew, a blade moving
between sites, and the relay's buffer living only in memory — are in
`ARCHITECTURE-SITES.md` §10.

### Address plan with several BladeRunners

A BladeRunner has 2, 4, 10 or 20 slots. Each one gets a **block of 20 addresses** reserved,
regardless of its actual size — that keeps addresses stable if it is later replaced by a larger
one. Five fit into a /24 that way, per site.

| Value | Rule | Example: site 1, runner 1, slot 3 |
|---|---|---|
| IP | `<site.net_base>.(ip_offset + slot)` | `10.0.0.103` |
| MAC | `02:b1:ad:<runner>:00:<slot>` | `02:b1:ad:01:00:03` |
| Hostname | `blade-r<runner>s<slot>` | `blade-r1s03` |
| TFTP directory | lower 4 bytes of the serial number | `/tftpboot/9ffefdef/` |

The runner number is itself derived from the address block —
`(ip_offset - site.offset_base) / site.offset_step + 1` — not from a database row id, so a blade
that moves gets the name of its new place rather than the name of its old row.

Every one of these values can be derived from every other — except the serial number, which comes
from the hardware and is linked to the position in the inventory. Derived values follow a blade
when it moves; a hand-set hostname and a real vendor MAC do not, because someone chose those.
During bring-up the MAC can be set to exactly this value via `MAC_ADDRESS`.

**A blade is thus hung on two things:** the *serial number* is the identity and stays with the
device, the *position* (site + BladeRunner + slot) carries address, name and role. Replacing a
blade means a new serial number in the same slot — address and role stay.

### Where to read further

`INSTALLATION.md` has the concrete setup on metal, including the traps that went off along the
way. `ARCHITECTURE-SITES.md` has the split across network segments. `CHANGELOG.md` has what
changed and, usually, why.
