// Package firewall opens the Norca ports on Linux nodes with ufw, firewalld
// or, as a fallback, iptables. Windows nodes are not configured.
//
// Every rule the deployer adds carries an identifier (ufw and iptables
// comments), so that a later apply can remove its own rules that the plan no
// longer asks for (e.g. after a CIDR change) without touching other rules.
// New rules are added before stale ones are removed, so a port that stays
// open is never closed in between.
package firewall

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/bacanoo-io/norca-deployer/pkg/connector"
)

// Ports are the agent ports from the plan.
type Ports struct {
	HTTP, RPC, Serf int
}

const (
	// Default range of the dynamic allocation ports.
	dynamicLow  = 20000
	dynamicHigh = 32000

	// idPrefix starts the identifier of every rule the deployer manages.
	idPrefix = "norca:"
	// legacyPrefix starts the ufw comments of earlier deployer versions.
	legacyPrefix = "Norca "
)

// Spec describes what a node needs.
type Spec struct {
	Ports  Ports
	Server bool // the node runs a server: HTTP, RPC and Serf
	Client bool // the node runs a client: allocation ports and CNI forwarding

	OpenEphemeral bool
	// DynamicMin/Max is the allocation port range; 0 uses 20000-32000.
	DynamicMin, DynamicMax int

	// Sources; an empty list means any source.
	ClusterCIDRs   []string // RPC and Serf
	APICIDRs       []string // HTTP API
	EphemeralCIDRs []string // allocation ports; default ClusterCIDRs

	// DefaultDeny turns on ufw with incoming traffic denied by default
	// (firewalld denies by default already). SSHPorts are always allowed
	// then, so the deployer cannot lock itself out.
	DefaultDeny bool
	SSHPorts    []int

	// Extra are further ports of the node (static workload ports).
	Extra []Extra
}

// Extra is a static port or range ("4222" or "4222:4223") of one protocol.
type Extra struct {
	Port, Proto string
	Sources     []string // empty: any
}

// Rule is one allowed inbound port or range from one source.
type Rule struct {
	Port   string // "4647" or "20000:32000"
	Proto  string // tcp or udp
	Source string // CIDR or address; "" = any
	// Forward rules allow forwarded traffic to the CNI bridge: allocation
	// ports in bridge mode are DNATed to the allocation in PREROUTING, so
	// the traffic never reaches INPUT, and FORWARD sees the allocation's own
	// port. Forward rules therefore match the source only, not the port.
	Forward bool
}

// ID identifies a rule: norca:<port>/<proto>:<source|any>, or
// norca:fwd:<source|any> for a forward rule.
func (r Rule) ID() string {
	src := r.Source
	if src == "" {
		src = "any"
	}
	if r.Forward {
		return idPrefix + "fwd:" + src
	}
	return idPrefix + r.Port + "/" + r.Proto + ":" + src
}

func sources(list []string) []string {
	if len(list) == 0 {
		return []string{""}
	}
	return list
}

// Rules returns the rules the node needs.
func (s Spec) Rules() []Rule {
	var rules []Rule
	add := func(port, proto string, srcs []string, forward bool) {
		for _, src := range sources(srcs) {
			rules = append(rules, Rule{Port: port, Proto: proto, Source: src, Forward: forward})
		}
	}
	if s.DefaultDeny {
		for _, p := range s.SSHPorts {
			add(strconv.Itoa(p), "tcp", nil, false)
		}
	}
	if s.Server {
		add(strconv.Itoa(s.Ports.HTTP), "tcp", s.APICIDRs, false)
		add(strconv.Itoa(s.Ports.RPC), "tcp", s.ClusterCIDRs, false)
		add(strconv.Itoa(s.Ports.Serf), "tcp", s.ClusterCIDRs, false)
		add(strconv.Itoa(s.Ports.Serf), "udp", s.ClusterCIDRs, false)
	}
	if s.Client && s.OpenEphemeral {
		eph := s.EphemeralCIDRs
		if len(eph) == 0 {
			eph = s.ClusterCIDRs
		}
		low, high := s.dynamicRange()
		rng := fmt.Sprintf("%d:%d", low, high)
		for _, proto := range []string{"tcp", "udp"} {
			add(rng, proto, eph, false) // host network mode
		}
		add("", "", eph, true) // bridge mode (DNAT to cni0)
	}
	for _, e := range s.Extra {
		add(e.Port, e.Proto, e.Sources, false)
	}
	return rules
}

