#!/bin/sh
# Install the CX4300 scanner tool: build the binary, grant USB access, and
# disable the one SANE backend that bricks this scanner until it is power
# cycled.
#
# Run from the repository root:   sudo ./install.sh
# Add --service to also run it in the background from boot, as a systemd unit.
set -eu

BIN_DIR="${BIN_DIR:-/usr/local/bin}"
UDEV_RULE="/etc/udev/rules.d/60-epson-cx4300.rules"
UNIT="/etc/systemd/system/escan.service"
CONF="/etc/escan.conf"
VID=04b8
PID=083f

WANT_SERVICE=no
for arg in "$@"; do
    case "$arg" in
        --service) WANT_SERVICE=yes ;;
        -h|--help)
            printf 'usage: sudo ./install.sh [--service]\n'
            printf '  --service  also install and enable the systemd unit\n'
            exit 0 ;;
        *) printf 'error: unknown option %s\n' "$arg" >&2; exit 1 ;;
    esac
done

say()  { printf '%s\n' "$*"; }
step() { printf '\n== %s\n' "$*"; }
die()  { printf 'error: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run this with sudo (it installs a udev rule and a binary)"

# The user who invoked sudo is the one who needs scanner access.
TARGET_USER="${SUDO_USER:-}"
[ -n "$TARGET_USER" ] || TARGET_USER="$(logname 2>/dev/null || true)"

step "Detecting package manager"
if   command -v apt-get      >/dev/null 2>&1; then PM=apt
elif command -v xbps-install >/dev/null 2>&1; then PM=xbps
elif command -v pacman       >/dev/null 2>&1; then PM=pacman
elif command -v dnf          >/dev/null 2>&1; then PM=dnf
else PM=none
fi
say "using: $PM"

install_pkgs() {
    case "$PM" in
        apt)    DEBIAN_FRONTEND=noninteractive apt-get update -qq
                DEBIAN_FRONTEND=noninteractive apt-get install -y "$@" ;;
        xbps)   xbps-install -Sy "$@" ;;
        pacman) pacman -Sy --needed --noconfirm "$@" ;;
        dnf)    dnf install -y "$@" ;;
        none)   die "no supported package manager found; install Go manually, then re-run" ;;
    esac
}

step "Build dependencies"
# Go is needed only to build; the finished binary has no runtime dependencies
# (no libusb, no Python, no SANE).
if command -v go >/dev/null 2>&1; then
    say "go already present: $(go version)"
else
    case "$PM" in
        apt)    install_pkgs golang-go ;;
        xbps)   install_pkgs go ;;
        pacman) install_pkgs go ;;
        dnf)    install_pkgs golang ;;
    esac
fi
command -v go >/dev/null 2>&1 || die "Go is still not on PATH; install it and re-run"

step "Building escan"
[ -f go.mod ] || die "run this from the repository root (go.mod not found)"
# Keep the build cache inside the tree so root does not scribble in ~/.cache.
# -buildvcs=false: running under sudo in a repository owned by another user
# makes git refuse to report status, which would otherwise fail the build.
GOCACHE="${PWD}/.gocache" go build -trimpath -buildvcs=false -o "${BIN_DIR}/escan" ./cmd/escan
say "installed ${BIN_DIR}/escan"

step "USB access"
if ! getent group scanner >/dev/null 2>&1; then
    groupadd -r scanner
    say "created group 'scanner'"
fi
cat > "$UDEV_RULE" <<RULE
# Epson Stylus CX4300 family scanner (also CX4400/CX5500/CX5600/DX4400/DX4450).
# Lets members of the scanner group talk to it, so escan needs no root.
SUBSYSTEM=="usb", ATTR{idVendor}=="${VID}", ATTR{idProduct}=="${PID}", MODE="0664", GROUP="scanner"
RULE
say "wrote $UDEV_RULE"
if [ -n "$TARGET_USER" ] && id "$TARGET_USER" >/dev/null 2>&1; then
    usermod -aG scanner "$TARGET_USER"
    say "added $TARGET_USER to the scanner group (log out and back in to pick it up)"
fi
udevadm control --reload-rules 2>/dev/null || true
udevadm trigger --subsystem-match=usb 2>/dev/null || true

