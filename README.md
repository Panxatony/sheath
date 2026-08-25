# Rookery

Management for [Compute Blades](https://docs.computeblade.com/) in BladeRunner chassis.

Slot in an empty blade, pick an image in the interface — Rookery does the rest:
netboot, write the image to the NVMe, seed credentials and the agent, hand out a
fixed address, and watch it from then on.

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
server/      Go, SQLite, web interface and REST API      → /srv/rookery/rookery
agent/       Go, runs on every blade                     → /usr/local/bin/rookery-agent
installer/   Go, runs in the mini OS during netboot      → inside boot.img
tools/       pxeprobe.py — checks whether a blade is allowed to netboot
assets/      Mark (SVG)
```

Alongside those: `docs/ARCHITECTURE.md` (why it is built this way) and `docs/INSTALLATION.md`
(how to install it on the metal, including the traps that went off along the way).

## Building

```sh
cd server    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o rookery .
cd agent     && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o rookery-agent .
cd installer && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o rookery-installer .
```

All static, arm64, no runtime dependencies. The agent needs 1 MB of memory.

## The name

A *rook* is the chess piece that looks like a tower — the silhouette of a rack. A
*rookery* is also a colony of many nests of the same kind.

## License

Apache License 2.0 — see [LICENSE](LICENSE).

## Trademarks

BladeRunner and Compute Blade are trademarks of [Uptime Lab](https://docs.computeblade.com/).
Rookery is not affiliated with Uptime Lab; the names refer here to the hardware
being managed.