func (s Spec) dynamicRange() (int, int) {
	if s.DynamicMin > 0 && s.DynamicMax > 0 {
		return s.DynamicMin, s.DynamicMax
	}
	return dynamicLow, dynamicHigh
}

// Result reports what was done, for the apply output.
type Result struct {
	Backend  string
	Warnings []string
}

// Configure applies the spec on a Linux node.
func Configure(conn connector.Connector, s Spec) (*Result, error) {
	s.SSHPorts = mergePorts(s.SSHPorts, detectSSHPorts(conn))
	switch {
	case active(conn, "ufw status", "Status: active"):
		return configureUFW(conn, s, false)
	case active(conn, "firewall-cmd --state", "running"):
		return configureFirewalld(conn, s)
	case s.DefaultDeny && installed(conn, "ufw"):
		return configureUFW(conn, s, true)
	case s.DefaultDeny:
		return nil, fmt.Errorf("default_deny needs ufw or a running firewalld (install ufw with host_packages)")
	case installed(conn, "iptables"):
		return configureIPTables(conn, s)
	}
	return &Result{Backend: "none", Warnings: []string{"no ufw, firewalld or iptables found: no firewall rules applied"}}, nil
}

func active(conn connector.Connector, cmd, marker string) bool {
	bin := strings.Fields(cmd)[0]
	out, err := conn.ExecuteCommand("command -v " + bin + " >/dev/null && " + cmd)
	return err == nil && strings.Contains(out, marker)
}

func installed(conn connector.Connector, bin string) bool {
	_, err := conn.ExecuteCommand("command -v " + bin + " >/dev/null")
	return err == nil
}

// detectSSHPorts returns the TCP ports sshd listens on.
func detectSSHPorts(conn connector.Connector) []int {
	out, err := conn.ExecuteCommand(`ss -Htlnp 2>/dev/null | grep '"sshd"' || true`)
	if err != nil {
		return nil
	}
	var ports []int
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		local := f[3]
		if i := strings.LastIndex(local, ":"); i >= 0 {
			if p, err := strconv.Atoi(local[i+1:]); err == nil {
				ports = append(ports, p)
			}
		}
	}
	return ports
}

func mergePorts(a, b []int) []int {
	set := map[int]bool{}
	var res []int
	for _, p := range append(append([]int{}, a...), b...) {
		if p > 0 && !set[p] {
			set[p] = true
			res = append(res, p)
		}
	}
	sort.Ints(res)
	return res
}

func (res *Result) run(conn connector.Connector, cmd string) {
	if out, err := conn.ExecuteCommand(cmd); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("%s: %s", cmd, strings.TrimSpace(out)))
	}
}

// ---------------------------------------------------------------------------
// ufw
// ---------------------------------------------------------------------------

// ufwSpec is the rule in the form `ufw show added` prints it, without the
// comment.
func ufwSpec(r Rule) string {
	if r.Forward {
		if r.Source == "" {
			return "route allow out on cni0"
		}
		return "route allow out on cni0 from " + r.Source
	}
	if r.Source == "" {
		return "allow " + r.Port + "/" + r.Proto
	}
	return "allow from " + r.Source + " to any port " + r.Port + " proto " + r.Proto
}

// cniEgress lets allocations in bridge mode reach the network.
const ufwCNIEgress = "route allow in on cni0"

