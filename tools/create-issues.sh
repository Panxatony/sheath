#!/usr/bin/env bash
# Creates the open work packages as issues in the GitHub repository.
#
# Needs the GitHub CLI, authenticated:  gh auth login
#
#   ./tools/create-issues.sh              # create them
#   DRY_RUN=1 ./tools/create-issues.sh    # only print what would be created
#
# Labels are created on demand — a label that does not exist yet would
# otherwise fail the whole call.

set -euo pipefail

REPO=${GITHUB_REPO:-$(gh repo view --json nameWithOwner -q .nameWithOwner 2>/dev/null || echo "")}
[ -n "$REPO" ] || { echo "no repository — run inside a clone or set GITHUB_REPO" >&2; exit 1; }

ensure_labels() {
  local IFS=,
  for l in $1; do
    gh label create "$l" --repo "$REPO" >/dev/null 2>&1 || true
  done
}

create() {
  local title="$1" labels="$2" body="$3"
  if [ -n "${DRY_RUN:-}" ]; then
    printf '\n── %s\n   [%s]\n%s\n' "$title" "$labels" "$body"
    return
  fi
  ensure_labels "$labels"
  url=$(gh issue create --repo "$REPO" --title "$title" --label "$labels" --body "$body")
  printf '  ✓ %s  %s\n' "$url" "$title"
}

# ── Sites: the path laid out in docs/ARCHITECTURE-SITES.md ──────────────────

create "Site model and per-site address management" "sites,server" \
'The foundation for everything else. Without this step the rest has no place to hang.

**Scope**
- Table `sites`: name, location, `net_cidr`, gateway, DNS, pool range, `rack_step`, domain, token, `last_seen`
- `bladerunners.site_id` (today `racks`)
- Address management moves from global to per-site: `net_base` moves out of `settings` and into the site
- Migration: existing data moves to "Site 1"
- A blade address becomes `(site, BladeRunner, slot)`; names gain the site

**Why first**
Separate first, then distribute. An interface that comes into being before the seam cannot be changed afterwards.

See docs/ARCHITECTURE-SITES.md §4.'

create "Split sheath-site out as its own program" "sites,site" \
'Split the network presence out of the server — run on the same machine to begin with.

**Moves over (not rewritten)**
- `syncDHCP` — generating the dnsmasq reservations
- Netboot detection from the dnsmasq log
- TFTP root and `cmdline.txt`
- Image delivery over HTTP

**New**
- `GET /api/v1/site/{id}/desired` with ETag
- `POST /api/v1/site/{id}/events` (batched)
- `POST /api/v1/site/{id}/status`
- Relaying the blade endpoints under the same paths

The server loses code in the process. Agent and installer stay unchanged — they only ever talk to the address from their seed or their `cmdline.txt` anyway.

See docs/ARCHITECTURE-SITES.md §5.'

create "Site credentials and authorisation boundaries" "sites,security" \
'Every site gets its own credentials and may do exactly three things:
- read the desired state of **its** BladeRunners
- report observations about **its** blades
- relay requests from **its** blades

A compromised site must be able to neither read foreign configuration nor reimage foreign blades. This is not a nicety — a site may well sit in an office with looser access than the data centre.

**Boundary stated explicitly:** because the site relays and is meant to answer while offline, it does see the configuration and tokens of its own blades. End-to-end encryption would cost exactly that offline capability; the design deliberately decides against it.

See docs/ARCHITECTURE-SITES.md §3.5.'

create "Offline behaviour: cache and buffering" "sites,site" \
'A WAN outage must not stop blades from booting.

**Must work without the central server**
- Address assignment, netboot lock, installation from the cache
- The agent takes its configuration from the cache
- Status reports are buffered and delivered later

**Must not work without the central server**
- Assigning a new image — that is a decision

**Make it visible in the interface**
A site with stale data is marked as such; actions are queued as "will run at next contact" rather than reported as done.

See docs/ARCHITECTURE-SITES.md §6 — the outage table is the touchstone.'

create "Per-site image cache with inventory display" "sites,site" \
'Catalogue central, bytes local. An image crosses the site link once per site, not once per blade.

- Table `site_images`: state `absent | fetching | ready | error`, local size, checksum verified, timestamp
- The site fetches an image as soon as it is assigned to one of its blades
- The interface shows honestly whether a site has an image in stock

At 1.2 GB over a narrow link this is the difference between eleven minutes and an hour.'

create "Site layer in the interface" "sites,ui" \
'A site layer above the BladeRunners:
- Per-site overview with state (online / stale / offline) and last contact
- BladeRunner cards grouped under their site
- Netboot panel per site instead of global
- Mark stale data as stale rather than hiding it'

# ── Open design questions ──────────────────────────────────────────────

create "Clock skew between site and central server" "sites,open" \
'Commands expire after 15 minutes. With clocks drifting apart they expire wrongly — too early, or not at all.

To be decided: make NTP at the site a requirement, or subtract the reported skew when comparing. The site reports its skew in the heartbeat anyway.'

create "Blade moves between sites" "sites,open" \
'The serial stays, address and name change.

Unresolved: is that a move (history is kept, a new address is assigned) or a new blade (the old record is retired)? Both are defensible, but it has to be decided before the second site exists.'

create "Bootstrapping a new site" "sites,open" \
'How does a new site get its credentials?

Proposal: a one-time enrollment code from the interface — the same pattern as blade enrollment. The site signs in with it once and receives its permanent token.'

# ── From operations ────────────────────────────────────────────────────

create "Verify DietPi on NVMe" "images,open" \
'DietPi builds **no initramfs** by default (`SKIP_INITRAMFS_GEN=yes`). Ubuntu loads the NVMe driver as a module from the initrd — without an initrd, NVMe has to be built into the kernel, otherwise DietPi will not find its root and will not boot at all.

**First test, before DietPi enters the catalogue:** write the image to an NVMe and let it boot. A negative outcome costs one line in the catalogue, no design.'

create "Link instability under load" "hardware,open" \
'During the first installation attempt the blade’s Ethernet link started flapping under load from second 300 onwards (`bcmgenet eth0: Link is Down/Up`) and tore the download at 64 %.

The installer now survives drops via HTTP range resume, but the cause remains.

**Suspects:** PoE budget (802.3af instead of at?), Energy Efficient Ethernet on the switch port, cabling. Independently of that, portfast belongs on all blade ports — otherwise the switch spends around 30 seconds checking for loops after link-up and netboot becomes unreliable.'

create "Set BOOT_ORDER and DHCP_TIMEOUT during bring-up" "hardware,open" \
'Measured: the locked netboot costs **~45 seconds on every normal start** — three DHCP attempts at 16-second intervals, the factory `DHCP_TIMEOUT` of 45000 ms.

Recommendation per blade during bring-up via `rpiboot`:
```
BOOT_ORDER=0xf62        # network → NVMe → loop, without the useless SD/USB attempts
DHCP_TIMEOUT=10000      # 10 s instead of 45
NET_BOOT_MAX_RETRIES=1
MAC_ADDRESS=02:b1:ad:<runner>:00:<slot>
```

That cuts the 62 seconds to Ubuntu down to about 15. On CM4 `rpi-eeprom-update` is locked from the factory; the route via `flashrom`/SPI is possible but not atomic — for a fleet, `rpiboot` is the safe way.'

echo
if [ -n "${DRY_RUN:-}" ]; then
  echo "Preview only — run without DRY_RUN to create the issues."
else
  echo "Done."
fi
