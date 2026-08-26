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

# ── The multi-site remainder ───────────────────────────────────────────
#
# The site model, the sheath-site program, its credentials, the offline
# behaviour, the per-site image cache and the site layer in the interface are
# built and running. What follows is what those left behind.

create "A new site gets its token by hand" "sites,open" \
'Today a site is enrolled by generating a token in the interface, copying it
into `/etc/sheath-site/token` on the site machine, and setting the mode. That
works and it is one step too many to be trusted with fingers.

**Proposal:** a one-time enrollment code, the same pattern the blades already
use. The site signs in with it once and receives its permanent token, which
never travels through a terminal history.

See docs/ARCHITECTURE-SITES.md §10.'

create "Clock skew is measured but not subtracted" "sites,open" \
'Commands expire after fifteen minutes. The site reports its skew against the
centre in every heartbeat and nothing is done with the number: with clocks
apart, a command expires too early at one site and too late at another.

To decide: subtract the reported skew when comparing, or make NTP at the site a
requirement and say so in the interface when it is not met.'

create "A blade that moves between sites" "sites,open" \
'The serial stays, address and name change. Unresolved: is that a move — the
history is kept and a new address assigned — or a new blade, the old record
retired?

Both are defensible. Two sites now exist, so it is no longer hypothetical.'

create "The relay loses buffered reports on a restart" "sites,site" \
'While the centre is unreachable, `sheath-site` buffers status reports and
events in memory and delivers them when contact returns. A restart of the site
process in that window drops them silently.

**Scope:** write the queue to `/var/lib/sheath-site/`, in the same order it
would have been delivered, and drain it at startup. A blade that reported a
failed installation during an outage should not have to report it twice.'

create "The netboot payload still names the centre" "sites,site" \
'`cmdline.txt` in the payload carries `sheath_server=` pointing at the central
server, and each site serves that payload as it received it. It works because
the relay is reachable both ways here, but a site on a link that is down would
hand a blade an address it cannot reach.

The desired state already carries `cmdline_url`, and nothing reads it. Either
the site rewrites the payload for itself, or the field goes.'

# ── Server and agent ───────────────────────────────────────────────────

create "offline_after_min is read per site and applied globally" "server,bug" \
'The health verdict honours a site policy for how long a blade may stay silent.
The sweep that flips a blade to `offline` uses the global value. A site allowed
a longer silence is therefore marked offline on time and unhealthy late — two
answers to the same question.'

create "no_clock_sync is declared and never read" "installer,bug" \
'`no_clock_sync` is a field of the install options and appears nowhere else in
the installer. It could not work as written either: `syncClock` runs before the
job is fetched, so the clock is already set by the time the option arrives.

Either set the clock lazily, or drop the option and say in the documentation
that the mini OS always syncs.'

# ── Images ─────────────────────────────────────────────────────────────

create "Preparing an image compresses it twice" "images,perf" \
'`mirror-image.sh` compresses the disk image with `xz -3`; `prepare-image.sh`
then decompresses it, changes it, and compresses it again. On a Compute Module
that is roughly ten minutes of CPU spent producing a file nobody will ever read.

Where preparation is requested, the mirror step should hand the raw image over
and only the prepared result should be compressed.'

create "Nothing says which installer a site is serving" "sites,ui" \
'The netboot payload is built by hand and copied to the centre; the sites fetch
it from there. Nothing in the interface says which version a site currently
serves, so an installer fix is deployed by remembering to deploy it.

**Scope:** stamp the payload with a version, report it in the site heartbeat,
show it beside the site — and say so when it differs from the centre.'

# ── Not built, and wanted ──────────────────────────────────────────────

create "Nobody is told when a blade goes bad" "server,open" \
'The health verdict is computed, coloured and logged. That is all it does.
A blade that overheats at three in the morning is amber on a page nobody is
looking at.

**Scope:** a notification path — mail, or a webhook — for a verdict that gets
worse and stays worse, with enough hysteresis that a blade rebooting does not
send anything.'

create "Power cycle from the interface" "hardware,open" \
'A blade that hangs before its agent starts can only be recovered by switching
its PoE port. That means logging in to the switch.

**Scope:** a switch adapter (SNMP or a vendor API), a port recorded per slot,
and one button — behind a confirmation, because it cuts power to a running
machine.'

# ── From operations ────────────────────────────────────────────────────

create "Link instability under load" "hardware,open" \
'During the first installation attempt the blade’s Ethernet link started
flapping under load from second 300 onwards (`bcmgenet eth0: Link is Down/Up`)
and tore the download at 64 %.

The installer now survives drops via HTTP range resume, but the cause remains.

**Suspects:** PoE budget (802.3af instead of at?), Energy Efficient Ethernet on
the switch port, cabling. Independently of that, portfast belongs on all blade
ports — otherwise the switch spends around 30 seconds checking for loops after
link-up and netboot becomes unreliable.'

create "Set BOOT_ORDER and DHCP_TIMEOUT during bring-up" "hardware,open" \
'Measured: the locked netboot costs **~45 seconds on every normal start** —
three DHCP attempts at 16-second intervals, the factory `DHCP_TIMEOUT` of
45000 ms.

Recommendation per blade during bring-up via `rpiboot`:
```
BOOT_ORDER=0xf62        # network → NVMe → loop, without the useless SD/USB attempts
DHCP_TIMEOUT=10000      # 10 s instead of 45
NET_BOOT_MAX_RETRIES=1
MAC_ADDRESS=02:b1:ad:<runner>:00:<slot>
```

That cuts the 62 seconds to Ubuntu down to about 15. On CM4 `rpi-eeprom-update`
is locked from the factory; the route via `flashrom`/SPI is possible but not
atomic — for a fleet, `rpiboot` is the safe way.'

echo
if [ -n "${DRY_RUN:-}" ]; then
  echo "Preview only — run without DRY_RUN to create the issues."
else
  echo "Done."
fi
