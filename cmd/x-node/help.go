package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// The help text is data, not a wall of Printf calls: the same table feeds the
// short overview, `x-node help <command>` and `x-node <command> -h`, so the
// three can never drift apart.

type flagHelp struct {
	Flag    string
	Default string
	Env     string
	Text    string
}

type argHelp struct {
	Name string
	Text string
}

type commandHelp struct {
	Name     string
	Summary  string
	Usage    []string
	Details  string
	Args     []argHelp
	Flags    []flagHelp
	Examples []string
	SeeAlso  []string
}

var commandOrder = []string{
	"serve", "invite", "allow", "clients", "usage", "quota", "peers",
	"status", "doctor", "update", "web", "dns", "keygen", "version", "help",
}

var commands = map[string]commandHelp{
	"serve": {
		Name:    "serve",
		Summary: "Run the exit node (this is what systemd starts)",
		Usage:   []string{"x-node serve [-config PATH]"},
		Details: `Starts the SOCKS5 listener, the registration API, the peer supervisor
and, when enabled, the web interface. The process stays in the foreground and
logs to stderr; systemd captures it.

Nothing is exposed to the clearnet by default: the SOCKS5 and API sockets are
bound to the Yggdrasil address of this node.`,
		Flags: []flagHelp{
			{Flag: "-config PATH", Default: "/etc/x-node/config.json", Env: "X_NODE_CONFIG", Text: "configuration file to read"},
		},
		Examples: []string{"x-node serve", "x-node serve -config ./config.json"},
		SeeAlso:  []string{"status", "doctor"},
	},
	"invite": {
		Name:    "invite",
		Summary: "Create, list and remove invitations (QR code and link)",
		Usage: []string{
			"x-node invite [-config PATH] [-uses N] [-ttl 24h] [-note TEXT] [-json]",
			"x-node invite list [-config PATH] [-json]",
			"x-node invite remove <id> [-config PATH]",
		},
		Details: `An invite is a one-time secret that lets a Y-Node client add this exit.
The command prints a QR code, a yggnode:// link and the token itself. The link
can be shown again with "x-node invite list": the token is stored in the state
directory, which only the service user and root can read. The hash is what the
registration endpoint compares, so editing the file by hand cannot forge an
invite without knowing the token.

The -note is a free-form label ("family phone") that is shown in "invite list"
and in the web interface, so several open invites can be told apart.

The client must already be connected to Yggdrasil: registration goes over the
tunnel, not over the clearnet.`,
		Flags: []flagHelp{
			{Flag: "-config PATH", Default: "/etc/x-node/config.json", Env: "X_NODE_CONFIG", Text: "configuration file to read"},
			{Flag: "-uses N", Default: "inviteUses from the config (1)", Text: "how many clients may use this invite"},
			{Flag: "-ttl 24h", Default: "inviteTTL from the config (24h)", Text: "how long the invite stays valid"},
			{Flag: "-note TEXT", Default: "", Text: "free-form note shown in the invite list and the client list"},
			{Flag: "-json", Default: "off", Text: "print JSON instead of the QR code, for scripts"},
		},
		Examples: []string{
			"x-node invite",
			`x-node invite -uses 3 -ttl 48h -note "family phones"`,
			"x-node invite list",
			"x-node invite list -json",
			"x-node invite remove a1b2c3d4",
			"x-node invite -json | jq -r .link",
		},
		SeeAlso: []string{"allow", "clients"},
	},
	"allow": {
		Name:    "allow",
		Summary: "Manage the client allowlist and API tokens",
		Usage: []string{
			"x-node allow list [-config PATH]",
			"x-node allow add <address> [-label TEXT] [-config PATH]",
			"x-node allow remove <address> [-config PATH]",
			"x-node allow token <address> [-config PATH]",
			"x-node allow link <address> [-config PATH] [-json]",
		},
		Details: `The allowlist is the set of Yggdrasil addresses that may use this exit when
allowAll is false. An address can get in through an invite (the normal path) or
be added by hand.

"allow token" issues a new API token for an address that is already allowed and
invalidates the previous one. The token is what the client sends to /v1/status
and /v1/token; it is printed once and only its hash is stored.

"allow link" prints the QR code and the yggnode:// link that adds this exit to
a client that is already allowed. The link has no invite: the exit recognises
the device by its Yggdrasil address, and the app fetches an API token from
/v1/token on its own. Use it when the exit profile was deleted in the app.`,
		Args: []argHelp{
			{Name: "address", Text: "Yggdrasil node address, e.g. 201:1ba8:15c:b1be:7555:b3d7:7651:8e2e"},
		},
		Flags: []flagHelp{
			{Flag: "-config PATH", Default: "/etc/x-node/config.json", Env: "X_NODE_CONFIG", Text: "configuration file to read"},
			{Flag: "-label TEXT", Default: "", Text: "human-readable name for the client (add only)"},
			{Flag: "-json", Default: "off", Text: "machine-readable output (link only)"},
		},
		Examples: []string{
			"x-node allow list",
			"x-node allow add 201:1ba8:15c:b1be:7555:b3d7:7651:8e2e -label laptop",
			"x-node allow link 201:1ba8:15c:b1be:7555:b3d7:7651:8e2e",
			"x-node allow link 201:1ba8:15c:b1be:7555:b3d7:7651:8e2e -json | jq -r .link",
			"x-node allow token 201:1ba8:15c:b1be:7555:b3d7:7651:8e2e",
			"x-node allow remove 201:1ba8:15c:b1be:7555:b3d7:7651:8e2e",
		},
		SeeAlso: []string{"invite", "clients"},
	},
	"clients": {
		Name:    "clients",
		Summary: "Show registered clients, live connections and traffic",
		Usage:   []string{"x-node clients [-config PATH] [-json]"},
		Details: `Merges the allowlist with the live counters the service publishes in
runtime.json: open TCP connections, UDP associations, refusals caused by the
limits and the traffic of the current period.

When the service is not running only the allowlist is shown.

"x-node allow link <address>" prints the QR code and the link that adds this
exit to a client that is already in the allowlist, so a deleted exit profile
can be restored without a new invite.`,
		Flags: []flagHelp{
			{Flag: "-config PATH", Default: "/etc/x-node/config.json", Env: "X_NODE_CONFIG", Text: "configuration file to read"},
			{Flag: "-json", Default: "off", Text: "machine-readable output"},
		},
		Examples: []string{"x-node clients", "x-node clients -json"},
		SeeAlso:  []string{"usage", "quota", "allow"},
	},
	"usage": {
		Name:    "usage",
		Summary: "Show traffic accounting per client",
		Usage:   []string{"x-node usage [-config PATH] [-since 24h] [-json]"},
		Details: `Prints the traffic the exit carried for every client: the current day, the
current month and the running totals. The counters live in usage.json in the
state directory and survive a restart.

"rx" is traffic from the client into the exit (upload), "tx" is traffic from the
exit back to the client (download).`,
		Flags: []flagHelp{
			{Flag: "-config PATH", Default: "/etc/x-node/config.json", Env: "X_NODE_CONFIG", Text: "configuration file to read"},
			{Flag: "-since 24h", Default: "current period", Text: "only show clients seen within this window"},
			{Flag: "-json", Default: "off", Text: "machine-readable output"},
		},
		Examples: []string{"x-node usage", "x-node usage -since 168h", "x-node usage -json"},
		SeeAlso:  []string{"quota", "clients"},
	},
	"quota": {
		Name:    "quota",
		Summary: "Show, set and reset per-client traffic quotas",
		Usage: []string{
			"x-node quota show [address] [-config PATH] [-json]",
			"x-node quota set <address> [-monthly 100GB] [-daily 5GB] [-down 10Mbps] [-up 5Mbps] [-config PATH]",
			"x-node quota reset <address>|--all [-config PATH]",
		},
		Details: `A quota caps the traffic of one client and optionally throttles its speed.
Limits are enforced when a new connection or UDP association starts: once the
cap is reached the exit refuses new requests with a SOCKS5 "not allowed" reply
and writes a line to the log. Connections that are already open are left alone.

Traffic caps are stored as bytes and can be written in MB (10^6), MiB (2^20),
GB, GiB and so on. The web interface uses megabytes, so a cap can be set with
the precision of one megabyte. Speeds are kilobits per second internally and
can be written as Kbps, Mbps or Gbps; they are shown in Mbit/s, the unit a
speed test reports. Zero means "no limit". A client without its own entry uses
the defaults from the "quotas" section of the configuration.

The speed limit is shared by every TCP connection and every UDP association of
the client, so ten parallel downloads cannot multiply it.`,
		Args: []argHelp{
			{Name: "address", Text: "Yggdrasil node address of an allowlisted client"},
		},
		Flags: []flagHelp{
			{Flag: "-config PATH", Default: "/etc/x-node/config.json", Env: "X_NODE_CONFIG", Text: "configuration file to read"},
			{Flag: "-monthly SIZE", Default: "unchanged", Text: "monthly traffic cap (100GB, 500MB), 0 removes it"},
			{Flag: "-daily SIZE", Default: "unchanged", Text: "daily traffic cap (5GB, 500MB), 0 removes it"},
			{Flag: "-down SPEED", Default: "unchanged", Text: "download throttle in Mbit/s (20Mbps), 0 removes it"},
			{Flag: "-up SPEED", Default: "unchanged", Text: "upload throttle in Mbit/s (5Mbps), 0 removes it"},
			{Flag: "-json", Default: "off", Text: "machine-readable output (show)"},
			{Flag: "--all", Default: "off", Text: "reset every client (reset)"},
		},
		Examples: []string{
			"x-node quota show",
			"x-node quota set 201:1ba8:15c:b1be:7555:b3d7:7651:8e2e -monthly 100GB -down 20Mbps",
			"x-node quota reset 201:1ba8:15c:b1be:7555:b3d7:7651:8e2e",
		},
		SeeAlso: []string{"usage", "clients"},
	},
	"peers": {
		Name:    "peers",
		Summary: "Inspect and tune the public Yggdrasil peers",
		Usage: []string{
			"x-node peers [status] [-config PATH] [-json]",
			"x-node peers count N [-config PATH]",
			"x-node peers auto on|off [-config PATH]",
			"x-node peers refresh [-config PATH]",
		},
		Details: `The supervisor keeps PeerCount public peers connected and, when
peerAutoReplace is on, swaps a dead or clearly slower one for a better candidate
from the public list. It talks to Yggdrasil over the admin socket, so the node
never restarts.

Inbound peers — the clients that connect to this exit — are never counted,
replaced or dropped by the supervisor.

Peers added by hand (the "manualPeers" list in the configuration, or the Manual
peers editor in the web interface) are always kept and never replaced by the
supervisor. The web interface shows them with a "manual" badge.`,
		Flags: []flagHelp{
			{Flag: "-config PATH", Default: "/etc/x-node/config.json", Env: "X_NODE_CONFIG", Text: "configuration file to read"},
			{Flag: "-json", Default: "off", Text: "machine-readable output (status)"},
		},
		Examples: []string{
			"x-node peers",
			"x-node peers count 5",
			"x-node peers auto off",
			"x-node peers refresh",
		},
		SeeAlso: []string{"status", "doctor"},
	},
	"status": {
		Name:    "status",
		Summary: "Print the effective configuration and live counters",
		Usage:   []string{"x-node status [-config PATH]"},
		Details: `Shows what the daemon would use right now, grouped into Node, Yggdrasil,
Access, Outbound, Web interface, Quotas, Live and Files. Every line has a
human-readable label: "SOCKS5 port: 1080" instead of a bare number.

The command reads the configuration and the state directory directly, so it
also works while the service is stopped. Live counters (connections, UDP
associations, refusals, traffic) come from runtime.json and usage.json and say
"service is not running" when the daemon is down.`,
		Flags: []flagHelp{
			{Flag: "-config PATH", Default: "/etc/x-node/config.json", Env: "X_NODE_CONFIG", Text: "configuration file to read"},
		},
		Examples: []string{"x-node status"},
		SeeAlso:  []string{"doctor", "clients", "usage"},
	},
	"doctor": {
		Name:    "doctor",
		Summary: "Check the installation and diagnose common problems",
		Usage:   []string{"x-node doctor [-config PATH]"},
		Details: `Runs a series of checks: the configuration, the Yggdrasil address on the
interface, the state directory, the allowlist, the listening ports, the admin
socket, the public peers, the IPv6 route used for outbound connections and the
web interface.

Every failure is printed with a hint. The exit code is non-zero when a check
fails, so the command is usable from monitoring.`,
		Flags: []flagHelp{
			{Flag: "-config PATH", Default: "/etc/x-node/config.json", Env: "X_NODE_CONFIG", Text: "configuration file to read"},
		},
		Examples: []string{"x-node doctor", "sudo x-node doctor -config /etc/x-node/config.json"},
		SeeAlso:  []string{"status", "peers"},
	},
	"update": {
		Name:    "update",
		Summary: "Check GitHub releases and upgrade the installed binaries",
		Usage: []string{
			"x-node update check [-json]",
			"x-node update upgrade [-json] [-no-restart]",
		},
		Details: `"update check" asks GitHub for the latest release and compares it with the
running build. "update upgrade" downloads the archive that matches the machine,
verifies the published SHA-256, checks that the new binary reports the expected
version and replaces /usr/local/bin/x-node and x-node-peerprobe. The previous
binary is kept in /var/backups. The configuration and the state directory are
not touched.

The upgrade needs root because it writes the installed binaries. The web
interface uses the same code through the root helper, so an operator can upgrade
from the dashboard without a shell.`,
		Flags: []flagHelp{
			{Flag: "-json", Default: "off", Text: "print JSON instead of text"},
			{Flag: "-no-restart", Default: "off", Text: "replace the binaries without restarting the services (upgrade only)"},
		},
		Examples: []string{
			"x-node update check",
			"x-node update check -json",
			"sudo x-node update upgrade",
		},
		SeeAlso: []string{"version", "status"},
	},
	"web": {
		Name:    "web",
		Summary: "Show the web interface URL and manage its password",
		Usage: []string{
			"x-node web url [-config PATH]",
			"x-node web password [-config PATH] [-user NAME] [-password VALUE]",
			"x-node web reset-path [-config PATH]",
		},
		Details: `"web url" prints the address to open in a browser, including the random
path from the configuration. When "web.publicURL" is set (the installer fills it
in with the detected external address), that address is printed instead of the
local listen address, so the output can be pasted into a browser from anywhere.

"web password" without -password generates a new password (20 characters,
letters, digits and punctuation) and prints it once. With -password it sets the
value given on the command line, which is useful for automation but leaves the
password in the shell history. The running service reloads the file on the next
login attempt, so no restart is needed.

"web reset-path" replaces the URL prefix with a fresh random value.`,
		Flags: []flagHelp{
			{Flag: "-config PATH", Default: "/etc/x-node/config.json", Env: "X_NODE_CONFIG", Text: "configuration file to read"},
			{Flag: "-user NAME", Default: "admin", Text: "login name (password)"},
			{Flag: "-password VALUE", Default: "generate", Text: "explicit password instead of a generated one"},
		},
		Examples: []string{
			"x-node web url",
			"sudo x-node web password",
			"sudo x-node web reset-path",
		},
		SeeAlso: []string{"status"},
	},
	"dns": {
		Name:    "dns",
		Summary: "Turn the Alfis DNS server for clients on or off",
		Usage: []string{
			"x-node dns status [-config PATH]",
			"sudo x-node dns on [-config PATH]",
			"sudo x-node dns off [-config PATH]",
		},
		Details: `The DNS server is Alfis running as "x-node-dns.service". It listens on
the Yggdrasil address of this exit, so a client that already reaches the exit
can use it for names in the Yggdrasil network. The same switch is on the
dashboard and in Settings → DNS server; the web UI opens the Alfis interface in
a new tab.

"on" generates the Alfis web password when it is empty, writes the effective
settings and starts the unit. "off" stops and disables the unit but keeps the
configuration, so turning it back on does not lose the password.`,
		Flags: []flagHelp{
			{Flag: "-config PATH", Default: "/etc/x-node/config.json", Env: "X_NODE_CONFIG", Text: "configuration file to read"},
		},
		Examples: []string{
			"x-node dns status",
			"sudo x-node dns on",
			"sudo x-node dns off",
		},
		SeeAlso: []string{"status", "web"},
	},
	"keygen": {
		Name:    "keygen",
		Summary: "Generate an Ed25519 key pair and its Yggdrasil address",
		Usage:   []string{"x-node keygen [-json]"},
		Details: `Generates a new Ed25519 key pair and prints three values:

  privateKey  the 64-byte secret key as hex. Put it in the "PrivateKey" field
              of yggdrasil.conf (Yggdrasil also accepts the same hex value in
              its own "yggdrasil -genconf" output) and never share it: anyone
              who has it can impersonate the node and use its address.
  publicKey   the 32-byte public key as hex, derived from the private key. It
              is what other nodes see and what the address is derived from.
  address     the Yggdrasil IPv6 address (200::/7) derived from the public key
              with the same algorithm as Yggdrasil and Y-Node. This is the
              identity the exit allowlist uses.

The command does not touch any file and does not need the configuration: it is
a pure key generator. With -json the three values are printed as one JSON
object, which is convenient for scripts and for pasting into a vault.`,
		Flags: []flagHelp{
			{Flag: "-json", Default: "off", Text: "print a JSON object instead of three lines"},
		},
		Examples: []string{
			"x-node keygen",
			"x-node keygen -json",
			"x-node keygen -json | jq -r .address",
		},
		SeeAlso: []string{"allow", "invite"},
	},
	"version": {
		Name:    "version",
		Summary: "Print the version",
		Usage:   []string{"x-node version [-json]"},
		Details: `Prints the daemon version. With -json it also prints the Go version and
the target platform, which is what a bug report should include.`,
		Flags: []flagHelp{
			{Flag: "-json", Default: "off", Text: "print version, Go version and platform as JSON"},
		},
		Examples: []string{"x-node version", "x-node version -json"},
	},
	"help": {
		Name:     "help",
		Summary:  "Show this help or the help of one command",
		Usage:    []string{"x-node help [command]"},
		Examples: []string{"x-node help", "x-node help invite"},
	},
}

