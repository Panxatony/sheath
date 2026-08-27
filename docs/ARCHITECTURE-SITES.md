# Sheath across multiple networks and sites

Describes what is built, as of 26.08.2026. `sheath-site` exists as its own
program and runs alongside the central `sheathd`. How to install both on the
metal is `INSTALLATION.md`; why the split looks like this is here.

The reasoning is kept as it was written, because it is what the document is
for: the decisions below are cheap to read and expensive to re-derive. What is
still open is named in §10 and nowhere else — everything before that section
describes running code.

---

## 1. The cut

Sheath used to combine two roles in a single process:

| Role | What it does | Where it has to sit |
|---|---|---|
| **Management** | Inventory, interface, decisions, configuration | anywhere — as long as it is reachable |
| **Network presence** | DHCP, TFTP, image delivery, netboot detection | **in the same broadcast segment as the blades** |

Only the second role is tied to the network segment. DHCP does not work across a
router, TFTP does not want to see WAN latency, and pulling a 1.2 GB image over a
site link is a bad idea once per blade.

> **The split runs along exactly this seam:** `sheathd` at the central location,
> and one slim `sheath-site` per site.

This was a decomposition, not a rebuild: the machine that used to do both is now
"site 1, whose central server happens to stand next to it". That configuration
still works, and it is what `-local-dhcp=true` means.

---

## 2. Structure

```text
                 ┌──────────────────────────────────────────┐
                 │  sheathd (central)                       │
                 │  Inventory · Interface · Image catalogue │
                 │  Configuration · Policy · Audit trail    │
                 └───────────────┬──────────────────────────┘
                                 │  HTTPS, outbound from the site
        ┌────────────────────────┼────────────────────────┐
        │                        │                        │
┌───────┴────────┐      ┌────────┴───────┐       ┌────────┴───────┐
│ sheath-site    │      │ sheath-site    │       │ sheath-site    │
│ Basement       │      │ Data centre    │       │ Hamburg office │
│                │      │                │       │                │
│ dnsmasq        │      │ dnsmasq        │       │ dnsmasq        │
│ Image cache    │      │ Image cache    │       │ Image cache    │
│ Blade relay    │      │ Blade relay    │       │ Blade relay    │
└───────┬────────┘      └────────┬───────┘       └────────┬───────┘
        │ own VLAN               │                        │
   ┌────┴────┐              ┌────┴────┐              ┌────┴────┐
   │ Blades  │              │ Blades  │              │ Blades  │
   └─────────┘              └─────────┘              └─────────┘
```

A site is a **network segment**, not a building. Two VLANs in the same rack are
two sites in the sense of this design — the boundary is drawn by the broadcast
domain, because that is the only place DHCP reaches.

`sheath-site` holds no state of its own beyond a cache. It takes flags, not a
configuration file:

```text
-server       central Sheath server, e.g. https://sheath.example   (required)
-site         id of this site                                      (required)
-token-file   /etc/sheath-site/token
-dhcp-hosts   /etc/sheath/dhcp-hosts        dnsmasq dhcp-hostsdir
-dnsmasq-log  /srv/sheath/logs/dnsmasq.log  log to watch
-images       /srv/sheath/images            image cache
-tftp         /srv/sheath/tftp              TFTP root
-state        /var/lib/sheath-site          where the last desired state lives
-interval     30s                           between two passes
-listen       :8081                         the blade relay; empty turns it off
```

One pass is: fetch the desired state, write the reservations, make sure the boot
payload and the images are there, hand over what was observed, hand over the
blade reports that were buffered, report status. Every step may fail on its own —
a site with a stale image list still has to write its reservations.

---

## 3. Five decisions

### 3.1 The truth lies centrally, the effect at the site

The site **decides nothing**. It receives a desired state and turns it into
dnsmasq host records, a TFTP payload and cached image bytes. If the link goes
down, it carries on with whatever it last received.

That is the decisive availability property: **a WAN outage must not stop blades
from booting.** A site that could do nothing without the central server would be
worse than the single machine it replaced.

### 3.2 Connections only go outbound

The site calls the central server, never the other way round. That means:

- no inbound firewall rules, no VPN needed, works behind NAT
- a site in someone else's network only needs outbound HTTPS
- the same pattern as with the blade agent — one mechanism instead of two

