#!/bin/sh
# Build a .run self-extracting installer: a shell header with a compressed tar
# payload appended to it. Running the result unpacks into a temporary directory
# and hands over to install.sh, which installs the prebuilt binary rather than
# building one.
#
#   scripts/make-run.sh <binary> <arch-label> <output.run> [libsane-cx4300.so.1]
#
# The SANE backend is optional: it is a cgo shared library, so it only exists
# for the architectures the release build has a C compiler for. Without it the
# installer sets up the web UI alone.
#
# Deliberately has no dependency on makeself: the header below is the whole of
# it, and tar/gzip are everywhere the result needs to run anyway.
set -eu

[ $# -ge 3 ] && [ $# -le 4 ] ||
    { printf 'usage: %s <binary> <arch-label> <output.run> [backend.so]\n' "$0" >&2; exit 1; }

BINARY="$1"
ARCH="$2"
OUT="$3"
BACKEND="${4:-}"

REPO_ROOT="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
[ -f "$BINARY" ] || { printf 'error: no such binary: %s\n' "$BINARY" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
PAYLOAD="${WORK}/payload"
mkdir -p "${PAYLOAD}/examples"

install -m 0755 "$BINARY" "${PAYLOAD}/escan"
if [ -n "$BACKEND" ]; then
    [ -f "$BACKEND" ] || { printf 'error: no such backend: %s\n' "$BACKEND" >&2; exit 1; }
    install -m 0644 "$BACKEND" "${PAYLOAD}/libsane-cx4300.so.1"
fi
install -m 0755 "${REPO_ROOT}/install.sh" "${PAYLOAD}/install.sh"
cp -R "${REPO_ROOT}/examples/." "${PAYLOAD}/examples/"

tar -C "$PAYLOAD" -czf "${WORK}/payload.tar.gz" .

cat > "$OUT" <<HEADER
#!/bin/sh
# escan installer for linux/${ARCH} - self-extracting archive.
# Everything below the __PAYLOAD__ line is a gzipped tar of the binary, the
# SANE backend where one was built, the installer and the example unit files.
#
#   sudo ./$(basename "$OUT")              install the binary and the udev rule
#   sudo ./$(basename "$OUT") --service    also run it from boot via systemd
#   ./$(basename "$OUT") --extract DIR     just unpack, install nothing
set -eu

SELF="\$0"
case "\$SELF" in /*) ;; *) SELF="\$PWD/\$SELF" ;; esac

# The payload starts on the line after the marker. awk rather than grep -a:
# it stops at the marker without reading the binary tail, and busybox awk has
# no trouble with it where busybox grep has no -a at all.
SKIP=\$(awk '/^__PAYLOAD__\$/ { print NR + 1; exit }' "\$SELF")
[ -n "\$SKIP" ] || { printf 'error: corrupt installer: no payload marker\n' >&2; exit 1; }

unpack() { tail -n +"\$SKIP" "\$SELF" | tar -xzf - -C "\$1"; }

if [ "\${1:-}" = "--extract" ]; then
    [ -n "\${2:-}" ] || { printf 'usage: %s --extract DIR\n' "\$0" >&2; exit 1; }
    mkdir -p "\$2"
    unpack "\$2"
    printf 'extracted to %s\n' "\$2"
    exit 0
fi

DIR="\$(mktemp -d)"
trap 'rm -rf "\$DIR"' EXIT
unpack "\$DIR"

cd "\$DIR"
exec ./install.sh "\$@"
__PAYLOAD__
HEADER

cat "${WORK}/payload.tar.gz" >> "$OUT"
chmod +x "$OUT"
printf 'wrote %s (%s)\n' "$OUT" "$(du -h "$OUT" | cut -f1)"
