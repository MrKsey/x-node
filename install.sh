#!/usr/bin/env bash
#
# X-Node node installer for Debian and Ubuntu.
#
# It installs Yggdrasil, picks the requested number of working outbound peers,
# creates a service user, writes the x-node configuration, starts both
# services and prints the first registration QR code. x-node keeps that
# public list alive: a dead or clearly slower peer is replaced through the
# Yggdrasil admin socket, while the clients of the exit node are never touched.
# Nothing is exposed to the clearnet: the SOCKS5 and registration sockets are
# bound to the Yggdrasil address only.
#
# The built-in web interface is served by the same binary under a random path.
# Behind nginx (the default for wss peering) it is proxied on the same TLS port
# and stays on loopback; without nginx it listens directly with a self-signed
# certificate. The password is generated once and written to
# /root/x-node-credentials.txt.
#
# An installation made by the old ygg-exit package is migrated automatically:
# the allowlist, the invites and the peer list are copied, the old service is
# stopped and removed, and ygg-exit becomes a symlink to x-node.
#
# Run it as root from the directory that contains install.sh, bin/x-node and
# bin/x-node-peerprobe.

set -euo pipefail

# sudo and some SSH setups hand over a minimal PATH; the installer calls ip,
# ss, openssl and nginx by name, so make sure the usual directories are there.
export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin${PATH:+:$PATH}"

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

# The node name is NODE_NAME and not NAME: /etc/os-release, which is sourced
# below, defines NAME="Ubuntu" and would silently overwrite the flag.
NODE_NAME="X-Node"
NODE_NAME_SET=0
SOCKS_PORT=1080
SOCKS_PORT_SET=0
API_PORT=1081
API_PORT_SET=0
YGG_PORT=13338
YGG_PORT_SET=0
# The peering transport. wss is the default: a WebSocket behind nginx on 443
# looks like ordinary HTTPS to a DPI box, while a plain tls listener does not.
YGG_PROTO=wss
YGG_PROTO_SET=0
# Yggdrasil itself only speaks ws://; nginx terminates TLS in front of it.
YGG_WS_PORT=13338
YGG_WS_PORT_SET=0
TLS_DIR="/etc/x-node/tls"
TLS_CERT="$TLS_DIR/x-node.crt"
TLS_KEY="$TLS_DIR/x-node.key"
NGINX_SITE="/etc/nginx/sites-available/x-node.conf"
NGINX_LINK="/etc/nginx/sites-enabled/x-node.conf"
NGINX_HTTP_SITE="/etc/nginx/sites-available/x-node-http.conf"
NGINX_HTTP_LINK="/etc/nginx/sites-enabled/x-node-http.conf"
STUB_DIR="/var/www/x-node"
# The web interface. Empty WEB_BIND/WEB_TLS mean "decide after the peering
# settings are known": loopback behind nginx, otherwise every address with a
# self-signed certificate.
WEB_ENABLED=1
WEB_ENABLED_SET=0
WEB_PORT=0
WEB_PORT_SET=0
WEB_BIND=""
WEB_BIND_SET=0
WEB_TLS=""
WEB_TLS_SET=0
WEB_PATH=""
WEB_PATH_SET=0
WEB_USER="admin"
WEB_PASSWORD=""
WEB_PASSWORD_SET=0
# Set when nginx terminates TLS in front of the interface: the public port is
# then the peering port, not WEB_PORT.
WEB_PUBLIC_PORT=""
CREDENTIALS_FILE="/root/x-node-credentials.txt"
IPV6_MODE="auto"
IPV6_MODE_SET=0
UPGRADE=0
# The old ygg-exit installation is migrated when it is found; --no-migrate
# leaves it alone, --migrate forces the step even if nothing looks installed.
MIGRATE=1
OLD_SERVICE="ygg-exit"
OLD_CONFIG="/etc/ygg-exit/config.json"
OLD_STATE_DIR="/var/lib/ygg-exit"
OLD_BIN="/usr/local/bin/ygg-exit"
OLD_PROBE="/usr/local/bin/ygg-peerprobe"
MIGRATED=0
PUBLIC_HOST=""
PUBLIC_HOST_SET=0
ALLOW_ALL=0
ALLOW_ALL_SET=0
REGISTRATION=1
REGISTRATION_SET=0
PEERS=""
PEERS_SET=0
SKIP_PEERS=0
SKIP_PEERS_SET=0
PEER_COUNT=3
PEER_COUNT_SET=0
PEER_AUTO_REPLACE=1
PEER_AUTO_REPLACE_SET=0
FIREWALL=0
FIREWALL_SET=0
ASSUME_YES=0
MAKE_INVITE=1
MAKE_INVITE_SET=0
UNINSTALL=0
SKIP_YGG_CONFIG=0
YGG_VERSION="${YGG_VERSION:-0.5.15}"
YGG_DEB=""
YGG_CONF=""
BIN_DIR=""
YGGDRASIL_WAS_INSTALLED=0
YGG_CONF_BACKUP=""

YGG_CONF_DEFAULT="/etc/yggdrasil/yggdrasil.conf"
EXIT_CONFIG="/etc/x-node/config.json"
STATE_DIR="/var/lib/x-node"
SERVICE_USER="x-node"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
    cat <<'EOF'
X-Node node installer

Usage:
  sudo ./install.sh [options]

Options:
  --name TEXT           name shown in the app and in the QR code
                        (default: "X-Node")
  --ygg-port N          TCP port for incoming Yggdrasil peers (default: 443
                        when it is free, otherwise 13338; 0 = outbound only)
  --ygg-proto PROTO     peering transport: wss (default, WebSocket behind nginx
                        on the port above, looks like HTTPS) or tls (direct
                        TLS listener, no nginx)
  --public-host HOST    public IPv4 address or hostname of this server, used
                        in the QR code for the direct peer link (default: ask
                        and auto-detect)
  --socks-port N        SOCKS5 port on the Yggdrasil address (default: 1080)
  --api-port N          registration/status port (default: 1081);
                        0 = share the SOCKS5 port and serve /v1/... there
  --allow-all           open mode: every Yggdrasil node may use the exit
  --no-registration     disable the registration endpoint
  --peers LIST          comma-separated peer URIs; skips the probe
  --skip-peers          keep the peers already present in yggdrasil.conf
  --peer-count N        public peers to keep connected (default: 3, 1..50)
  --no-peer-replace     keep a dead or slow public peer instead of replacing it
  --ipv6 MODE           outbound IPv6: auto (default), prefer or off
  --firewall            install the optional nftables rules
  --no-invite           do not create the first invite
  --web                 enable the web interface (default: ask, default yes)
  --no-web              do not serve the web interface at all
  --web-port N          port of the web interface (default: 8443 behind nginx,
                        otherwise 443 when free, else 8080)
  --web-bind TARGET     all, yggdrasil, loopback or an IP address
                        (default: loopback behind nginx, otherwise all)
  --web-tls MODE        auto, self (self-signed certificate) or off
                        (default: off behind nginx, otherwise self)
  --web-path PATH       URL prefix, e.g. /abc123/web (default: random)
  --web-user NAME       login name (default: admin)
  --web-password PASS   explicit password (default: generated once and
                        written to /root/x-node-credentials.txt)
  --upgrade             replace the binaries and restart; keep the existing
                        configuration, clients and password
  --no-migrate          do not migrate an existing ygg-exit installation
  --migrate             migrate ygg-exit even if the detection is unsure
  --uninstall           remove the installation (runs uninstall.sh)
  --ygg-version VER     Yggdrasil version to install (default: 0.5.15)
  --ygg-deb PATH        install Yggdrasil from a local .deb instead of GitHub
  --ygg-conf PATH       path to yggdrasil.conf (default: auto-detect)
  --bin-dir DIR         directory with x-node and x-node-peerprobe
  --yes                 non-interactive mode
  -h, --help            show this help

Examples:
  sudo ./install.sh --name "Berlin exit" --yes
  sudo ./install.sh --peers "tls://a.example:13338,tls://b.example:13338"
  sudo ./install.sh --peer-count 5 --yes
  sudo ./install.sh --peer-count 3 --no-peer-replace --yes
  sudo ./install.sh --ygg-proto tls --ygg-port 13338 --yes
  sudo ./install.sh --socks-port 443 --api-port 0 --yes
  sudo ./install.sh --ygg-port 0 --yes
  sudo ./install.sh --web-port 9443 --web-bind all --web-tls self --yes
  sudo ./install.sh --no-web --yes
  sudo ./install.sh --upgrade --yes
  sudo ./install.sh --firewall --allow-all
EOF
}

need_value() {
    [ "$#" -ge 2 ] || die "option $1 needs a value"
}

# True when something already listens on the TCP port, IPv4 or IPv6.
port_busy() {
    local port="$1"
    if command -v ss >/dev/null 2>&1; then
        ss -ltnH 2>/dev/null | awk '{print $4}' | grep -qE "[:.]${port}\$"
    elif command -v python3 >/dev/null 2>&1; then
        python3 - "$port" <<'PY'
import socket
import sys

sock = socket.socket(socket.AF_INET6, socket.SOCK_STREAM)
try:
    sock.bind(("::", int(sys.argv[1])))
except OSError:
    sys.exit(1)
finally:
    sock.close()
PY
    else
        return 1
    fi
}

# The manifest of the previous installation may still live in the ygg-exit
# state directory: the migration runs later in this script, so the peering port
# and protocol must be read from there before the new state directory exists.
manifest_path() {
    if [ -f "$STATE_DIR/install.json" ]; then
        printf '%s\n' "$STATE_DIR/install.json"
    else
        printf '%s\n' "$OLD_STATE_DIR/install.json"
    fi
}

