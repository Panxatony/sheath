#!/usr/bin/env python3
"""Turn a copy of the live database into one that can be photographed.

The repository is public and the rack it manages is not. A screenshot of the
inventory carries serial numbers, MAC addresses, host names, site names and
addresses — so the pictures in the README come from a copy of the database
with every one of those replaced, served by a throwaway instance.

    sqlite3 /srv/sheath/data/sheath.db ".backup /tmp/demo.db"   # on the centre
    scripts/demo-db.py /tmp/demo.db

It works in two passes, because replacing identifiers column by column misses
one every time: first collect every serial, MAC and host name the database
mentions anywhere, then replace all of them as text in every text column —
prose in the event log, the written dnsmasq configuration, the facts a blade
reported, the scope of a per-blade setting. Afterwards a regex sweep catches
anything shaped like hardware that the collection did not know about, every
heartbeat is stamped to now so nothing reads as stale in a picture, and
notifications go off so the copy cannot send mail.

The copy is rewritten in place and VACUUMed: replacing a shorter string leaves
the original in the page slack, and this file travels to another machine.
"""
import json
import os
import pathlib
import re
import sqlite3
import sys
import time

# What the real names and networks are called cannot live here: this file is
# public, the installation is not. The mapping is read from local/demo-map.json
# (gitignored), or from $SHEATH_DEMO_MAP. See local/AGENTS.md; the shape is
#
#   {"sites": {"1": ["Hall A", "10.20.0", "ha"]},
#    "nets":  {"<real net>": "10.20.0"},
#    "words": {"<real site name>": "Hall A", "blade-xx-": "blade-ha-"}}
MAP = os.environ.get("SHEATH_DEMO_MAP") or str(
    pathlib.Path(__file__).resolve().parent.parent / "local" / "demo-map.json")
ACCOUNTS = [("ada", "admin"), ("olive", "operator")]
# A hash of nothing: the demo instance is reached with its own token, never a
# password, and a real one must not travel into a copy that gets photographed.
NO_PASSWORD = "pbkdf2-sha256$240000$AAAA$AAAA"

MAC_RE = re.compile(r"(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}")
SERIAL_RE = re.compile(r"\b[0-9a-fA-F]{16}\b")
FAKE_MAC = "02:00:5e"
# A real serial of this hardware reads 10000000 plus eight hex digits, so it
# begins the way a replacement does. "Starts with the fake prefix" therefore
# says nothing — whether a value is already a replacement is only known from
# the ones actually handed out, and `done` keeps them.
FAKE_SERIAL = "1000000"


def load_map():
    try:
        m = json.loads(pathlib.Path(MAP).read_text())
    except FileNotFoundError:
        sys.exit("no mapping at %s — see local/AGENTS.md" % MAP)
    sites = {int(k): tuple(v) for k, v in m["sites"].items()}
    return sites, m.get("nets", {}), m.get("words", {})


def text_columns(c):
    """Every column that can hold a string, table by table."""
    for (table,) in list(c.execute(
            "select name from sqlite_master where type='table' "
            "and name not like 'sqlite_%'")):
        cols = [r[1] for r in c.execute("pragma table_info(%s)" % table)]
        if cols:
            yield table, cols


