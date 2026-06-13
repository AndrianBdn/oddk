#!/bin/sh
# install.sh - Install or update ODDK on a Linux server
# POSIX compliant; downloads release binaries from GitHub.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/Hypersequent/oddk/main/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/Hypersequent/oddk/main/install.sh | sh -s -- --version v0.1.31
#
# Fresh installs use the FHS layout: binary in /usr/local/bin, state in
# /var/lib/oddk. An existing installation is updated in place, wherever it
# lives: the binary path is taken from the oddk.service systemd unit, so
# legacy layouts (e.g. /home/oddk/bin) keep working unchanged.

set -eu

REPO="Hypersequent/oddk"
SERVICE="oddk"
SUMS_FILE="SHA256SUMS"
# ASSET / ARCHIVE_BINARY are set from the host architecture in pre-flight checks.

# Fresh-install (FHS) layout
USER_NAME="oddk"
FHS_BINARY="/usr/local/bin/oddk"
FHS_HOME="/var/lib/oddk"
SYSTEMD_UNIT="/etc/systemd/system/oddk.service"

# Colors for output (POSIX compatible)
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

print_msg() {
    color="$1"
    shift
    printf "${color}%s${NC}\n" "$*"
}

TMP_DIR=$(mktemp -d /tmp/oddk-install.XXXXXX)
trap 'rm -rf "$TMP_DIR"' EXIT INT TERM

usage() {
    echo "Usage: $0 [--version vX.Y.Z]"
    echo
    echo "Without --version the latest GitHub release is installed."
}

# --- Parse arguments ---

VERSION=""
while [ $# -gt 0 ]; do
    case "$1" in
        --version)
            [ $# -ge 2 ] || { print_msg "$RED" "--version requires an argument"; exit 1; }
            VERSION="$2"
            shift 2
            ;;
        --version=*)
            VERSION="${1#--version=}"
            shift
            ;;
        -h|--help)
            usage
            exit 0
            ;;
        *)
            print_msg "$RED" "Unknown argument: $1"
            usage
            exit 1
            ;;
    esac
done

case "$VERSION" in
    ""|v*) ;;
    *) VERSION="v$VERSION" ;;
esac

if [ -n "$VERSION" ]; then
    BASE_URL="https://github.com/$REPO/releases/download/$VERSION"
else
    BASE_URL="https://github.com/$REPO/releases/latest/download"
fi

print_msg "$YELLOW" "ODDK Install/Update Script"
echo

# --- Pre-flight checks ---

if [ "$(uname -s)" != "Linux" ]; then
    print_msg "$RED" "This script must run on Linux (detected: $(uname -s))"
    exit 1
fi

case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    aarch64|arm64) ARCH=arm64 ;;
    *)
        print_msg "$RED" "Unsupported architecture: $(uname -m) (need x86_64 or aarch64)"
        exit 1
        ;;
esac
ASSET="oddk-linux-$ARCH.tar.gz"
ARCHIVE_BINARY="oddk-linux-$ARCH"

for tool in curl tar sha256sum systemctl; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        print_msg "$RED" "Required tool not found: $tool"
        exit 1
    fi
done

if [ ! -d /run/systemd/system ]; then
    print_msg "$RED" "systemd is not running on this system"
    exit 1
fi

SUDO="sudo"
if [ "$(id -u)" -eq 0 ]; then
    SUDO=""
elif ! sudo -n true 2>/dev/null; then
    print_msg "$RED" "This script requires root or passwordless sudo"
    exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
    print_msg "$RED" "Docker is not installed"
    print_msg "$YELLOW" "Install Docker first: https://docs.docker.com/engine/install/"
    exit 1
fi

if ! systemctl is-active --quiet docker; then
    print_msg "$RED" "Docker service is not running"
    print_msg "$YELLOW" "Start it with: sudo systemctl start docker"
    exit 1
fi

# --- Detect existing installation ---
#
# This script only ever uses the FHS layout (/usr/local/bin + /var/lib/oddk).
# A fresh install creates it; an update replaces the binary in place. A legacy
# /home/oddk installation is NOT updated here - it must first be relocated with
# scripts/remote/oddk-migrate.sh, after which this script handles it normally.

MODE="install"
TARGET_BINARY="$FHS_BINARY"

if systemctl list-unit-files 2>/dev/null | grep -q "^${SERVICE}\.service"; then
    EXEC_START=$(systemctl cat "$SERVICE.service" 2>/dev/null | sed -n 's/^ExecStart=//p' | head -1)
    INSTALLED_BINARY="${EXEC_START%% *}"
    if [ "$INSTALLED_BINARY" != "$FHS_BINARY" ]; then
        print_msg "$RED" "Non-FHS installation detected (binary at: ${INSTALLED_BINARY:-unknown})"
        print_msg "$YELLOW" "This installer manages the FHS layout ($FHS_BINARY)."
        print_msg "$YELLOW" "Relocate the existing install first with oddk-migrate.sh, then re-run."
        exit 1
    fi
    if [ ! -f "$TARGET_BINARY" ]; then
        print_msg "$RED" "$SERVICE.service points at $TARGET_BINARY but the file is missing"
        exit 1
    fi
    MODE="update"
    print_msg "$GREEN" "Existing FHS installation detected: $TARGET_BINARY"
