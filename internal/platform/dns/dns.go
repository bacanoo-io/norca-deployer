// Package dns makes Linux nodes resolve the Consul domain through the DNS
// interface of the local Consul agent (consul.dns_forwarding): conditional
// forwarding, so Consul answers that domain and every other name keeps being
// answered by the resolver the host has. systemd-resolved also asks its
// global DNS= server (Consul) for other names, in parallel; Consul refuses
// them.
//
// The work is done by one POSIX sh script (consul_dns.sh): it detects the
// local resolver (systemd-resolved or dnsmasq), checks what it needs before
// it changes anything, writes the files and restarts the resolver when one
// changed. Its steps (Steps) upload it and run it, over SSH and in the
// install script of a scaled machine alike. The caller tells it the init
// system of the node: systemd, or OpenRC (Alpine), where dnsmasq is the
// only resolver and rc-service starts it.
package dns

import (
	"context"
	_ "embed"
	"fmt"
	"net/netip"
	"path"
	"strconv"
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/platform/step"
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

// Vars returns the parameters of the script for a forwarding block on a
// node whose init system is initSystem (config.InitSystemd or
// config.InitOpenRC); nil or disabled removes what an earlier run wrote. An
// empty initSystem leaves NCD_INIT out: the caller puts it into the
// environment of the script (the install script of a scaled machine, from
// the family of the machine).
func Vars(f *config.ConsulDNSForwarding, initSystem string) ([]Var, error) {
	return vars(f, initSystem, "")
}

func vars(f *config.ConsulDNSForwarding, initSystem, root string) ([]Var, error) {
	switch initSystem {
	case "", config.InitSystemd, config.InitOpenRC:
	default:
		return nil, fmt.Errorf("consul.dns_forwarding: the init system %q is not %s or %s", initSystem, config.InitSystemd, config.InitOpenRC)
	}
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
	vs := []Var{
		{"NCD_ENABLED", enabled},
		{"NCD_DOMAIN", domain},
		{"NCD_RESOLVED_DNS", resolved + ":" + strconv.Itoa(port)},
		{"NCD_DNSMASQ_SERVER", ip + "#" + strconv.Itoa(port)},
		{"NCD_PORT", strconv.Itoa(port)},
		{"NCD_METHOD", method},
		{"NCD_DOCKER", docker},
	}
	if initSystem != "" {
		vs = append(vs, Var{"NCD_INIT", initSystem})
	}
	return append(vs, Var{"NCD_ROOT", root}), nil
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

// StepName names the steps of the forwarding in the log and in the errors.
const StepName = "consul.dns_forwarding"

// Steps returns the steps of a forwarding block, the same for the SSH path
// and the install script of a scaled machine: the script is written to
// ScriptPath and run with the parameters; its result lines decide the
// change, the warnings and the errors. nil or disabled removes what an
// earlier run wrote: the script runs only on a node that has a file of an
// earlier run, and the script goes afterwards. initSystem is the init
// system of the node, as for Vars: empty when the caller sets NCD_INIT in
// the environment of the step.
func Steps(f *config.ConsulDNSForwarding, initSystem string) ([]step.Step, error) {
	return steps(f, initSystem, "")
}

func steps(f *config.ConsulDNSForwarding, initSystem, root string) ([]step.Step, error) {
	vs, err := vars(f, initSystem, root)
	if err != nil {
		return nil, err
	}
	run := step.Step{
		Name:  StepName,
		Dirs:  []step.Dir{{Path: path.Dir(ScriptPath), Mode: 0o755}},
		Files: []step.File{{Path: ScriptPath, Mode: 0o700, Content: []byte(Script())}},
		Apply: Assignments(vs, " ") + " sh " + ScriptPath,
		Lines: true,
	}
	if f != nil && f.Enabled {
		return []step.Step{run}, nil
	}
	run.When = anyManagedFile(root)
	// Nothing is managed any more: the script goes too.
	run.Apply += "\nrm -f " + ScriptPath
	return []step.Step{run}, nil
}

// Apply configures the forwarding of a Linux node: f enabled forwards the
// domain, nil or disabled removes what an earlier apply wrote. node names
// the node in the errors; initSystem is its init system
// (config.InitSystemd or config.InitOpenRC, from its facts). A node without
// a suitable resolver is an error, found before anything is changed on it.
func Apply(ctx context.Context, conn connector.Connector, node, initSystem string, f *config.ConsulDNSForwarding) (*Result, error) {
	return apply(ctx, conn, node, initSystem, f, "")
}

// apply is Apply with every path of the node below root (tests).
func apply(ctx context.Context, conn connector.Connector, node, initSystem string, f *config.ConsulDNSForwarding, root string) (*Result, error) {
	steps, err := steps(f, initSystem, root)
	if err != nil {
		return nil, err
	}
	ex := &step.Executor{Conn: conn, Host: node}
	res := &Result{}
	for i := range steps {
		r, err := ex.Run(ctx, &steps[i])
		if r != nil && steps[i].Lines && r.Output != "" {
			res = Parse(r.Output)
		}
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// anyManagedFile is the condition that a file the script writes is on the
// node, so that a node that never had the forwarding gets no upload.
func anyManagedFile(root string) string {
	var tests []string
	for _, p := range []string{ResolvedDropIn, TrustAnchor, DnsmasqConf} {
		tests = append(tests, "[ -e "+connector.ShellQuote(root+p)+" ]")
	}
	return strings.Join(tests, " || ")
}