# Reads a field from an existing install manifest, so a repeated run keeps the
# original Yggdrasil state and the backup made before the first install.
manifest_get() {
    python3 - "$(manifest_path)" "$1" <<'PY'
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

# Reads a dotted path from the exit configuration, e.g. "web.port". A repeated
# run uses it to keep the settings the operator already has instead of
# generating a new path or moving the port.
config_get() {
    python3 - "$1" "$2" <<'PY'
import json
import sys

try:
    with open(sys.argv[1], "r", encoding="utf-8") as handle:
        data = json.load(handle)
except Exception:
    data = {}
value = data
for part in sys.argv[2].split("."):
    if isinstance(value, dict):
        value = value.get(part, "")
    else:
        value = ""
        break
if isinstance(value, bool):
    print("1" if value else "0")
elif value is None:
    print("")
else:
    print(value)
PY
}

# random_web_path builds the URL prefix of the interface: /<16 chars>/web. The
# prefix is not a secret (the login page protects the interface); it only keeps
# scanners from finding the form. It runs before the packages are installed, so
# it uses /dev/urandom and not python3.
random_web_path() {
    local out
    out="$(LC_ALL=C tr -dc 'a-km-z2-9' < /dev/urandom | head -c 16)"
    printf '/%s/web\n' "$out"
}

# valid_web_path mirrors the check in the daemon: one to four segments of
# letters, digits, dash and underscore.
valid_web_path() {
    local value="$1" rest segment count=0
    [ "${#value}" -le 200 ] || return 1
    case "$value" in
        /*) ;;
        *) return 1 ;;
    esac
    rest="${value#/}"
    [ -n "$rest" ] || return 1
    while :; do
        segment="${rest%%/*}"
        [ -n "$segment" ] && [ "${#segment}" -le 64 ] || return 1
        case "$segment" in
            *[!A-Za-z0-9_-]*) return 1 ;;
        esac
        count=$((count + 1))
        [ "$count" -le 4 ] || return 1
        [ "$rest" = "$segment" ] && break
        rest="${rest#*/}"
    done
    return 0
}

# web_public_url is the address a browser should open. Behind nginx it is the
# peering port with the path; without nginx it is the interface port with the
# scheme the daemon will use.
web_public_url() {
    local scheme="http" host port suffix
    if [ -n "$WEB_PUBLIC_PORT" ]; then
        # nginx terminates TLS in front of the interface.
        scheme="https"
        port="$WEB_PUBLIC_PORT"
        host="${PUBLIC_HOST:-$(hostname -f 2>/dev/null || hostname)}"
    else
        case "$WEB_TLS" in
            self) scheme="https" ;;
            auto)
                if [ -f "$STATE_DIR/tls/web.crt" ]; then
                    scheme="https"
                fi
                ;;
        esac
        port="$WEB_PORT"
        case "$WEB_BIND" in
            loopback)  host="127.0.0.1" ;;
            yggdrasil) host="[$ADDRESS]" ;;
            *)         host="${PUBLIC_HOST:-$(hostname -f 2>/dev/null || hostname)}" ;;
        esac
    fi
    suffix=""
    if ! { [ "$scheme" = "https" ] && [ "$port" = "443" ]; } \
        && ! { [ "$scheme" = "http" ] && [ "$port" = "80" ]; }; then
        suffix=":$port"
    fi
    printf '%s://%s%s%s/' "$scheme" "$host" "$suffix" "$WEB_PATH"
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --name)           need_value "$@"; NODE_NAME="$2"; NODE_NAME_SET=1; shift 2 ;;
        --ygg-port)       need_value "$@"; YGG_PORT="$2"; YGG_PORT_SET=1; shift 2 ;;
        --ygg-proto)      need_value "$@"; YGG_PROTO="$2"; YGG_PROTO_SET=1; shift 2 ;;
        --public-host)    need_value "$@"; PUBLIC_HOST="$2"; PUBLIC_HOST_SET=1; shift 2 ;;
        --socks-port)     need_value "$@"; SOCKS_PORT="$2"; SOCKS_PORT_SET=1; shift 2 ;;
        --api-port)       need_value "$@"; API_PORT="$2"; API_PORT_SET=1; shift 2 ;;
        --allow-all)      ALLOW_ALL=1; ALLOW_ALL_SET=1; shift ;;
        --no-registration) REGISTRATION=0; REGISTRATION_SET=1; shift ;;
        --peers)          need_value "$@"; PEERS="$2"; PEERS_SET=1; shift 2 ;;
        --skip-peers)     SKIP_PEERS=1; SKIP_PEERS_SET=1; shift ;;
        --peer-count)     need_value "$@"; PEER_COUNT="$2"; PEER_COUNT_SET=1; shift 2 ;;
        --no-peer-replace) PEER_AUTO_REPLACE=0; PEER_AUTO_REPLACE_SET=1; shift ;;
        --firewall)       FIREWALL=1; FIREWALL_SET=1; shift ;;
        --no-invite)      MAKE_INVITE=0; MAKE_INVITE_SET=1; shift ;;
        --ipv6)           need_value "$@"; IPV6_MODE="$2"; IPV6_MODE_SET=1; shift 2 ;;
        --web)            WEB_ENABLED=1; WEB_ENABLED_SET=1; shift ;;
        --no-web)         WEB_ENABLED=0; WEB_ENABLED_SET=1; shift ;;
        --web-port)       need_value "$@"; WEB_PORT="$2"; WEB_PORT_SET=1; shift 2 ;;
        --web-bind)       need_value "$@"; WEB_BIND="$2"; WEB_BIND_SET=1; shift 2 ;;
        --web-tls)        need_value "$@"; WEB_TLS="$2"; WEB_TLS_SET=1; shift 2 ;;
        --web-path)       need_value "$@"; WEB_PATH="$2"; WEB_PATH_SET=1; shift 2 ;;
        --web-user)       need_value "$@"; WEB_USER="$2"; shift 2 ;;
        --web-password)   need_value "$@"; WEB_PASSWORD="$2"; WEB_PASSWORD_SET=1; shift 2 ;;
        --upgrade)        UPGRADE=1; shift ;;
        --no-migrate)     MIGRATE=0; shift ;;
        --migrate)        MIGRATE=1; shift ;;
        --uninstall)      UNINSTALL=1; shift ;;
        --ygg-version)    need_value "$@"; YGG_VERSION="$2"; shift 2 ;;
        --ygg-deb)        need_value "$@"; YGG_DEB="$2"; shift 2 ;;
        --ygg-conf)       need_value "$@"; YGG_CONF="$2"; shift 2 ;;
        --bin-dir)        need_value "$@"; BIN_DIR="$2"; shift 2 ;;
        --yes)            ASSUME_YES=1; shift ;;
        -h|--help)        usage; exit 0 ;;
        *)                die "unknown option: $1 (try --help)" ;;
    esac
done

YGG_VERSION="${YGG_VERSION#v}"

# --uninstall hands the whole job to uninstall.sh: one entry point is easier to
# remember, and the uninstaller already knows about the manifest and Yggdrasil.
if [ "$UNINSTALL" -eq 1 ]; then
    [ -f "$SCRIPT_DIR/uninstall.sh" ] || die "uninstall.sh is missing next to install.sh"
    if [ "$ASSUME_YES" -eq 1 ]; then
        exec bash "$SCRIPT_DIR/uninstall.sh" --yes
    fi
    exec bash "$SCRIPT_DIR/uninstall.sh"
fi

[ "$(id -u)" -eq 0 ] || die "run this installer as root"
[ -r /etc/os-release ] || die "cannot read /etc/os-release"# shellcheck disable=SC1091
. /etc/os-release
case "${ID:-} ${ID_LIKE:-}" in
    *debian*|*ubuntu*) ;;
    *) die "this installer supports Debian and Ubuntu only" ;;
esac
command -v systemctl >/dev/null 2>&1 || die "systemd is required"
command -v apt-get >/dev/null 2>&1 || die "apt-get is required"

ARCH="$(uname -m)"
case "$ARCH" in
    x86_64|amd64)   ARCH=amd64 ;;
    aarch64|arm64)  ARCH=arm64 ;;
    *) die "unsupported architecture: $ARCH" ;;
esac

# The default logic below compares the port numerically, so a bad --ygg-port
# must be rejected before that, not after the questions.
case "$YGG_PORT" in ''|*[!0-9]*) die "Yggdrasil peering port must be a number" ;; esac
[ "$YGG_PORT" -ge 0 ] && [ "$YGG_PORT" -le 65535 ] || die "Yggdrasil peering port is out of range"

# A repeated run keeps the peering port and protocol of the existing install,
# otherwise the default check would see nginx on 443 and move the port.
if [ -f "$(manifest_path)" ] && command -v python3 >/dev/null 2>&1; then
    if [ "$YGG_PORT_SET" -eq 0 ]; then
        previous_port="$(manifest_get listenPort || true)"
        case "$previous_port" in
            ''|*[!0-9]*) ;;
            *) YGG_PORT="$previous_port"; YGG_PORT_SET=1 ;;
        esac
    fi
    if [ "$YGG_PROTO_SET" -eq 0 ]; then
        previous_proto="$(manifest_get listenProto || true)"
        case "$previous_proto" in
            wss|tls) YGG_PROTO="$previous_proto"; YGG_PROTO_SET=1 ;;
            *)
                # Manifest v1 did not record the protocol: those installs used
                # a direct TLS listener, so a plain rerun keeps it.
                YGG_PROTO=tls; YGG_PROTO_SET=1
                ;;
        esac
    fi
    previous_ws="$(manifest_get wsPort || true)"
    case "$previous_ws" in
        ''|*[!0-9]*|0) ;;
        *) YGG_WS_PORT="$previous_ws"; YGG_WS_PORT_SET=1 ;;
    esac
fi

# A repeated run keeps the web interface, its port, its bind address and its
# path, and the IPv6 mode: only a flag changes them. The configuration may
# still be the ygg-exit one, because the migration runs later.
PREVIOUS_CONFIG="$EXIT_CONFIG"
if [ ! -f "$PREVIOUS_CONFIG" ] && [ -f "$OLD_CONFIG" ]; then
    PREVIOUS_CONFIG="$OLD_CONFIG"
