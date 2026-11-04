# X-Node

An exit node for the [Yggdrasil](https://yggdrasil-network.github.io/) network:
it gives clients a way out to the regular internet, meters their traffic,
hands out access by invite and shows all of it in a web interface.

X-Node is the server half of the pair with the
[Y-Node](https://github.com/MrKsey/y-node) Android app. The app brings up
Yggdrasil on the phone and sends traffic through this node; X-Node also works
with any other SOCKS5 client.

*[Русская версия](README.ru.md)*

## What an exit node is

Yggdrasil is an encrypted network that runs on top of the regular internet.
Every participant gets an IPv6 address, but the network itself does not lead
anywhere outside: it only delivers packets between its own nodes.

An exit node is the server on the border. It accepts traffic from Yggdrasil and
sends it to the open internet on its own behalf. A client cannot forge its
address: the Yggdrasil address is its key, and the same address is the basis of
all access control.

X-Node does three things: accepts SOCKS5 connections over Yggdrasil, checks
what the client is allowed to do and meters its traffic. Everything else —
invites, quotas, peers, the web interface — serves those three jobs.

## Features

- **SOCKS5 for TCP and UDP** — regular sites, DNS and QUIC all work through the
  node.
- **IPv6 and domain names** — the node resolves names itself and can reach
  destinations over IPv4 and IPv6, picking whichever works.
- **Access by invite** — a one-time link with a QR code; registration goes over
  Yggdrasil, so there is nothing to type by hand.
- **Traffic accounting and quotas** — day, month and total per client, with
  volume and speed limits.
- **Web interface** — node health, clients, invites, settings and logs in a
  browser, phones included.
- **Peer supervision** — the node keeps several public peers alive and replaces
  a dead or too slow one without touching connected clients.
- **One-command install** — the installer asks the rest and sets everything up.

## Installation

On a server with Ubuntu 24.04 or Debian 12, as root:

```bash
sudo bash <(curl -fsSL https://raw.githubusercontent.com/MrKsey/x-node/main/install.sh)
```

The installer downloads the latest release, asks a few questions (every one has
a default, so pressing Enter is fine), installs Yggdrasil, configures peering
and the web interface, starts the services and offers to create the first
invite with a QR code.

At the end it prints the web address, the login and the password. The same
details are stored in `/root/x-node-credentials.txt`.

If you prefer the archive, download `x-node-<version>-linux-amd64.tar.gz` from
the [releases](https://github.com/MrKsey/x-node/releases), unpack it and run
`sudo ./install.sh` from the resulting directory.

### Unattended

```bash
sudo ./install.sh --yes --name "Berlin exit" --public-host 203.0.113.10
```

The main flags (`./install.sh --help` lists them all):

| Flag | What it does |
| --- | --- |
| `--name TEXT` | node name in the app and in the QR code |
| `--public-host HOST` | public address of the server for the peer link |
| `--ygg-proto wss\|tls` | peering transport: WebSocket behind nginx (default) or direct TLS |
| `--ygg-port N` | peering port; `0` means outbound connections only |
| `--socks-port N` | SOCKS5 port on the Yggdrasil address (default 1080) |
| `--api-port N` | registration port; `0` shares the SOCKS5 port |
| `--peer-count N` | public peers to keep (default 3) |
| `--no-peer-replace` | keep dead and slow peers instead of replacing them |
| `--allow-all` | let any Yggdrasil node in without an invite |
| `--no-registration` | disable registration |
| `--ipv6 auto\|prefer\|off` | outbound IPv6 policy |
| `--web` / `--no-web` | enable or disable the web interface |
| `--web-port N` | web interface port |
| `--web-password PASS` | set the password instead of generating one |
| `--firewall` | install nftables rules |
| `--upgrade` | replace the binaries, keep settings and clients |
| `--uninstall` | remove the installation |
| `--yes` | ask no questions |

A few flags that are not needed every day:

- `--ygg-version VER` — which Yggdrasil version to install (default 0.5.15).
- `--ygg-deb PATH` — install Yggdrasil from a local `.deb`, for example on a
  server without access to GitHub.
- `--ygg-conf PATH` — use a specific `yggdrasil.conf` instead of auto-detection.
- `--bin-dir DIR` — directory to take `x-node` and `x-node-peerprobe` from when
  they are not next to the installer.

### Upgrade and removal

```bash
sudo ./install.sh --upgrade --yes   # new version, settings and clients kept
sudo ./uninstall.sh                 # remove X-Node, keep the state
sudo ./uninstall.sh --purge         # remove everything, Yggdrasil included
```

## Ports and access

Only the peering port has to be open to the internet: **443** by default for
both `wss` and `tls`. If 443 is already taken, the installer picks 13338 and
says so.

SOCKS5 and the API listen on the Yggdrasil address only — they are not reachable
from the regular internet. The web interface is reachable from outside:

- with `wss` — at `https://<server>/<path>/` on the same 443;
- with `tls` — on its own port (usually 8080), because Yggdrasil holds 443.

`sudo x-node web url` always prints the exact address.

## Web interface

It shows node health and live connections, clients with quotas, invites,
settings and the latest log lines. Clients can be added, renamed and limited
right in the browser.

```bash
sudo x-node web url          # address to sign in
sudo x-node web password     # new password
```

The login and password from the installation are in
`/root/x-node-credentials.txt`.

## Clients

### Y-Node

1. Install the app and connect the tunnel — it shows the Yggdrasil address and
   the number of peers.
2. In the app open **Exit node → +** and scan the QR code printed by
   `sudo x-node invite`.
3. Enable the exit. Traffic now goes through X-Node.

The invite is one-time. If a phone is lost, remove its address:
`sudo x-node allow remove <address>`.

### Linux

```bash
curl --socks5-hostname "[200:1234:5678:9abc:def0:1234:5678:9abc]:1080" https://ifconfig.me
export ALL_PROXY="socks5h://[200:1234:5678:9abc:def0:1234:5678:9abc]:1080"
```

The `socks5h` scheme resolves names on the exit node.

### Windows

Install the Yggdrasil client for Windows, start the service and point any
SOCKS5 client (Nekoray, v2rayN, Proxifier) at `[address]:1080` without
authentication. `curl.exe` can do it directly:

```powershell
curl.exe --socks5-hostname "[200:1234:5678:9abc:def0:1234:5678:9abc]:1080" https://ifconfig.me
```

A machine is added the same way as a phone: with an invite or
`sudo x-node allow add <address>`.

## Quotas and traffic accounting

The node counts upload and download per client for the day, the month and the
total. Counters survive a restart; the day and month change in the
`usage.timezone` time zone.

```bash
sudo x-node usage                                    # usage per client
sudo x-node quota set <address> -monthly 100GiB -down 20Mbps
sudo x-node quota set <address> -daily 5GiB
sudo x-node quota reset <address>                    # drop the limits
sudo x-node quota show                               # limits and usage
```

Zero means "no limit". A quota is checked on a new connection: a client over
the limit is refused, while already open connections keep working. The same
settings live in the web interface on the **Clients** tab.

## Peers

```bash
sudo x-node peers                 # the list and live links
sudo x-node peers count 5         # keep five public peers
sudo x-node peers auto off        # do not replace dead or slow peers
sudo x-node peers refresh         # refresh the public list
```

Clients of the exit node are not counted here and are never replaced.

## Certificate

The installer creates a self-signed certificate, so the browser warns once. For
permanent use, issue a real one:

1. Point the domain's DNS record at the server.
2. Open TCP/80 and TCP/443.
3. Issue the certificate:

   ```bash
   sudo apt-get install -y certbot
   sudo certbot certonly --standalone -d exit.example.com
   ```

4. Put the paths into `/etc/nginx/sites-available/x-node.conf`:

   ```nginx
   ssl_certificate     /etc/letsencrypt/live/exit.example.com/fullchain.pem;
   ssl_certificate_key /etc/letsencrypt/live/exit.example.com/privkey.pem;
   ```

5. Reload nginx: `sudo nginx -t && sudo systemctl reload nginx`.

If the web interface runs without nginx (`tls` mode), set the same files in
`/etc/x-node/config.json` as `web.certFile` and `web.keyFile` and restart the
service. Renewal requires reloading nginx or restarting X-Node again.

## Troubleshooting

```bash
sudo x-node doctor              # installation check with hints
sudo x-node status              # what is active right now
sudo x-node clients             # who is connected
sudo journalctl -u x-node -f    # the log
```

**The app cannot see the exit.** Check that Yggdrasil runs on both sides, the
phone's address is in `x-node allow list` and the SOCKS5 port matches.

**No IPv6.** The node keeps working over IPv4 — that is not an error.
`x-node doctor` prints the reason.

**A client gets "not allowed".** It is not in the allowlist or its quota is
exhausted: `x-node quota show` shows both.

**The browser complains about the certificate.** That is expected with a
self-signed certificate; the section above explains how to install a real one.

## Development

```bash
go build ./...          # daemon and CLI
go test ./...           # tests
go vet ./...
./build.sh              # linux/amd64 into bin/
GOARCH=arm64 ./build.sh # linux/arm64
```

## License

MIT, see [LICENSE](LICENSE).
