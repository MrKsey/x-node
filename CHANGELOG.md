# Changelog

All notable changes to X-Node are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/).

## [1.4.0] — 2026-11-03

The first release under the name X-Node. The project was previously called
`ygg-exit`; the binary, the systemd units, the configuration directory and the
environment variables were renamed, and an existing installation is migrated by
the installer.

### Added

- Built-in web interface served from the same binary: dashboard, clients,
  invites, quotas, settings and logs, under a random URL prefix.
- Web authentication: PBKDF2-HMAC-SHA256 password hashing (600 000 iterations),
  signed session cookies, per-address lockout with a growing delay, and
  `X-Requested-With` enforcement for mutating requests.
- Traffic accounting per client: upload and download for the current day, the
  current month and the running total, persisted in `usage.json`.
- Per-client quotas and speed limits, with defaults in the configuration and
  enforcement on new connections and UDP associations.
- `x-node usage`, `x-node quota show|set|reset` and the quota tab in the web
  interface; sizes and speeds are parsed from human-readable values (`100GiB`,
  `20Mbps`).
- `x-node doctor` gained checks for the IPv6 route, DNS resolution, the web
  listener, the credentials file and the accounting, and warns when the
  configuration file changed after the service started.
- `x-node status` now prints the IPv6 mode, the web URL, the quota defaults,
  the accounting state and the settings overlay.
- IPv6 modes `auto`, `prefer` and `off`, with a five-minute availability probe
  and no restart when the provider changes.
- Happy Eyeballs for outbound connections: `ATYP=domain` is resolved by the
  exit, every result is filtered by the destination policy, and up to six
  candidates are raced with a 250 ms stagger.
- Full `-h`/`--help` coverage: an overview, per-command help with flags,
  defaults and examples, and help that works without a configuration file.
- `--upgrade` mode in the installer: replaces the binaries and the units, keeps
  the configuration, the allowlist, the counters and the web password.
- Installer flags for IPv6, the web interface, its bind target, TLS mode, port,
  path and credentials, and `--uninstall`.
- nginx `location` block for the web interface, with `X-Forwarded-For` and
  `X-Forwarded-Proto`, and `trustedProxies` in the configuration.
- `SECURITY.md` with the threat model, the secrets inventory and a hardening
  checklist.

### Changed

- Renamed `ygg-exit` to `x-node` throughout: binary, unit names, `/etc/x-node`,
  `/var/lib/x-node`, `X_NODE_*` environment variables. `YGG_EXIT_CONFIG` is
  still read when `X_NODE_CONFIG` is unset.
- Renamed `ygg-peerprobe` to `x-node-peerprobe`.
- The installer now reads the web and IPv6 settings of an existing
  configuration before asking, so an upgrade does not reset them.
- The quota and usage commands are no-ops when accounting is disabled instead of
  writing an empty `usage.json`.
- `formatBytes` uses KiB/MiB/GiB/TiB instead of decimal units.

### Fixed

- `x-node quota -h` and the other help paths no longer fail on a machine without
  `/etc/x-node/config.json`.
- `x-node web password` takes effect on the next login: the service reloads
  `web.json` when the file changes, instead of keeping the password from its
  start-up until a restart.
- `TestUDPAssociationEcho` waits for the counters instead of racing them.

### Migration

Existing `ygg-exit` installations are migrated automatically by `install.sh`:
the old service is stopped, the configuration and the state files are copied,
the old state directory is renamed to `/var/lib/ygg-exit.migrated`, and
`/usr/local/bin/ygg-exit` becomes a symlink to `x-node`. Registered clients keep
their addresses and do not need to be re-registered.

[1.4.0]: https://github.com/MrKsey/x-node/releases/tag/v1.4.0
