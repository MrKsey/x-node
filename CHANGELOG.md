# Changelog

All notable changes to X-Node are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/).

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

[1.5.1]: https://github.com/MrKsey/x-node/releases/tag/v1.5.1
[1.5.0]: https://github.com/MrKsey/x-node/releases/tag/v1.5.0
[1.4.0]: https://github.com/MrKsey/x-node/releases/tag/v1.4.0
