# Sheath

Management for [Compute Blades](https://docs.computeblade.com/) in BladeRunner chassis.

> **How this was built** — the code in this repository was written by
> [Claude](https://claude.com/claude-code) (Anthropic), working from the ideas,
> the design decisions and the review of [Panxatony](https://github.com/Panxatony),
> whose rack it runs on. What to build, which trade-offs to accept and what
> counts as finished came from there; so did every correction that mattered.
> Nothing here is a sketch: each part was tried on the hardware before it was
> called done.

Slot in an empty blade, pick an image in the interface — Sheath does the rest:
netboot, write the image to the NVMe, seed credentials and the agent, hand out a
fixed address, and watch it from then on.

## What it looks like

The overview: every site, every BladeRunner, every slot as one square — green
where a blade is in sync, amber where something wants looking at, hatched where
the slot is empty.

![The overview, with two sites and three BladeRunners](docs/img/overview.png)

One BladeRunner: what sits in each slot, which distribution it runs, how warm it
is, what its fan is doing, and underneath it what the blades in this enclosure
have been up to.

![A BladeRunner with four blades and its activity log](docs/img/bladerunner.png)

The inventory: what is screwed into the racks across every site. The module and
its revision, the memory, the eMMC or the absence of one, the NVMe, the
bootloader and how each blade came up this time. It is read from the revision
code the firmware leaves in the OTP and from the device tree — and the mini OS
reads it too, so a blade waiting for someone to choose an image already says
what it is.

![The inventory across two sites](docs/img/inventory.png)

The screenshots come from a demonstration instance with invented blades — the
addresses and serial numbers in them belong to nothing.

## What it does

| Pillar | |
|---|---|
| **Provision** | Netboot into a mini OS that writes the assigned image to the NVMe, the eMMC or a card |
| **Configure** | The agent pulls its desired state every 60 s and applies it idempotently |
| **Address** | DHCP reservations from the inventory; netboot switchable per blade |
| **Monitor** | Temperatures, fan, throttling, disk usage — seen from inside and outside, and said out loud by mail when a blade stays unwell |
| **Account** | What is in the racks: module and revision, memory, storage, firmware — read by the mini OS before an image is even chosen |
| **Reinstall** | Assign a different image, request installation, restart the blade |

Several distributions run side by side: the distribution is an attribute of the
blade, not a build process. Ubuntu, Debian and DietPi share the same flow.

## Layout

```
server/      Go, SQLite, web interface and REST API      → /srv/sheath/sheathd
site/        Go, one per site: DHCP reservations,        → /usr/local/bin/sheath-site
             netboot detection, image cache, relay
agent/       Go, runs on every blade                     → /usr/local/bin/sheath-agent
installer/   Go, runs in the mini OS during netboot      → inside boot.img
tools/       mirror and prepare images; build the mini OS and the netboot payload
deploy/      Ansible: the centre and the sites, from a release or a local build
assets/      Mark (SVG)
docs/        architecture, installation, screenshots
```

Alongside those: `docs/ARCHITECTURE.md` (why it is built this way),
`docs/ARCHITECTURE-SITES.md` (what a site is and what it may decide alone) and
`docs/INSTALLATION.md` (how to install it on the metal, including the traps
that went off along the way).

## More than one site

A site is one broadcast domain with a machine in it. `sheath-site` runs there
and is the only thing the blades of that site talk to: it writes the DHCP
reservations, holds the images and the payload, and answers its blades from
that cache while the centre is unreachable. The centre keeps the inventory and
the decisions and never touches a wire.

A new site signs itself in with a one-time code from the interface — the token
is written on the site machine and never travels through a terminal history:

```sh
sheath-site --server http://sheath-server:8080 --enroll ABCD-EFGH-JKLM
```

## Deploying

```sh
cd deploy
cp inventory.example.ini inventory.ini      # and edit it
ansible-playbook site.yml --check --diff    # look first
ansible-playbook site.yml -e sheath_version=v0.8.2-alpha
```

Two roles, one for the centre and one for a site. Binaries come from a release
or, with `sheath_local_bin_dir`, from a build of your own. DHCP is off unless
you ask for it: a second DHCP server in a segment that already has one is an
outage rather than a setting.

## Building

```sh
cd server    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sheathd .
cd site      && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sheath-site .
cd agent     && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sheath-agent .
cd installer && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sheath-installer .
```

All static, no runtime dependencies; arm64 for the blades, and the two server
parts build for amd64 as well. The agent needs 1 MB of memory.

The netboot payload is built rather than kept: `tools/build-minios.sh` takes
the Raspberry Pi network installer — pinned by checksum — strips what only a
screen needs, and puts the Sheath installer in the imager's place;
`tools/build-bootimg.sh` packs that into the `boot.img` the blades load.

The server binary is `sheathd`, not `sheath` — the plain name is kept free for
a command-line client.

## The name

A sheath is what a blade lives in when it is not in use: it holds the blade,
keeps its edge, and gives it a place. That is the whole job here — the blades
do the work, and this keeps them in order.

## License

Apache License 2.0 — see [LICENSE](LICENSE).

## Trademarks

BladeRunner and Compute Blade are trademarks of [Uptime Lab](https://docs.computeblade.com/).
Sheath is not affiliated with Uptime Lab; the names refer here to the hardware
being managed.