fi
if [ -f "$PREVIOUS_CONFIG" ] && command -v python3 >/dev/null 2>&1; then
    if [ "$WEB_ENABLED_SET" -eq 0 ]; then
        case "$(config_get "$PREVIOUS_CONFIG" web.enabled)" in
            0) WEB_ENABLED=0 ;;
            1) WEB_ENABLED=1 ;;
        esac
    fi
    if [ "$WEB_PORT_SET" -eq 0 ]; then
        previous_web_port="$(config_get "$PREVIOUS_CONFIG" web.port)"
        case "$previous_web_port" in
            ''|*[!0-9]*) ;;
            *) WEB_PORT="$previous_web_port"; WEB_PORT_SET=1 ;;
        esac
    fi
    if [ "$WEB_BIND_SET" -eq 0 ]; then
        previous_web_bind="$(config_get "$PREVIOUS_CONFIG" web.listen)"
        case "$previous_web_bind" in
            all|yggdrasil|loopback) WEB_BIND="$previous_web_bind"; WEB_BIND_SET=1 ;;
        esac
    fi
    if [ "$WEB_TLS_SET" -eq 0 ]; then
        previous_web_tls="$(config_get "$PREVIOUS_CONFIG" web.tls)"
        case "$previous_web_tls" in
            auto|self|off) WEB_TLS="$previous_web_tls"; WEB_TLS_SET=1 ;;
        esac
    fi
    if [ "$WEB_PATH_SET" -eq 0 ]; then
        previous_web_path="$(config_get "$PREVIOUS_CONFIG" web.path)"
        if [ -n "$previous_web_path" ] && valid_web_path "$previous_web_path"; then
            WEB_PATH="$previous_web_path"
        fi
    fi
    if [ "$IPV6_MODE_SET" -eq 0 ]; then
        previous_ipv6="$(config_get "$PREVIOUS_CONFIG" ipv6)"
        case "$previous_ipv6" in
            auto|prefer|off) IPV6_MODE="$previous_ipv6" ;;
        esac
    fi
fi

# The defaults are chosen here, before the questions, so the prompt shows the
# port the installer will actually use. 443 behind nginx is the DPI-resistant
# choice; a busy port falls back to the historical 13338.
if [ "$YGG_PROTO_SET" -eq 0 ]; then
    YGG_PROTO=wss
fi
if [ "$YGG_PORT_SET" -eq 0 ]; then
    if port_busy 443; then
        YGG_PORT=13338
        echo "Port 443 is already in use; the Yggdrasil peering port defaults to $YGG_PORT."
    else
        YGG_PORT=443
    fi
fi
case "$YGG_PROTO" in
    wss|tls) ;;
    *) die "unknown peering protocol: $YGG_PROTO (use wss or tls)" ;;
esac
# Yggdrasil has no wss listener of its own: nginx terminates TLS and forwards
# to a plain WebSocket listener on localhost.
if [ "$YGG_PROTO" = "wss" ] && [ "$YGG_PORT" -gt 0 ] && [ "$YGG_WS_PORT_SET" -eq 0 ]; then
    if [ "$YGG_WS_PORT" -eq "$YGG_PORT" ]; then
        YGG_WS_PORT=$((YGG_WS_PORT + 1))
    fi
    while port_busy "$YGG_WS_PORT"; do
        YGG_WS_PORT=$((YGG_WS_PORT + 1))
    done
fi

# --upgrade replaces the binaries and restarts the services. Everything the
# operator configured stays as it is: the questions are skipped, the Yggdrasil
# configuration is left alone and the exit configuration is only topped up with
# the keys a new version added.
if [ "$UPGRADE" -eq 1 ]; then
    [ -f "$EXIT_CONFIG" ] || [ -f "$OLD_CONFIG" ] \
        || die "--upgrade needs an existing $EXIT_CONFIG (or a ygg-exit installation)"
    ASSUME_YES=1
    SKIP_PEERS=1
    SKIP_PEERS_SET=1
    MAKE_INVITE=0
    MAKE_INVITE_SET=1
    SKIP_YGG_CONFIG=1
fi

# Ask for the name, the ports, the peers and the optional steps before printing
# the summary. Every question has a default, so pressing Enter keeps the value
# shown in brackets; a flag skips its question.
if [ "$ASSUME_YES" -eq 0 ]; then
    [ -t 0 ] || die "stdin is not a terminal; pass --yes for non-interactive mode"
    cat <<'EOF'
Every question has a default in brackets: press Enter to accept it.

  --name         the exit node name the app and the invite QR code show.
  --ygg-port     the TCP port the phone connects to as a pinned peer. 443 is
                 the default when it is free; 0 disables incoming peers.
  --ygg-proto    wss (default) serves the peering as a WebSocket behind nginx
                 with TLS, so it looks like ordinary HTTPS. tls opens the
                 Yggdrasil TLS listener directly, without nginx.
  --public-host  the public IPv4 address or hostname written into the QR peer
                 link; blank lets the installer detect it.
  --socks-port   the SOCKS5 port on the Yggdrasil address. 443 makes the
                 provider see ordinary HTTPS traffic instead of a proxy port.
  --api-port     the registration/status API. 0 keeps it on the SOCKS5 port
                 under /v1/..., so only one port has to be open.
  --peers        comma-separated peering URIs, or "auto" to probe the public
                 list, or "keep" to leave yggdrasil.conf untouched.
  --peer-count   how many public peers the exit keeps connected. Clients that
                 dial the exit node are extra: they are never counted and never
                 replaced.
  --no-peer-replace  keep a dead or slow public peer instead of swapping it for
                 a better one from the public list.
  --firewall     install the optional nftables rules (a second line of
                 defence around the SOCKS5/API ports).
  --no-invite    do not create the first invite QR code.

EOF
    if [ "$NODE_NAME_SET" -eq 0 ]; then
        read -r -p "Exit node name [$NODE_NAME]: " answer
        [ -z "$answer" ] || NODE_NAME="$answer"
    fi
    if [ "$YGG_PROTO_SET" -eq 0 ]; then
        read -r -p "Peering protocol, wss = HTTPS-like, tls = direct [$YGG_PROTO]: " answer
        [ -z "$answer" ] || YGG_PROTO="$answer"
    fi
    if [ "$YGG_PORT_SET" -eq 0 ]; then
        read -r -p "Yggdrasil peering port, 0 = outbound only [$YGG_PORT]: " answer
        [ -z "$answer" ] || YGG_PORT="$answer"
    fi
    if [ "$SOCKS_PORT_SET" -eq 0 ]; then
        read -r -p "SOCKS5 port, 443 = look like HTTPS [$SOCKS_PORT]: " answer
        [ -z "$answer" ] || SOCKS_PORT="$answer"
    fi
    if [ "$API_PORT_SET" -eq 0 ]; then
        read -r -p "API port, 0 = share the SOCKS5 port under /v1/... [$API_PORT]: " answer
        [ -z "$answer" ] || API_PORT="$answer"
    fi
    if [ "$PUBLIC_HOST_SET" -eq 0 ] && [ "$YGG_PORT" -gt 0 ]; then
        read -r -p "Public address for the QR peer link, blank = auto-detect: " answer
        PUBLIC_HOST="$answer"
    fi
    if [ "$PEERS_SET" -eq 0 ] && [ "$SKIP_PEERS_SET" -eq 0 ]; then
        read -r -p "Peers: list, \"auto\" or \"keep\" [auto]: " answer
        case "${answer:-auto}" in
            auto|AUTO) ;;
            keep|KEEP) SKIP_PEERS=1 ;;
            *)         PEERS="$answer" ;;
        esac
    fi
    if [ "$PEER_COUNT_SET" -eq 0 ]; then
        read -r -p "Public peers to keep connected, clients are extra [$PEER_COUNT]: " answer
        [ -z "$answer" ] || PEER_COUNT="$answer"
        PEER_COUNT_SET=1
    fi
    if [ "$PEER_AUTO_REPLACE_SET" -eq 0 ]; then
        read -r -p "Replace dead or slow public peers automatically? [Y/n]: " answer
        case "$answer" in
            n|N|no|NO) PEER_AUTO_REPLACE=0 ;;
        esac
        PEER_AUTO_REPLACE_SET=1
    fi
    if [ "$FIREWALL_SET" -eq 0 ]; then
        read -r -p "Install the optional nftables rules? [y/N]: " answer
        case "$answer" in
            y|Y|yes|YES) FIREWALL=1 ;;
        esac
    fi
    if [ "$MAKE_INVITE_SET" -eq 0 ]; then
        read -r -p "Create the first invite QR code now? [Y/n]: " answer
        case "$answer" in
            n|N|no|NO) MAKE_INVITE=0 ;;
        esac
    fi
fi

# One question is enough for the web interface: the bind address, the TLS mode
# and the random path follow from the peering settings chosen above and are
# shown in the summary before anything is changed.
if [ "$ASSUME_YES" -eq 0 ] && [ "$WEB_ENABLED_SET" -eq 0 ]; then
    read -r -p "Serve the web interface (browser dashboard)? [Y/n]: " answer
    case "$answer" in
        n|N|no|NO) WEB_ENABLED=0 ;;
    esac
fi

[ -n "${NODE_NAME// /}" ] || die "the exit node name must not be empty"
case "$YGG_PROTO"  in wss|tls) ;; *) die "the peering protocol must be wss or tls" ;; esac
case "$YGG_PORT"   in ''|*[!0-9]*) die "Yggdrasil peering port must be a number" ;; esac
case "$SOCKS_PORT" in ''|*[!0-9]*) die "SOCKS5 port must be a number" ;; esac
case "$API_PORT"   in ''|*[!0-9]*) die "API port must be a number" ;; esac
[ "$YGG_PORT" -ge 0 ] && [ "$YGG_PORT" -le 65535 ] || die "Yggdrasil peering port is out of range"
[ "$SOCKS_PORT" -ge 1 ] && [ "$SOCKS_PORT" -le 65535 ] || die "SOCKS5 port is out of range"
[ "$API_PORT" -ge 0 ] && [ "$API_PORT" -le 65535 ] \
    || die "API port is out of range (0 = share the SOCKS5 port)"
