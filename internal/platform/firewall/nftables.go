package firewall

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
)

// The nftables backend: a table of its own, inet norca, that the deployer
// owns. Every apply writes the whole table into NFTRulesetPath and loads
// it with one nft -f, an atomic transaction: the file deletes the table it
// replaces, so no rule of an earlier plan stays and no other table is
// touched. A boot unit (NFTUnitPath, systemd hosts) or an OpenRC service
// of the boot runlevel (NFTInitScriptPath, Alpine) loads the file again at
// boot.
//
// The input chain holds the rules of the spec; with default_deny its
// policy is drop and it lets in what a host needs (loopback, replies, ICMP,
// DHCP replies) and the SSH ports. The forward chain filters only new
// connections into the bridge of the client (its policy stays accept, so
// the forwarding of Docker and other bridges is left alone): with
// default_deny they come from the sources of the bridge mode rules only.
// A packet must pass every table of a hook, so the policy drop of this
// table filters traffic that other tables accept.
const (
	// NFTTable is the table of the deployer.
	NFTTable = "inet norca"
	// NFTRulesetPath is the ruleset the deployer writes and loads.
	NFTRulesetPath = "/etc/nftables.d/norca.nft"
	// NFTUnitPath loads NFTRulesetPath at boot.
	NFTUnitPath = "/etc/systemd/system/" + nftUnit
	nftUnit     = "norca-nftables.service"
	// NFTInitScriptPath loads NFTRulesetPath at boot on OpenRC hosts; it is
	// a service of the boot runlevel.
	NFTInitScriptPath = "/etc/init.d/" + nftService
	nftService        = "norca-nftables"
	nftDir            = "/etc/nftables.d"
)

var (
	// nftPortRe is a port or a range ("4222" or "4222:4223").
	nftPortRe = regexp.MustCompile(`^[0-9]{1,5}(:[0-9]{1,5})?$`)
	// nftSourceRe is an address or a prefix of IPv4 or IPv6.
	nftSourceRe = regexp.MustCompile(`^[0-9A-Fa-f.:]+(/[0-9]{1,3})?$`)
	// nftIfaceRe is the name of a network interface.
	nftIfaceRe = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)
)

