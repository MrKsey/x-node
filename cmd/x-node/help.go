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
	"status", "doctor", "web", "keygen", "version", "help",
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
		Summary: "Create an invitation (QR code and link) for a client",
		Usage:   []string{"x-node invite [-config PATH] [-uses N] [-ttl 24h] [-note TEXT] [-json]"},
		Details: `An invite is a one-time secret that lets a Y-Node client add this exit.
The command prints a QR code, a yggnode:// link and the token itself. Only the
SHA-256 of the token is stored, so a leaked state file cannot be used to
register.

The client must already be connected to Yggdrasil: registration goes over the
tunnel, not over the clearnet.`,
		Flags: []flagHelp{
			{Flag: "-config PATH", Default: "/etc/x-node/config.json", Env: "X_NODE_CONFIG", Text: "configuration file to read"},
			{Flag: "-uses N", Default: "inviteUses from the config (1)", Text: "how many clients may use this invite"},
			{Flag: "-ttl 24h", Default: "inviteTTL from the config (24h)", Text: "how long the invite stays valid"},
			{Flag: "-note TEXT", Default: "", Text: "free-form note shown in the client list"},
			{Flag: "-json", Default: "off", Text: "print JSON instead of the QR code, for scripts"},
		},
		Examples: []string{
			"x-node invite",
			`x-node invite -uses 3 -ttl 48h -note "family phones"`,
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
		},
		Details: `The allowlist is the set of Yggdrasil addresses that may use this exit when
allowAll is false. An address can get in through an invite (the normal path) or
be added by hand.

"allow token" issues a new API token for an address that is already allowed and
invalidates the previous one. The token is what the client sends to /v1/status
and /v1/token; it is printed once and only its hash is stored.`,
		Args: []argHelp{
			{Name: "address", Text: "Yggdrasil node address, e.g. 201:1ba8:15c:b1be:7555:b3d7:7651:8e2e"},
		},
		Flags: []flagHelp{
			{Flag: "-config PATH", Default: "/etc/x-node/config.json", Env: "X_NODE_CONFIG", Text: "configuration file to read"},
			{Flag: "-label TEXT", Default: "", Text: "human-readable name for the client (add only)"},
		},
		Examples: []string{
			"x-node allow list",
			"x-node allow add 201:1ba8:15c:b1be:7555:b3d7:7651:8e2e -label laptop",
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

When the service is not running only the allowlist is shown.`,
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
			"x-node quota set <address> [-monthly 100GiB] [-daily 5GiB] [-down 10Mbps] [-up 5Mbps] [-config PATH]",
			"x-node quota reset <address>|--all [-config PATH]",
		},
		Details: `A quota caps the traffic of one client and optionally throttles its speed.
Limits are enforced when a new connection or UDP association starts: once the
cap is reached the exit refuses new requests with a SOCKS5 "not allowed" reply
and writes a line to the log. Connections that are already open are left alone.

Sizes accept B, KiB, MiB, GiB, TiB and the short forms K, M, G, T. Speeds accept
Kbps, Mbps and Gbps. Zero means "no limit". A client without its own entry uses
the defaults from the "quotas" section of the configuration.`,
		Args: []argHelp{
			{Name: "address", Text: "Yggdrasil node address of an allowlisted client"},
		},
		Flags: []flagHelp{
			{Flag: "-config PATH", Default: "/etc/x-node/config.json", Env: "X_NODE_CONFIG", Text: "configuration file to read"},
			{Flag: "-monthly SIZE", Default: "unchanged", Text: "monthly traffic cap, 0 removes it"},
			{Flag: "-daily SIZE", Default: "unchanged", Text: "daily traffic cap, 0 removes it"},
			{Flag: "-down SPEED", Default: "unchanged", Text: "download throttle, 0 removes it"},
			{Flag: "-up SPEED", Default: "unchanged", Text: "upload throttle, 0 removes it"},
			{Flag: "-json", Default: "off", Text: "machine-readable output (show)"},
			{Flag: "--all", Default: "off", Text: "reset every client (reset)"},
		},
		Examples: []string{
			"x-node quota show",
			"x-node quota set 201:1ba8:15c:b1be:7555:b3d7:7651:8e2e -monthly 100GiB -down 20Mbps",
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
replaced or dropped by the supervisor.`,
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
		Details: `Shows what the daemon would use right now: address, ports, peer settings,
UDP relay, web interface, quotas and the counters from runtime.json. It reads
the configuration file directly, so it also works while the service is stopped.`,
		Flags: []flagHelp{
			{Flag: "-config PATH", Default: "/etc/x-node/config.json", Env: "X_NODE_CONFIG", Text: "configuration file to read"},
		},
		Examples: []string{"x-node status"},
		SeeAlso:  []string{"doctor", "clients"},
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
	"web": {
		Name:    "web",
		Summary: "Show the web interface URL and manage its password",
		Usage: []string{
			"x-node web url [-config PATH]",
			"x-node web password [-config PATH] [-user NAME] [-password VALUE]",
			"x-node web reset-path [-config PATH]",
		},
		Details: `"web url" prints the address to open in a browser, including the random
path from the configuration.

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
	"keygen": {
		Name:    "keygen",
		Summary: "Generate an Ed25519 key pair and its Yggdrasil address",
		Usage:   []string{"x-node keygen [-json]"},
		Details: `Prints a private key, the matching public key and the Yggdrasil address
derived from it. The same derivation is used by the app and by the allowlist, so
the address is a stable identity for a node.

The private key is a secret: anyone who has it can impersonate the node.`,
		Flags: []flagHelp{
			{Flag: "-json", Default: "off", Text: "print a JSON object instead of three lines"},
		},
		Examples: []string{"x-node keygen", "x-node keygen -json"},
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
so every command accepts -config. Sizes in the quota commands accept KiB, MiB,
GiB, TiB and K, M, G, T.`)
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
