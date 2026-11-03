# Security policy

## Reporting a vulnerability

Please do not open a public issue for a security problem. Send a private
message to the maintainer through GitHub (`https://github.com/MrKsey`) or open
a draft security advisory in the repository. Include the version, the
configuration and a reproduction if you can.

Expect an answer within a few days. Once a fix is ready, the advisory is
published together with the release notes.

## What X-Node assumes

X-Node is an exit node for the Yggdrasil network. It is reachable only from
Yggdrasil: the SOCKS5 listener, the registration API and the optional web
interface are bound to the Yggdrasil address, to loopback, or to both. Nothing
listens on the clearnet by default.

The threat model is:

* **The Yggdrasil network is untrusted.** Every SOCKS5 request is checked
  against the allowlist (or `allowAll`), the destination policy and the quotas.
  The source address of a connection is its Yggdrasil identity.
* **A client is identified by its Yggdrasil address.** There is no per-request
  password on the SOCKS5 side: the address is derived from the node key, so a
  client cannot forge another client's address.
* **The host is trusted.** The service user `x-node` can read the state
  directory, which contains the allowlist, the traffic counters and the web
  password hash. Root can read everything.
* **The clearnet is untrusted.** The exit resolves names itself, applies the
  policy to every resolved address and refuses loopback, private, link-local
  and Yggdrasil destinations by default (`blockedPorts` adds port filtering).

## Secrets on disk

| File | Content | Mode |
| --- | --- | --- |
| `/etc/x-node/config.json` | configuration, peer URI, no secrets | `0640 root:x-node` |
| `/var/lib/x-node/allowlist.json` | client addresses and API token hashes | `0600 x-node` |
| `/var/lib/x-node/web.json` | PBKDF2 password hash, session key | `0600 x-node` |
| `/var/lib/x-node/usage.json` | traffic counters per client | `0600 x-node` |
| `/root/x-node-credentials.txt` | the web password in clear text | `0600 root` |

The web password is stored only as a PBKDF2-HMAC-SHA256 hash (600 000
iterations, per-installation salt). Sessions are signed cookies with an
HMAC-SHA256 key kept in `web.json`, so a restart does not log everyone out.

## Hardening checklist

* Keep `apiToken` at `required`; `off` lets any Yggdrasil node read the status.
* Keep `registration` on only while you are adding devices.
* Set `allowAll: false` and use invites.
* Serve the web interface over HTTPS (a real certificate in front of it, or
  `web.tls: "self"`), never over plain HTTP on a public address.
* Leave `web.listen` at `loopback` behind nginx, or `yggdrasil` to keep the
  interface inside the mesh.
* Enable the optional nftables rules if the host has a public IPv4 address.
* Watch `x-node usage` and set quotas for clients you do not fully trust.

## Supported versions

The latest release receives fixes. Older releases are not patched; the daemon
is a single static binary, so upgrading is a file replacement plus a restart.