func configureUFW(conn connector.Connector, s Spec, enable bool) (*Result, error) {
	res := &Result{Backend: "ufw"}
	want := map[string]bool{}
	for _, r := range s.Rules() {
		spec := ufwSpec(r)
		want[spec] = true
		res.run(conn, "ufw "+spec+" comment "+connector.ShellQuote(r.ID()))
	}
	if s.Client {
		want[ufwCNIEgress] = true
		res.run(conn, "ufw "+ufwCNIEgress+" comment "+connector.ShellQuote(idPrefix+"cni-egress"))
	}

	// Remove the deployer's rules the plan no longer asks for.
	out, _ := conn.ExecuteCommand("ufw show added")
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		spec, comment, ok := splitUFWLine(line)
		if !ok || !(strings.HasPrefix(comment, idPrefix) || strings.HasPrefix(comment, legacyPrefix)) || want[spec] {
			continue
		}
		if strings.HasPrefix(spec, "route ") {
			res.run(conn, "ufw route delete "+strings.TrimPrefix(spec, "route "))
		} else {
			res.run(conn, "ufw delete "+spec)
		}
	}

	if policy, _ := conn.ExecuteCommand(`grep -s '^DEFAULT_FORWARD_POLICY=' /etc/default/ufw`); strings.Contains(policy, "ACCEPT") {
		res.Warnings = append(res.Warnings, "DEFAULT_FORWARD_POLICY is ACCEPT in /etc/default/ufw (set by an earlier deployer version?): "+
			"forwarded traffic to allocations is not filtered; set it to DROP to apply ephemeral_cidrs to bridge mode")
	}

	if enable || s.DefaultDeny {
		res.run(conn, "ufw default deny incoming")
		res.run(conn, "ufw default allow outgoing")
		res.run(conn, "ufw --force enable")
	} else {
		res.run(conn, "ufw reload")
	}
	return res, nil
}

// splitUFWLine splits a line of `ufw show added` ("ufw allow ... comment
// 'x'") into the rule and the comment.
func splitUFWLine(line string) (spec, comment string, ok bool) {
	if !strings.HasPrefix(line, "ufw ") {
		return "", "", false
	}
	spec = strings.TrimPrefix(line, "ufw ")
	if i := strings.Index(spec, " comment "); i >= 0 {
		comment = strings.Trim(spec[i+len(" comment "):], `'"`)
		spec = spec[:i]
	}
	return spec, comment, true
}

// ---------------------------------------------------------------------------
// firewalld
// ---------------------------------------------------------------------------

func firewalldRich(r Rule) string {
	family := "ipv4"
	if strings.Contains(r.Source, ":") {
		family = "ipv6"
	}
	return fmt.Sprintf(`rule family="%s" source address="%s" port port="%s" protocol="%s" accept`,
		family, r.Source, strings.Replace(r.Port, ":", "-", 1), r.Proto)
}

func configureFirewalld(conn connector.Connector, s Spec) (*Result, error) {
	res := &Result{Backend: "firewalld"}
	wantPorts := map[string]bool{}
	wantRich := map[string]bool{}
	managed := map[string]bool{} // ports the deployer owns
	for _, p := range []int{s.Ports.HTTP, s.Ports.RPC, s.Ports.Serf} {
		managed[strconv.Itoa(p)] = true
	}
	// The default range and the configured one, so that a changed range
	// replaces the old rule.
	managed[fmt.Sprintf("%d-%d", dynamicLow, dynamicHigh)] = true
	low, high := s.dynamicRange()
	managed[fmt.Sprintf("%d-%d", low, high)] = true
	for _, e := range s.Extra {
		managed[strings.Replace(e.Port, ":", "-", 1)] = true
	}

	for _, r := range s.Rules() {
		if r.Forward {
			// firewalld filters forwarding to the trusted cni0 zone itself.
			continue
		}
		if r.Source == "" {
			p := strings.Replace(r.Port, ":", "-", 1) + "/" + r.Proto
			wantPorts[p] = true
			res.run(conn, "firewall-cmd --permanent --add-port="+p)
		} else {
			rich := firewalldRich(r)
			wantRich[rich] = true
			res.run(conn, "firewall-cmd --permanent --add-rich-rule="+connector.ShellQuote(rich))
		}
	}
	if s.Client {
		res.run(conn, "firewall-cmd --permanent --zone=trusted --add-interface=cni0")
		res.run(conn, "firewall-cmd --permanent --zone=trusted --add-masquerade")
	}

	// Remove Norca ports and rich rules the plan no longer asks for.
	out, _ := conn.ExecuteCommand("firewall-cmd --permanent --list-ports")
	for _, p := range strings.Fields(out) {
		port, _, _ := strings.Cut(p, "/")
		if managed[port] && !wantPorts[p] {
			res.run(conn, "firewall-cmd --permanent --remove-port="+p)
		}
	}
	out, _ = conn.ExecuteCommand("firewall-cmd --permanent --list-rich-rules")
	for _, rich := range strings.Split(out, "\n") {
		rich = strings.TrimSpace(rich)
		if rich == "" || wantRich[rich] {
			continue
		}
		for port := range managed {
			if strings.Contains(rich, `port port="`+port+`"`) {
				res.run(conn, "firewall-cmd --permanent --remove-rich-rule="+connector.ShellQuote(rich))
				break
			}
		}
	}
	res.run(conn, "firewall-cmd --reload")
	return res, nil
}