def main(path: str) -> None:
    sites, nets, words = load_map()
    db = sqlite3.connect(path)
    c = db.cursor()
    now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())

    # ---- pass one: what identifies this installation -----------------------
    rack_site = {r[0]: r[1] for r in c.execute("select id,site_id from racks")}
    blades = list(c.execute(
        "select serial,hostname,mac,rack_id from blades order by serial"))

    swap: dict[str, str] = {}
    done: set[str] = set()
    minted = [0]

    def give(real: str, fake: str) -> str:
        swap[real] = fake
        done.add(fake)
        return fake

    def mint(kind: str, real: str) -> str:
        if real not in swap:
            minted[0] += 1
            n = minted[0]
            give(real, "%s:ff:%02x:%02x" % (FAKE_MAC, n // 256, n % 256)
                 if kind == "mac" else "%s%09d" % (FAKE_SERIAL, n))
        return swap[real]

    for i, (serial, host, mac, rack) in enumerate(blades):
        give(serial, "%s%09d" % (FAKE_SERIAL, i + 1))
        minted[0] = max(minted[0], i + 1)
        if mac:
            give(mac, "%s:%02x:%02x:%02x" % (
                FAKE_MAC, i + 1, (i * 7) % 256, (i * 13) % 256))
        if host:
            pfx = sites.get(rack_site.get(rack, 0), ("", "", "x"))[2]
            suffix = host.split("-", 2)[2] if host.count("-") >= 2 else host
            give(host, "blade-%s-%s" % (pfx, suffix))

    # Serials and MACs of hardware that is no longer in the inventory still sit
    # in the event log, in old netboot rows and in per-blade settings.
    for table, col in (("events", "serial"), ("commands", "serial"),
                       ("samples", "serial"), ("netboot", "serial"),
                       ("netboot", "mac"), ("configs", "scope")):
        try:
            rows = list(c.execute(
                "select distinct %s from %s where %s is not null" % (col, table, col)))
        except sqlite3.OperationalError:
            continue
        for (val,) in rows:
            if not isinstance(val, str):
                continue
            raw = val.split(":", 1)[1] if col == "scope" and val.startswith("blade:") else val
            if not raw or raw in swap or raw in done:
                continue
            if MAC_RE.fullmatch(raw):
                mint("mac", raw)
            elif SERIAL_RE.fullmatch(raw):
                mint("serial", raw)

    swap.update(nets)
    swap.update(words)

    # ---- pass two: replace all of it, everywhere ---------------------------
    # Longest first: a host name contains the prefix that also stands for a
    # site, and replacing the short one first would leave half a name behind.
    order = sorted(swap, key=len, reverse=True)
    for table, cols in text_columns(c):
        rows = list(c.execute("select rowid,%s from %s" % (",".join(cols), table)))
        for row in rows:
            rowid, values = row[0], row[1:]
            changed = {}
            for col, val in zip(cols, values):
                if not isinstance(val, str) or not val:
                    continue
                new = val
                for real in order:
                    if real in new:
                        new = new.replace(real, swap[real])
                # Anything still shaped like hardware was never in a column we
                # collected from — a MAC in a log line from a blade that was
                # forgotten, say. It gets a replacement too.
                new = MAC_RE.sub(
                    lambda m: m.group(0) if m.group(0) in done
                    else mint("mac", m.group(0)), new)
                new = SERIAL_RE.sub(
                    lambda m: m.group(0) if m.group(0) in done
                    else mint("serial", m.group(0)), new)
                if new != val:
                    changed[col] = new
            if changed:
                c.execute("update %s set %s where rowid=?" % (
                    table, ",".join("%s=?" % k for k in changed)),
                    list(changed.values()) + [rowid])

    # ---- the rest of the picture -------------------------------------------
    for sid, (name, net, pfx) in sites.items():
        c.execute(
            "update sites set name=?,net_base=?,host_prefix=?,relay_url=?,trouble='' "
            "where id=?", (name, net, pfx, "http://%s.9:8081" % net, sid))
    c.execute("update blades set short_serial=substr(serial,-8)")

    c.execute("delete from users")
    for name, role in ACCOUNTS:
        c.execute(
            "insert into users(name,pass,role,created,last_login) values(?,?,?,?,?)",
            (name, NO_PASSWORD, role, now, now))

    # Nothing in a picture should read as stale, and the copy must not mail.
    c.execute("update sites set last_seen=?", (now,))
    c.execute("update blades set last_seen=? where last_seen<>''", (now,))
    c.execute("update settings set value='0' where key='notify_enabled'")

    db.commit()
    # A plain VACUUM keeps the file's pages, and an overwritten string survives
    # in the unused space inside one — two real serial numbers were still
    # findable with `strings` after everything had been replaced. VACUUM INTO
    # writes a fresh file from the live rows alone, so nothing is left over.
    tmp = path + ".vacuum"
    pathlib.Path(tmp).unlink(missing_ok=True)
    c.execute("vacuum into ?", (tmp,))
    db.close()
    os.replace(tmp, path)
    print("rewritten: %s (%d identifiers replaced)" % (path, len(swap)))


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    main(sys.argv[1])
