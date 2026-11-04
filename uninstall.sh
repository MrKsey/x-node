#!/usr/bin/env bash
#
# X-Node node uninstaller for Debian and Ubuntu.
#
# It removes the x-node service, its configuration and its binaries. The
# allowlist, the public peer list and the Yggdrasil package are kept unless
# --purge is given, so a reinstall does not lose the registered clients.
#
# Run it as root from the directory that contains uninstall.sh.

set -euo pipefail

MANIFEST="/var/lib/x-node/install.json"
STATE_DIR="/var/lib/x-node"
SERVICE_USER="x-node"
YGG_CONF=""
YGG_BACKUP=""
LISTEN_PORT="0"
WS_PORT="0"
LISTEN_PROTO=""
WAS_INSTALLED="0"

ASSUME_YES=0
PURGE=0
# Keeping the state is the safe default: a reinstall keeps the registered
# devices. --purge removes it unless --keep-state is given.
KEEP_STATE=1
KEEP_STATE_SET=0
# Keeping Yggdrasil is the safe default: it may serve other services on the
# host, and --purge is the explicit way to remove it.
KEEP_YGGDRASIL=1
KEEP_YGGDRASIL_SET=0

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<'EOF'
X-Node node uninstaller

Usage:
  sudo ./uninstall.sh [options]

Options:
  --purge             also delete /var/lib/x-node and remove the Yggdrasil
                      package together with its configuration
  --keep-state        keep /var/lib/x-node (the default without --purge)
  --keep-yggdrasil    never touch the Yggdrasil package or its configuration,
                      even with --purge
  --ygg-conf PATH     path to yggdrasil.conf (default: from the install manifest)
  --yes               non-interactive mode
  -h, --help          show this help

Without --purge the allowlist, the invites, the public peer list and Yggdrasil
itself are kept, so the exit can be installed again without registering every
device.

Examples:
  sudo ./uninstall.sh
  sudo ./uninstall.sh --yes
  sudo ./uninstall.sh --purge --yes
  sudo ./uninstall.sh --purge --keep-yggdrasil --yes
EOF
}

need_value() {
    [ "$#" -ge 2 ] || die "option $1 needs a value"
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --purge)          PURGE=1; shift ;;
        --keep-state)     KEEP_STATE=1; KEEP_STATE_SET=1; shift ;;
        --keep-yggdrasil) KEEP_YGGDRASIL=1; KEEP_YGGDRASIL_SET=1; shift ;;
        --ygg-conf)       need_value "$@"; YGG_CONF="$2"; shift 2 ;;
        --yes)            ASSUME_YES=1; shift ;;
        -h|--help)        usage; exit 0 ;;
        *)                die "unknown option: $1 (try --help)" ;;
    esac
done

[ "$(id -u)" -eq 0 ] || die "run this uninstaller as root"

manifest_get() {
    python3 - "$MANIFEST" "$1" <<'PY'
import json
import sys

try:
    with open(sys.argv[1], "r", encoding="utf-8") as handle:
        data = json.load(handle)
except Exception:
    data = {}
value = data.get(sys.argv[2], "")
if isinstance(value, bool):
    print("1" if value else "0")
else:
    print(value)
PY
}

if [ -f "$MANIFEST" ]; then
    [ -n "$YGG_CONF" ] || YGG_CONF="$(manifest_get yggConf)"
    YGG_BACKUP="$(manifest_get yggConfBackup)"
    LISTEN_PORT="$(manifest_get listenPort)"
    WS_PORT="$(manifest_get wsPort)"
    LISTEN_PROTO="$(manifest_get listenProto)"
    WAS_INSTALLED="$(manifest_get yggdrasilWasInstalled)"
fi
[ -n "$YGG_CONF" ] || YGG_CONF="/etc/yggdrasil/yggdrasil.conf"
[ -n "$LISTEN_PORT" ] || LISTEN_PORT=0
[ -n "$WS_PORT" ] || WS_PORT=0

if [ "$PURGE" -eq 1 ]; then
    [ "$KEEP_STATE_SET" -eq 1 ] || KEEP_STATE=0
    [ "$KEEP_YGGDRASIL_SET" -eq 1 ] || KEEP_YGGDRASIL=0
fi

if [ "$ASSUME_YES" -eq 0 ]; then
    [ -t 0 ] || die "stdin is not a terminal; pass --yes for non-interactive mode"
    if [ "$PURGE" -eq 0 ] && [ "$KEEP_STATE_SET" -eq 0 ]; then
        read -r -p "Delete the allowlist and invites in $STATE_DIR? [y/N] " answer
        case "$answer" in
            y|Y|yes|YES) KEEP_STATE=0 ;;
            *) KEEP_STATE=1 ;;
        esac
    fi
    if [ "$PURGE" -eq 0 ] && [ "$KEEP_YGGDRASIL_SET" -eq 0 ]; then
        if [ "$WAS_INSTALLED" = "1" ]; then
            # Yggdrasil existed before the installer: keep it by default.
            read -r -p "Remove the Yggdrasil package that was already installed? [y/N] " answer
            case "$answer" in
                y|Y|yes|YES) KEEP_YGGDRASIL=0 ;;
                *) KEEP_YGGDRASIL=1 ;;
            esac
        else
            # The installer added Yggdrasil itself. Keeping it is still the
            # safe default; --purge removes it without asking.
            read -r -p "Remove the Yggdrasil package installed by this installer? [y/N] " answer
            case "$answer" in
                y|Y|yes|YES) KEEP_YGGDRASIL=0 ;;
                *) KEEP_YGGDRASIL=1 ;;
            esac
        fi
    fi
    read -r -p "Continue? [y/N] " answer
    case "$answer" in
        y|Y|yes|YES) ;;
        *) echo "Aborted."; exit 1 ;;
    esac