// ---------------------------------------------------------------------------
// iptables (fallback; rules are not persisted across reboots)
// ---------------------------------------------------------------------------

func iptablesRule(r Rule) string {
	src := ""
	if r.Source != "" {
		src = " -s " + r.Source
	}
	if r.Forward {
		return fmt.Sprintf("FORWARD -o cni0%s -m comment --comment %s -j ACCEPT", src, r.ID())
	}
	return fmt.Sprintf("INPUT%s -p %s --dport %s -m comment --comment %s -j ACCEPT", src, r.Proto, r.Port, r.ID())
}

// legacyIPTables are the untagged rules of earlier deployer versions.
func legacyIPTables(p Ports) []string {
	rng := fmt.Sprintf("%d:%d", dynamicLow, dynamicHigh)
	return []string{
		fmt.Sprintf("INPUT -p tcp --dport %d -j ACCEPT", p.HTTP),
		fmt.Sprintf("INPUT -p tcp --dport %d -j ACCEPT", p.RPC),
		fmt.Sprintf("INPUT -p tcp --dport %d -j ACCEPT", p.Serf),
		fmt.Sprintf("INPUT -p udp --dport %d -j ACCEPT", p.Serf),
		"INPUT -p tcp --dport " + rng + " -j ACCEPT",
		"INPUT -p udp --dport " + rng + " -j ACCEPT",
	}
}

func configureIPTables(conn connector.Connector, s Spec) (*Result, error) {
	res := &Result{Backend: "iptables"}
	want := map[string]bool{}
	for _, r := range s.Rules() {
		want[r.ID()] = true
		rule := iptablesRule(r)
		res.run(conn, "iptables -C "+rule+" 2>/dev/null || iptables -I "+rule)
	}
	if s.Client {
		id := idPrefix + "cni-egress"
		want[id] = true
		rule := "FORWARD -i cni0 -m comment --comment " + id + " -j ACCEPT"
		res.run(conn, "iptables -C "+rule+" 2>/dev/null || iptables -I "+rule)
	}

	// Remove tagged rules the plan no longer asks for, and legacy rules.
	for _, chain := range []string{"INPUT", "FORWARD"} {
		out, _ := conn.ExecuteCommand("iptables -S " + chain)
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			i := strings.Index(line, "--comment ")
			if !strings.HasPrefix(line, "-A ") || i < 0 {
				continue
			}
			id := strings.Trim(strings.Fields(line[i+len("--comment "):])[0], `"`)
			if strings.HasPrefix(id, idPrefix) && !want[id] {
				res.run(conn, "iptables -D "+strings.TrimPrefix(line, "-A "))
			}
		}
	}
	for _, rule := range legacyIPTables(s.Ports) {
		res.run(conn, "while iptables -C "+rule+" 2>/dev/null; do iptables -D "+rule+"; done")
	}
	for _, rule := range []string{"FORWARD -i cni0 -j ACCEPT", "FORWARD -o cni0 -j ACCEPT"} {
		res.run(conn, "while iptables -C "+rule+" 2>/dev/null; do iptables -D "+rule+"; done")
	}

	if out, _ := conn.ExecuteCommand("iptables -S INPUT | head -1"); strings.Contains(out, "-P INPUT ACCEPT") {
		res.Warnings = append(res.Warnings, "the iptables INPUT policy is ACCEPT, so these rules restrict nothing; "+
			"install ufw and set tuning.firewall.default_deny to filter traffic")
	}
	return res, nil
}
