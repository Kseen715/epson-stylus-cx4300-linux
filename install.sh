#!/bin/sh
# Install the CX4300 scanner tool: build the binary and the SANE backend, grant
# USB access, and disable the Epson SANE backends that brick this scanner until
# it is power cycled.
#
# Run from the repository root:   sudo ./install.sh
# Add --service to also run it in the background from boot, as a systemd unit.
# Add --no-sane to install only the web UI, leaving SANE alone.
set -eu

BIN_DIR="${BIN_DIR:-/usr/local/bin}"
UDEV_RULE="/etc/udev/rules.d/60-epson-cx4300.rules"
UNIT="/etc/systemd/system/escan.service"
CONF="/etc/escan.conf"
SANE_CONF_DIR="/etc/sane.d"
BACKEND=cx4300
VID=04b8
PID=083f

# Backends that probe this device with ESC/I. Any one of them turns a single
# "scanimage -L" into a scanner that answers nothing until its mains lead is
# pulled, so they are disabled rather than left to race with ours.
ESCI_BACKENDS="epkowa epson epson2 epsonds"

WANT_SERVICE=no
WANT_SANE=yes
for arg in "$@"; do
    case "$arg" in
        --service) WANT_SERVICE=yes ;;
        --no-sane) WANT_SANE=no ;;
        -h|--help)
            printf 'usage: sudo ./install.sh [--service] [--no-sane]\n'
            printf '  --service  also install and enable the systemd unit\n'
            printf '  --no-sane  skip the SANE backend (web UI only)\n'
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

# Inside a .run installer the binary is already built and sits next to this
# script, so the whole toolchain half of the install is skipped. Everything
# after it - udev rule, epkowa, the unit - is identical either way.
# An if, not a && chain: under `set -e` a false chain would end the script here.
PREBUILT=""
PREBUILT_BACKEND=""
if [ -x "./escan" ] && [ ! -f go.mod ]; then
    PREBUILT="./escan"
    if [ -f "./libsane-${BACKEND}.so.1" ]; then
        PREBUILT_BACKEND="./libsane-${BACKEND}.so.1"
    fi
fi

if [ -n "$PREBUILT" ]; then
    step "Installing the bundled escan binary"
    install -m 0755 "$PREBUILT" "${BIN_DIR}/escan"
    say "installed ${BIN_DIR}/escan (prebuilt, no toolchain needed)"
else

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

if [ "$WANT_SANE" = yes ]; then
    step "Building the SANE backend"
    # A shared library, so this half needs a C compiler where the binary did
    # not. Without one, the web UI still installs and works.
    if ! command -v cc >/dev/null 2>&1 && ! command -v gcc >/dev/null 2>&1; then
        case "$PM" in
            apt)    install_pkgs gcc ;;
            xbps)   install_pkgs gcc ;;
            pacman) install_pkgs gcc ;;
            dnf)    install_pkgs gcc ;;
        esac
    fi
    if command -v cc >/dev/null 2>&1 || command -v gcc >/dev/null 2>&1; then
        GOCACHE="${PWD}/.gocache" CGO_ENABLED=1 go build -trimpath -buildvcs=false \
            -buildmode=c-shared -o "./libsane-${BACKEND}.so.1" ./cmd/libsane-${BACKEND}
        PREBUILT_BACKEND="./libsane-${BACKEND}.so.1"
        say "built libsane-${BACKEND}.so.1"
    else
        WANT_SANE=no
        say "no C compiler, so the SANE backend was skipped; the web UI is unaffected"
    fi
fi
fi

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

