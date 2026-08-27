#!/usr/bin/env bash
# Fetches the Raspberry Pi boot firmware the netboot payload is built from.
#
#   tools/fetch-firmware.sh [target-dir]        # default /srv/sheath/build
#   FIRMWARE_REF=1.20250430 tools/fetch-firmware.sh
#
# Three files, from the official firmware repository at a pinned revision:
# the second-stage loader, its fixup, and the device tree for the CM4. They
# end up inside boot.img, which is what the blade actually loads.
#
# This exists because the build directory used to be something somebody had
# populated once, on one machine, from a boot partition they happened to have.
# That works until the machine is gone.
#
# What this does *not* fetch is the mini OS itself — Image.gz and the rootfs
# skeleton in build/rootfs. Those are still built by hand and are the last
# part of the payload without a recipe.

set -euo pipefail

DEST=${1:-/srv/sheath/build}
REF=${FIRMWARE_REF:-1.20250430}
BASE="https://raw.githubusercontent.com/raspberrypi/firmware/$REF/boot"

FILES=(start4.elf fixup4.dat bcm2711-rpi-cm4.dtb bcm2711-rpi-cm4-io.dtb)

mkdir -p "$DEST"
say() { printf '[%s] %s\n' "$(date +%T)" "$1"; }

say "firmware $REF → $DEST"
for f in "${FILES[@]}"; do
  tmp="$DEST/$f.part"
  if ! curl -fsSL --retry 3 -o "$tmp" "$BASE/$f"; then
    rm -f "$tmp"
    echo "could not fetch $f from $BASE" >&2
    exit 1
  fi
  # Only move it into place once it is whole: a truncated start4.elf is a
  # payload that fails at a point where nobody is watching a console.
  mv "$tmp" "$DEST/$f"
  printf '  %-26s %8s bytes  %s\n' "$f" "$(stat -c%s "$DEST/$f")" \
    "$(sha256sum "$DEST/$f" | cut -c1-16)"
done

printf '%s\n' "$REF" > "$DEST/.firmware-ref"
say "recorded as $DEST/.firmware-ref"