// usage prints the overview. It goes to stderr on the error path and to stdout
// when the user asked for it.
func usage() {
	printOverview(os.Stderr)
}

func printOverview(out *os.File) {
	var b strings.Builder
	b.WriteString("x-node — SOCKS5 exit node for the Yggdrasil network\n\n")
	b.WriteString("Usage:\n  x-node <command> [flags]\n\n")
	b.WriteString("Commands:\n")
	width := 0
	for _, name := range commandOrder {
		if len(name) > width {
			width = len(name)
		}
	}
	for _, name := range commandOrder {
		command := commands[name]
		fmt.Fprintf(&b, "  %-*s  %s\n", width, name, command.Summary)
	}
	b.WriteString(`
Common flags:
  -config PATH   configuration file (default /etc/x-node/config.json,
                 environment X_NODE_CONFIG)
  -h, --help     help for the command

Run "x-node help <command>" for the flags and examples of one command.

The daemon reads its configuration once at start; the CLI reads the same file,
so every command accepts -config. Sizes in the quota commands accept MB, MiB,
GB, GiB and the short forms M and G; speeds accept Kbps, Mbps and Gbps.
`)
	_, _ = out.WriteString(b.String())
}

// printCommandHelp writes the detailed help of one command.
func printCommandHelp(name string) bool {
	command, ok := commands[name]
	if !ok {
		return false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n\n", command.Name, command.Summary)
	b.WriteString("Usage:\n")
	for _, line := range command.Usage {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	if command.Details != "" {
		b.WriteString("\n")
		b.WriteString(command.Details)
		b.WriteString("\n")
	}
	if len(command.Args) > 0 {
		b.WriteString("\nArguments:\n")
		for _, arg := range command.Args {
			fmt.Fprintf(&b, "  %-12s %s\n", arg.Name, arg.Text)
		}
	}
	if len(command.Flags) > 0 {
		b.WriteString("\nFlags:\n")
		for _, item := range command.Flags {
			fmt.Fprintf(&b, "  %s\n", item.Flag)
			if item.Text != "" {
				fmt.Fprintf(&b, "      %s\n", item.Text)
			}
			var meta []string
			if item.Default != "" {
				meta = append(meta, "default: "+item.Default)
			}
			if item.Env != "" {
				meta = append(meta, "env: "+item.Env)
			}
			if len(meta) > 0 {
				fmt.Fprintf(&b, "      (%s)\n", strings.Join(meta, ", "))
			}
		}
	}
	if len(command.Examples) > 0 {
		b.WriteString("\nExamples:\n")
		for _, example := range command.Examples {
			fmt.Fprintf(&b, "  %s\n", example)
		}
	}
	if len(command.SeeAlso) > 0 {
		b.WriteString("\nSee also:\n  ")
		b.WriteString(strings.Join(command.SeeAlso, ", "))
		b.WriteString("\n")
	}
	_, _ = os.Stdout.WriteString(b.String())
	return true
}

// helpForCommand prints the help of a command and reports whether it exists.
func helpForCommand(name string) bool {
	return printCommandHelp(name)
}

// wantsHelp reports whether the arguments ask for help. The check runs before
// dispatch because most commands need the configuration file: without it
// "x-node quota -h" on a machine without /etc/x-node/config.json would fail
// instead of printing the help. Scanning stops at "--", and a value that
// happens to look like a flag is only ever a note, a label or a password.
func wantsHelp(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "--":
			return false
		case "-h", "-help", "--help":
			return true
		}
	}
	return false
}

// commandNames returns the known names, sorted, for error messages.
func commandNames() []string {
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