step "Protecting the scanner from the ESC/I backends"
# Any ESC/I probe of this device latches it into refusing everything until mains
# power is cut, and one "scanimage -L" loads every enabled backend. So the ones
# that probe Epson devices are disabled wherever they are enabled.
#
# SANE reads *every* file in dll.d, whatever it is called - a `.dpkg-new` left
# by an iscan upgrade enables epkowa just as effectively as a live drop-in, and
# so would a backup copy left in that directory. So nothing in dll.d is skipped
# for its name, and the backups go in the parent directory, which SANE only
# reads per-backend .conf files from.
BACKUP_DIR="${SANE_CONF_DIR}"
disabled_any=no
for f in "${SANE_CONF_DIR}"/dll.conf "${SANE_CONF_DIR}"/dll.d/*; do
    [ -f "$f" ] || continue
    case "$f" in
        *.bak-cx4300) continue ;;   # our own backups, already dealt with
    esac
    for be in $ESCI_BACKENDS; do
        if grep -qE "^[[:space:]]*${be}[[:space:]]*$" "$f"; then
            backup="${BACKUP_DIR}/$(basename "$f").bak-cx4300"
            cp -n "$f" "$backup" 2>/dev/null || true
            sed -i "s/^[[:space:]]*${be}[[:space:]]*\$/# ${be} disabled by cx4300 install.sh: its ESC\/I probe locks this scanner up/" "$f"
            say "disabled ${be} in $f (backup: ${backup})"
            disabled_any=yes
        fi
    done
done
[ "$disabled_any" = yes ] || say "no ESC/I backend was enabled; nothing to do"

# Say so plainly if one is still enabled somewhere this script did not look:
# the next "scanimage -L" would lock the scanner up, and the symptom - the
# device vanishing from USB - looks nothing like a configuration problem.
still=""
for f in "${SANE_CONF_DIR}"/dll.conf "${SANE_CONF_DIR}"/dll.d/*; do
    [ -f "$f" ] || continue
    case "$f" in
        *.bak-cx4300) continue ;;
    esac
    for be in $ESCI_BACKENDS; do
        if grep -qE "^[[:space:]]*${be}[[:space:]]*$" "$f"; then
            still="${still} ${be}:${f}"
        fi
    done
done
if [ -n "$still" ]; then
    say ""
    say "WARNING: an ESC/I backend is still enabled:${still}"
    say "Comment those lines out by hand, or the next scan from any SANE"
    say "application will lock the scanner up until its mains lead is pulled."
fi

if [ "$WANT_SANE" = yes ] && [ -n "$PREBUILT_BACKEND" ]; then
    step "Installing the SANE backend"
    # Next to the backends already on the system, whichever directory that is.
    SANE_LIB_DIR=""
    for d in /usr/lib/"$(uname -m)"-linux-gnu/sane /usr/lib64/sane /usr/lib/sane /usr/local/lib/sane; do
        if [ -d "$d" ]; then SANE_LIB_DIR="$d"; break; fi
    done
    if [ -z "$SANE_LIB_DIR" ]; then
        say "SANE does not appear to be installed (no backend directory found)."
        say "Install it - sane-utils/sane-backends - and re-run this script to add"
        say "the backend; the web UI works either way."
    else
        install -m 0644 "$PREBUILT_BACKEND" "${SANE_LIB_DIR}/libsane-${BACKEND}.so.1"
        say "installed ${SANE_LIB_DIR}/libsane-${BACKEND}.so.1"
        # A drop-in where the distribution supports one, so dll.conf is left as
        # its package manager wrote it.
        if [ -d "${SANE_CONF_DIR}/dll.d" ]; then
            printf '# Epson Stylus CX4300 family, via this repository\n%s\n' "$BACKEND" \
                > "${SANE_CONF_DIR}/dll.d/${BACKEND}"
            say "enabled it in ${SANE_CONF_DIR}/dll.d/${BACKEND}"
        elif [ -f "${SANE_CONF_DIR}/dll.conf" ]; then
            if ! grep -qE "^[[:space:]]*${BACKEND}[[:space:]]*$" "${SANE_CONF_DIR}/dll.conf"; then
                printf '%s\n' "$BACKEND" >> "${SANE_CONF_DIR}/dll.conf"
            fi
            say "enabled it in ${SANE_CONF_DIR}/dll.conf"
        else
            install -d "$SANE_CONF_DIR"
            printf '%s\n' "$BACKEND" > "${SANE_CONF_DIR}/dll.conf"
            say "wrote ${SANE_CONF_DIR}/dll.conf"
        fi
        if command -v scanimage >/dev/null 2>&1; then
            say "check it with:   scanimage -L"
        fi
    fi
fi

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
if [ "$WANT_SANE" = yes ] && [ -n "$PREBUILT_BACKEND" ]; then
    say "XSane, GIMP, simple-scan and scanimage can now use the scanner too."
    say "The two share the device: whichever starts a scan first gets it."
fi
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