if [ "$API_PORT" -gt 0 ]; then
    [ "$SOCKS_PORT" -ne "$API_PORT" ] \
        || die "SOCKS5 and API ports must differ (use --api-port 0 to share)"
fi
case "$PEER_COUNT" in ''|*[!0-9]*) die "peer count must be a number" ;; esac
[ "$PEER_COUNT" -ge 1 ] && [ "$PEER_COUNT" -le 50 ] || die "peer count is out of range (1..50)"
if [ "$YGG_PORT" -gt 0 ]; then
    [ "$YGG_PORT" -ne "$SOCKS_PORT" ] \
        || die "the Yggdrasil peering port must differ from SOCKS5"
    if [ "$API_PORT" -gt 0 ]; then
        [ "$YGG_PORT" -ne "$API_PORT" ] \
            || die "the Yggdrasil peering port must differ from the API port"
    fi
fi
case "$PUBLIC_HOST" in
    *[!A-Za-z0-9.:_-]*) die "the public address must be an IP address or a hostname" ;;
esac

# Yggdrasil has no wss listener of its own: nginx terminates TLS and forwards
# to a plain WebSocket listener on localhost. The port must be free on the
# loopback interface; the answer from the prompt above can change the choice.
if [ "$YGG_PROTO" = "wss" ] && [ "$YGG_PORT" -gt 0 ]; then
    # A manifest written in tls mode records the local port even though it was
    # not used; switching to wss must not put nginx and Yggdrasil on one port.
    if [ "$YGG_WS_PORT" -eq "$YGG_PORT" ]; then
        YGG_WS_PORT=$((YGG_WS_PORT + 1))
        YGG_WS_PORT_SET=0
    fi
    if [ "$YGG_WS_PORT_SET" -eq 0 ]; then
        while port_busy "$YGG_WS_PORT"; do
            YGG_WS_PORT=$((YGG_WS_PORT + 1))
        done
    fi
fi

# The web interface. Behind nginx it shares the TLS port of the peering: one
# certificate, one port, nothing extra to open in the provider firewall. Without
# nginx it listens on its own port with a self-signed certificate.
if [ "$WEB_ENABLED" -eq 1 ]; then
    if [ "$YGG_PROTO" = "wss" ] && [ "$YGG_PORT" -gt 0 ]; then
        [ -n "$WEB_BIND" ] || WEB_BIND=loopback
        [ -n "$WEB_TLS" ] || WEB_TLS=off
        WEB_PUBLIC_PORT="$YGG_PORT"
        if [ "$WEB_PORT_SET" -eq 0 ] || [ "$WEB_PORT" -eq 0 ]; then
            WEB_PORT=8443
            while [ "$WEB_PORT" -eq "$YGG_WS_PORT" ] || [ "$WEB_PORT" -eq "$YGG_PORT" ] \
                || port_busy "$WEB_PORT"; do
                WEB_PORT=$((WEB_PORT + 1))
            done
        fi
    else
        [ -n "$WEB_BIND" ] || WEB_BIND=all
        [ -n "$WEB_TLS" ] || WEB_TLS=self
        if [ "$WEB_PORT_SET" -eq 0 ] || [ "$WEB_PORT" -eq 0 ]; then
            WEB_PORT=443
            # The Yggdrasil, SOCKS5 and API ports are already taken by this
            # installation, and yggdrasil is not running yet when this check
            # happens, so port_busy alone would not see them.
            while [ "$WEB_PORT" -eq "$YGG_PORT" ] || [ "$WEB_PORT" -eq "$SOCKS_PORT" ] \
                || { [ "$API_PORT" -gt 0 ] && [ "$WEB_PORT" -eq "$API_PORT" ]; } \
                || port_busy "$WEB_PORT"; do
                if [ "$WEB_PORT" -eq 443 ]; then
                    WEB_PORT=8080
                else
                    WEB_PORT=$((WEB_PORT + 1))
                fi
            done
        fi
    fi
    if [ -z "$WEB_PATH" ]; then
        WEB_PATH="$(random_web_path)"
    fi
fi

case "$IPV6_MODE" in auto|prefer|off) ;; *) die "ipv6 must be auto, prefer or off" ;; esac
if [ "$WEB_ENABLED" -eq 1 ]; then
    case "$WEB_PORT" in ''|*[!0-9]*) die "the web port must be a number" ;; esac
    [ "$WEB_PORT" -ge 1 ] && [ "$WEB_PORT" -le 65535 ] || die "the web port is out of range"
    [ "$WEB_PORT" -ne "$SOCKS_PORT" ] || die "the web port must differ from the SOCKS5 port"
    if [ "$API_PORT" -gt 0 ]; then
        [ "$WEB_PORT" -ne "$API_PORT" ] || die "the web port must differ from the API port"
    fi
    if [ -z "$WEB_PUBLIC_PORT" ] && [ "$YGG_PORT" -gt 0 ]; then
        [ "$WEB_PORT" -ne "$YGG_PORT" ] || die "the web port must differ from the Yggdrasil peering port"
    fi
    case "$WEB_BIND" in
        all|yggdrasil|loopback) ;;
        *)
            if ! python3 -c 'import ipaddress,sys; ipaddress.ip_address(sys.argv[1])' "$WEB_BIND" 2>/dev/null; then
                die "--web-bind must be all, yggdrasil, loopback or an IP address"
            fi
            ;;
    esac
    case "$WEB_TLS" in auto|self|off) ;; *) die "--web-tls must be auto, self or off" ;; esac
    valid_web_path "$WEB_PATH" || die "the web path must look like /abc123/web (letters, digits, dash, underscore)"
    case "$WEB_USER" in ''|*[!A-Za-z0-9._-]*) die "the web login must be a simple name" ;; esac
    # A plain-HTTP interface on a public address sends the password in clear
    # text. It is a valid choice behind another TLS terminator, so it is only a
    # warning, but a loud one.
    if [ "$WEB_BIND" != "loopback" ] && [ "$WEB_TLS" = "off" ]; then
        warn "the web interface will be served over plain HTTP on a public address;"
        warn "use --web-tls self, --web-bind loopback or terminate TLS in front of it."
    fi
fi

# The port the HTTP API is reachable on; 0 means it shares the SOCKS5 listener.
API_PORT_EFFECTIVE="$API_PORT"
[ "$API_PORT_EFFECTIVE" -ne 0 ] || API_PORT_EFFECTIVE="$SOCKS_PORT"
API_DESCRIPTION="$API_PORT"
[ "$API_PORT" -ne 0 ] || API_DESCRIPTION="$API_PORT (shared with SOCKS5, /v1/...)"

cat <<EOF
X-Node node installer

  Name:          $NODE_NAME
  Yggdrasil:     $YGG_VERSION
  Yggdrasil port: $YGG_PORT$([ "$YGG_PORT" -eq 0 ] && echo " (outbound only)" || echo " (incoming peers, $YGG_PROTO)")
$([ "$YGG_PROTO" = "wss" ] && [ "$YGG_PORT" -gt 0 ] && printf '  WS backend:    127.0.0.1:%s (nginx terminates TLS)\n' "$YGG_WS_PORT")
  Public host:   ${PUBLIC_HOST:-auto-detect}
  SOCKS5 port:   $SOCKS_PORT
  API port:      $API_DESCRIPTION
  Open mode:     $ALLOW_ALL
  Registration:  $REGISTRATION
  Peers:         $([ "$SKIP_PEERS" -eq 1 ] && echo "keep the existing list" || echo "${PEERS:-auto (best $PEER_COUNT peers)}")
  Public peers:  $PEER_COUNT, automatic replacement $([ "$PEER_AUTO_REPLACE" -eq 1 ] && echo "on" || echo "off")
  IPv6:          $IPV6_MODE
  Web interface: $([ "$WEB_ENABLED" -eq 1 ] && echo "on, $WEB_TLS TLS, $WEB_BIND:${WEB_PORT}${WEB_PATH}" || echo "off")
  Firewall:      $FIREWALL
  First invite:  $MAKE_INVITE

The installer will install packages, rewrite the Yggdrasil configuration
(a backup is kept) and start two systemd services.
EOF

if [ "$ASSUME_YES" -eq 0 ]; then
    read -r -p "Continue? [y/N] " answer
    case "$answer" in
        y|Y|yes|YES) ;;
        *) echo "Aborted."; exit 1 ;;
    esac
fi

log "Installing dependencies"
need_packages=0
command -v curl >/dev/null 2>&1 || need_packages=1
command -v python3 >/dev/null 2>&1 || need_packages=1
command -v ip >/dev/null 2>&1 || need_packages=1
[ -d /etc/ssl/certs ] || need_packages=1
if [ "$FIREWALL" -eq 1 ] && ! command -v nft >/dev/null 2>&1; then
    need_packages=1
fi
if [ "$YGG_PROTO" = "wss" ] && [ "$YGG_PORT" -gt 0 ]; then
    command -v nginx >/dev/null 2>&1 || need_packages=1
    command -v openssl >/dev/null 2>&1 || need_packages=1
fi
if [ "$need_packages" -eq 1 ]; then
    apt-get update -y
    DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
        ca-certificates curl python3 iproute2
    if [ "$FIREWALL" -eq 1 ] && ! command -v nft >/dev/null 2>&1; then
        DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends nftables
    fi
    if [ "$YGG_PROTO" = "wss" ] && [ "$YGG_PORT" -gt 0 ]; then
        DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
            nginx openssl
    fi
fi

