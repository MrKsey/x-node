# Changelog

All notable changes to X-Node are documented here. The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses [Semantic Versioning](https://semver.org/).

The release workflow publishes the section of the tag as the GitHub release body, so every release has a short human-readable summary of what changed. Keep each bullet on a single line: GitHub turns every newline in a release body into a hard break, so a wrapped bullet would render as a narrow column instead of using the full width.

## [1.6.18] — 2026-11-07

### Changed

- On the **Node** settings card the **Usage timezone** is a normal-width field beside **Node name** and **Log level**, and the traffic accounting switch has its own line.
- On the **UDP relay** card both switches span the full card and their toggles line up vertically; the **Resolve domain names (ATYP=domain)** label stays on one line.
- On the **IPv6 outbound** card the **Mode** select and the read-only **Source address** field sit side by side and have the same width as **Max UDP associations**; the separate **Interface** row is gone.
- On the **Configuration** tab the file picker is as wide as a settings field and the action buttons sit to its right.
- The **Configuration files** and **Recent log lines** cards are as tall as the browser window, so the editor and the log box use the free space on any screen.

## [1.6.17] — 2026-11-07

### Fixed

- The **Project** link in the Yggdrasil card stays on one line on a phone instead of breaking in the middle of the repository name.

## [1.6.16] — 2026-11-07

### Changed

- On a tablet the client and peer cards are packed into two columns, so a wide screen no longer shows one card per row with the other half empty.

## [1.6.15] — 2026-11-07

### Added

- The **Yggdrasil** card on the dashboard has a **Project** row linking to the yggdrasil-go repository.
- The **IPv6 outbound** settings card names the source address and the interface the exit uses for outbound IPv6 connections, or says that the host has no global IPv6 address.

### Changed

- On the **Node** settings card the traffic accounting switch sits next to the usage timezone instead of above it.
- The **Public URL** field of the Web interface card is read-only: the installer fills it, and a wrong value is fixed in the Configuration tab.
- A tablet gets the one-column layout: the paired dashboard cards stack, and the clients and peers tables become cards instead of squeezing nine columns into the viewport.
- Table headers no longer wrap, and a card header keeps its title on the baseline of the status text beside it, so the **Yggdrasil** heading lines up with **X-Node**.

## [1.6.14] — 2026-11-06

### Changed

- The **Yggdrasil peers** table on the dashboard shows the allowlist label next to the address of a connected client, so a phone is recognised by its name and not only by an IPv6 address.
- An inbound link carries a **client** badge in the Managed column, in the accent colour, instead of an empty cell.

## [1.6.13] — 2026-11-06

### Changed

- On the dashboard **System Info** and **Update** now sit directly under the X-Node/Yggdrasil pair, and the **Yggdrasil peers** table moved to the bottom of the page.

## [1.6.12] — 2026-11-06

### Changed

- On a phone narrower than 480 px the client action buttons wrap into three and two per row; on a wider screen they stay on one row.
- The web assets are revalidated on every load, so an upgrade shows the new interface without a hard refresh.

## [1.6.11] — 2026-11-06

### Added

- The dashboard has a **System Info** card left of **Update** with CPU and memory charts for the last 15 minutes. The daemon samples `/proc` every five seconds, so the history survives a page reload and starts empty after a restart.
- **Find peers** now reports how many peers the public catalogue contains, how many were checked and how many answered. The numbers update while the search runs.
- `/api/system` serves the CPU and memory history. On a non-Linux host the card says that the history is unavailable.

### Changed

- **Max UDP associations** and **UDP idle timeout** share one row on a desktop screen; a phone still stacks them.
- A manual peer no longer shows both `auto` and `manual`: the operator's peer is manual, and the automatic count no longer includes it.
- On a phone the action buttons of a client or an invite fill the card width evenly, and the "Actions" label is gone.
- On a phone a table row and its value are aligned on the same baseline instead of the value sitting lower than its label.

### Fixed

- A client suspended by the operator was reported as hitting the connection limit ("The exit refused 2 connection(s) at its limit of 256"), and the warning stayed after Resume. Suspension refusals are counted separately now, and `limitRefused` in the status API excludes both quota and suspension refusals.

## [1.6.10] — 2026-11-06

### Added

- `x-node update check` compares the running build with the latest GitHub release and `sudo x-node update upgrade` installs it: the archive for the machine is downloaded, its SHA-256 is checked against `SHA256SUMS.txt`, the new binary must report the expected version, and the previous executable is kept in `/var/backups`. Both commands accept `-json`.
- The dashboard has an **Update** card with the installed and latest versions, a manual re-check and a one-click upgrade. The upgrade goes through the root helper, so the web interface needs no root of its own.

### Changed

- The **Yggdrasil address** card opens Settings again instead of copying the address.
- **Find peers** shows a spinner while the catalogue probe runs and returns to its normal label when the probe finishes, without a page reload.
- The client details window no longer explains the Refusals counter.

### Fixed

- The peer probe button stayed on "Searching…" when the probe was already running as the page opened. The button now follows the daemon until the probe ends.

## [1.6.9] — 2026-11-06

### Added

- The Clients tab refreshes itself every five seconds while it is open, so a connection that starts or ends no longer needs a page reload or a tab switch.
- Each client row has **Disconnect all**, which closes every TCP connection and UDP association of that client without touching its allowlist entry.
- Each client row has **Suspend / Resume**. A suspended client stays in the allowlist with its counters, quota and token, but the exit closes its open connections and refuses new ones. The state is reported by the status API and shown by Y-Node in the exit card and in the notification.
- The dashboard **Today** and **This month** cards open a floating window with the traffic of every client and its share of the total.
- The Web interface settings have a **Compact** switch: on a desktop screen the interface becomes 25% narrower. The setting is stored on the server and is ignored on a phone, where the layout already fills the screen.

### Changed

- The quota watcher closes open TCP and UDP connections when a client reaches its daily or monthly cap. Previously the quota was checked only when a connection started, so a long download could keep running after the cap.
- The dashboard shows **Yggdrasil peers** between the X-Node/Yggdrasil pair and the Quotas/Web interface pair, which is now at the bottom.
- The **Yggdrasil address** card copies the address instead of opening Settings.
- Settings cards are packed into two independent columns on a desktop, so a short card is followed by the next one instead of a gap left by its taller neighbour. **Blocked ports** is a normal-width field and shares a row with **Dial timeout**.
- Both **Manual Yggdrasil peers** forms use the same width for the field and the button, and the peer probe asks for 25 spare candidates instead of 15.
- The Configuration and Logs text areas follow the browser height.
- The **Load** button is gone from Configuration: choosing a file loads it.
- Settings explains **Refusals** (connection-limit or quota refusals since the exit started) and **Dial timeout** (how long a destination may take to accept a TCP connection).

### Fixed

- The **Resolve domain names (ATYP=domain)** switch was squeezed by the flex layout and rendered as a stretched pill instead of a toggle.
- The web interface password is kept on any reinstall, not only when `install.sh --upgrade` is used, as long as `web.json` already exists.

## [1.6.8] — 2026-11-05

### Fixed

- `install.sh --upgrade` generated a new web interface password on every run and rewrote `/root/x-node-credentials.txt`, although the README promises that an upgrade keeps the password. The password is now kept when `web.json` already exists; the credentials file is only rewritten when a new password was set, either by the first installation or by `--web-password`.

## [1.6.7] — 2026-11-05

### Changed

- The client details open in a floating card next to the icon instead of expanding the table row. The card closes with its ×, with Escape or with a click anywhere outside it, and the icon is a drawn SVG instead of the ⓘ text glyph, which rendered without smoothing.
- Settings shows two cards per row on a wide screen and one per row on a phone.
- The peer probe asks for a dozen spare candidates (at least fifteen), so the manual peer picker offers about ten peers instead of four or five.

### Fixed

- The Yggdrasil card drew two separator lines above **Peer link**: the last table row and the block below it both had a border.
- The **Peer link** value is shown in full again; it wraps inside the card instead of being shortened to `key=…`.

## [1.6.6] — 2026-11-05

### Added

- The manual peer picker fills itself: opening the dashboard or Settings asks the supervisor for a catalogue probe when nothing has been probed yet, so the list is no longer empty until **Find peers** is pressed.

## [1.6.5] — 2026-11-05

### Changed

- The peering link on the dashboard is shown as `wss://host:443/ygg?key=…`, so it fits one line; the full URI stays in the copy button and the tooltip.

## [1.6.4] — 2026-11-05

### Changed

- The dashboard shows the peering link on one line: the 64-digit key is shortened in the middle, while the copy button and the tooltip keep the full URI.

## [1.6.3] — 2026-11-05

### Fixed

- An installation made before the binary was renamed kept `peerProbePath` pointing at `ygg-peerprobe`. The installer removed that file, the daemon fell back to it, and the probed peers arrived without their country and city, so the manual peer picker showed bare URIs. The installer now rewrites the path, removes the old binary, and the daemon falls back to `/usr/local/bin/x-node-peerprobe` when the configured file is missing.

## [1.6.2] — 2026-11-05

### Added

- Every client connection and UDP association in the log carries the client's label next to the Yggdrasil address, including the `UDP association closed` line.
- The web interface has a separate **Configuration** tab between Settings and Logs: it edits `config.json` and `yggdrasil.conf`, validates them, keeps a backup and restarts the services.
- Settings is split into cards with their own **Save** button, which appears only after something on that card has changed.
- The usage timezone is a list instead of a free-text field, and the open mode (`allowAll`) asks for confirmation with a warning.
- Manual Yggdrasil peers can be picked from the probed public candidates, with country, city and latency, and refreshed with **Find peers**.
- The dashboard shows the IPv6 source address, a legend for the traffic chart and a **Clients** tile that opens the Clients page.
- The client list shows live details (UDP, volume, destinations, refusals) in a popover behind the ⓘ icon.

### Changed

- A client row keeps the label next to the address and shows only the device, the app version and the registration time; the rest is in the popover.
- Limits are shown as separate client slots and connections instead of one summary line; **Max clients** moved from Access to Limits.
- UDP relay and IPv6 outbound are separate settings cards.
- Invite defaults (devices allowed and lifetime) live only on the Invites page; Settings no longer duplicates them.
- The QR code panel can be closed, and "Uses" is explained as the number of devices that may register with one invite.
- Quotas and the web interface block share a row on the dashboard.
- Form rows and table cells no longer overflow on phones.
- The README describes the Linux and Windows setup, `--public-host`, `--upgrade`, removal and the files in `/var/lib/x-node`.

### Fixed

- The app version of an already allowlisted client is updated when the app reconnects, instead of staying at the version from the first registration.
- Peer candidates keep the country and city reported by the probe.

## [1.6.1] — 2026-11-04

### Fixed

- The dashboard's restart and configuration buttons did not work: the daemon runs with `NoNewPrivileges=yes`, so the `sudo` rule installed for `x-node helper` was refused. The helper is now a separate root service, `x-node-helper.service`, on a unix socket that only the `x-node` group can open. The installer starts it when the web interface is enabled and the uninstaller removes it.

## [1.6.0] — 2026-11-04

### Added

- Per-client speed limits are one shared token bucket: every TCP connection and every UDP association of a client waits on the same pair, so ten parallel downloads cannot multiply the rate, and QUIC is shaped too.
- `/v1/status` reports the effective quota, the used volume and the split between connection-limit and quota refusals. The Y-Node app shows the limits and says "traffic quota exhausted" instead of blaming the connection limit.
- `x-node invite list` and `x-node invite remove` show and revoke the invites that are still usable. The token is stored next to its hash, so a lost QR code can be shown again.
- The web interface shows the QR code of every invite, copies the link or the token, and creates invites with a note.
- The Settings page can change the node name, the admin password, the public web URL, the configuration files and the manual Yggdrasil peers.
- The dashboard can restart `x-node` and `yggdrasil` and edit `config.json`/`yggdrasil.conf`, through the new internal `x-node helper` command. Every write is validated and backed up.
- Manual peers (`manualPeers`) are pinned: the supervisor keeps them on top of the automatic count and never replaces or trims them.
- The dashboard shows the node's own `wss://…/ygg?key=…` (or `tls://…`) peering link with a copy button and a hint on how to add it to another node.
- The dashboard reports everything `x-node clients` does: device name, app version, UDP datagrams and bytes, destinations and refusals.

### Changed

- Traffic is measured and displayed in decimal units: 1 MB = 1 000 000 bytes, 1 GB = 1 000 000 000 bytes. The `MiB`/`GiB`/`TiB` spellings keep the binary meaning for quotas written with them.
- Speed limits are set and shown in Mbit/s, the unit a speed test reports.
- The traffic chart is drawn in bit/s, is 25% taller and has a tooltip with the exact time and both rates; it also shows how old the data is.
- `x-node status` prints labelled blocks (Node, Yggdrasil, Access, Outbound, Web interface, Quotas, Live, Files) instead of a flat list.
- Every CLI table shows a client as `address (label)`.
- `x-node keygen` explains the private key, the public key and the derived address, and `-json` prints all three.
- `x-node web url` and the dashboard use `web.publicURL`, which the installer fills with the detected external address.
- `x-node -h` ends without an extra blank line.
- The web interface calls the service block "X-Node" instead of "Health" and aligns switches with the fields next to them.

### Fixed

- A client over its traffic quota is reported as a quota refusal, not as a connection-limit problem.
- `yggdrasil.conf` can be read by the web editor: the file belongs to the `yggdrasil` group, so the helper reads it instead of the service user.

## [1.5.2] — 2026-11-04

### Fixed

- `--upgrade` restarts the service, so the new binary actually runs. `systemctl enable --now` left an already running daemon untouched, and the node kept reporting the previous version after an upgrade.

## [1.5.1] — 2026-11-04

### Fixed

- The installer no longer restarts Yggdrasil on `--upgrade`: the configuration is not touched there, and a restart dropped every runtime peer and the connected clients for no reason. A fresh install still restarts it.
- The installer waits longer for the Yggdrasil address after a restart, so a slow start does not abort the installation.
- The `--migrate` and `--no-migrate` flags left over from the old name were removed; they were parsed but did nothing.

## [1.5.0] — 2026-11-04

### Added

- The dashboard chart now shows real traffic history: the daemon samples the counters every five seconds and keeps a day of samples, so the chart survives a page reload and offers 15 minutes, 1 hour, 6 hours and 24 hours.
- The dashboard lists every live Yggdrasil link with its address, direction, round-trip time, uptime and traffic, plus the node's own key, version, routing entries and subnet.
- Clients can be edited in the web interface: label, monthly and daily caps, speed limits and a traffic reset.
- The web interface adapts to phones: the sidebar becomes a drawer and tables turn into readable cards.

### Changed

- Quotas and Clients are one tab: the per-client editor and the default limits live together on **Clients**.
- The dashboard no longer shows "offline" when only the admin socket or a status helper fails; the health pill follows the daemon itself.
- The installer can be piped from `curl`: it downloads the matching release and continues from there, so installing is one command.
- Both peering transports use port 443 by default; the web interface stays reachable from the internet in both modes.
- The web interface is documented as the main management tool; the README was rewritten around installation, ports, clients and quotas.

### Fixed

- Public peers were counted twice: peers already present in `yggdrasil.conf` and the ones added through the admin socket appeared as separate links. The supervisor now adopts existing outbound peers instead of adding another copy, and re-adds managed peers after a Yggdrasil restart.

## [1.4.0] — 2026-11-03

First public release: SOCKS5 over Yggdrasil with TCP and UDP, IPv6 modes and Happy Eyeballs, invites and the allowlist, traffic accounting and quotas, the web interface, peer supervision, `x-node doctor` and the installer.

[1.6.1]: https://github.com/MrKsey/x-node/releases/tag/v1.6.1 [1.6.0]: https://github.com/MrKsey/x-node/releases/tag/v1.6.0 [1.5.2]: https://github.com/MrKsey/x-node/releases/tag/v1.5.2 [1.5.1]: https://github.com/MrKsey/x-node/releases/tag/v1.5.1 [1.5.0]: https://github.com/MrKsey/x-node/releases/tag/v1.5.0 [1.4.0]: https://github.com/MrKsey/x-node/releases/tag/v1.4.0
