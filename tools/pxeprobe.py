#!/usr/bin/env python3
"""Send a DHCPDISCOVER that poses as the Raspberry Pi bootloader and report
whether dnsmasq answers with option 43 ("Raspberry Pi Boot").

That tells you whether netboot is unlocked for a given MAC — without
restarting a blade.
"""
import random
import socket
import struct
import sys

MAGIC = b"\x63\x82\x53\x63"
VENDOR = b"PXEClient:Arch:00000:UNDI:002001"


def mac_bytes(s):
    return bytes(int(x, 16) for x in s.split(":"))


def build_discover(mac, xid):
    p = b""
    p += struct.pack("!BBBB", 1, 1, 6, 0)          # op, htype, hlen, hops
    p += struct.pack("!I", xid)
    p += struct.pack("!HH", 0, 0x8000)             # secs, flags=broadcast
    p += b"\x00" * 16                              # ci/yi/si/gi addr
    p += mac_bytes(mac) + b"\x00" * 10             # chaddr
    p += b"\x00" * 192                             # sname + file
    p += MAGIC
    p += bytes([53, 1, 1])                         # message type = DISCOVER
    p += bytes([57, 2]) + struct.pack("!H", 1400)  # max message size
    p += bytes([60, len(VENDOR)]) + VENDOR         # vendor class -> PXE
    p += bytes([93, 2, 0, 0])                      # client arch
    p += bytes([94, 3, 1, 2, 1])                   # client NDI
    p += bytes([55, 4, 1, 3, 6, 43])               # parameter request list
    p += b"\xff"
    return p


def parse_options(pkt):
    if len(pkt) < 240 or pkt[236:240] != MAGIC:
        return {}
    out, i = {}, 240
    while i < len(pkt):
        code = pkt[i]
        if code == 255:
            break
        if code == 0:
            i += 1
            continue
        ln = pkt[i + 1]
        out[code] = pkt[i + 2:i + 2 + ln]
        i += 2 + ln
    return out


def probe(mac, iface, timeout=6.0):
    xid = random.getrandbits(32)
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    s.setsockopt(socket.SOL_SOCKET, socket.SO_BROADCAST, 1)
    try:
        s.setsockopt(socket.SOL_SOCKET, socket.SO_BINDTODEVICE, iface.encode())
    except PermissionError:
        print("  (needs root)")
        return None
    s.bind(("", 68))
    s.settimeout(timeout)
    s.sendto(build_discover(mac, xid), ("255.255.255.255", 67))

    while True:
        try:
            data, _ = s.recvfrom(2048)
        except socket.timeout:
            return None
        if len(data) < 44:
            continue
        if struct.unpack("!I", data[4:8])[0] != xid:
            continue
        return parse_options(data), data[16:20]


if __name__ == "__main__":
    mac = sys.argv[1]
    iface = sys.argv[2] if len(sys.argv) > 2 else "eth0"
    r = probe(mac, iface)
    if r is None:
        print(f"  {mac}: no answer")
        sys.exit(1)
    opts, yiaddr = r
    ip = ".".join(str(b) for b in yiaddr)
    o43 = opts.get(43, b"")
    # The RPi bootloader only netboots if option 43 carries a menu entry
    # with exactly this text. The mere presence of option 43 says nothing:
    # dnsmasq sends it even without a boot offer.
    boot = b"Raspberry Pi Boot" in o43
    print(f"  {mac}: offered {ip}")
    print(f"    Option 43: {o43.hex() if o43 else '-'}")
    print(f"    Entry 'Raspberry Pi Boot': {'YES' if boot else 'NO'}")
    print(f"    ==> Netboot: {'UNLOCKED' if boot else 'BLOCKED (falls through to NVMe)'}")