else
    if [ -f "$FHS_BINARY" ]; then
        print_msg "$RED" "$FHS_BINARY exists but no $SERVICE.service unit was found"
        print_msg "$YELLOW" "Remove the stray binary or restore the unit, then re-run"
        exit 1
    fi
    print_msg "$GREEN" "No existing installation, performing fresh install"
fi
echo

# --- Download and verify ---

print_msg "$YELLOW" "Downloading $ASSET (${VERSION:-latest})..."
if ! curl -fsSL -o "$TMP_DIR/$ASSET" "$BASE_URL/$ASSET"; then
    print_msg "$RED" "Failed to download $BASE_URL/$ASSET"
    print_msg "$YELLOW" "Check that the release exists: https://github.com/$REPO/releases"
    exit 1
fi

if ! curl -fsSL -o "$TMP_DIR/$SUMS_FILE" "$BASE_URL/$SUMS_FILE"; then
    print_msg "$RED" "Failed to download checksums ($BASE_URL/$SUMS_FILE)"
    exit 1
fi

print_msg "$YELLOW" "Verifying checksum..."
if ! (cd "$TMP_DIR" && grep -F "$ASSET" "$SUMS_FILE" | sha256sum -c - >/dev/null 2>&1); then
    print_msg "$RED" "Checksum verification FAILED - aborting"
    exit 1
fi
print_msg "$GREEN" "Checksum OK"

print_msg "$YELLOW" "Extracting..."
tar -xzf "$TMP_DIR/$ASSET" -C "$TMP_DIR"
TEMP_BINARY="$TMP_DIR/$ARCHIVE_BINARY"
if [ ! -f "$TEMP_BINARY" ]; then
    print_msg "$RED" "Binary not found in archive (expected: $ARCHIVE_BINARY)"
    exit 1
fi
chmod +x "$TEMP_BINARY"

if ! "$TEMP_BINARY" --version >/dev/null 2>&1; then
    print_msg "$RED" "Downloaded binary failed sanity check (--version)"
    exit 1
fi
NEW_VERSION=$("$TEMP_BINARY" --version 2>&1)
print_msg "$GREEN" "Binary OK: $NEW_VERSION"
echo

# =====================================================================
# Update path: replace the binary in place, restart, roll back on failure
# =====================================================================

if [ "$MODE" = "update" ]; then
    OLD_VERSION=$("$TARGET_BINARY" --version 2>&1 || echo "unknown")
    PREV_BINARY="${TARGET_BINARY}.prev"
    OWNER=$(stat -c '%U:%G' "$TARGET_BINARY")

    print_msg "$YELLOW" "Updating: $OLD_VERSION -> $NEW_VERSION"

    rollback() {
        print_msg "$RED" "Update failed, rolling back..."
        if [ -f "$PREV_BINARY" ]; then
            $SUDO cp -p "$PREV_BINARY" "$TARGET_BINARY"
            $SUDO systemctl start "$SERVICE" || true
            print_msg "$YELLOW" "Rollback completed, previous binary restored"
        else
            print_msg "$RED" "No backup found, manual intervention required"
        fi
        exit 1
    }

    print_msg "$YELLOW" "Backing up current binary to $PREV_BINARY"
    $SUDO cp -p "$TARGET_BINARY" "$PREV_BINARY"

    print_msg "$YELLOW" "Stopping $SERVICE service..."
    $SUDO systemctl stop "$SERVICE" || rollback
    sleep 2

    print_msg "$YELLOW" "Installing new binary..."
    $SUDO cp "$TEMP_BINARY" "$TARGET_BINARY" || rollback
    $SUDO chown "$OWNER" "$TARGET_BINARY"
    $SUDO chmod 755 "$TARGET_BINARY"

    print_msg "$YELLOW" "Starting $SERVICE service..."
    $SUDO systemctl start "$SERVICE" || rollback
    sleep 3
    systemctl is-active --quiet "$SERVICE" || rollback

    print_msg "$GREEN" "Service restarted successfully"
    echo
    $SUDO systemctl status "$SERVICE" --no-pager -l | head -15
    echo
    print_msg "$GREEN" "ODDK update completed: $NEW_VERSION"
    print_msg "$YELLOW" "Previous binary kept at: $PREV_BINARY"
    exit 0
fi

# =====================================================================
# Fresh install: FHS layout
# =====================================================================

# Resolve a nologin shell - the oddk account is a service identity that nobody
# logs into. The CLI talks to the daemon over HTTP (localhost + bearer token),
# so administration never requires becoming the oddk user.
NOLOGIN=$(command -v nologin || echo /usr/sbin/nologin)

print_msg "$YELLOW" "Creating $USER_NAME user..."
if id "$USER_NAME" >/dev/null 2>&1; then
    print_msg "$YELLOW" "User $USER_NAME already exists, skipping"
