# Rookery across multiple networks and sites

Proposal, as of 25.08.2026. Describes a target state, not today's one —
what exists today is documented in `INSTALLATION.md`.

---

## 1. The cut that suggests itself

Today Rookery combines two roles in a single process:

| Role | What it does | Where it has to sit |
|---|---|---|
| **Management** | Inventory, interface, decisions, configuration | anywhere — as long as it is reachable |
| **Network presence** | DHCP, TFTP, image delivery, netboot detection | **in the same broadcast segment as the blades** |

Only the second role is tied to the network segment. DHCP does not work across a
router, TFTP does not want to see WAN latency, and pulling a 1.2 GB image over a
site link is a bad idea once per blade.

> **The proposal is therefore to split along exactly this seam:**
> a **Rookery Server** at the central location, and one slim **Rookery Site** per site.

This is not a rebuild of what exists but a decomposition: today's single machine
becomes "site 1, whose central server happens to stand next to it".

---

## 2. Structure

```
                 ┌──────────────────────────────────────────┐
                 │  Rookery Server (central)                │
                 │  Inventory · Interface · Image catalogue │
                 │  Configuration · Audit trail             │
                 └───────────────┬──────────────────────────┘
                                 │  HTTPS, outbound from the site
        ┌────────────────────────┼────────────────────────┐
        │                        │                        │
┌───────┴────────┐      ┌────────┴───────┐       ┌────────┴───────┐
│ Rookery Site   │      │ Rookery Site   │       │ Rookery Site   │
│ Basement       │      │ Data centre    │       │ Hamburg office │
│                │      │                │       │                │
│ dnsmasq        │      │ dnsmasq        │       │ dnsmasq        │
│ Image cache    │      │ Image cache    │       │ Image cache    │
│ Agent relay    │      │ Agent relay    │       │ Agent relay    │
└───────┬────────┘      └────────┬───────┘       └────────┬───────┘
        │ own VLAN               │                        │
   ┌────┴────┐              ┌────┴────┐              ┌────┴────┐
   │ Blades  │              │ Blades  │              │ Blades  │
   └─────────┘              └─────────┘              └─────────┘
```

A site is a **network segment**, not a building. Two VLANs in the same rack are
two sites in the sense of this design — the boundary is drawn by the broadcast
domain, not by geography.

---

## 3. Five decisions

### 3.1 The truth lies centrally, the effect at the site

The site **decides nothing**. It receives a desired state and turns it into
dnsmasq configuration, TFTP directories and image provisioning. If the link goes
down, it carries on with whatever it last received.

That is the decisive availability property: **a WAN outage must not stop blades
from booting.** A site that can do nothing without the central server would be
worse than today's single machine.

### 3.2 Connections only go outbound

The site calls the central server, never the other way round. That means:

- no inbound firewall rules, no VPN needed, works behind NAT
- a site in someone else's network only needs outbound HTTPS
- the same pattern as with the blade agent — one mechanism instead of two

### 3.3 Blades talk to their site, not to the central server

The obvious thing would be to point the agents straight at the central server.
But that would require a route to the central server from **every blade** — with
ten sites, ten VPNs or a routed blade network. Both contradict the isolated
segment that netboot needs anyway.

> Instead the site relays: the agent only knows
> `https://site.local:8443`, the site forwards the request — and answers from the
> cache when the central server is currently unreachable.

A blade therefore **never** needs WAN access. Only the site has it.

### 3.4 Catalogue central, bytes local

The image catalogue (which images exist, which checksum) lives centrally. The
**files** live at the site. An image crosses the site link **once per site**, not
once per blade.

The site fetches an image as soon as it is assigned to one of its blades,
verifies the checksum and reports what it holds. The interface can then show
honestly whether a site already has an image in stock — with a 1.2 GB image over
a narrow link that is the difference between eleven minutes and an hour.

### 3.5 A site only sees itself

Every site gets its own credentials. With them it may only:

- read the desired state of **its** BladeRunners
- report observations from **its** blades
- relay requests from **its** blades

A compromised site must be able neither to read other sites' configuration nor to
re-image other sites' blades. This is not a nicety: a site may well stand in an
office that more people have access to than the data centre.

> **A limit named honestly:** because the site relays requests and is meant to be
> able to answer them offline, **it sees the configuration and the tokens of its
> own blades**. End-to-end encryption between the central server and the blade
> would be possible, but it would cost exactly that offline capability. The design
> deliberately decides against it and makes the site the trust boundary of its segment.

---

## 4. What changes in the data model

```yaml
Site:                          # new
  id, name, location
  net_cidr:    10.0.0.0/24
  gateway:     10.0.0.1
  dns:         10.0.0.10
  pool_from:   210             # dynamic range for unknown blades
  pool_to:     240
  rack_step:   20              # address block per BladeRunner
  domain:      basement.rookery.lan
  token, last_seen, agent_version
  wan_state:   online | stale | offline

BladeRunner:
  + site_id                    # belongs to exactly one site

Blade:
  (unchanged — the site follows from the BladeRunner)

Image:
  (catalogue unchanged)
SiteImage:                     # new: inventory per site
  site_id, image_id, state: absent | fetching | ready | error
  bytes_local, sha_ok, fetched_at
```

**Address management becomes per site instead of global.** Today `net_base` is a
single setting; in future it belongs to the site. The block mechanism stays
unchanged — it is simply applied within the network of the respective site.