if [ "$YGG_PORT" -gt 0 ] && [ -z "$PUBLIC_HOST" ]; then
    log "Detecting the public address"
    for url in https://api.ipify.org https://ifconfig.me/ip https://icanhazip.com; do
        detected="$(curl -4 -fsS --max-time 6 "$url" 2>/dev/null | tr -d '[:space:]' || true)"
        case "$detected" in
            ''|*[!0-9.]*) continue ;;
        esac
        PUBLIC_HOST="$detected"
        break
    done
    if [ -n "$PUBLIC_HOST" ]; then
        echo "    public address: $PUBLIC_HOST"
    else
        warn "could not detect the public address; the QR code will not contain a direct peer link."
        warn "pass --public-host HOST and run the installer again to enable it."
    fi
fi

log "Checking Yggdrasil"
current_version=""
if command -v yggdrasil >/dev/null 2>&1; then
    current_version="$(yggdrasil -version 2>/dev/null | awk '/Build version:/ {print $3; exit}')"
fi
if [ -n "$current_version" ]; then
    YGGDRASIL_WAS_INSTALLED=1
    echo "    installed: $current_version"
    if dpkg --compare-versions "$current_version" ge "0.6.0" 2>/dev/null; then
        warn "Yggdrasil $current_version is newer than the tested 0.5.x series; keeping it."
    fi
fi
if [ -z "$current_version" ] || ! dpkg --compare-versions "$current_version" ge "$YGG_VERSION" 2>/dev/null; then
    deb="$(mktemp --suffix=.deb)"
    trap 'rm -f "$deb"' EXIT
    if [ -n "$YGG_DEB" ]; then
        [ -f "$YGG_DEB" ] || die "no such file: $YGG_DEB"
        cp "$YGG_DEB" "$deb"
    else
        url="https://github.com/yggdrasil-network/yggdrasil-go/releases/download/v${YGG_VERSION}/yggdrasil-${YGG_VERSION}-${ARCH}.deb"
        log "Downloading $url"
        curl -fsSL --retry 3 --connect-timeout 15 -o "$deb" "$url"
    fi
    log "Installing Yggdrasil $YGG_VERSION"
    if ! dpkg -i "$deb"; then
        DEBIAN_FRONTEND=noninteractive apt-get -f install -y
    fi
    rm -f "$deb"
    trap - EXIT
else
    log "Yggdrasil $current_version is already installed"
fi

getent group yggdrasil >/dev/null 2>&1 || groupadd --system yggdrasil
id -u yggdrasil >/dev/null 2>&1 || useradd --system --gid yggdrasil \
    --home-dir /var/lib/yggdrasil --shell /usr/sbin/nologin yggdrasil

if [ -z "$YGG_CONF" ]; then
    if [ -f /etc/yggdrasil/yggdrasil.conf ]; then
        YGG_CONF=/etc/yggdrasil/yggdrasil.conf
    elif [ -f /etc/yggdrasil.conf ]; then
        YGG_CONF=/etc/yggdrasil.conf
    else
        YGG_CONF="$YGG_CONF_DEFAULT"
    fi
fi
mkdir -p "$(dirname "$YGG_CONF")"

log "Locating the binaries"
if [ -z "$BIN_DIR" ]; then
    for candidate in "$SCRIPT_DIR/bin" "$SCRIPT_DIR"; do
        if [ -f "$candidate/x-node" ] && [ -f "$candidate/x-node-peerprobe" ]; then
            BIN_DIR="$candidate"
            break
        fi
    done
fi
[ -n "$BIN_DIR" ] || die "x-node and x-node-peerprobe were not found; pass --bin-dir"
[ -f "$BIN_DIR/x-node" ] || die "missing $BIN_DIR/x-node"
[ -f "$BIN_DIR/x-node-peerprobe" ] || die "missing $BIN_DIR/x-node-peerprobe"
# A folder copied from another machine often loses the executable bit; the
# binaries are run below, before they are installed, so fix it here.
chmod 0755 "$BIN_DIR/x-node" "$BIN_DIR/x-node-peerprobe" \
    || die "could not make $BIN_DIR/x-node and x-node-peerprobe executable"
if ! "$BIN_DIR/x-node" version >/dev/null 2>&1; then
    die "$BIN_DIR/x-node cannot run on this machine (wrong architecture?)"
fi
if ! "$BIN_DIR/x-node-peerprobe" -source file:///nonexistent -count 1 -pool 1 \
        -timeout 1ms -plain >/dev/null 2>&1; then
    die "$BIN_DIR/x-node-peerprobe cannot run on this machine (wrong architecture?)"
fi
install -m 0755 -o root -g root "$BIN_DIR/x-node" /usr/local/bin/x-node
install -m 0755 -o root -g root "$BIN_DIR/x-node-peerprobe" /usr/local/bin/x-node-peerprobe

PEERS_MODE="set"
SELECTED_PEERS=""
if [ "$SKIP_PEERS" -eq 1 ]; then
    PEERS_MODE=keep
elif [ -n "$PEERS" ]; then
    SELECTED_PEERS="$(printf '%s\n' "$PEERS" | tr ',' '\n' \
        | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' | grep -v '^$' || true)"
    [ -n "$SELECTED_PEERS" ] || die "--peers did not contain a single URI"
else
    log "Probing public Yggdrasil peers (this can take a minute)"
    SELECTED_PEERS="$(/usr/local/bin/x-node-peerprobe -count "$PEER_COUNT" -plain 2>/dev/null || true)"
    if [ -z "$SELECTED_PEERS" ]; then
        warn "The peer probe returned nothing; using the built-in seeds."
        SELECTED_PEERS="$(printf '%s\n' \
            'tls://box.paulll.cc:13338' \
            'tcp://yggno.de:18226' \
            'quic://novosibirsk.mie.foundation:9952')"
    fi
fi
if [ -n "$SELECTED_PEERS" ]; then
    echo "    peers:"
    printf '%s\n' "$SELECTED_PEERS" | sed 's/^/      /'
fi

log "Writing the Yggdrasil configuration"
# An upgrade keeps the file as it is: the operator's Listen and Peers lists are
# what makes the node reachable, and they were already written by an earlier
# run of this installer.
if [ "$SKIP_YGG_CONFIG" -eq 1 ]; then
    echo "    kept: $YGG_CONF (upgrade)"
else
systemctl stop yggdrasil 2>/dev/null || true
ygg_tmp="$(mktemp)"
trap 'rm -f "$ygg_tmp"' EXIT
if [ -f "$YGG_CONF" ]; then
    backup="/var/backups/yggdrasil.conf.$(date +%Y%m%d-%H%M%S)"
    mkdir -p /var/backups
    cp -a "$YGG_CONF" "$backup"
    YGG_CONF_BACKUP="$backup"
    echo "    backup: $backup"
    yggdrasil -useconffile "$YGG_CONF" -normaliseconf -json > "$ygg_tmp"
else
    yggdrasil -genconf -json > "$ygg_tmp"
fi

peers_json="$(printf '%s\n' "$SELECTED_PEERS" | python3 -c \
    'import json,sys; print(json.dumps([line.strip() for line in sys.stdin if line.strip()]))')"
python3 - "$ygg_tmp" "$PEERS_MODE" "$peers_json" "$NODE_NAME" "$YGG_PORT" \
    "$YGG_PROTO" "$YGG_WS_PORT" <<'PY'
import json
import sys

path, mode, peers_raw, name, listen_port, proto, ws_port = sys.argv[1:8]
with open(path, "r", encoding="utf-8") as handle:
    config = json.load(handle)

# Incoming peering is optional. With wss nginx terminates TLS on the public
# port and forwards to a plain WebSocket listener on localhost, so the node
# only has to speak ws://. The tls mode opens the Yggdrasil TLS listener
# directly on the public interface. Multicast discovery stays off either way.
if int(listen_port) > 0:
    if proto == "wss":
        config["Listen"] = ["ws://127.0.0.1:%s" % ws_port]
    else:
        config["Listen"] = ["tls://[::]:%s" % listen_port]
else:
    config["Listen"] = []
config["MulticastInterfaces"] = []
# x-node doctor and yggdrasilctl talk to the admin socket. The Debian build
# already defaults to this path, but an explicit value keeps a locally built
# yggdrasil usable as well. An existing "none" is preserved.
if not config.get("AdminListen"):
    config["AdminListen"] = "unix:///var/run/yggdrasil/yggdrasil.sock"
if mode == "set":
    config["Peers"] = json.loads(peers_raw)
elif mode == "clear":
    config["Peers"] = []
# mode == "keep": leave the existing Peers list alone.

node_info = config.get("NodeInfo")
if not isinstance(node_info, dict):
    node_info = {}
node_info["name"] = name
config["NodeInfo"] = node_info

with open(path, "w", encoding="utf-8") as handle:
    json.dump(config, handle, indent=2, ensure_ascii=False)
    handle.write("\n")
PY

install -m 0640 -o root -g yggdrasil "$ygg_tmp" "$YGG_CONF"
rm -f "$ygg_tmp"
trap - EXIT
fi

# x-node starts from the same list that Yggdrasil connects to at boot, so a
# fresh install has no window with zero managed peers. The live list is then
# kept in the state directory and may differ from this seed.
SEED_PEERS="$(python3 - "$YGG_CONF" <<'PY'
import json
import sys

with open(sys.argv[1], "r", encoding="utf-8") as handle:
    config = json.load(handle)
print(json.dumps(config.get("Peers") or []))
PY
)"

ADDRESS="$(yggdrasil -useconffile "$YGG_CONF" -address)"
case "$ADDRESS" in
    [23][0-9a-fA-F][0-9a-fA-F]:*) ;;
    *) die "unexpected Yggdrasil address: $ADDRESS" ;;
esac
echo "    address: $ADDRESS"

PUBLIC_KEY="$(yggdrasil -useconffile "$YGG_CONF" -publickey)"
case "$PUBLIC_KEY" in
    *[!0-9a-fA-F]*) die "unexpected public key: $PUBLIC_KEY" ;;
esac
[ "${#PUBLIC_KEY}" -eq 64 ] || die "unexpected public key length: ${#PUBLIC_KEY}"

