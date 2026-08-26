#!/usr/bin/env bash
# Mirrors an image onto the Sheath server and enters it in the catalogue.
# After that the installation runs over HTTP inside our own network instead of
# over the internet link — once per site rather than once per blade, and
# without depending on a remote server that may be slow or offline.
#
#   tools/mirror-image.sh <id> <url> [os-id]
#
#   tools/mirror-image.sh ubuntu-24.04-arm64 https://…/ubuntu…img.xz ubuntu
#   tools/mirror-image.sh debian-13-arm64    https://…/debian-13-raspi-arm64-….tar.xz debian
#
# A .tar.xz (the Debian cloud images ship that way) is unpacked and the disk
# image inside it is mirrored — the installer writes disk images, not archives.

set -euo pipefail

# --root=DIR ahead of everything else, because sudo drops the environment and
# a tool that silently works in the wrong directory is worse than one that
# refuses to start.
ROOT=${SHEATH_ROOT:-/srv/sheath}
case "${1:-}" in
--root=*) ROOT=${1#--root=}; shift ;;
esac
DIR="$ROOT/images"
ID=${1:?usage: mirror-image.sh <id> <url> [os-id]}
URL=${2:?usage: mirror-image.sh <id> <url> [os-id]}
OS=${3:-$(printf '%s' "$ID" | cut -d- -f1)}

# The server may invoke this through sudo, so the arguments are checked here
# too and not only where they were typed: an id with a slash in it would write
# as root wherever the slash pointed.
case "$ID" in
*[!A-Za-z0-9._-]* | "" | .* ) echo "invalid image id: $ID" >&2; exit 2 ;;
esac
case "$URL" in
http://* | https://*) ;;
*) echo "invalid URL: $URL" >&2; exit 2 ;;
esac

BASE=$(basename "$URL")
TMP="$ROOT/tmp/mirror.$$"
mkdir -p "$TMP" "$DIR"
trap 'rm -rf "$TMP"' EXIT

say() { printf '[%s] %s\n' "$(date +%T)" "$1"; }

say "fetching $ID from $URL"
curl -fL --retry 3 --retry-delay 5 -o "$TMP/$BASE" "$URL"

case "$BASE" in
*.tar.xz | *.tar.gz | *.tar.zst | *.tar)
  say "unpacking the archive"
  tar -xf "$TMP/$BASE" -C "$TMP"
  # Debian's cloud tarball carries disk.raw; others name it *.img.
  INNER=$(find "$TMP" -maxdepth 2 -type f \( -name 'disk.raw' -o -name '*.img' -o -name '*.raw' \) \
    -printf '%s %p\n' | sort -rn | head -1 | cut -d' ' -f2-)
  [ -n "$INNER" ] || { echo "no disk image found inside $BASE" >&2; exit 1; }
  say "found $(basename "$INNER") ($(( $(stat -c%s "$INNER") / 1048576 )) MB raw)"
  # Compressed, because the blade pulls it over the network and unpacks it
  # while writing anyway. -T0 -3 rather than -9: on a CM4 the difference in
  # size is minutes of CPU, the difference in transfer is seconds.
  FILE="$ID.img.xz"
  say "compressing to $FILE"
  xz -T0 -3 -c "$INNER" > "$DIR/$FILE.part"
  ;;
*)
  FILE="$BASE"
  mv "$TMP/$BASE" "$DIR/$FILE.part"
  ;;
esac

mv "$DIR/$FILE.part" "$DIR/$FILE"
SUM=$(sha256sum "$DIR/$FILE" | cut -d' ' -f1)
SZ=$(stat -c%s "$DIR/$FILE")
say "$ID: $((SZ / 1048576)) MB, sha256 $SUM"

T=$(sudo cat "$ROOT/data/admin-token")
curl -sS -X POST -H "Authorization: Bearer $T" -H "Content-Type: application/json" \
  -d "{\"id\":\"$ID\",\"url\":\"$URL\",\"local\":\"$FILE\",\"sha256\":\"$SUM\",\"bytes\":$SZ,\"seed\":\"generic\",\"os_id\":\"$OS\",\"notes\":\"mirrored locally\"}" \
  http://127.0.0.1:8080/api/v1/images >/dev/null
say "entered in the catalogue"
