# Sheath

Management for [Compute Blades](https://docs.computeblade.com/) in BladeRunner chassis.

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

The screenshots come from a demonstration instance with invented blades — the
addresses and serial numbers in them belong to nothing.

## What it does

| Pillar | |
|---|---|
| **Provision** | Netboot into a mini OS that writes the assigned image to the NVMe |
| **Configure** | The agent pulls its desired state every 60 s and applies it idempotently |
| **Address** | DHCP reservations from the inventory; netboot switchable per blade |
| **Monitor** | Temperatures, fan, throttling, disk usage — seen from inside and outside |
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
tools/       mirror, prepare and publish images; build the netboot payload
assets/      Mark (SVG)
docs/        architecture, installation, screenshots
```

Alongside those: `docs/ARCHITECTURE.md` (why it is built this way) and `docs/INSTALLATION.md`
(how to install it on the metal, including the traps that went off along the way).

## Building

```sh
cd server    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sheathd .
cd site      && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sheath-site .
cd agent     && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sheath-agent .
cd installer && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sheath-installer .
```

All static, no runtime dependencies; arm64 for the blades, and the two server
parts build for amd64 as well. The agent needs 1 MB of memory.

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