// RenderNFTables renders the ruleset of a spec: the table inet norca,
// written whole. The words of the rules are checked, so that a value of
// the plan cannot change the ruleset.
func RenderNFTables(s Spec) (string, error) {
	bridge := s.bridge()
	if !nftIfaceRe.MatchString(bridge) {
		return "", fmt.Errorf("the bridge %q is no interface name", bridge)
	}
	var input, forward []string
	for _, r := range s.Rules() {
		match := ""
		if r.Source != "" {
			if !nftSourceRe.MatchString(r.Source) {
				return "", fmt.Errorf("source %q of rule %s is no address or prefix", r.Source, r.ID())
			}
			family := "ip"
			if strings.Contains(r.Source, ":") {
				family = "ip6"
			}
			match = family + " saddr " + r.Source + " "
		}
		comment := ` comment "` + r.ID() + `"`
		if r.Forward {
			forward = append(forward, `oifname "`+bridge+`" `+match+"accept"+comment)
			continue
		}
		if !nftPortRe.MatchString(r.Port) || (r.Proto != "tcp" && r.Proto != "udp") {
			return "", fmt.Errorf("rule %s has no port and protocol nftables takes", r.ID())
		}
		input = append(input, match+r.Proto+" dport "+strings.Replace(r.Port, ":", "-", 1)+" accept"+comment)
	}

	policy := "accept"
	var b strings.Builder
	b.WriteString("# Managed by norca-deployer. Do not edit: the deployer writes the table\n")
	b.WriteString("# " + NFTTable + " whole on every apply, and loads this file at boot.\n")
	// Declaring the table first makes the delete work when it does not
	// exist yet: the file is one transaction either way.
	b.WriteString("table " + NFTTable + "\n")
	b.WriteString("delete table " + NFTTable + "\n")
	b.WriteString("table " + NFTTable + " {\n")
	b.WriteString("\tchain input {\n")
	if s.DefaultDeny {
		policy = "drop"
	}
	b.WriteString("\t\ttype filter hook input priority filter; policy " + policy + ";\n")
	if s.DefaultDeny {
		for _, l := range []string{
			`iifname "lo" accept`,
			"ct state established,related accept",
			"ct state invalid drop",
			"meta l4proto { icmp, ipv6-icmp } accept",
			"udp sport 67 udp dport 68 accept",
			"udp sport 547 udp dport 546 accept",
		} {
			b.WriteString("\t\t" + l + "\n")
		}
	}
	for _, l := range input {
		b.WriteString("\t\t" + l + "\n")
	}
	b.WriteString("\t}\n")
	if s.Client {
		b.WriteString("\tchain forward {\n")
		b.WriteString("\t\ttype filter hook forward priority filter; policy accept;\n")
		if s.DefaultDeny {
			b.WriteString("\t\t" + `oifname "` + bridge + `" ct state established,related accept` + "\n")
			b.WriteString("\t\t" + `oifname "` + bridge + `" iifname "` + bridge + `" accept` + "\n")
		}
		for _, l := range forward {
			b.WriteString("\t\t" + l + "\n")
		}
		if s.DefaultDeny {
			b.WriteString("\t\t" + `oifname "` + bridge + `" ct state new drop` + "\n")
		}
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n")
	return b.String(), nil
}

// nftUnitFile is the boot unit of the ruleset. It runs early, before the
// network, and after nftables.service, whose ruleset may start with flush
// ruleset.
func nftUnitFile(nft string) string {
	return `# Managed by norca-deployer. Do not edit.
[Unit]
Description=Norca firewall rules (nftables table ` + NFTTable + `)
Wants=network-pre.target
Before=network-pre.target shutdown.target
After=nftables.service
Conflicts=shutdown.target
DefaultDependencies=no

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=` + nft + ` -f ` + NFTRulesetPath + `

[Install]
WantedBy=sysinit.target
`
}

// NFTInitScript is the OpenRC service that loads the ruleset at boot:
// before the network, and after the nftables service, whose ruleset starts
// with flush ruleset. The deployer does not enable the nftables service of
// Alpine: its default ruleset drops what it does not list, SSH included.
func NFTInitScript(nft string) string {
	return `#!/sbin/openrc-run
# Managed by norca-deployer. Do not edit.
description="Norca firewall rules (nftables table ` + NFTTable + `)"

depend() {
	need localmount
	after nftables iptables ip6tables
	before net
}

start() {
	ebegin "Loading the nftables table ` + NFTTable + `"
	` + nft + ` -f ` + NFTRulesetPath + `
	eend $?
}
`
}

// configureNFTables writes and loads the table of the spec and makes the
// boot unit (systemd) or the boot service (OpenRC) load it again. A ruleset
// that cannot be loaded fails the node with default_deny and is a warning
// otherwise.
func configureNFTables(conn connector.Connector, s Spec, nft string) (*Result, error) {
	res := &Result{Backend: "nftables"}
	ruleset, err := RenderNFTables(s)
	if err != nil {
		return nil, err
	}
	if err := conn.MkdirAll(nftDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create %s: %w", nftDir, err)
	}
	if _, err := connector.SyncFile(conn, NFTRulesetPath, []byte(ruleset), 0o644); err != nil {
		return nil, err
	}
	// The table is loaded on every apply: it may have been flushed (a
	// restart of nftables.service) since the file was written.
	if out, err := conn.ExecuteCommand("nft -f " + NFTRulesetPath); err != nil {
		msg := fmt.Sprintf("nft -f %s failed: %s", NFTRulesetPath, strings.TrimSpace(out))
		if s.DefaultDeny {
			return nil, fmt.Errorf("default_deny: %s", msg)
		}
		res.Warnings = append(res.Warnings, msg)
		return res, nil
	}
	if _, err := conn.ExecuteCommand("[ -d /run/systemd/system ] && command -v systemctl >/dev/null"); err == nil {
		changed, err := connector.SyncFile(conn, NFTUnitPath, []byte(nftUnitFile(nft)), 0o644)
		if err != nil {
			return nil, err
		}
		if changed {
			res.run(conn, "systemctl daemon-reload")
		}
		res.run(conn, "systemctl is-enabled --quiet "+nftUnit+" || systemctl enable --quiet "+nftUnit)
	} else if _, err := conn.ExecuteCommand("[ -d /run/openrc ] && command -v rc-update >/dev/null"); err == nil {
		if _, err := connector.SyncFile(conn, NFTInitScriptPath, []byte(NFTInitScript(nft)), 0o755); err != nil {
			return nil, err
		}
		res.run(conn, "[ -e /etc/runlevels/boot/"+nftService+" ] || rc-update add "+nftService+" boot >/dev/null")
	} else {
		res.Warnings = append(res.Warnings, "neither systemd nor OpenRC runs: "+NFTRulesetPath+" is not loaded at boot by the deployer")
	}
	if !s.DefaultDeny {
		res.Warnings = append(res.Warnings, "the input policy of the nftables table "+NFTTable+" is accept, so its rules restrict nothing; "+
			"set tuning.firewall.default_deny to filter traffic")
	}
	return res, nil
}

// RemoveNFTablesCommand removes the table, the ruleset and the boot unit or
// service of the nftables backend; it prints "removed" when there was
// something.
const RemoveNFTablesCommand = `if command -v nft >/dev/null 2>&1 && nft list table ` + NFTTable + ` >/dev/null 2>&1; then
  nft delete table ` + NFTTable + `
  echo removed
fi
if [ -e ` + NFTUnitPath + ` ]; then
  systemctl disable --quiet ` + nftUnit + ` >/dev/null 2>&1 || true
  rm -f ` + NFTUnitPath + `
  systemctl daemon-reload >/dev/null 2>&1 || true
  echo removed
fi
if [ -e ` + NFTInitScriptPath + ` ]; then
  rc-update del ` + nftService + ` boot >/dev/null 2>&1 || true
  rm -f ` + NFTInitScriptPath + `
  echo removed
fi
if [ -e ` + NFTRulesetPath + ` ]; then
  rm -f ` + NFTRulesetPath + `
  echo removed
fi`

// removeNFTables removes what the nftables backend left when another
// backend filters the node: its table with a policy drop would still
// filter what the other one lets in.
func removeNFTables(conn connector.Connector, res *Result) {
	out, err := conn.ExecuteCommand(RemoveNFTablesCommand)
	switch {
	case err != nil:
		res.Warnings = append(res.Warnings, "the nftables table "+NFTTable+" of an earlier apply could not be removed: "+strings.TrimSpace(out))
	case strings.Contains(out, "removed"):
		res.Warnings = append(res.Warnings, "the nftables table "+NFTTable+" of an earlier apply was removed: "+res.Backend+" filters the node now")
	}
}