else
    $SUDO useradd -r -d "$FHS_HOME" -s "$NOLOGIN" "$USER_NAME"
    print_msg "$GREEN" "User $USER_NAME created (home: $FHS_HOME, shell: $NOLOGIN)"
fi

print_msg "$YELLOW" "Creating directories..."
$SUDO mkdir -p "$FHS_HOME/data" "$FHS_HOME/backups"
$SUDO chown -R "$USER_NAME:$USER_NAME" "$FHS_HOME"
print_msg "$GREEN" "Directories created: $FHS_HOME/{data,backups}"

print_msg "$YELLOW" "Adding $USER_NAME to docker group..."
if id -nG "$USER_NAME" | grep -qw docker; then
    print_msg "$YELLOW" "User already in docker group, skipping"
else
    $SUDO usermod -aG docker "$USER_NAME"
    print_msg "$GREEN" "User added to docker group"
fi

print_msg "$YELLOW" "Installing binary to $FHS_BINARY..."
$SUDO cp "$TEMP_BINARY" "$FHS_BINARY"
$SUDO chown root:root "$FHS_BINARY"
$SUDO chmod 755 "$FHS_BINARY"
print_msg "$GREEN" "Binary installed"
echo

print_msg "$YELLOW" "Creating systemd service..."
$SUDO tee "$SYSTEMD_UNIT" > /dev/null << 'UNIT'
[Unit]
Description=ODDK
After=docker.service
Requires=docker.service

[Service]
Type=simple
User=oddk
ExecStart=/usr/local/bin/oddk daemon --data-dir /var/lib/oddk/data --backup-dir /var/lib/oddk/backups
Restart=always
RestartSec=5
StartLimitIntervalSec=600
StartLimitBurst=5
WorkingDirectory=/var/lib/oddk

[Install]
WantedBy=multi-user.target
UNIT
print_msg "$GREEN" "Systemd unit created: $SYSTEMD_UNIT"

print_msg "$YELLOW" "Enabling and starting service..."
$SUDO systemctl daemon-reload
$SUDO systemctl enable "$SERVICE"
$SUDO systemctl start "$SERVICE"
sleep 3

if ! systemctl is-active --quiet "$SERVICE"; then
    print_msg "$RED" "Service failed to start"
    print_msg "$YELLOW" "Check logs with: sudo journalctl -u $SERVICE -n 50"
    exit 1
fi
print_msg "$GREEN" "Service started successfully"
echo

# --- Bootstrap the admin's CLI config ---
# On first start the daemon writes the bearer token + URL to .oddk-cli.json in
# its working directory. Copy it into the invoking admin's ~/.config/oddk/cli.json
# so they can run `oddk ...` directly - no sudo, no becoming the oddk user.
TOKEN_FILE="$FHS_HOME/.oddk-cli.json"
CLI_CONFIGURED=""
ADMIN_USER="${SUDO_USER:-}"
if [ -n "$ADMIN_USER" ] && [ "$ADMIN_USER" != "root" ] && $SUDO test -f "$TOKEN_FILE"; then
    ADMIN_HOME=$(getent passwd "$ADMIN_USER" | cut -d: -f6)
    if [ -n "$ADMIN_HOME" ] && [ -d "$ADMIN_HOME" ]; then
        $SUDO mkdir -p "$ADMIN_HOME/.config/oddk"
        $SUDO cp "$TOKEN_FILE" "$ADMIN_HOME/.config/oddk/cli.json"
        $SUDO chown -R "$ADMIN_USER:" "$ADMIN_HOME/.config/oddk"
        $SUDO chmod 600 "$ADMIN_HOME/.config/oddk/cli.json"
        CLI_CONFIGURED="$ADMIN_USER"
        print_msg "$GREEN" "CLI configured for user '$ADMIN_USER' (~/.config/oddk/cli.json)"
    fi
fi
echo

$SUDO systemctl status "$SERVICE" --no-pager -l | head -15
echo
print_msg "$GREEN" "ODDK installation completed: $NEW_VERSION"
echo
if [ -z "$CLI_CONFIGURED" ]; then
    print_msg "$YELLOW" "To use the CLI as your own user, mint a token and install the config:"
    echo "  eval \"\$(sudo -u oddk /usr/local/bin/oddk cli-auth)\""
    echo
fi
# Some minimal distros omit /usr/local/bin from the default PATH. Warn so the
# admin knows why `oddk` might be "command not found" (sudo invocations above
# use the absolute path and are unaffected).
case ":$PATH:" in
    *:/usr/local/bin:*) ;;
    *) print_msg "$YELLOW" "Note: /usr/local/bin is not in your PATH - add it, or run /usr/local/bin/oddk directly." ;;
esac

print_msg "$YELLOW" "Useful commands:"
echo "  eval \"\$(sudo -u oddk /usr/local/bin/oddk cli-auth)\"  - Configure CLI for another user"
echo "  oddk list                    - List instances"
echo "  oddk pull --version 17       - Pull PostgreSQL 17"
echo "  sudo systemctl status oddk   - Check service status"
echo "  sudo journalctl -u oddk -f   - View logs"