A blade address thus becomes `(site, BladeRunner, slot)`. Two sites may use the
same network as long as they are separated; the combination is unique, the
address on its own no longer is.

**Names** gain the site: `r1s03.basement.rookery.lan`. Without that, `blade-r1s01`
from Basement and Hamburg collide as soon as you see both in the same list.

---

## 5. The interface between the central server and the site

Three endpoints are enough — the same pattern as with the agent: report, fetch, apply.

```
GET  /api/v1/site/{id}/desired        everything the site has to apply:
                                      reservations, netboot permissions,
                                      TFTP settings, required images
                                      → with ETag; unchanged = 304

POST /api/v1/site/{id}/events         batched observations:
                                      DHCP requests, TFTP deliveries,
                                      netboot progress

POST /api/v1/site/{id}/status         heartbeat: version, image inventory,
                                      dnsmasq state, clock offset
```

Plus the relayed blade endpoints, which the site offers under the same paths as
the central server does today — `/api/v1/blades/{serial}/config`, `/status`,
`/commands`, `/api/v1/provision/{serial}`. The agent notices nothing of the
rebuild; it just gets a different address in its seed.

### What the site builds from it

| From the desired state | becomes at the site |
|---|---|
| Reservations | one file each in `dhcp-hostsdir` |
| Netboot permission per blade | `set:bootnet` in the same file |
| Network, pool, gateway, DNS | the basic dnsmasq configuration |
| Assigned images | download into the local cache |
| TFTP settings | `boot.img`, `cmdline.txt` with **its** server address |

The last point matters: in future the `cmdline.txt` will contain
`rookery_server=https://site.basement:8443` — the address of the site, not of the
central server. Otherwise the mini-OS would have to go out to the WAN.

---

## 6. What still works when the link is down

This table is the real touchstone of the design.

| Operation | Central server reachable | Link dead |
|---|---|---|
| Blade boots from NVMe | ✓ | ✓ — needs nobody |
| Blade obtains an address | ✓ | ✓ — dnsmasq runs locally |
| Netboot blocked/allowed | ✓ | ✓ — the state is stored locally |
| Installation, image in the cache | ✓ | ✓ |
| Installation, image missing locally | ✓ | ✗ — no way to get at the bytes |
| Agent fetches configuration | ✓ | ✓ — from the cache |
| Agent reports status | ✓ | buffered, delivered later |
| **Assign a new image** | ✓ | ✗ — that is the central server's decision |
| Interface for this site | ✓ | shows "stale since …" |

The only hard loss is the **decision**. Everything already running keeps running.
That is the right division: without the central server a site should not be able
to act, but it should remain able to operate.

The interface must show the difference instead of concealing it: a site whose
data is ten minutes old is marked `stale`, and actions that need it are
queued as "will be carried out at the next contact" instead of being reported
as done.

---

## 7. What this means for existing components

| Component | Change |
|---|---|
| `server/` | site model, site endpoints, IPAM per site instead of global; the interface gains a site level above the BladeRunners |
| `agent/` | **none** — it keeps talking to the address from its seed, which in future points to the site |
| `installer/` | **none** — `rookery_server=` comes from the `cmdline.txt` that the site writes |
| `site/` | **new** — dnsmasq generator, image cache, relaying, log observation |

Much of this already exists: `syncDHCP`, the netboot detection from the dnsmasq
log and the image delivery move out of the server into the site instead of being
rewritten. The server loses code in the process.

---

## 8. The way there

| Step | Content | Result |
|---|---|---|
| **1** | `Site` into the data model, IPAM per site; existing data moves into "site 1" | One site, everything as before — but the model carries more |
| **2** | Split `site/` out as its own program, run it on the same machine | Two processes instead of one, the interface proven in real operation |
| **3** | Site credentials, permission boundaries, cache, offline behaviour | A second site would be possible |
| **4** | Image cache with inventory display, batched events | Bandwidth under control |
| **5** | Site level in the interface, stale marking | Multiple sites manageable |

Steps 1 and 2 are the actual work; after that the second site is mostly
configuration. The order matters: **separate first, then distribute.** Designing a
distributed system before the seam is right leads to an interface that can no
longer be changed later.

---

## 9. What I deliberately do not propose

- **No message bus.** With ten sites and a few hundred blades, HTTPS with ETag is
  sufficient and vastly simpler to operate than Kafka or MQTT.
- **No distributed database.** The central server remains the only source; the site
  has a cache, not a share in the truth. That means there is no conflict
  resolution — the most common problem of distributed systems never arises.
- **No VPN as a prerequisite.** It should be possible, but not necessary. Outbound
  HTTPS is the baseline assumption.
- **No inbound access to sites.** Everything the central server wants, the site
  fetches. That costs latency on actions and permanently saves firewall discussions.

---

## 10. Open points

- **Clock.** The site reports its offset to the central server. Commands expire
  after 15 minutes — with skewed clocks they expire wrongly. Either mandatory NTP
  at the site or a conversion via the reported offset.
- **A blade moves between sites.** The serial number stays, address and name
  change. So far unresolved whether that is a move or a new blade.
- **Two sites, the same network.** Allowed and common, but it means the IP alone
  is no longer unique — everything that looks things up by address today has to
  carry the site along.
- **Bootstrapping a site.** How does a new site get its credentials? Proposal: a
  one-time enrollment code from the interface, as with blade enrollment — the same
  pattern once again.
