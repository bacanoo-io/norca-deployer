// Package dns makes Linux nodes resolve the Consul domain through the DNS
// interface of the local Consul agent (consul.dns_forwarding): conditional
// forwarding, so only that domain goes to Consul and every other name keeps
// going to the resolver the host has.
//
// The work is done by one POSIX sh script (consul_dns.sh): it detects the
// local resolver (systemd-resolved or dnsmasq), checks what it needs before
// it changes anything, writes the files and restarts the resolver when one
// changed. The SSH path uploads it and runs it (Apply); the install script
// of a scaled machine embeds its functions (Functions, Assignments).
package dns

import (
	_ "embed"
	"fmt"
	"net/netip"
	"path"
	"strconv"
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

//go:embed consul_dns.sh
var functions string

// Files on the node.
const (
	// ScriptPath is where the SSH path uploads the script.
	ScriptPath = "/var/lib/norca-deployer/consul-dns.sh"

	ResolvedDropIn = "/etc/systemd/resolved.conf.d/norca-consul.conf"
	// TrustAnchor is the negative DNSSEC trust anchor of the domain: the
	// answers of the Consul agent are not signed.
	TrustAnchor = "/etc/dnssec-trust-anchors.d/norca-consul.negative"
	DnsmasqConf = "/etc/dnsmasq.d/norca-consul.conf"
	// DaemonJSON gets the "dns" key with docker: true.
	DaemonJSON = "/etc/docker/daemon.json"
)

// Functions returns the POSIX sh functions of the script; norca_consul_dns
// runs them with the variables of Vars set.
func Functions() string { return functions }

// Script returns the file the SSH path uploads: the functions and their call.
func Script() string {
	return "#!/bin/sh\n# Managed by norca-deployer. Do not edit.\n\n" + functions + "\nnorca_consul_dns\n"
}

// Var is one parameter of the script.
type Var struct{ Name, Value string }

// Vars returns the parameters of the script for a forwarding block; nil or
// disabled removes what an earlier run wrote.
func Vars(f *config.ConsulDNSForwarding) ([]Var, error) {
	return vars(f, "")
}

func vars(f *config.ConsulDNSForwarding, root string) ([]Var, error) {
	enabled, docker := "0", "0"
	addrStr, domain, method := config.DefaultConsulDNSAddress, config.DefaultConsulDNSDomain, config.DNSMethodAuto
	if f != nil {
		if f.Enabled {
			enabled = "1"
		}
		if f.Docker {
			docker = "1"
		}
		if f.Address != "" {
			addrStr = f.Address
		}
		if f.Domain != "" {
			domain = f.Domain
		}
		if f.Method != "" {
			method = f.Method
		}
	}
	addr, port, err := config.SplitDNSAddress(addrStr)
	if err != nil {
		return nil, fmt.Errorf("consul.dns_forwarding.address: %w", err)
	}
	ip := addr.String()
	resolved := ip
	if addr.Is6() {
		resolved = "[" + ip + "]"
	}
	return []Var{
		{"NCD_ENABLED", enabled},
		{"NCD_DOMAIN", domain},
		{"NCD_RESOLVED_DNS", resolved + ":" + strconv.Itoa(port)},
		{"NCD_DNSMASQ_SERVER", ip + "#" + strconv.Itoa(port)},
		{"NCD_PORT", strconv.Itoa(port)},
		{"NCD_METHOD", method},
		{"NCD_DOCKER", docker},
		{"NCD_ROOT", root},
	}, nil
}

// Assignments renders the parameters as shell assignments joined by sep:
// " " for the prefix of a command, "\n" for lines of a script.
func Assignments(vs []Var, sep string) string {
	parts := make([]string, 0, len(vs))
	for _, v := range vs {
		parts = append(parts, v.Name+"="+connector.ShellQuote(v.Value))
	}
	return strings.Join(parts, sep)
}

// Result is what the script did on a node.
type Result struct {
	// Method is the resolver used; empty when the forwarding is off.
	Method    string
	Changed   []string // files written
	Unchanged []string
	Removed   []string
	Restarted []string // services restarted
	Warnings  []string
	// Bridge is the address of docker0 with its prefix length, with
	// docker: true; invalid otherwise.
	Bridge netip.Prefix
	// Errors are the error lines of the script: each ends a sentence whose
	// subject is the node.
	Errors []string
}

// Parse reads the output lines of the script; lines of other programs are
// left out.
func Parse(out string) *Result {
	res := &Result{}
	for line := range strings.SplitSeq(out, "\n") {
		kind, rest, _ := strings.Cut(strings.TrimRight(line, "\r"), " ")
		switch kind {
		case "method":
			res.Method = rest
		case "changed":
			res.Changed = append(res.Changed, rest)
		case "unchanged":
			res.Unchanged = append(res.Unchanged, rest)
		case "removed":
			res.Removed = append(res.Removed, rest)
		case "restarted":
			res.Restarted = append(res.Restarted, rest)
		case "warn":
			res.Warnings = append(res.Warnings, rest)
		case "error":
			res.Errors = append(res.Errors, rest)
		case "bridge":
			if p, err := netip.ParsePrefix(rest); err == nil {
				res.Bridge = p
			}
		}
	}
	return res
}

// Summary describes the result in one line for the apply output.
func (r *Result) Summary() string {
	var parts []string
	if r.Method != "" {
		parts = append(parts, r.Method)
	} else {
		parts = append(parts, "off")
	}
	if len(r.Changed) > 0 {
		parts = append(parts, "written: "+strings.Join(r.Changed, ", "))
	}
	if len(r.Removed) > 0 {
		parts = append(parts, "removed: "+strings.Join(r.Removed, ", "))
	}
	if len(r.Restarted) > 0 {
		parts = append(parts, "restarted: "+strings.Join(r.Restarted, ", "))
	}
	if len(r.Changed)+len(r.Removed)+len(r.Restarted) == 0 {
		parts = append(parts, "unchanged")
	}
	return strings.Join(parts, "; ")
}

// Apply configures the forwarding of a Linux node: f enabled forwards the
// domain, nil or disabled removes what an earlier apply wrote. node names
// the node in the errors. A node without a suitable resolver is an error,
// found before anything is changed on it.
func Apply(conn connector.Connector, node string, f *config.ConsulDNSForwarding) (*Result, error) {
	return apply(conn, node, f, "")
}

// apply is Apply with every path of the node below root (tests).
func apply(conn connector.Connector, node string, f *config.ConsulDNSForwarding, root string) (*Result, error) {
	vs, err := vars(f, root)
	if err != nil {
		return nil, err
	}
	enabled := f != nil && f.Enabled
	if !enabled {
		found, err := anyManagedFile(conn, root)
		if err != nil {
			return nil, fmt.Errorf("consul.dns_forwarding: %s: %w", node, err)
		}
		if !found {
			return &Result{}, nil
		}
	}

	if err := conn.MkdirAll(path.Dir(ScriptPath), 0o755); err != nil {
		return nil, fmt.Errorf("consul.dns_forwarding: %s: %w", node, err)
	}
	if _, err := connector.SyncFile(conn, ScriptPath, []byte(Script()), 0o700); err != nil {
		return nil, fmt.Errorf("consul.dns_forwarding: %s: %w", node, err)
	}
	out, runErr := conn.ExecuteCommand(Assignments(vs, " ") + " sh " + ScriptPath)
	res := Parse(out)
	if len(res.Errors) > 0 {
		return res, fmt.Errorf("consul.dns_forwarding: %s %s", node, strings.Join(res.Errors, "; "))
	}
	if runErr != nil {
		return res, fmt.Errorf("consul.dns_forwarding: %s: the script failed: %w (output: %s)", node, runErr, strings.TrimSpace(out))
	}
	if !enabled {
		// Nothing is managed any more: the script goes too.
		if out, err := conn.ExecuteCommand("rm -f " + ScriptPath); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("could not remove %s: %s", ScriptPath, strings.TrimSpace(out)))
		}
	}
	return res, nil
}

// anyManagedFile reports whether a file the script writes is on the node,
// so that a node that never had the forwarding gets no upload.
func anyManagedFile(conn connector.Connector, root string) (bool, error) {
	var tests []string
	for _, p := range []string{ResolvedDropIn, TrustAnchor, DnsmasqConf} {
		tests = append(tests, "[ -e "+connector.ShellQuote(root+p)+" ]")
	}
	out, err := conn.ExecuteCommand("if " + strings.Join(tests, " || ") + "; then echo yes; else echo no; fi")
	if err != nil {
		return false, fmt.Errorf("could not look for the files of an earlier apply: %w (output: %s)", err, strings.TrimSpace(out))
	}
	return strings.TrimSpace(out) == "yes", nil
}