The price is latency on actions: a change made in the interface reaches the wire
at the site's next pass, which is 30 seconds by default. The one case where that
was too slow got its own trigger — see §5.

### 3.3 Blades talk to their site, not to the central server

The obvious thing would be to point the agents straight at the central server.
But that would require a route to the central server from **every blade** — with
ten sites, ten VPNs or a routed blade network. Both contradict the isolated
segment that netboot needs anyway.

> Instead the site relays: the agent knows one address, the site forwards the
> request — and answers from the cache when the central server is currently
> unreachable.

A blade therefore **never** needs WAN access. Only the site has it.

### 3.4 Catalogue central, bytes local

The image catalogue (which images exist, which checksum) lives centrally. The
**files** live at the site. An image crosses the site link **once per site**, not
once per blade.

The site fetches an image as soon as it is assigned to one of its blades,
verifies the checksum and reports what it holds. The interface shows honestly
whether a site already has an image in stock — with a 1.2 GB image over a narrow
link that is the difference between eleven minutes and an hour.

### 3.5 A site only sees itself

Every site has its own credential. With it, it may only:

- read the desired state of **its** blades
- report observations from **its** site
- relay requests from **its** blades

The id in the path and the token have to agree; a token belonging to another site
is a 401. A compromised site can neither read another site's configuration nor
re-image another site's blades. This is not a nicety: a site may well stand in an
office that more people have access to than the data centre.

> **A limit named honestly:** because the site relays requests and has to be able
> to answer them offline, **it sees the configuration and the tokens of its own
> blades**. They arrive in the desired state and are written to
> `/var/lib/sheath-site/desired.json`, mode 0640 in a 0750 directory. End-to-end
> encryption between the central server and the blade would be possible, but it
> would cost exactly that offline capability. The design deliberately decides
> against it and makes the site the trust boundary of its segment.
>
> The one thing the document does **not** carry is the site's own token: it is
> blanked before the response is written, so the file a site keeps on disk does
> not contain the key to itself.

---

## 4. The data model

```sql
CREATE TABLE sites (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    location    TEXT    NOT NULL DEFAULT '',
    net_base    TEXT    NOT NULL,              -- e.g. 10.0.0
    gateway     TEXT    NOT NULL DEFAULT '',
    dns         TEXT    NOT NULL DEFAULT '',
    domain      TEXT    NOT NULL DEFAULT '',
    pool_from   INTEGER NOT NULL DEFAULT 210,  -- loan addresses for
    pool_to     INTEGER NOT NULL DEFAULT 240,  -- blades not yet in the inventory
    offset_base INTEGER NOT NULL DEFAULT 100,  -- first address block
    offset_step INTEGER NOT NULL DEFAULT 20,   -- block size per BladeRunner
    local       INTEGER NOT NULL DEFAULT 0,    -- this process serves the wire here
    token       TEXT    NOT NULL DEFAULT '',
    policy_json TEXT    NOT NULL DEFAULT '',   -- per-site threshold overrides
    last_seen   TEXT    NOT NULL DEFAULT '',
    created     TEXT    NOT NULL
);

CREATE TABLE racks (
    ...
    site_id   INTEGER NOT NULL DEFAULT 1,
    ip_offset INTEGER NOT NULL,
    -- Per site, not globally: two sites are two networks, and .100 in one is
    -- a different address from .100 in the other.
    UNIQUE (site_id, ip_offset)
);

CREATE TABLE site_images (              -- what a site says it actually holds
    site_id  INTEGER NOT NULL,
    image_id TEXT    NOT NULL,
    state    TEXT    NOT NULL DEFAULT 'absent',  -- absent | fetching | ready | error
    bytes    INTEGER NOT NULL DEFAULT 0,
    note     TEXT    NOT NULL DEFAULT '',
    ts       TEXT    NOT NULL,
    PRIMARY KEY (site_id, image_id)
);
```

A blade is unchanged: the site follows from its BladeRunner. The image catalogue
is unchanged; `site_images` is what a site reports on top of it, and the
difference between the two is worth showing — an image assigned to a blade at a
site that has not fetched it yet is an installation that will wait.

