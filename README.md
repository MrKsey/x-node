# X-Node

An exit node for the [Yggdrasil](https://yggdrasil-network.github.io/) network
with a built-in web interface, per-client traffic accounting and quotas.

X-Node is the server half of the Y-Node pair. The Android app
[Y-Node](https://github.com/MrKsey/y-node) joins Yggdrasil and sends the traffic
of the phone through this exit node; X-Node is what runs on the VPS. Both speak
plain SOCKS5 over Yggdrasil, so any SOCKS5 client can use the exit, not only the
app.

*[Русская версия](README.ru.md)*

---

## 1. Overview

### 1.1 What Yggdrasil is

Yggdrasil is an encrypted IPv6 mesh network. Every node has a key pair, and the
public key becomes its address inside `200::/7` — for example
`202:24ae:c219:8a4a:de94:eae2:b97e:ac87`. Nodes find each other through public
peers and form a self-healing overlay: there is no central server, no NAT
traversal to configure and no DNS. Traffic between two addresses is encrypted
end to end by the mesh itself.

For this project the important consequences are:

* A client is identified by its Yggdrasil address, which it cannot forge. That
  address is the whole authentication story on the SOCKS5 side.
* The exit node is reachable from anywhere in the mesh, but not from the
  clearnet unless you deliberately open a port for peering.
* Yggdrasil does not provide a way out to the ordinary internet. That is what an
  exit node is for.

### 1.2 What X-Node does

```
  phone / laptop                    Yggdrasil mesh                VPS
 ┌──────────────┐   encrypted   ┌───────────────────┐   SOCKS5   ┌──────────┐
 │ Y-Node or    │──────────────▶│  X-Node address   │───────────▶│ internet │
 │ any SOCKS5   │  IPv6 overlay │  200:....:ac87    │  clearnet  │          │
 │ client       │◀──────────────│                   │◀───────────│          │
 └──────────────┘               └───────────────────┘            └──────────┘
```

X-Node listens on its Yggdrasil address and:

* accepts SOCKS5 `CONNECT` (TCP) and `UDP ASSOCIATE` (UDP) requests,
* checks the client against the allowlist, the destination policy and its quota,
* resolves domain names itself when the client sends `ATYP=domain`,
* opens the outbound connection over IPv4 or IPv6 (Happy Eyeballs),
* counts the traffic of every client and enforces the quotas,
* serves a small web interface with the dashboard, clients, invites, quotas,
  settings and logs.

It also keeps the public Yggdrasil peers of the host alive: a dead or clearly
slower peer is replaced through the admin socket, without restarting Yggdrasil
and without touching the clients connected to the exit.

### 1.3 Files and ports

| Path | Purpose |
| --- | --- |
| `/usr/local/bin/x-node` | the daemon and the CLI (one binary) |
| `/usr/local/bin/x-node-peerprobe` | measures public peers for the supervisor |
| `/etc/x-node/config.json` | configuration, written by the installer |
| `/var/lib/x-node/` | state: allowlist, invites, peers, usage, quotas, web credentials |
| `/var/lib/x-node/web.json` | web password hash and session key |
| `/var/lib/x-node/usage.json` | traffic counters per client |
| `/root/x-node-credentials.txt` | web URL, login and password (mode `0600`) |

| Port | Default | Reachable from |
| --- | --- | --- |
| SOCKS5 | 1080 | the Yggdrasil address |
| registration API | 1081, or the SOCKS5 port with `apiPort: 0` | the Yggdrasil address |
| peering | 443 (`wss` behind nginx) or 13338 (`tls`) | the clearnet, optional |
| web interface | 8443 on loopback behind nginx, or 443/8080 directly | as configured |

Nothing listens on a public IPv4 address by default.

---

## 2. Installation

### 2.1 Requirements

* Ubuntu 24.04 or Debian 12, root access, systemd.
* A public IPv4 address if you want incoming peering (optional: an exit node
  can work with outbound peers only).
* About 100 MB of disk space and 30 MB of RAM.

### 2.2 Quick start

Download the release archive for your architecture, unpack it and run the
installer:

```bash
tar -xzf x-node-1.4.0-linux-amd64.tar.gz
cd x-node-1.4.0-linux-amd64
sudo ./install.sh
```

The installer asks a handful of questions, each with a default in brackets, and
then:

1. installs the dependencies and Yggdrasil (the `.deb` from the official release),
2. picks working public peers with `x-node-peerprobe`,
3. writes `/etc/yggdrasil/yggdrasil.conf` (a backup is kept in `/var/backups`),
4. configures nginx as the TLS front for the WebSocket peering,
5. creates the `x-node` user and writes `/etc/x-node/config.json`,
6. generates the web password and writes `/root/x-node-credentials.txt`,
7. starts `yggdrasil` and `x-node`, waits for the API and prints the summary.

At the end it offers to create the first invite QR code.

### 2.3 Non-interactive install

```bash
sudo ./install.sh --yes --name "Berlin exit" --public-host 203.0.113.10
```

Useful flags (`./install.sh --help` lists all of them):

| Flag | Meaning |
| --- | --- |
| `--name TEXT` | name shown in the app and in the QR code |
| `--ygg-port N` | peering port, `0` = outbound only |
| `--ygg-proto wss\|tls` | WebSocket behind nginx (default) or a direct TLS listener |
| `--public-host HOST` | public address for the peer link in the QR code |
| `--socks-port N` | SOCKS5 port on the Yggdrasil address |
| `--api-port N` | API port, `0` = share the SOCKS5 port under `/v1/...` |
| `--peers LIST` / `--skip-peers` | explicit peer URIs, or keep the existing list |
| `--peer-count N` | public peers to keep connected (default 3) |
| `--no-peer-replace` | never swap a dead or slow public peer |
| `--allow-all` | every Yggdrasil node may use the exit (no allowlist) |
| `--no-registration` | disable the registration endpoint |
| `--ipv6 auto\|prefer\|off` | outbound IPv6 policy |
| `--web` / `--no-web` | enable or disable the web interface |
| `--web-port N` | web port (default 8443 behind nginx, else 443 or 8080) |
| `--web-bind TARGET` | `all`, `yggdrasil`, `loopback` or an IP address |
| `--web-tls auto\|self\|off` | plain HTTP, a self-signed certificate, or automatic |
| `--web-path PATH` | URL prefix, random by default |
| `--web-password PASS` | explicit password instead of a generated one |
| `--firewall` | install the optional nftables rules |
| `--no-invite` | do not create the first invite |
| `--upgrade` | replace the binaries, keep the configuration and the clients |
| `--no-migrate` | do not migrate an existing ygg-exit installation |
| `--uninstall` | run `uninstall.sh` |
| `--yes` | no questions |

Advanced: `--ygg-version`, `--ygg-deb`, `--ygg-conf`, `--bin-dir`.

### 2.4 What the installer leaves behind

```
/etc/x-node/config.json               the configuration
/etc/x-node/tls/x-node.crt|key        certificate for the peering front
/var/lib/x-node/                      state, owned by x-node:x-node
/root/x-node-credentials.txt          web URL, login, password
/var/backups/yggdrasil.conf.<date>    the Yggdrasil configuration before the change
```

### 2.5 Upgrading

```bash
sudo ./install.sh --upgrade --yes
```

The binaries and the systemd units are replaced, the configuration is only
topped up with the keys a new version added, and the services restart. The
allowlist, the invites, the traffic counters and the web password stay.

A manual upgrade is the same thing without the script:

```bash
sudo install -m 0755 bin/x-node bin/x-node-peerprobe /usr/local/bin/
sudo systemctl restart x-node
```

### 2.6 Uninstalling

```bash
sudo ./uninstall.sh            # keeps the state and Yggdrasil
sudo ./uninstall.sh --purge    # removes everything, including Yggdrasil
```

Without `--purge` the allowlist and the invites stay in `/var/lib/x-node`, so a
reinstall does not require registering every device again.

### 2.7 Migration from ygg-exit

X-Node is the renamed continuation of `ygg-exit`, so an existing installation is
migrated automatically when the installer finds one:

* the old service is stopped and disabled, its units are removed,
* `/etc/ygg-exit/config.json` becomes `/etc/x-node/config.json`,
* `allowlist.json`, `invites.json`, `peers.json`, `usage.json`, `quotas.json`,
  `web.json` are copied into `/var/lib/x-node` (existing files are kept),
* the old state directory is renamed to `/var/lib/ygg-exit.migrated`,
* `/usr/local/bin/ygg-exit` and `ygg-peerprobe` become symlinks to the new
  binaries, and the daemon still reads `YGG_EXIT_CONFIG` when
  `X_NODE_CONFIG` is unset.

Registered phones keep working: their Yggdrasil addresses do not change. Delete
`/var/lib/ygg-exit.migrated` once the new service is verified.

Use `--no-migrate` to leave an old installation alone.

---

## 3. Configuration

The daemon reads `/etc/x-node/config.json` once at start. Most limits are also
in the overlay `/var/lib/x-node/settings.json`, which the web interface and the
CLI write; the daemon re-reads it every two seconds, so those changes apply
without a restart. Listener addresses, ports and the TLS mode come from the file
only and need `systemctl restart x-node`.

`x-node status` prints the configuration in effect, including the overlay.
`x-node doctor` checks the installation and explains what is wrong.

### 3.1 IPv6 and address selection

The exit can reach the internet over IPv4, IPv6 or both.

```json
{ "ipv6": "auto" }
```

| Value | Behaviour |
| --- | --- |
| `auto` (default) | use IPv6 when the host has a global IPv6 address and a route; fall back to IPv4 otherwise |
| `prefer` | always try IPv6 first, then IPv4 |
| `off` | never open an IPv6 connection, even if the host has one |

Availability is probed by asking the kernel for the source address it would use
for a global IPv6 destination; a UDP "connect" sends no packet, so this works
even when the host is offline. The answer is re-checked every five minutes.

When a client sends a domain name (`ATYP=domain`), the exit resolves it, filters
every result by the destination policy and the IPv6 mode, and then races the
candidates with the 250 ms stagger of RFC 8305 (Happy Eyeballs). A host whose
AAAA record is unusable therefore costs a quarter of a second, not a timeout.
At most six addresses are tried per request.

Limitations worth knowing:

* A host without an IPv6 default route cannot reach IPv6-only destinations.
  `x-node doctor` prints `IPv6 is not available (...)` and the exit keeps
  working over IPv4 — this is not an error.
* Yggdrasil is itself IPv6 (`200::/7`), but that is the mesh, not the internet.
  Destinations inside `200::/7` are treated as local and are never sent to the
  clearnet.
* `ipv6: "off"` does not affect loopback, Yggdrasil or IPv4 destinations; it
  only removes global IPv6 addresses from the candidate list.
* If the provider changes IPv6 availability, no restart is needed: `auto`
  follows the probe within five minutes.

### 3.2 Quotas and traffic accounting

The exit counts the traffic of every client: upload (`rx`, from the client into
the exit) and download (`tx`, from the exit back to the client), per day, per
month and in total. The counters live in `/var/lib/x-node/usage.json` and
survive a restart. The day and the month change at local midnight of the
`usage.timezone` zone.

```json
{
  "usage": { "enabled": true, "timezone": "Europe/Moscow" },
  "quotas": {
    "monthlyBytes": 0,
    "dailyBytes": 0,
    "downKbps": 0,
    "upKbps": 0
  }
}
```

The `quotas` block holds the defaults for a client without its own entry. Zero
means "no limit", which is the default: quotas are opt-in.

#### Setting a quota

```bash
# 100 GiB per month and 20 Mbps download for one client
sudo x-node quota set 202:24ae:c219:8a4a:de94:eae2:b97e:ac87 -monthly 100GiB -down 20Mbps

# 5 GiB per day, no speed limit
sudo x-node quota set 202:24ae:c219:8a4a:de94:eae2:b97e:ac87 -daily 5GiB

# remove the quota, the defaults apply again
sudo x-node quota reset 202:24ae:c219:8a4a:de94:eae2:b97e:ac87

# every client
sudo x-node quota reset --all
```

Sizes accept `B`, `KiB`, `MiB`, `GiB`, `TiB` and the short forms `K`, `M`, `G`,
`T`; `KB`/`MB`/`GB` are decimal. Speeds accept `Kbps`, `Mbps` and `Gbps`. The
same limits are on the **Quotas** tab of the web interface.

#### How enforcement works

* The quota is checked when a new TCP connection or UDP association starts. A
  client over its cap is refused with a SOCKS5 "not allowed" reply and a log
  line, so the app shows a clear error instead of a silent hang.
* Connections that are already open are **not** torn down. A quota is a
  fair-use limit, not a kill switch; the client sees the refusal on its next
  connection.
* Speed limits use a token bucket per direction per connection, so a client
  with several connections does not get several times the speed.
* Enforcement needs accounting: with `usage.enabled: false` the counters stay
  at zero and no quota is ever reached. `x-node doctor` says so.

#### Reading the numbers

```bash
sudo x-node usage                    # today, this month and the total per client
sudo x-node usage -since 168h        # only clients seen during the last week
sudo x-node usage -json              # for scripts
sudo x-node clients                  # live connections, refusals and the state
sudo x-node quota show               # limits and the traffic against them
```

`x-node quota show` prints `unlimited` for a client without a cap. `x-node
usage` prints `rx = upload, tx = download` at the bottom, because the two are
easy to mix up.

### 3.3 Web interface

The interface is served by the same binary, under a random path such as
`/k3f9x2m8q1w7z4ab/web`. The path is not a secret — the login page protects the
interface — but it keeps scanners away.

```bash
sudo x-node web url          # the address to open
sudo x-node web password     # a new password, printed once
sudo x-node web reset-path   # a new random path
```

The password is stored as PBKDF2-HMAC-SHA256 with 600 000 iterations and a
per-installation salt. A new password from `x-node web password` is used on the
next login attempt: the service notices the changed `web.json` and reloads it,
so no restart is needed. A session is a signed cookie (`HttpOnly`,
`SameSite=Strict`, `Secure` over HTTPS) that stays valid for `web.sessionTTL`
(12 hours by default) and survives a restart. Five wrong passwords inside
`web.lockout` (15 minutes) block the source address for 15 minutes; every
further block adds another 15 minutes, up to 24 hours. Only the hash is stored,
so a lost password is replaced, never recovered.

What the tabs show:

| Tab | Content |
| --- | --- |
| Dashboard | address, uptime, IPv6 state, live connections, traffic today and this month, peer count |
| Clients | the allowlist with labels, live counters, traffic, quota and a token button |
| Invites | create, list and delete invites; the QR code and the link |
| Quotas | defaults and per-client limits |
| Settings | the hot-reloadable limits, the log level, the web session settings |
| Logs | the last lines of the daemon log |

All mutating requests need the `X-Requested-With: x-node` header, which a
cross-origin form cannot set; together with `SameSite=Strict` that removes the
need for a CSRF token. The interface sends a strict CSP and `X-Frame-Options:
DENY`, and never loads anything from a third party.

#### Behind nginx (default)

With `wss` peering the installer puts the interface on `127.0.0.1:8443` and adds
a `location /<path>` to the existing TLS server block, so the interface is
reachable at `https://<host>/<path>/` on the same port as the peering, with the
same certificate. Nothing extra has to be opened.

```nginx
location /k3f9x2m8q1w7z4ab/web {
    proxy_pass http://127.0.0.1:8443;
    proxy_set_header Host $http_host;              # the daemon compares it with Origin
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

`web.trustedProxies` (loopback by default) makes the daemon believe
`X-Forwarded-For`, so the login lockout counts the real address and not nginx.

#### Directly

Without nginx the installer binds the interface to every address with
`web.tls: "self"`: the daemon creates a self-signed certificate in
`/var/lib/x-node/tls/` on first start and serves HTTPS on 443 (or 8080 when 443
is taken). Browsers warn once per certificate; see 3.4 for the real thing.

### 3.4 TLS certificates: from self-signed to real

A self-signed certificate encrypts the traffic but no browser trusts it, so
every new device shows a warning — and after a habit of clicking through, the
password can be typed into the wrong page. For anything beyond a personal test,
use a real certificate.

The web interface does not have to terminate TLS itself. The recommended setup
is nginx in front, exactly like the peering: one port, one certificate.

#### Option A: Let's Encrypt with nginx

1. Point a DNS `A`/`AAAA` record at the server, for example
   `exit.example.com`.
2. Open TCP/80 and TCP/443 in the provider firewall (port 80 is only needed for
   the challenge).
3. Install certbot and issue the certificate:

   ```bash
   sudo apt-get install -y certbot
   sudo certbot certonly --standalone -d exit.example.com
   ```

   `--standalone` needs port 80 free for a moment; if nginx already uses it, use
   `--webroot -w /var/www/x-node` instead.

4. Replace the certificate paths in `/etc/nginx/sites-available/x-node.conf`:

   ```nginx
   ssl_certificate     /etc/letsencrypt/live/exit.example.com/fullchain.pem;
   ssl_certificate_key /etc/letsencrypt/live/exit.example.com/privkey.pem;
   ```

5. Reload nginx:

   ```bash
   sudo nginx -t && sudo systemctl reload nginx
   ```

6. Renewal is handled by the `certbot.timer` package unit; check it with
   `systemctl list-timers | grep certbot`. After a renewal nginx keeps serving
   the old certificate until it is reloaded, so add a deploy hook:

   ```bash
   echo 'systemctl reload nginx' | sudo tee /etc/letsencrypt/renewal-hooks/deploy/reload-nginx.sh
   sudo chmod +x /etc/letsencrypt/renewal-hooks/deploy/reload-nginx.sh
   ```

7. Make sure the interface is not reachable in plain text: `web.listen` stays
   `loopback` and `web.tls` stays `off` when nginx is in front. `sudo x-node
   status` prints the URL the daemon thinks it serves.

The peering keeps working: Yggdrasil pins the node key and does not verify the
CA chain, so a real certificate changes nothing for the mesh and everything for
the browser.

#### Option B: the daemon serves a real certificate

If the interface is exposed directly, point it at the files instead of letting
it create its own:

```json
{
  "web": {
    "enabled": true,
    "listen": "all",
    "port": 443,
    "tls": "auto",
    "certFile": "/etc/letsencrypt/live/exit.example.com/fullchain.pem",
    "keyFile": "/etc/letsencrypt/live/exit.example.com/privkey.pem"
  }
}
```

```bash
sudo systemctl restart x-node
```

`tls: "auto"` uses HTTPS when both files exist and plain HTTP otherwise, so a
missing certificate is visible in `x-node status` instead of breaking the
service. The files are read at start: after a renewal, restart the daemon — or
use the nginx setup above, which avoids that.

Two practical notes:

* Let's Encrypt cannot issue a certificate for a Yggdrasil address — it is not a
  public DNS name. Use a hostname, or keep the self-signed certificate for the
  mesh-only listener (`web.listen: "yggdrasil"`).
* A certificate in `/etc/letsencrypt/live/...` is a symlink into `archive/` and
  is readable only by root. Either run the daemon as root (not recommended) or
  copy the pair into `/etc/x-node/tls/` with `x-node:x-node` ownership and a
  renewal hook that copies and restarts.

#### Removing the self-signed certificate

Once a real certificate works, delete the old one so a misconfiguration cannot
silently fall back to it:

```bash
sudo rm -f /var/lib/x-node/tls/web.crt /var/lib/x-node/tls/web.key
```

With `tls: "auto"` and no `certFile`, the daemon would then serve plain HTTP —
which is why the nginx setup, where TLS is somebody else's job, is the safer
default.

---

## 4. Clients

### 4.1 Y-Node (Android)

1. Install Y-Node and let it generate or import a node key.
2. Make sure the tunnel is up: the app shows the Yggdrasil address and the peer
   count.
3. **Exit node → + → scan the QR code** printed by `sudo x-node invite`.
4. The app registers over Yggdrasil, receives an API token and shows the exit in
   the list.
5. Turn the exit on. Traffic now leaves through X-Node.

The invite is a one-time secret: only its SHA-256 is stored on the server. If a
phone is lost, remove its address with `sudo x-node allow remove <address>`; its
open connections end at the next limit check.

### 4.2 Linux

Yggdrasil first (the official package, or the one the installer added):

```bash
sudo systemctl status yggdrasil
ip -6 addr show tun0        # or the interface named in yggdrasil.conf
```

Then use the exit as an ordinary SOCKS5 proxy:

```bash
# resolve the name on the exit, which is what the app does
curl --socks5-hostname "[202:24ae:c219:8a4a:de94:eae2:b97e:ac87]:1080" https://ifconfig.me

# or resolve locally and send only the connection through the exit
curl --socks5 "[202:24ae:c219:8a4a:de94:eae2:b97e:ac87]:1080" https://example.com
```

For a whole shell, point the proxy variables at it:

```bash
export ALL_PROXY="socks5h://[202:24ae:c219:8a4a:de94:eae2:b97e:ac87]:1080"
curl https://ifconfig.me
```

`ALL_PROXY` with `socks5h://` resolves names on the exit. Some tools ignore it;
`proxychains-ng` with a `socks5` line in `/etc/proxychains.conf` is the
fallback.

### 4.3 Windows

1. Install the Yggdrasil client for Windows from the official releases and
   start the service. `ipconfig` then shows the `200::/7` address.
2. Point a SOCKS5 client at `[202:...:ac87]:1080`. Browsers do not speak SOCKS5
   with IPv6 literals everywhere, so a small proxy client is easier:
   * **Nekoray / v2rayN**: add a SOCKS5 outbound, host = the Yggdrasil address,
     port 1080, no authentication.
   * **Proxifier**: a SOCKS5 proxy with the same values, then a rule for the
     applications that should use it.
3. `curl.exe` from Windows 10+ works directly:

   ```powershell
   curl.exe --socks5-hostname "[202:24ae:c219:8a4a:de94:eae2:b97e:ac87]:1080" https://ifconfig.me
   ```

The address is the same one the allowlist uses, so a Windows machine is added
exactly like a phone: an invite, or `x-node allow add <address>`.

### 4.4 Any other SOCKS5 client

The exit speaks RFC 1928 with no authentication, `CONNECT` and `UDP ASSOCIATE`,
`ATYP=ipv4`, `ipv6` and `domain`. `BIND` is not implemented (reply `7`,
"command not supported"). UDP fragmentation (`FRAG != 0`) is not reassembled;
the client must let the transport fragment.

---

## 5. Operations

### 5.1 Command line

```
x-node <command> [flags]
```

| Command | Purpose |
| --- | --- |
| `serve` | run the daemon (systemd starts this) |
| `invite` | create an invite: QR code, link and token |
| `allow` | `list`, `add`, `remove`, `token` — the allowlist and API tokens |
| `clients` | registered clients, live connections, refusals |
| `usage` | traffic per client, day, month and total |
| `quota` | `show`, `set`, `reset` |
| `peers` | `status`, `count`, `auto`, `refresh` — the public peers |
| `status` | the configuration in effect and the live counters |
| `doctor` | check the installation, with a hint per failure |
| `web` | `url`, `password`, `reset-path` |
| `keygen` | an Ed25519 key pair and its Yggdrasil address |
| `version` | the version, or `-json` with the build details |
| `help` | the overview, or `x-node help <command>` |

Every command accepts `-config PATH` and `-h`. The exit code is non-zero on
failure, so the commands are usable from monitoring.

### 5.2 Daily checks

```bash
sudo x-node doctor                 # is everything where it should be?
sudo x-node status                 # what is in effect right now?
sudo x-node clients                # who is connected?
sudo journalctl -u x-node -f       # what is happening?
```

`doctor` exits non-zero when a check fails, which makes it a cheap Nagios/Zabbix
probe. It checks the configuration, the Yggdrasil address, the state directory,
the allowlist, the listening ports, the admin socket, the public peers, the API,
the IPv6 route, DNS resolution, the web interface and the accounting.

### 5.3 Peers

```bash
sudo x-node peers                  # managed list and live state
sudo x-node peers count 5          # keep five public peers
sudo x-node peers auto off         # stop replacing dead or slow peers
sudo x-node peers refresh          # probe the public list now
```

The supervisor talks to Yggdrasil over the admin socket, so nothing restarts.
Inbound peers — the clients that dial this exit — are never counted, replaced
or dropped by it.

### 5.4 Logs

`journalctl -u x-node` is the daemon log; the web interface keeps a ring buffer
in memory and shows it on the **Logs** tab. `logLevel` is `info` by default;
`debug` adds one line per connection and is meant for troubleshooting.

### 5.5 Adding X-Node as a peer

Yggdrasil already routes to the exit through the mesh, so a client does not have
to peer with it directly. A direct link helps when the path through the public
peers is long: it shortens the route and makes the connection independent of the
public peer list.

When `peerURI` is configured, `x-node status` and `x-node invite` print the
peering address of this node, for example
`wss://exit.example.com:443/ygg?key=<public key>`:

```bash
sudo x-node status | grep -i peer
```

On the other node add it to `Peers` in `yggdrasil.conf`:

```json
{
  "Peers": [
    "wss://exit.example.com:443/ygg?key=..."
  ]
}
```

and restart Yggdrasil. Through the admin socket the same is done without a
restart:

```bash
sudo yggdrasilctl addpeer uri='wss://exit.example.com:443/ygg?key=...'
```

The key in the URI pins the identity of the exit node: a peer that presents a
different key is rejected, so DNS or a hostile network cannot redirect the link
to an impostor. Remove the entry to go back to routing over the public peers.

---

## 6. Security

* The allowlist is the access control: with `allowAll: false` only registered
  addresses are served. An invite stores only the SHA-256 of its token.
* The API is authenticated with a bearer token per client (`apiToken:
  "required"`), rate-limited per source (`apiRateLimit`).
* The web interface: PBKDF2 password, signed cookie, per-address lockout,
  `X-Requested-With` for mutations, CSP, `X-Frame-Options: DENY`.
* The exit refuses loopback, private, link-local and Yggdrasil destinations, and
  port 25 by default (`blockedPorts`). The policy is applied to every address a
  name resolves to, not only to the first one.
* The daemon runs as the unprivileged user `x-node`; only the configuration
  directory and the state directory are writable by it.

See [SECURITY.md](SECURITY.md) for the threat model, the file modes and the
hardening checklist.

---

## 7. Troubleshooting

**The app says the exit is unreachable.** Check that Yggdrasil is up on both
sides (`x-node doctor`, peer count in the app), that the phone's address is in
the allowlist (`x-node allow list`), and that the SOCKS5 port matches.

**`FAIL Yggdrasil address is assigned: is yggdrasil running and is the interface
up?`** Yggdrasil is not running or the interface is down:
`systemctl status yggdrasil`, `journalctl -u yggdrasil -n 50`.

**No IPv6.** That is not an error: the exit falls back to IPv4. `doctor` prints
the reason. Use `ipv6: "prefer"` only when the host really has a route.

**A client is refused with "not allowed".** Either it is not in the allowlist,
or its quota is exhausted: `x-node quota show` prints the caps and the usage.

**The web interface does not open.** `x-node status` shows the URL and the bind
address; behind nginx check `nginx -t`, the `location` block and the port in
`/etc/x-node/config.json`. `x-node doctor` dials the listener and reports it.

**The browser warns about the certificate.** Expected with `tls: "self"`. See
3.4 for a real certificate.

**nginx and Yggdrasil both want port 443.** With `wss` nginx owns the port and
Yggdrasil listens on `ws://127.0.0.1:<wsPort>`; the installer picks a free
`wsPort`. With `tls` Yggdrasil owns the port, so the web interface moves to 443
only if it is free, otherwise 8080.

---

## 8. Development

```bash
go build ./...          # the daemon and the CLI
go test ./...           # the test suite
go vet ./...
./build.sh              # linux/amd64 into bin/
GOARCH=arm64 ./build.sh # linux/arm64
```

The code is one package in `cmd/x-node` plus the embedded assets in
`internal/webui`. The tests cover the SOCKS5 protocol, the store, the API, the
peer supervisor, the web interface, the accounting and the quota parser.

CI runs `go vet`, `go test -race`, `shellcheck` and builds `linux/amd64`,
`linux/arm64` and `linux/armv7` on every push; a tag builds the release archives
with `SHA256SUMS.txt`.

## 9. License

MIT, see [LICENSE](LICENSE).