step "Protecting the scanner from epkowa"
# Any SANE probe of this device sends an ESC/I command it does not implement,
# which latches it into refusing everything until mains power is cut. One
# "scanimage -L" is enough, so the backend is disabled here.
disabled_any=no
for f in /etc/sane.d/dll.conf /etc/sane.d/dll.d/*; do
    [ -f "$f" ] || continue
    # Skip packaging leftovers and our own backups; editing those changes nothing.
    case "$f" in
        *.dpkg-*|*.rpmnew|*.rpmsave|*.bak*|*~) continue ;;
    esac
    if grep -qE '^[[:space:]]*epkowa[[:space:]]*$' "$f"; then
        cp -n "$f" "${f}.bak-cx4300" 2>/dev/null || true
        sed -i 's/^[[:space:]]*epkowa[[:space:]]*$/# epkowa disabled by cx4300 install.sh: its ESC\/I probe locks this scanner up/' "$f"
        say "disabled epkowa in $f (backup: ${f}.bak-cx4300)"
        disabled_any=yes
    fi
done
[ "$disabled_any" = yes ] || say "epkowa was not enabled anywhere; nothing to do"

if [ "$WANT_SERVICE" = yes ]; then
    step "Installing the systemd service"
    command -v systemctl >/dev/null 2>&1 || die "--service needs systemd, which is not present here"
    [ -n "$TARGET_USER" ] && id "$TARGET_USER" >/dev/null 2>&1 ||
        die "--service needs to know which user to run as; run it with sudo from that user's shell"
    HOME_DIR="$(getent passwd "$TARGET_USER" | cut -d: -f6)"
    [ -n "$HOME_DIR" ] || die "cannot determine the home directory of $TARGET_USER"
    OUT_DIR="${HOME_DIR}/scans"
    # escan reads /etc/escan.conf if it exists; drop the commented example in
    # so an SMB share can be configured there without touching the unit. Never
    # overwrite one that is already there - it holds a password.
    if [ -f examples/escan.conf ] && [ ! -e "$CONF" ]; then
        # Owned by the service user, not root: escan reads it as that user, and
        # 0600 means nobody else can read the password it may come to hold.
        install -o "$TARGET_USER" -g "$TARGET_USER" -m 0600 examples/escan.conf "$CONF"
        # The settings go in the file rather than into the unit, so changing one
        # later is an edit here and a restart, with nothing to override it.
        printf '\naddr = 127.0.0.1:8080\nout = %s\n' "$OUT_DIR" >> "$CONF"
        say "wrote $CONF (mode 0600; escan refuses SMB credentials from a readable file)"
    elif [ -e "$CONF" ]; then
        say "$CONF already exists, leaving it alone; escan takes its settings from there"
    fi
    # The unit is rendered from the committed example, so the file on disk and
    # the one in examples/ never drift apart.
    [ -f examples/systemd/escan.service ] || die "examples/systemd/escan.service not found"
    sed -e "s/%USER%/${TARGET_USER}/g" \
        -e "s#%OUT%#${OUT_DIR}#g" \
        -e "s#/usr/local/bin/escan#${BIN_DIR}/escan#g" \
        examples/systemd/escan.service > "$UNIT"
    install -d -o "$TARGET_USER" -g "$TARGET_USER" -m 0755 "$OUT_DIR"
    systemctl daemon-reload
    systemctl enable --now escan
    say "escan runs as $TARGET_USER, writing to $OUT_DIR"
    say "  status:  systemctl status escan     logs: journalctl -u escan -f"
    say "  stop it: sudo systemctl disable --now escan"
else
    say ""
    say "To run it in the background from boot instead:  sudo ./install.sh --service"
    say "(a per-user alternative is in examples/systemd/escan.user.service)"
fi

step "Done"
say "Start it with:   escan            then open http://127.0.0.1:8080/"
say "Save scans elsewhere with:   escan --out ~/scans"
say ""
say "Two things this scanner insists on:"
say "  * plug it straight into a root-hub port - behind any USB hub its identify"
say "    step fails and the scan area reads back as zero"
say "  * if it stops responding, or ignores its own power button, unplug it from"
say "    mains for ~30s; re-plugging USB does not clear a firmware lockup"

if grep -qi microsoft /proc/version 2>/dev/null; then
    say ""
    say "This is WSL, so the scanner has to be handed over from Windows first."
    say "In an Administrator PowerShell on the Windows side:"
    say "    usbipd list"
    say "    usbipd bind --force --busid <busid>"
    say "    usbipd attach --wsl --busid <busid>"
    say "usbip presents it on a virtual root hub, which satisfies the no-hub rule."
fi