**Address management is per site.** `net_base` belongs to the site, and the block
mechanism is applied within that site's network:

| Value | Rule | Example |
|---|---|---|
| IP | `<site.net_base>.(rack.ip_offset + slot)` | `10.0.0.103` |
| MAC | `02:b1:ad:<runner>:00:<slot>` | `02:b1:ad:01:00:03` |
| Hostname | `blade-r<runner>s<slot>` | `blade-r1s03` |

The runner number is derived from the address block, not from the row id:
`(ip_offset - site.offset_base) / site.offset_step + 1`. A blade address is thus
`(site, BladeRunner, slot)`. Two sites may use the same network as long as they
are separated; the combination is unique, the address on its own is not — which
is why a netboot session records the site that saw it, and why the block
uniqueness moved from `UNIQUE(ip_offset)` to `UNIQUE(site_id, ip_offset)` in a
table rebuild. Before that, the second site could not have a `.100` block because
the first one already did.

Moving a BladeRunner to another site takes a fresh offset there and renumbers
every blade in it. Hand-set hostnames and real vendor MACs survive the move;
derived ones are regenerated, because a derived name that no longer matches its
place is worse than no name.

---

## 5. The interface between the central server and the site

Three endpoints — the same pattern as with the agent: fetch, report, report.

```http
GET  /api/v1/site/{id}/desired    everything the site has to apply:
                                  its own blades with reservation and netboot
                                  flag, the images those blades need, the boot
                                  payload → with ETag; unchanged = 304

POST /api/v1/site/{id}/events     batched observations: DHCP requests, TFTP
                                  deliveries, netboot progress, and the site's
                                  own notes

POST /api/v1/site/{id}/status     heartbeat: version, applied state, clock,
                                  image stock
```

All three are authenticated with `Authorization: Bearer <site token>`, compared
in constant time against `sites.token`. The token is issued or rotated by
`POST /api/v1/sites/{id}/token`, which needs the admin token and shows the value
once — a site that lost it gets a new one rather than the old one back. A site
without a token is a legitimate state, not a fault: the interface calls it
"no site process".

### The desired state

```json
{
  "site":  { "id": 2, "net_base": "10.0.0", "gateway": "…", "pool_from": 210, … },
  "blades": [
    { "serial": "…", "mac": "…", "hostname": "blade-r1s03", "ip": "10.0.0.103",
      "rack": "rack-1", "slot": 3, "netboot": true, "image": "debian-13-arm64",
      "token": "…", "config": { … }, "config_version": "sha256:…",
      "install_state": "pending" }
  ],
  "images": [ { "id": "…", "url": "…", "sha256": "…", "bytes": 0, "local": "…" } ],
  "boot":   { "boot_img": "…/boot/boot.img", "server_url": "…" },
  "version": "sha256:9f2c…", "produced": "…"
}
```

Only blades of that site, and only those that have a place and an address. Only
images one of those blades is assigned to. `netboot` is true when the blade's
install state is `pending` or `wipe` — writing an image and erasing a disk both
need the blade to come up in the mini OS, and this flag is the only thing that
arms either.

`version` is the content, hashed, and it doubles as the ETag. Only the content is
hashed: `last_seen` changes on every request the site makes, and hashing the site
row whole would hand out a new version every thirty seconds and make the
conditional request pointless.

### What the site builds from it

| From the desired state | becomes at the site |
|---|---|
| A blade's MAC, hostname, IP | `blade-<serial>.conf` in `dhcp-hostsdir` |
| `netboot: true` | `set:bootnet,` in the same file |
| Assigned images | download into `/srv/sheath/images` |
| `boot.boot_img` | `boot.img` in the TFTP root |
| A blade's config and token | the answer the relay gives while offline |

A reservation file holds **only** what would otherwise stand to the right of
`dhcp-host=`:

```text
# Sheath – generated by sheath-site, do not edit by hand
# Blade 10000000abcdef12  BladeRunner rack-1  Slot 3
# install requested – boots over the network
d8:3a:dd:11:22:33,set:bootnet,blade-r1s03,10.0.0.103,infinite
```