PEER_URI=""
if [ "$YGG_PORT" -gt 0 ] && [ -n "$PUBLIC_HOST" ]; then
    case "$PUBLIC_HOST" in
        *:*) PEER_HOST="[$PUBLIC_HOST]" ;;
        *)   PEER_HOST="$PUBLIC_HOST" ;;
    esac
    if [ "$YGG_PROTO" = "wss" ]; then
        PEER_URI="wss://${PEER_HOST}:${YGG_PORT}/ygg?key=${PUBLIC_KEY}"
    else
        PEER_URI="tls://${PEER_HOST}:${YGG_PORT}?key=${PUBLIC_KEY}"
    fi
    echo "    peer link: $PEER_URI"
elif [ "$YGG_PORT" -gt 0 ]; then
    warn "no public address: the QR code will not contain a direct peer link."
else
    echo "    incoming peers: disabled (outbound only)"
fi

log "Starting Yggdrasil"
systemctl enable yggdrasil >/dev/null 2>&1 || true
systemctl restart yggdrasil
for _ in $(seq 1 60); do
    if ip -6 addr show 2>/dev/null | grep -qF "$ADDRESS"; then
        break
    fi
    sleep 1
done
if ! ip -6 addr show 2>/dev/null | grep -qF "$ADDRESS"; then
    journalctl -u yggdrasil -n 30 --no-pager 2>/dev/null || true
    die "Yggdrasil did not assign $ADDRESS; check the log above"
fi

if [ "$YGG_PROTO" = "wss" ] && [ "$YGG_PORT" -gt 0 ]; then
    log "Configuring nginx as the TLS front for the WebSocket peering"
    install -d -m 0755 /etc/x-node
    install -d -m 0750 "$TLS_DIR"
    # A neutral page instead of the nginx default: the host should look like a
    # deliberately configured server, not like a fresh nginx installation.
    [ -f "$SCRIPT_DIR/deploy/www/index.html" ] || die "missing $SCRIPT_DIR/deploy/www/index.html"
    install -d -m 0755 "$STUB_DIR"
    install -m 0644 -o root -g root "$SCRIPT_DIR/deploy/www/index.html" "$STUB_DIR/index.html"
    if [ ! -s "$TLS_CERT" ] || [ ! -s "$TLS_KEY" ]; then
        cert_cn="${PUBLIC_HOST:-$(hostname -f 2>/dev/null || hostname)}"
        # Yggdrasil pins the node key and does not verify the CA chain, so a
        # self-signed certificate is enough: the app checks ?key= instead.
        openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 \
            -sha256 -nodes -days 3650 \
            -keyout "$TLS_KEY" -out "$TLS_CERT" \
            -subj "/CN=$cert_cn" >/dev/null 2>&1 \
            || die "could not create the TLS certificate for nginx"
        chmod 0600 "$TLS_KEY"
        chmod 0644 "$TLS_CERT"
        echo "    certificate: $TLS_CERT"
    else
        echo "    certificate: $TLS_CERT (kept)"
    fi
    # The web interface rides on the same TLS port: nginx terminates TLS and
    # forwards to the daemon on loopback, so the password never crosses the
    # network in clear text and no extra port has to be opened.
    WEB_NGINX_LOCATION=""
    if [ "$WEB_ENABLED" -eq 1 ] && [ "$WEB_BIND" = "loopback" ] && [ "$WEB_TLS" = "off" ]; then
        WEB_NGINX_LOCATION="$(cat <<EOF

    # X-Node web interface: proxied to the daemon on 127.0.0.1:${WEB_PORT}.
    # The login page protects it; the random path only keeps scanners away.
    # \$http_host keeps the port, which the daemon compares with Origin.
    location ${WEB_PATH} {
        proxy_pass http://127.0.0.1:${WEB_PORT};
        proxy_http_version 1.1;
        proxy_set_header Host \$http_host;
        proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto \$scheme;
        proxy_read_timeout 60s;
        proxy_send_timeout 60s;
    }
EOF
)"
    fi
    cat > "$NGINX_SITE" <<EOF
# X-Node: TLS termination for the Yggdrasil WebSocket peering.
# Yggdrasil itself listens on ws://127.0.0.1:${YGG_WS_PORT}. Every other path
# serves the neutral stub page, so the host does not look like an unconfigured
# nginx: there is no default "Welcome to nginx!" page here.
server {
    listen ${YGG_PORT} ssl;
    listen [::]:${YGG_PORT} ssl;
    server_name _;

    ssl_certificate     ${TLS_CERT};
    ssl_certificate_key ${TLS_KEY};
    ssl_protocols TLSv1.2 TLSv1.3;

    root  ${STUB_DIR};
    index index.html;
${WEB_NGINX_LOCATION}
    location / {
        try_files \$uri \$uri/ =404;
    }

    location /ygg {
        proxy_pass http://127.0.0.1:${YGG_WS_PORT};
        proxy_http_version 1.1;
        proxy_set_header Upgrade \$http_upgrade;
        proxy_set_header Connection "upgrade";
        proxy_set_header Host \$host;
        proxy_read_timeout 3600s;
        proxy_send_timeout 3600s;
    }
}
EOF
    chmod 0644 "$NGINX_SITE"
    ln -sf "$NGINX_SITE" "$NGINX_LINK"

    # The plain-HTTP face of the host: the same stub page, nothing else. The
    # block is not default_server, so an existing site on port 80 keeps working;
    # only requests for this hostname land here.
    cat > "$NGINX_HTTP_SITE" <<EOF
# X-Node: plain HTTP serves the same neutral stub page and nothing else.
server {
    listen 80;
    listen [::]:80;
    server_name ${PUBLIC_HOST:-_};

    root  ${STUB_DIR};
    index index.html;

    location / {
        try_files \$uri \$uri/ =404;
    }
}
EOF
    chmod 0644 "$NGINX_HTTP_SITE"
    ln -sf "$NGINX_HTTP_SITE" "$NGINX_HTTP_LINK"

    if ! nginx -t; then
        die "nginx rejected $NGINX_SITE; check the configuration above"
    fi
    systemctl enable nginx >/dev/null 2>&1 || true
    if ! systemctl reload nginx 2>/dev/null; then
        systemctl restart nginx || die "could not start nginx"
    fi
    echo "    public endpoint: https://${PUBLIC_HOST:-<host>}:${YGG_PORT}/ygg"
    echo "    stub page:       http(s)://${PUBLIC_HOST:-<host>}/"
fi

log "Creating the service user and the state directory"
if ! getent group "$SERVICE_USER" >/dev/null 2>&1; then
    groupadd --system "$SERVICE_USER"
fi
if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
    useradd --system --gid "$SERVICE_USER" --home-dir "$STATE_DIR" \
        --shell /usr/sbin/nologin "$SERVICE_USER"
fi
install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_USER" "$STATE_DIR"
chown -R "$SERVICE_USER:$SERVICE_USER" "$STATE_DIR"
install -d -m 0750 -o root -g "$SERVICE_USER" /etc/x-node

# Migration from ygg-exit, the old name of the same daemon. The state files are
# compatible, so the allowlist and the invites move over unchanged and the
# registered phones keep working. The old service is stopped first: both
# daemons bind the same SOCKS5 port.
if [ "$MIGRATE" -eq 1 ] && { [ -d "$OLD_STATE_DIR" ] || [ -f "$OLD_CONFIG" ] \
        || systemctl list-unit-files "$OLD_SERVICE.service" 2>/dev/null | grep -q "$OLD_SERVICE"; }; then
    log "Migrating the ygg-exit installation"
    MIGRATED=1
    systemctl disable --now ygg-exit-firewall 2>/dev/null || true
    systemctl disable --now "$OLD_SERVICE" 2>/dev/null || true
    if [ -f "$OLD_CONFIG" ] && [ ! -f "$EXIT_CONFIG" ]; then
        install -m 0640 -o root -g "$SERVICE_USER" "$OLD_CONFIG" "$EXIT_CONFIG"
        echo "    configuration: $OLD_CONFIG -> $EXIT_CONFIG"
    fi
    # The old directory is kept next to the new one: a repeated run must not
    # mistake it for an installation that still has to be migrated.
    if [ -d /etc/ygg-exit ] && [ -f "$EXIT_CONFIG" ] && [ ! -d /etc/ygg-exit.migrated ]; then
        mv /etc/ygg-exit /etc/ygg-exit.migrated
        echo "    backup: /etc/ygg-exit.migrated"
    fi
    if [ -d "$OLD_STATE_DIR" ]; then
        # Only the files the daemon owns; runtime.json is left behind because a
        # stale copy would make the CLI believe a dead process is running.
        for name in allowlist.json invites.json peers.json usage.json quotas.json \
                    settings.json web.json install.json; do
            source_file="$OLD_STATE_DIR/$name"
            [ -f "$source_file" ] || continue
            if [ -f "$STATE_DIR/$name" ]; then
                echo "    state: $name kept (already present)"
                continue
            fi
            cp -a "$source_file" "$STATE_DIR/$name"
            echo "    state: $name copied"
        done
        if [ -d "$OLD_STATE_DIR/tls" ] && [ ! -d "$STATE_DIR/tls" ]; then
            cp -a "$OLD_STATE_DIR/tls" "$STATE_DIR/tls"
            echo "    state: tls/ copied"
        fi
        chown -R "$SERVICE_USER:$SERVICE_USER" "$STATE_DIR"
        # The old directory is kept as a backup instead of being deleted: it is
        # small, and a failed migration must not lose the allowlist.
        if [ ! -d "$OLD_STATE_DIR.migrated" ]; then
            mv "$OLD_STATE_DIR" "$OLD_STATE_DIR.migrated"
            echo "    backup: $OLD_STATE_DIR.migrated"
        fi
    fi
    rm -f /etc/systemd/system/ygg-exit.service \
          /etc/systemd/system/ygg-exit-firewall.service
    rm -f /etc/nginx/sites-enabled/ygg-exit.conf /etc/nginx/sites-available/ygg-exit.conf \
          /etc/nginx/sites-enabled/ygg-exit-http.conf /etc/nginx/sites-available/ygg-exit-http.conf
    rm -rf /var/www/ygg-exit
    # Both sites existed for a moment, so nginx warned about conflicting server
    # names and could have kept the old block. With the old files gone, reload
    # so only the X-Node site is active.
    systemctl reload nginx 2>/dev/null || true
    systemctl daemon-reload 2>/dev/null || true
    # Old scripts and habits keep working: the names point at the new binary,
    # and the daemon still reads YGG_EXIT_CONFIG when X_NODE_CONFIG is unset.
    if [ ! -e "$OLD_BIN" ]; then
        ln -s /usr/local/bin/x-node "$OLD_BIN" 2>/dev/null || true
    fi
    if [ ! -e "$OLD_PROBE" ]; then
        ln -s /usr/local/bin/x-node-peerprobe "$OLD_PROBE" 2>/dev/null || true
    fi
    echo "    ygg-exit is now a symlink to x-node"