fi

log "Stopping x-node"
systemctl disable --now x-node-firewall 2>/dev/null || true
systemctl disable --now x-node 2>/dev/null || true

if command -v nft >/dev/null 2>&1; then
    nft delete table inet x-node 2>/dev/null || true
fi

log "Removing the systemd units"
rm -f /etc/systemd/system/x-node.service \
      /etc/systemd/system/x-node-firewall.service
systemctl daemon-reload 2>/dev/null || true

if [ -f /etc/nginx/sites-enabled/x-node.conf ] || [ -f /etc/nginx/sites-available/x-node.conf ] \
    || [ -f /etc/nginx/sites-enabled/x-node-http.conf ] || [ -f /etc/nginx/sites-available/x-node-http.conf ]; then
    log "Removing the nginx sites"
    rm -f /etc/nginx/sites-enabled/x-node.conf /etc/nginx/sites-available/x-node.conf \
          /etc/nginx/sites-enabled/x-node-http.conf /etc/nginx/sites-available/x-node-http.conf
    rm -rf /var/www/x-node
    if command -v nginx >/dev/null 2>&1 && nginx -t >/dev/null 2>&1; then
        systemctl reload nginx 2>/dev/null || true
    fi
fi

log "Removing the configuration and the binaries"
rm -rf /etc/x-node
rm -rf /usr/local/share/doc/x-node
rm -f /etc/nftables.d/x-node.nft
rm -f /usr/local/bin/x-node /usr/local/bin/x-node-peerprobe
# The password cannot be recovered from web.json, and a reinstall generates a
# new one, so the plaintext copy is not kept around.
rm -f /root/x-node-credentials.txt

if [ "$KEEP_STATE" -eq 0 ]; then
    log "Removing the state directory"
    rm -rf "$STATE_DIR"
else
    log "Keeping $STATE_DIR"
fi

if id -u "$SERVICE_USER" >/dev/null 2>&1; then
    log "Removing the $SERVICE_USER user"
    userdel "$SERVICE_USER" 2>/dev/null || warn "could not remove the user $SERVICE_USER; stop its processes first"
fi
if getent group "$SERVICE_USER" >/dev/null 2>&1; then
    groupdel "$SERVICE_USER" 2>/dev/null || warn "could not remove the group $SERVICE_USER"
fi

if [ "$KEEP_YGGDRASIL" -eq 0 ]; then
    log "Removing Yggdrasil"
    systemctl disable --now yggdrasil 2>/dev/null || true
    if ! DEBIAN_FRONTEND=noninteractive apt-get purge -y yggdrasil; then
        warn "apt-get could not remove yggdrasil; remove it manually with: apt-get purge yggdrasil"
    fi
    rm -rf /etc/yggdrasil /var/lib/yggdrasil
else
    log "Keeping Yggdrasil"
    if [ "$WAS_INSTALLED" = "1" ] && [ -n "$YGG_BACKUP" ] && [ -f "$YGG_BACKUP" ]; then
        log "Restoring the Yggdrasil configuration from $YGG_BACKUP"
        cp -a "$YGG_BACKUP" "$YGG_CONF"
        chown root:yggdrasil "$YGG_CONF" 2>/dev/null || true
        chmod 0640 "$YGG_CONF" 2>/dev/null || true
        systemctl restart yggdrasil 2>/dev/null || true
    elif [ -f "$YGG_CONF" ]; then
        # With wss the public port belongs to nginx and Yggdrasil listens on
        # the local WebSocket port, so that is the entry to take out.
        REMOVE_PORT="$LISTEN_PORT"
        if [ "$LISTEN_PROTO" = "wss" ]; then
            REMOVE_PORT="$WS_PORT"
        fi
        if [ "$REMOVE_PORT" != "0" ]; then
            log "Removing the peering listener on port $REMOVE_PORT from $YGG_CONF"
            python3 - "$YGG_CONF" "$REMOVE_PORT" <<'PY' || warn "could not edit $YGG_CONF; remove the listener manually"
import json
import sys

path, port = sys.argv[1], sys.argv[2]
suffix = ":" + port
try:
    with open(path, "r", encoding="utf-8") as handle:
        config = json.load(handle)
except Exception as error:
    print("  cannot parse %s: %s" % (path, error), file=sys.stderr)
    sys.exit(1)
listen = config.get("Listen") or []
kept = [item for item in listen if not str(item).endswith(suffix)]
if kept != listen:
    config["Listen"] = kept
    with open(path, "w", encoding="utf-8") as handle:
        json.dump(config, handle, indent=2, ensure_ascii=False)
        handle.write("\n")
    print("  removed %s from Listen" % suffix)
PY
            systemctl restart yggdrasil 2>/dev/null || true
        fi
    fi
fi

echo
log "The exit node was removed"
cat <<EOF

  Removed:   x-node service, /etc/x-node, /usr/local/bin/x-node,
             /usr/local/bin/x-node-peerprobe, /root/x-node-credentials.txt
  State:     $([ "$KEEP_STATE" -eq 0 ] && echo "removed ($STATE_DIR)" || echo "kept ($STATE_DIR)")
  Yggdrasil: $([ "$KEEP_YGGDRASIL" -eq 0 ] && echo "removed" || echo "kept")

  Backups of the old configurations are in /var/backups/.
  If Yggdrasil was kept, check that its Listen and Peers lists are what you
  expect: systemctl restart yggdrasil

EOF