Include the `dhcp-host=` prefix and dnsmasq reports "bad hex constant" and drops
the line silently — the reservation then has no effect at all while everything
above it reports success. A file whose content is already right is not rewritten;
files for blades no longer in the state are removed; and only when something
actually changed does the site run `sudo -n systemctl reload dnsmasq`. The reload
is needed because dnsmasq picks up *new* host records in a `dhcp-hostsdir` by
itself but only forgets a removed or rewritten one on SIGHUP.

### Watching the wire

dnsmasq's log is the only place where "a blade is booting right now" appears at
all — at that point the blade is a bootloader with no idea Sheath exists. The
site tails the log, seeking to the end on start so a restart does not report last
week's boots as if they were happening now, and handling a `copytruncate`
logrotate by reopening.

It reports four stages: `dhcp` when an address is asked for or acknowledged,
`tftp` when the DHCP vendor class contains `PXEClient` (that is the RPi
bootloader identifying itself, and the difference between a blade that *wanted*
to netboot and one that only took an address), `ramdisk` when a TFTP file was
delivered, and `error` when one failed. The transaction id is what ties a vendor
class to the MAC it belongs to.

Events carrying a stage and a MAC become netboot sessions at the centre rather
than log lines, stamped with the site that saw them.

### The relayed blade endpoints

The site offers, on `:8081`, the same paths the central server does:

```text
POST /api/v1/provision/{serial}          the installer's job
POST /api/v1/provision/{serial}/status   its progress
GET  /api/v1/blades/{serial}/config      the agent's desired state
POST /api/v1/blades/{serial}/status      its report
GET  /api/v1/blades/{serial}/commands    what a person asked for
POST /api/v1/enroll                      a blade introducing itself
GET  /images/…  /boot/…  /agent/…        bytes
GET  /healthz                            site id, online, applied, queued
```

The agent notices nothing of the split; it just has a different address in its
seed. Ten seconds without an answer from the centre counts as away — a blade
waiting on us is a blade not booting.

Two behaviours are worth naming because they are not plain proxying:

**The relay rewrites an image URL to itself when it holds the image.** The
centre names itself in that URL, which is correct and useless: the bytes are
already here, the site link may be slow, and — as one afternoon showed — the
centre can move to another machine mid-download and take two installations with
it. Only the `url` field is touched; the checksum the centre stated still
governs.

**An installer reporting `done` or `wiped` triggers an immediate pass.** A
finished installer restarts within seconds, and if the reservation still carries
the netboot tag at that moment, the blade lands in the installer again. Thirty
seconds of polling is fine for noticing a change somebody made elsewhere; it is
too slow for a change this program itself has just caused.

---

## 6. What still works when the link is down

This table is the real touchstone of the design.

| Operation | Central server reachable | Link dead |
|---|---|---|
| Blade boots from NVMe | yes | yes — needs nobody |
| Blade obtains an address | yes | yes — dnsmasq runs locally |
| Netboot blocked/allowed | yes | yes — the reservation is already written |
| Installation, image in the cache | yes | yes — answered from the cached state |
| Installation, image missing locally | yes | no — `waiting`, "has not arrived here yet" |
| Erasing a disk, already requested | yes | yes — needs no image and no new decision |
| Agent fetches configuration | yes | yes — from the cache, against the blade's token |
| Agent reports status | yes | buffered, delivered in order later |
| A blade nobody has enrolled yet | yes | no — `waiting`; enrolling is a decision |
| **Assign a new image** | yes | no — that is the central server's decision |
| **A command** (identify, reboot, reimage) | yes | none — an empty list, not a guess |
| Interface for this site | yes | shows `stale`, then `offline` |

The only hard loss is the **decision**. Everything already running keeps running.
That is the right division: without the central server a site should not be able
to act, but it should remain able to operate.

Commands deserve the emphasis they get. A command is something a person asked
for, and while the centre is unreachable there is nothing to ask; an empty list
is the honest answer and the agent will ask again. The alternative — a site
inferring what somebody probably wanted — is how a fleet reboots itself.

Reports are the mirror image: they are facts about what happened, and losing them
would erase exactly the part of the story nobody else saw. The relay answers the
blade `202` with the queue depth, keeps the request whole — method, path, body
and the blade's own `Authorization` header, so the centre still authenticates the
original blade — and replays them oldest first at the end of the next successful
pass. On the first failure it stops and puts the remainder back in order: a
progress report that arrived after the "done" it precedes would tell the wrong
story.