fi

log "Writing the exit configuration"
# The day and month of the traffic counters change at local midnight. A wrong
# name here would stop the daemon, so the value is checked before it is used.
SYSTEM_TIMEZONE="UTC"
if [ -r /etc/timezone ]; then
    SYSTEM_TIMEZONE="$(head -n 1 /etc/timezone 2>/dev/null | tr -d '[:space:]')"
elif [ -L /etc/localtime ]; then
    SYSTEM_TIMEZONE="$(readlink -f /etc/localtime 2>/dev/null | sed -n 's|.*/zoneinfo/||p')"
fi
[ -n "$SYSTEM_TIMEZONE" ] || SYSTEM_TIMEZONE="UTC"
if ! python3 -c 'import sys, zoneinfo; zoneinfo.ZoneInfo(sys.argv[1])' "$SYSTEM_TIMEZONE" 2>/dev/null; then
    SYSTEM_TIMEZONE="UTC"
fi
exit_tmp="$(mktemp)"
trap 'rm -f "$exit_tmp"' EXIT
allow_arg="keep"; [ "$ALLOW_ALL_SET" -eq 0 ] || allow_arg="$ALLOW_ALL"
registration_arg="keep"; [ "$REGISTRATION_SET" -eq 0 ] || registration_arg="$REGISTRATION"
# An empty value keeps the peer settings already present in the file; a
# repeated non-interactive run therefore does not reset them.
peer_count_arg=""; [ "$PEER_COUNT_SET" -eq 0 ] || peer_count_arg="$PEER_COUNT"
peer_replace_arg=""; [ "$PEER_AUTO_REPLACE_SET" -eq 0 ] || peer_replace_arg="$PEER_AUTO_REPLACE"
python3 - "$EXIT_CONFIG" "$exit_tmp" "$NODE_NAME" "$ADDRESS" "$SOCKS_PORT" "$API_PORT" \
    "$allow_arg" "$registration_arg" "$PEER_URI" "$peer_count_arg" \
    "$peer_replace_arg" "$SEED_PEERS" "$UPGRADE" "$WEB_ENABLED" "$WEB_PORT" \
    "$WEB_BIND" "$WEB_TLS" "$WEB_PATH" "$IPV6_MODE" "$SYSTEM_TIMEZONE" <<'PY'
import json
import os
import sys

(source, target, name, address, socks, api, allow_all, registration, peer_uri,
 peer_count, peer_replace, seed_peers, upgrade, web_enabled, web_port, web_bind,
 web_tls, web_path, ipv6_mode, timezone) = sys.argv[1:21]
defaults = {
    "inviteTTL": "24h",
    "inviteUses": 1,
    "maxClients": 64,
    "maxConnsPerClient": 64,
    "maxConnsTotal": 512,
    "dialTimeout": "15s",
    "idleTimeout": "5m",
    "blockedPorts": [25],
    "logLevel": "info",
    "peerCount": 3,
    "peerAutoReplace": True,
    "peerDeadTimeout": "90s",
    "peerCheckInterval": "30s",
    "peerRefreshInterval": "6h",
    "peerImprovementPercent": 25,
    "adminSocket": "unix:///var/run/yggdrasil/yggdrasil.sock",
    "peerProbePath": "/usr/local/bin/x-node-peerprobe",
}
config = {}
if os.path.exists(source):
    try:
        with open(source, "r", encoding="utf-8") as handle:
            config = json.load(handle)
    except Exception:
        config = {}
for key, value in defaults.items():
    config.setdefault(key, value)
config["stateDir"] = "/var/lib/x-node"
# An upgrade only tops the file up with the keys a new version added: the
# operator's addresses, ports and peers must survive a binary replacement.
if upgrade != "1":
    config["name"] = name
    config["listenAddress"] = address
    config["socksPort"] = int(socks)
    config["apiPort"] = int(api)
    if peer_count:
        config["peerCount"] = int(peer_count)
    if peer_replace:
        config["peerAutoReplace"] = peer_replace == "1"
    seed = json.loads(seed_peers or "[]")
    if seed:
        config["peers"] = seed
    else:
        config.pop("peers", None)
    if peer_uri:
        config["peerURI"] = peer_uri
    else:
        config.pop("peerURI", None)
    if allow_all != "keep":
        config["allowAll"] = allow_all == "1"
    else:
        config.setdefault("allowAll", False)
    if registration != "keep":
        config["registration"] = registration == "1"
    else:
        config.setdefault("registration", True)

# Outbound IPv6: auto falls back to IPv4 when the host has no global address.
config["ipv6"] = ipv6_mode

# The web interface keeps the settings it already has; only the keys added by a
# new version are filled in. A disabled interface stays disabled.
web = config.get("web")
if not isinstance(web, dict):
    web = {}
if web_enabled == "1" or "enabled" not in web:
    web["enabled"] = web_enabled == "1"
web.setdefault("listen", web_bind)
web.setdefault("port", int(web_port))
web.setdefault("path", web_path)
web.setdefault("tls", web_tls)
web.setdefault("sessionTTL", "12h")
web.setdefault("maxFailedLogins", 5)
web.setdefault("lockout", "15m")
web.setdefault("trustedProxies", ["127.0.0.1", "::1"])
if web_enabled == "1" and upgrade != "1":
    # A fresh install always takes the values from the answers above; on a
    # repeated run they are already in the file and must not drift.
    web["listen"] = web_bind
    web["port"] = int(web_port)
    web["path"] = web_path
    web["tls"] = web_tls
config["web"] = web

# Traffic accounting feeds the quotas and the web dashboard. The quotas start
# unlimited: they are a fair-use tool the operator turns on per client.
usage = config.get("usage")
if not isinstance(usage, dict):
    usage = {}
usage.setdefault("enabled", True)
usage.setdefault("timezone", timezone)
config["usage"] = usage
quotas = config.get("quotas")
if not isinstance(quotas, dict):
    quotas = {}
for key in ("monthlyBytes", "dailyBytes", "downKbps", "upKbps"):
    quotas.setdefault(key, 0)
config["quotas"] = quotas

with open(target, "w", encoding="utf-8") as handle:
    json.dump(config, handle, indent=2, ensure_ascii=False)
    handle.write("\n")
PY
if [ -f "$EXIT_CONFIG" ]; then
    backup="/var/backups/x-node.config.$(date +%Y%m%d-%H%M%S)"
    mkdir -p /var/backups
    cp -a "$EXIT_CONFIG" "$backup"
    echo "    backup: $backup"
fi
install -m 0640 -o root -g "$SERVICE_USER" "$exit_tmp" "$EXIT_CONFIG"

# The configuration file is the source of truth. An upgrade keeps the ports the
# operator already had, and they may differ from the answers above (for example
# apiPort 0, which shares the SOCKS5 listener). The health check below and the
# summary must use the effective values.
if command -v python3 >/dev/null 2>&1; then
    configured_socks="$(config_get "$EXIT_CONFIG" socksPort)"
    configured_api="$(config_get "$EXIT_CONFIG" apiPort)"
    case "$configured_socks" in
        ''|*[!0-9]*) ;;
        *) SOCKS_PORT="$configured_socks" ;;
    esac
    case "$configured_api" in
        ''|*[!0-9]*) ;;
        *) API_PORT="$configured_api" ;;
    esac
fi
API_PORT_EFFECTIVE="$API_PORT"
[ "$API_PORT_EFFECTIVE" -ne 0 ] || API_PORT_EFFECTIVE="$SOCKS_PORT"
rm -f "$exit_tmp"
trap - EXIT

log "Writing the install manifest"
INSTALLED_YGG_VERSION="$(yggdrasil -version 2>/dev/null | awk '/Build version:/ {print $3; exit}')"
# A repeated run sees the Yggdrasil installed by the first run and would
# otherwise record it as pre-existing and lose the original configuration
# backup. Keep the values from the first manifest instead.
if [ -f "$STATE_DIR/install.json" ]; then
    previous_was_installed="$(manifest_get yggdrasilWasInstalled)"
    previous_backup="$(manifest_get yggConfBackup)"
    [ -z "$previous_was_installed" ] || YGGDRASIL_WAS_INSTALLED="$previous_was_installed"
    [ -z "$previous_backup" ] || YGG_CONF_BACKUP="$previous_backup"
fi
python3 - "$STATE_DIR/install.json" "$INSTALLED_YGG_VERSION" "$YGGDRASIL_WAS_INSTALLED" \
    "$YGG_CONF" "$YGG_CONF_BACKUP" "$YGG_PORT" "$YGG_PROTO" "$YGG_WS_PORT" \
    "$SOCKS_PORT" "$API_PORT" "$PEER_URI" <<'PY'
import datetime
import json
import sys

(path, version, was_installed, ygg_conf, ygg_backup,
 listen_port, listen_proto, ws_port, socks_port, api_port, peer_uri) = sys.argv[1:12]
