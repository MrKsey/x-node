# Changelog

All notable changes to X-Node are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/).

## [1.6.3] — 2026-11-05

### Fixed

- An installation made before the binary was renamed kept `peerProbePath`
  pointing at `ygg-peerprobe`. The installer removed that file, the daemon fell
  back to it, and the probed peers arrived without their country and city, so
  the manual peer picker showed bare URIs. The installer now rewrites the path,
  removes the old binary, and the daemon falls back to
  `/usr/local/bin/x-node-peerprobe` when the configured file is missing.

## [1.6.2] — 2026-11-05

### Added

- Every client connection and UDP association in the log carries the client's
  label next to the Yggdrasil address, including the `UDP association closed`
  line.
- The web interface has a separate **Configuration** tab between Settings and
  Logs: it edits `config.json` and `yggdrasil.conf`, validates them, keeps a
  backup and restarts the services.
- Settings is split into cards with their own **Save** button, which appears
  only after something on that card has changed.
- The usage timezone is a list instead of a free-text field, and the open mode
  (`allowAll`) asks for confirmation with a warning.
- Manual Yggdrasil peers can be picked from the probed public candidates, with
  country, city and latency, and refreshed with **Find peers**.
- The dashboard shows the IPv6 source address, a legend for the traffic chart
  and a **Clients** tile that opens the Clients page.
- The client list shows live details (UDP, volume, destinations, refusals) in a
  popover behind the ⓘ icon.

### Changed

- A client row keeps the label next to the address and shows only the device,
  the app version and the registration time; the rest is in the popover.
- Limits are shown as separate client slots and connections instead of one
  summary line; **Max clients** moved from Access to Limits.
- UDP relay and IPv6 outbound are separate settings cards.
- Invite defaults (devices allowed and lifetime) live only on the Invites page;
  Settings no longer duplicates them.
- The QR code panel can be closed, and "Uses" is explained as the number of
  devices that may register with one invite.
- Quotas and the web interface block share a row on the dashboard.
- Form rows and table cells no longer overflow on phones.
- The README describes the Linux and Windows setup, `--public-host`,
  `--upgrade`, removal and the files in `/var/lib/x-node`.

### Fixed

- The app version of an already allowlisted client is updated when the app
  reconnects, instead of staying at the version from the first registration.
- Peer candidates keep the country and city reported by the probe.

## [1.6.1] — 2026-11-04

### Fixed

- The dashboard's restart and configuration buttons did not work: the daemon
  runs with `NoNewPrivileges=yes`, so the `sudo` rule installed for
  `x-node helper` was refused. The helper is now a separate root service,
  `x-node-helper.service`, on a unix socket that only the `x-node` group can
  open. The installer starts it when the web interface is enabled and the
  uninstaller removes it.

## [1.6.0] — 2026-11-04

### Added

- Per-client speed limits are one shared token bucket: every TCP connection and
  every UDP association of a client waits on the same pair, so ten parallel
  downloads cannot multiply the rate, and QUIC is shaped too.
- `/v1/status` reports the effective quota, the used volume and the split
  between connection-limit and quota refusals. The Y-Node app shows the limits
  and says "traffic quota exhausted" instead of blaming the connection limit.
- `x-node invite list` and `x-node invite remove` show and revoke the invites
  that are still usable. The token is stored next to its hash, so a lost QR code
  can be shown again.
- The web interface shows the QR code of every invite, copies the link or the
  token, and creates invites with a note.
- The Settings page can change the node name, the admin password, the public web
  URL, the configuration files and the manual Yggdrasil peers.
- The dashboard can restart `x-node` and `yggdrasil` and edit
  `config.json`/`yggdrasil.conf`, through the new internal `x-node helper`
  command. Every write is validated and backed up.
- Manual peers (`manualPeers`) are pinned: the supervisor keeps them on top of
  the automatic count and never replaces or trims them.
- The dashboard shows the node's own `wss://…/ygg?key=…` (or `tls://…`) peering
  link with a copy button and a hint on how to add it to another node.
- The dashboard reports everything `x-node clients` does: device name, app
  version, UDP datagrams and bytes, destinations and refusals.

### Changed

