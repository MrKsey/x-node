# X-Node

**English** · [Русский](README.ru.md)

**X-Node** is a server-side exit node for the [Yggdrasil](https://yggdrasil-network.github.io/) network. It gives connected nodes access to the regular Internet over SOCKS5, manages client access, meters traffic and lets you set individual quotas.

X-Node is designed to work with the [Y-Node](https://github.com/MrKsey/y-node) Android client, but is not tied to it: any SOCKS5 client with access to the Yggdrasil network can connect.

<p align="center">
  <img src="docs/screenshots/dashboard-1.png" width="49%" alt="X-Node dashboard">&nbsp;
  <img src="docs/screenshots/dashboard-2.png" width="49%" alt="X-Node traffic statistics">
</p>

## Purpose and how it works

Yggdrasil is a decentralized IPv6 overlay network. It provides connectivity between its own nodes, but does not by itself route traffic to the regular Internet.

An **exit node** acts as a gateway between Yggdrasil and external networks: it accepts client requests and sends them to the Internet on behalf of the server. X-Node provides a SOCKS5 interface for this and applies access rules and traffic limits.

To connect, a client needs a working Yggdrasil node and must be registered in the list of addresses allowed by X-Node. Identification is performed by the node's IPv6 address in the Yggdrasil network.

## Key features

- **SOCKS5 proxy.** Handling of TCP and UDP traffic, including DNS queries and QUIC.
- **IPv4 and IPv6 egress.** Domain name resolution on the server side and selection of an available protocol for the outgoing connection.
- **Access management.** Client registration via one-time invites with a QR code, or manual addition of addresses. Each client can be given a label and its access managed.
- **Quotas and speed limits.** Individual daily and monthly traffic limits, as well as inbound and outbound speed limits.
- **Traffic accounting.** Separate statistics for each client: per day, per month and for all time. Counters survive service restarts.
- **Web interface.** Monitoring of the node, clients, peers and traffic; management of invites, quotas and settings; log viewing and configuration editing.
- **Yggdrasil peer management.** Automatic selection of public peers, availability checks and replacement of unstable nodes. Peers added manually are kept without automatic replacement.
- **CLI.** Management of the server, clients, invites, quotas and peers from the command line.
- **Updates and diagnostics.** New version checks, binary updates and a built-in check of the configuration and installation state.

## Installation

Ubuntu 24.04 and Debian 12 are supported. Installation is performed as `root`:

```bash
sudo bash <(curl -fsSL https://raw.githubusercontent.com/MrKsey/x-node/main/install.sh)
```

The installer downloads the current release, installs Yggdrasil and the X-Node components, configures network parameters and systemd services. During the process you can specify the node name, public address, peering parameters and web interface settings.

After installation you are offered to create the first invite for connecting a client. The web interface address and administrator credentials are printed at the end of the installation and saved to:

```text
/root/x-node-credentials.txt
```

An alternative is to download the archive of the required release from [GitHub Releases](https://github.com/MrKsey/x-node/releases), unpack it and run `install.sh`:

```bash
sudo ./install.sh
```

### Non-interactive installation

```bash
sudo ./install.sh --yes --name "Exit Node" --public-host exit.example.com
```

Main installer options:

| Option | Purpose |
| --- | --- |
| `--name TEXT` | Node name shown in the app and the web interface |
| `--public-host HOST` | Public IP address or domain name for inbound peering |
| `--ygg-proto wss\|tls` | Inbound peering protocol: WebSocket via nginx or direct TLS |
| `--ygg-port N` | Inbound peering port; `0` disables inbound connections |
| `--socks-port N` | SOCKS5 port on the Yggdrasil interface (default `1080`) |
| `--api-port N` | Registration and status API port; `0` shares the SOCKS5 port |
| `--peers LIST\|auto\|keep` | Peer list, automatic selection or keeping the current configuration |
| `--peer-count N` | Number of public peers maintained automatically |
| `--allow-all` | Allow all Yggdrasil nodes to connect without invite checks and the access list |
| `--no-registration` | Disable client registration by invite |
| `--no-invite` | Do not create the first invite automatically |
| `--ipv6 auto\|prefer\|off` | Outbound IPv6 policy |
| `--web` / `--no-web` | Enable or disable the web interface |
| `--web-port N` | Web interface port |
| `--web-password PASS` | Set the administrator password |
| `--firewall` | Install nftables rules |
| `--upgrade` | Upgrade X-Node keeping settings and state |
| `--yes` | Disable interactive questions |

The full list of options is available via `./install.sh --help`.

The `--public-host` option sets the address that other nodes will use to establish an inbound peering connection to the server. If the server is behind NAT, uses a domain name or a non-standard port, the public address should be specified explicitly.

If inbound peers are not needed, use `--ygg-port 0`. In this mode the server only establishes outbound connections to public peers, and clients reach X-Node through the Yggdrasil network itself.

> **Warning:** the `--allow-all` mode disables invite checks and the client allowlist. Any Yggdrasil node will be able to use the server as an exit node. Do not enable this mode on a public server without understanding the consequences.

## Network ports and access

By default the inbound peering port is TCP/443. In `wss` mode peering runs over WebSocket and nginx; the web interface can use the same external HTTPS port. In `tls` mode a direct TLS connection is used, and the web interface is usually available on a separate port.

The SOCKS5 and API ports are bound to the server's Yggdrasil address and are not intended for direct access from the regular Internet. External access to the web interface depends on the chosen publication scheme.

The current panel address can be obtained with:

```bash
sudo x-node web url
```

## Management

X-Node provides a web interface and a CLI. The web panel includes the following sections:

- **Dashboard** — server state, traffic statistics, peers, quotas and the main node parameters.
- **Clients** — the list of registered clients, device details, statistics, access and limit management.
- **Invites** — creating and revoking invites, setting the expiry time and the number of registrations, viewing QR codes.
- **Settings** — node, access, limits, UDP, IPv6, public and manual peers, and web interface parameters.
- **Configuration** — viewing and editing `config.json` and `yggdrasil.conf` with validation before saving.
- **Logs** — viewing the latest log entries.

Administrative operations such as applying configuration and restarting services from the web interface are handled by a separate `x-node-helper` service. It performs privileged actions over a Unix socket without giving the web process full root access.

Main CLI commands:

```bash
sudo x-node status                 # server and component state
sudo x-node clients                # client list and statistics
sudo x-node invite                 # create an invite and QR code
sudo x-node invite list            # list active invites
sudo x-node invite remove <id>     # revoke an invite
sudo x-node keygen                 # create keys and get the Yggdrasil address
sudo x-node doctor                 # installation diagnostics
sudo x-node peers                  # peer state
sudo x-node usage                  # traffic statistics
sudo x-node quota show             # current quotas
sudo x-node web url                # web interface address
sudo x-node web password           # change the web interface password
```

## Connecting clients

### Y-Node for Android

1. Install [Y-Node](https://github.com/MrKsey/y-node) and connect to the Yggdrasil network.
2. Create an invite on the server:

   ```bash
   sudo x-node invite
   ```

3. In Y-Node open the **Exit node** section, add a profile and scan the QR code.
4. Activate the exit node in the app.

An invite is intended for one-time registration. After registration the device is added to the client allowlist.

### Other SOCKS5 clients

X-Node is compatible with SOCKS5 clients on Linux, Windows and other platforms. A Yggdrasil client must be running on the device to provide access to the server's Yggdrasil address.

The SOCKS5 address can be seen in the output of:

```bash
sudo x-node status
```

For an IPv6 address, the proxy settings take the address in square brackets together with the port, for example:

```text
[200:1234:5678:9abc:def0:1234:5678:9abc]:1080
```

To test the connection from Linux:

```bash
curl --socks5-hostname "[200:1234:5678:9abc:def0:1234:5678:9abc]:1080" https://ifconfig.me
```

For applications that support proxy environment variables:

```bash
export ALL_PROXY="socks5h://[200:1234:5678:9abc:def0:1234:5678:9abc]:1080"
```

Use `socks5h` mode or an equivalent remote DNS setting so that domain names are resolved on the exit node side.

## Quotas and statistics

For each client you can set a daily and a monthly traffic limit, as well as download and upload speed limits.

Example commands:

```bash
sudo x-node usage
sudo x-node quota set <address> -monthly 100GB -down 20Mbps
sudo x-node quota set <address> -daily 5GB -up 5Mbps
sudo x-node quota show
sudo x-node quota reset <address>
```

Volume values support decimal units (`MB`, `GB`) and binary ones (`MiB`, `GiB`, `TiB`). Speed is set in `Kbps`, `Mbps` or `Gbps`. A zero value means no limit.

The volume limit is checked when a new TCP connection or UDP association is created. Already established connections are not dropped when the quota is reached. The speed limit applies to the client's traffic as a whole, not to each connection separately.

## Updating and removal

Updating from a directory with the archive of a new version:

```bash
sudo ./install.sh --upgrade --yes
```

The update replaces the X-Node binaries and keeps the configuration, clients, invites, quotas, traffic counters and the peer list.

You can also check for a new version and upgrade via the CLI:

```bash
x-node update check
sudo x-node update upgrade
```

Removing X-Node while keeping the data:

```bash
sudo ./uninstall.sh
```

Full removal, including the data and the installed Yggdrasil package:

```bash
sudo ./uninstall.sh --purge
```

To remove X-Node but keep Yggdrasil:

```bash
sudo ./uninstall.sh --purge --keep-yggdrasil --yes
```

## Diagnostics

For a first check use:

```bash
sudo x-node doctor
sudo x-node status
sudo x-node clients
sudo journalctl -u x-node -f
```

If a client cannot connect, check the availability of the Yggdrasil network, whether its address is in the client allowlist and whether the SOCKS5 port is correct. The reasons for quota and limit refusals are shown in the client statistics.

## Building from source

Go tools are used to build and check the project:

```bash
go build ./...
go test ./...
go vet ./...

./build.sh          # Linux/amd64
GOARCH=arm64 ./build.sh
```

## Donations

You can support the project with a donation to any of the wallets below.

<table>
  <tr>
    <td align="center">
      <img src="docs/screenshots/donate/bitcoin.jpg" width="220" alt="Bitcoin QR code"><br>
      <b>Bitcoin</b><br>
      <code>bc1qdf9wryj6tnyyr7ed4dkfv03nn0dxjf27xshfcr</code>
    </td>
    <td align="center">
      <img src="docs/screenshots/donate/ethereum.jpg" width="220" alt="Ethereum QR code"><br>
      <b>Ethereum</b><br>
      <code>0xE22FAB5337A5506D9e7Ef9D4914De4d963Fbbf88</code>
    </td>
  </tr>
  <tr>
    <td align="center">
      <img src="docs/screenshots/donate/solana.jpg" width="220" alt="Solana QR code"><br>
      <b>Solana</b><br>
      <code>7XcNM2M9rndFbJdjRtyycxkQXq8dPXEP87toiQX4EZfx</code>
    </td>
    <td align="center">
      <img src="docs/screenshots/donate/tron.jpg" width="220" alt="Tron QR code"><br>
      <b>Tron</b><br>
      <code>TCYpTxSmdPN8p4fmRwtPGk3HHBf77FypeA</code>
    </td>
  </tr>
</table>

## License

The project is distributed under the [MIT](LICENSE) license.