manifest = {
    "version": 2,
    "installedAt": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "yggdrasilVersion": version,
    "yggdrasilWasInstalled": was_installed == "1",
    "yggConf": ygg_conf,
    "yggConfBackup": ygg_backup,
    "listenPort": int(listen_port),
    "listenProto": listen_proto,
    "wsPort": int(ws_port),
    "socksPort": int(socks_port),
    "apiPort": int(api_port),
    "peerURI": peer_uri,
    "serviceUser": "x-node",
    "stateDir": "/var/lib/x-node",
    "configPath": "/etc/x-node/config.json",
}
with open(path, "w", encoding="utf-8") as handle:
    json.dump(manifest, handle, indent=2, ensure_ascii=False)
    handle.write("\n")
PY
chown "$SERVICE_USER:$SERVICE_USER" "$STATE_DIR/install.json"
chmod 0600 "$STATE_DIR/install.json"

# The web password is set before the service starts, so the first log does not
# announce a temporary password nobody will use. The CLI writes web.json in the
# state directory, which belongs to the service user.
WEB_LOGIN=""
if [ "$WEB_ENABLED" -eq 1 ]; then
    log "Setting the web interface password"
    run_as_service() {
        if command -v runuser >/dev/null 2>&1; then
            runuser -u "$SERVICE_USER" -- "$@"
        else
            sudo -u "$SERVICE_USER" "$@"
        fi
    }
    if [ "$WEB_PASSWORD_SET" -eq 1 ]; then
        [ "${#WEB_PASSWORD}" -ge 12 ] || die "the web password must be at least 12 characters"
        run_as_service /usr/local/bin/x-node web password \
            -config "$EXIT_CONFIG" -user "$WEB_USER" -password "$WEB_PASSWORD" >/dev/null \
            || die "could not store the web password"
        WEB_LOGIN="$WEB_USER"
    else
        password_output="$(run_as_service /usr/local/bin/x-node web password \
            -config "$EXIT_CONFIG" -user "$WEB_USER" 2>&1)" \
            || die "could not generate the web password: $password_output"
        WEB_PASSWORD="$(printf '%s\n' "$password_output" | sed -n 's/^New password: //p' | head -n 1)"
        WEB_LOGIN="$(printf '%s\n' "$password_output" | sed -n 's/^Login: //p' | head -n 1)"
        [ -n "$WEB_PASSWORD" ] || die "the web password was not generated; run: x-node web password"
        [ -n "$WEB_LOGIN" ] || WEB_LOGIN="admin"
    fi
    echo "    login: $WEB_LOGIN"
fi

log "Installing systemd units"
install -m 0644 "$SCRIPT_DIR/deploy/systemd/x-node.service" /etc/systemd/system/x-node.service
if [ -f "$SCRIPT_DIR/README.md" ]; then
    install -d -m 0755 /usr/local/share/doc/x-node
    install -m 0644 "$SCRIPT_DIR/README.md" /usr/local/share/doc/x-node/README.md
fi
systemctl daemon-reload

log "Starting x-node"
if ! systemctl enable --now x-node; then
    systemctl status x-node --no-pager 2>/dev/null || true
    journalctl -u x-node -n 40 --no-pager 2>/dev/null || true
    die "x-node failed to start"
fi

log "Waiting for the registration API"
api_ready=0
for _ in $(seq 1 20); do
    if curl --noproxy '*' -fsS --max-time 2 "http://[$ADDRESS]:$API_PORT_EFFECTIVE/v1/health" >/dev/null 2>&1; then
        api_ready=1
        break
    fi
    sleep 1
done
if [ "$api_ready" -eq 0 ]; then
    systemctl status x-node --no-pager 2>/dev/null || true
    journalctl -u x-node -n 40 --no-pager 2>/dev/null || true
    die "the registration API did not answer on [$ADDRESS]:$API_PORT_EFFECTIVE"
fi

peers_up=0
for _ in $(seq 1 20); do
    peers_up="$(yggdrasilctl getPeers 2>/dev/null | grep -c -i 'true' || true)"
    [ "${peers_up:-0}" -gt 0 ] && break
    sleep 1
done
if [ "${peers_up:-0}" -eq 0 ]; then
    warn "no Yggdrasil peer is up yet; the exit will become reachable when one connects."
fi

if [ "$FIREWALL" -eq 1 ]; then
    log "Installing the optional nftables rules"
    install -d -m 0755 /etc/nftables.d
    sed -e "s/^define socks_port = .*/define socks_port = $SOCKS_PORT/" \
        -e "s/^define api_port = .*/define api_port = $API_PORT_EFFECTIVE/" \
        "$SCRIPT_DIR/deploy/nftables/x-node.nft" > /etc/nftables.d/x-node.nft
    chmod 0644 /etc/nftables.d/x-node.nft
    install -m 0644 "$SCRIPT_DIR/deploy/systemd/x-node-firewall.service" \
        /etc/systemd/system/x-node-firewall.service
    systemctl daemon-reload
    if ! systemctl enable --now x-node-firewall; then
        warn "the nftables rules did not load; run: nft -f /etc/nftables.d/x-node.nft"
    fi
fi

WEB_URL=""
if [ "$WEB_ENABLED" -eq 1 ]; then
    WEB_URL="$(web_public_url)"
    # The password is written once, next to the URL, because it cannot be read
    # back from web.json (only the PBKDF2 hash is stored).
    umask 077
    cat > "$CREDENTIALS_FILE" <<EOF
X-Node web interface
====================

URL:      $WEB_URL
Login:    $WEB_LOGIN
Password: $WEB_PASSWORD

The password is stored as a PBKDF2 hash in $STATE_DIR/web.json and cannot be
recovered. Replace it at any time with:

  sudo x-node web password -config $EXIT_CONFIG

The URL path is random but is not a secret: the login page protects the
interface. "x-node web reset-path" generates a new one.
EOF
    chmod 0600 "$CREDENTIALS_FILE"
    umask 022
    echo "    credentials: $CREDENTIALS_FILE (0600)"
fi

echo
log "The exit node is running"
cat <<EOF

  Name:        $NODE_NAME
  Address:     $ADDRESS
  Yggdrasil:   $([ "$YGG_PORT" -gt 0 ] && echo "TCP/$YGG_PORT, $YGG_PROTO" || echo "outbound only")
  Peer link:   ${PEER_URI:-not advertised}
  Public peers: $PEER_COUNT, automatic replacement $([ "$PEER_AUTO_REPLACE" -eq 1 ] && echo "on" || echo "off")
  SOCKS5:      [$ADDRESS]:$SOCKS_PORT
  API:         [$ADDRESS]:$API_PORT_EFFECTIVE$([ "$API_PORT" -eq 0 ] && echo " (shared with SOCKS5, /v1/...)" || echo "")
  IPv6:        $IPV6_MODE
  Web:         $([ "$WEB_ENABLED" -eq 1 ] && echo "$WEB_URL (login $WEB_LOGIN)" || echo "disabled")
  Config:      $EXIT_CONFIG
  State:       $STATE_DIR
  Allowlist:   $(x-node allow list -config "$EXIT_CONFIG" 2>/dev/null | tail -n +2 | wc -l) client(s)

Useful commands:
  x-node invite -config $EXIT_CONFIG
  x-node clients -config $EXIT_CONFIG
  x-node usage -config $EXIT_CONFIG
  x-node quota show -config $EXIT_CONFIG
  x-node allow list -config $EXIT_CONFIG
  x-node allow add 200:... -config $EXIT_CONFIG
  x-node web url -config $EXIT_CONFIG
  x-node web password -config $EXIT_CONFIG
  x-node peers status -config $EXIT_CONFIG
  x-node peers count 5 -config $EXIT_CONFIG
  x-node peers auto off -config $EXIT_CONFIG
  x-node peers refresh -config $EXIT_CONFIG
  x-node doctor -config $EXIT_CONFIG
  systemctl status x-node
  journalctl -u x-node -f
  $SCRIPT_DIR/uninstall.sh --help

EOF

if [ "$MIGRATED" -eq 1 ]; then
    echo "The old ygg-exit service was replaced. Its state directory is kept as"
    echo "$OLD_STATE_DIR.migrated; delete it once the new service works."
    echo
fi

if [ "$WEB_ENABLED" -eq 1 ] && [ "$WEB_TLS" != "off" ]; then
    echo "The web interface uses a self-signed certificate, so the browser warns"
    echo "once. See the README for switching to a real certificate."
    echo
fi

if [ "$YGG_PORT" -gt 0 ]; then
    if [ "$YGG_PROTO" = "wss" ]; then
        echo "Open TCP/$YGG_PORT in the provider firewall: nginx terminates TLS there"
        echo "and forwards the WebSocket to Yggdrasil on 127.0.0.1:$YGG_WS_PORT."
    else
        echo "Open TCP/$YGG_PORT in the provider firewall so the phone can peer directly."
    fi
    echo "The optional nftables table only guards the SOCKS5 and API ports."
    echo
fi

if [ "$MAKE_INVITE" -eq 1 ]; then
    log "Creating the first invite"
    echo
    if command -v runuser >/dev/null 2>&1; then
        runuser -u "$SERVICE_USER" -- /usr/local/bin/x-node invite \
            -config "$EXIT_CONFIG" -note "first device" || \
            warn "could not create the invite; run: x-node invite -config $EXIT_CONFIG"
    elif command -v sudo >/dev/null 2>&1; then
        sudo -u "$SERVICE_USER" /usr/local/bin/x-node invite \
            -config "$EXIT_CONFIG" -note "first device" || \
            warn "could not create the invite; run: x-node invite -config $EXIT_CONFIG"
    else
        /usr/local/bin/x-node invite -config "$EXIT_CONFIG" -note "first device" || \
            warn "could not create the invite; run: x-node invite -config $EXIT_CONFIG"
    fi
    echo
    echo "In the app: Exit node -> + -> scan the QR, then connect the tunnel."
    echo "Registration goes over Yggdrasil, so the tunnel must be up first."
fi