- Traffic is measured and displayed in decimal units: 1 MB = 1 000 000 bytes,
  1 GB = 1 000 000 000 bytes. The `MiB`/`GiB`/`TiB` spellings keep the binary
  meaning for quotas written with them.
- Speed limits are set and shown in Mbit/s, the unit a speed test reports.
- The traffic chart is drawn in bit/s, is 25% taller and has a tooltip with the
  exact time and both rates; it also shows how old the data is.
- `x-node status` prints labelled blocks (Node, Yggdrasil, Access, Outbound, Web
  interface, Quotas, Live, Files) instead of a flat list.
- Every CLI table shows a client as `address (label)`.
- `x-node keygen` explains the private key, the public key and the derived
  address, and `-json` prints all three.
- `x-node web url` and the dashboard use `web.publicURL`, which the installer
  fills with the detected external address.
- `x-node -h` ends without an extra blank line.
- The web interface calls the service block "X-Node" instead of "Health" and
  aligns switches with the fields next to them.

### Fixed

- A client over its traffic quota is reported as a quota refusal, not as a
  connection-limit problem.
- `yggdrasil.conf` can be read by the web editor: the file belongs to the
  `yggdrasil` group, so the helper reads it instead of the service user.

## [1.5.2] — 2026-11-04

### Fixed

- `--upgrade` restarts the service, so the new binary actually runs. `systemctl
  enable --now` left an already running daemon untouched, and the node kept
  reporting the previous version after an upgrade.

## [1.5.1] — 2026-11-04

### Fixed

- The installer no longer restarts Yggdrasil on `--upgrade`: the configuration
  is not touched there, and a restart dropped every runtime peer and the
  connected clients for no reason. A fresh install still restarts it.
- The installer waits longer for the Yggdrasil address after a restart, so a
  slow start does not abort the installation.
- The `--migrate` and `--no-migrate` flags left over from the old name were
  removed; they were parsed but did nothing.

## [1.5.0] — 2026-11-04

### Added

- The dashboard chart now shows real traffic history: the daemon samples the
  counters every five seconds and keeps a day of samples, so the chart survives
  a page reload and offers 15 minutes, 1 hour, 6 hours and 24 hours.
- The dashboard lists every live Yggdrasil link with its address, direction,
  round-trip time, uptime and traffic, plus the node's own key, version,
  routing entries and subnet.
- Clients can be edited in the web interface: label, monthly and daily caps,
  speed limits and a traffic reset.
- The web interface adapts to phones: the sidebar becomes a drawer and tables
  turn into readable cards.

### Changed

- Quotas and Clients are one tab: the per-client editor and the default limits
  live together on **Clients**.
- The dashboard no longer shows "offline" when only the admin socket or a
  status helper fails; the health pill follows the daemon itself.
- The installer can be piped from `curl`: it downloads the matching release and
  continues from there, so installing is one command.
- Both peering transports use port 443 by default; the web interface stays
  reachable from the internet in both modes.
- The web interface is documented as the main management tool; the README was
  rewritten around installation, ports, clients and quotas.

### Fixed

- Public peers were counted twice: peers already present in `yggdrasil.conf`
  and the ones added through the admin socket appeared as separate links.
  The supervisor now adopts existing outbound peers instead of adding another
  copy, and re-adds managed peers after a Yggdrasil restart.

## [1.4.0] — 2026-11-03

First public release: SOCKS5 over Yggdrasil with TCP and UDP, IPv6 modes and
Happy Eyeballs, invites and the allowlist, traffic accounting and quotas, the
web interface, peer supervision, `x-node doctor` and the installer.

[1.6.1]: https://github.com/MrKsey/x-node/releases/tag/v1.6.1
[1.6.0]: https://github.com/MrKsey/x-node/releases/tag/v1.6.0
[1.5.2]: https://github.com/MrKsey/x-node/releases/tag/v1.5.2
[1.5.1]: https://github.com/MrKsey/x-node/releases/tag/v1.5.1
[1.5.0]: https://github.com/MrKsey/x-node/releases/tag/v1.5.0
[1.4.0]: https://github.com/MrKsey/x-node/releases/tag/v1.4.0