The interface shows the difference instead of concealing it. A site is `online`
under three minutes since its last contact, `stale` under fifteen, `offline`
beyond that, and "no site process" when it has never been given a token.

---

## 7. What each program owns

| Component | Role |
|---|---|
| `server/` (`sheathd`) | Inventory, interface, image catalogue, configuration merge, policy, netboot *state* and the image *decision*, the audit trail. Serves the netboot payload at `GET /boot/` so a site needs no build tooling. |
| `site/` (`sheath-site`) | The wire: DHCP reservations, the netboot switch per blade, log watching, the image cache, the boot payload in the TFTP root, and the relay. |
| `agent/` | Unchanged by the split — it talks to the address in its seed, which now points at the site. |
| `installer/` | Unchanged by the split — its server address comes from `sheath_server=` in `cmdline.txt`. |

The centre keeps owning the netboot state machine and the image decision; it stops
owning the wire wherever a site is present. `-local-dhcp=false` is what says so:
the server then neither writes reservations nor tails the dnsmasq log, and
`syncDHCP` reports "written by sheath-site, not here" instead of doing it. Two
programs owning one directory would mean the loser is whoever wrote last.

Observations from a remote site flow into the same tables as those from a local
dnsmasq — `POST /events` calls the same `touchNetboot` the local watcher does.
There is one netboot state machine, fed from two places.

---

## 8. The route taken

| Step | Content | State |
|---|---|---|
| **1** | `Site` in the data model, IPAM per site; existing data became "site 1" | done |
| **2** | `sheath-site` split out as its own program, run on the same machine | done |
| **3** | Site credentials, permission boundary, cached state, offline behaviour | done |
| **4** | Image cache with a stock report, batched events | done |
| **5** | Site level in the interface: overview, site page, map, stale marking | done |

Steps 1 and 2 were the actual work; after them a second site is mostly
configuration. The order mattered: **separate first, then distribute.** Designing
a distributed system before the seam is right leads to an interface that can no
longer be changed later.

---

## 9. What is deliberately not there

- **No message bus.** With ten sites and a few hundred blades, HTTPS with an ETag
  is sufficient and vastly simpler to operate than Kafka or MQTT.
- **No distributed database.** The central server remains the only source; the
  site has a cache, not a share in the truth. There is no conflict resolution —
  the most common problem of distributed systems never arises.
- **No VPN as a prerequisite.** It is possible, but not necessary. Outbound HTTPS
  is the baseline assumption.
- **No inbound access to sites.** Everything the central server wants, the site
  fetches. That costs latency on actions and permanently saves firewall
  discussions.
- **No site scope in the configuration merge.** Configuration is layered
  `global → group → blade`. What differs per site is *policy* — thresholds and
  timings — and that has its own mechanism, because a threshold is a property of
  a place and a configuration is a property of a machine.

---

## 10. Decided, and open

### A blade belongs to its module, not to a site

Carry a blade from one site to another and it is the same blade: the serial is
the identity, the record stays, and everything ever written about it stays
readable. What changes is what the position decides — the address, derived from
(site, BladeRunner, slot), and the name, which takes the new site's prefix.
Nothing is retired and nothing is enrolled again.

The alternative — a new record at the new site — was rejected because it ends
the history of a module every time somebody picks it up, and the history is the
reason the inventory is worth keeping. `TestBladeKeepsItselfWhenItMovesBetweenSites`
holds this in place.

### Still open


- **Clock skew is measured and not used.** The site reports its clock in every
  heartbeat, and the centre logs the offset when it exceeds a minute. Commands
  expire after fifteen minutes on both sides — but that expiry is still computed
  against the centre's own clock. Either mandatory NTP at the site, or subtract
  the reported offset where commands expire. Today it is a note in the log.

- **Two sites, the same network.** Allowed and common, and the model handles it:
  the block uniqueness is per site and a netboot session records which site saw
  it. What has not been swept is every remaining lookup by address — anything
  that still resolves an IP without carrying the site along will find the wrong
  blade the day two sites overlap.
