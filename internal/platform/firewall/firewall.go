// Package firewall opens the Norca ports on Linux nodes with ufw, firewalld,
// nftables or, as a fallback, iptables. Windows nodes are not configured.
//
// Every rule the deployer adds carries an identifier (ufw, iptables and
// nftables comments), so that a later apply can remove its own rules that
// the plan no longer asks for (e.g. after a CIDR change) without touching
// other rules. New rules are added before stale ones are removed, so a port
// that stays open is never closed in between; the nftables backend writes
// its own table whole, in one transaction.
package firewall

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
)

// Ports are the agent ports from the plan.
type Ports struct {
	HTTP, RPC, Serf int

	// Bus and BusCluster are the ports of the system bus of the servers;
	// 0 opens nothing.
	Bus, BusCluster int

	// BusLeafNode is the port of the leaf nodes of the bus. It is opened
	// only with Spec.LeafNodes; without it a rule the deployer made for the
	// port earlier is removed.
	BusLeafNode int
}

const (
	// Default range of the dynamic allocation ports.
	dynamicLow  = 20000
	dynamicHigh = 32000

	// DefaultBridge is the bridge of the bridge network mode of a client
	// whose configuration does not name one (bridge_network_name): the
	// interface the allocations of that mode are attached to.
	DefaultBridge = "nomad"
	// staleBridge is the interface the rules of earlier deployer versions
	// named: the default bridge of the CNI bridge plugin, which is not the
	// one the client makes. Those rules matched nothing.
	staleBridge = "cni0"

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

	// Bridge is the bridge of the client's bridge network mode; empty is
	// DefaultBridge.
	Bridge string

	OpenEphemeral bool
	// DynamicMin/Max is the allocation port range; 0 uses 20000-32000.
	DynamicMin, DynamicMax int

	// Sources; an empty list means any source.
	ClusterCIDRs  []string // RPC and Serf
	APICIDRs      []string // HTTP API
	LeafNodeCIDRs []string // leaf nodes of the bus; default APICIDRs

	// LeafNodes opens Ports.BusLeafNode on the servers: the leaf node
	// listener of the bus is on.
	LeafNodes      bool
	EphemeralCIDRs []string // allocation ports; default ClusterCIDRs

	// DefaultDeny denies incoming traffic by default: the policy drop of
	// the nftables table, or ufw enabled with deny incoming (firewalld
	// denies by default already). SSHPorts are always allowed then, so the
	// deployer cannot lock itself out.
	DefaultDeny bool
	SSHPorts    []int

	// Extra are further ports of the node (static workload ports).
	Extra []Extra

	// HostDNSSources may ask the DNS stub of the host on port 53 (UDP and
	// TCP): the docker0 subnet when systemd-resolved listens there for the
	// containers (consul.dns_forwarding.docker). Empty opens nothing.
	HostDNSSources []string
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
	// Bridge is the bridge a forward rule is for.
	Bridge string
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

// bridge returns the bridge of the client's bridge network mode.
func (s Spec) bridge() string {
	if s.Bridge == "" {
		return DefaultBridge
	}
	return s.Bridge
}

// Rules returns the rules the node needs.
func (s Spec) Rules() []Rule {
	var rules []Rule
	add := func(port, proto string, srcs []string, forward bool) {
		for _, src := range sources(srcs) {
			r := Rule{Port: port, Proto: proto, Source: src, Forward: forward}
			if forward {
				r.Bridge = s.bridge()
			}
			rules = append(rules, r)
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
		// The system bus: nodes connect to the bus port, the servers of
		// the region route to each other on the cluster port.
		if s.Ports.Bus != 0 {
			add(strconv.Itoa(s.Ports.Bus), "tcp", s.ClusterCIDRs, false)
		}
		if s.Ports.BusCluster != 0 {
			add(strconv.Itoa(s.Ports.BusCluster), "tcp", s.ClusterCIDRs, false)
		}
		// Leaf nodes come from outside the cluster, as API clients do.
		if s.LeafNodes && s.Ports.BusLeafNode != 0 {
			leaf := s.LeafNodeCIDRs
			if len(leaf) == 0 {
				leaf = s.APICIDRs
			}
			add(strconv.Itoa(s.Ports.BusLeafNode), "tcp", leaf, false)
		}
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
		add("", "", eph, true) // bridge mode (DNAT to the bridge of the client)
	}
	for _, e := range s.Extra {
		add(e.Port, e.Proto, e.Sources, false)
	}
	if len(s.HostDNSSources) > 0 {
		add("53", "udp", s.HostDNSSources, false)
		add("53", "tcp", s.HostDNSSources, false)
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
	detected, warning := detectSSHPorts(conn)
	s.SSHPorts = mergePorts(s.SSHPorts, detected)
	res, err := configure(conn, s)
	if res != nil && warning != "" {
		res.Warnings = append(res.Warnings, warning)
	}
	return res, err
}

// configure chooses the backend: an active ufw, a running firewalld,
// nftables (nft), with default_deny an installed ufw that it enables, else
// iptables, else none. The table of the nftables backend is removed when
// another backend filters the node.
func configure(conn connector.Connector, s Spec) (*Result, error) {
	var res *Result
	var err error
	switch {
	case active(conn, "ufw status", "Status: active"):
		res, err = configureUFW(conn, s, false)
	case active(conn, "firewall-cmd --state", "running"):
		res, err = configureFirewalld(conn, s)
	default:
		if nft, ok := lookPath(conn, "nft"); ok {
			return configureNFTables(conn, s, nft)
		}
		switch {
		case s.DefaultDeny && installed(conn, "ufw"):
			res, err = configureUFW(conn, s, true)
		case s.DefaultDeny:
			return nil, fmt.Errorf("default_deny needs nftables (nft), ufw or a running firewalld (install nftables with host_packages)")
		case installed(conn, "iptables"):
			res, err = configureIPTables(conn, s)
		default:
			return &Result{Backend: "none", Warnings: []string{"no ufw, firewalld, nft or iptables found: no firewall rules applied"}}, nil
		}
	}
	if err != nil {
		return res, err
	}
	removeNFTables(conn, res)
	return res, nil
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

// lookPath returns the path of a command of the node; a command found
// without an absolute path (a fake node) is in /usr/sbin.
func lookPath(conn connector.Connector, bin string) (string, bool) {
	out, err := conn.ExecuteCommand("command -v " + bin)
	if err != nil {
		return "", false
	}
	p := strings.TrimSpace(out)
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, " \t\n") {
		p = "/usr/sbin/" + bin
	}
	return p, true
}

// sshPortsCommand lists the TCP ports sshd listens on: with ss when the
// node has it; else (busybox, a minimal image) the port lines of sshd -T and
// the sockets of /proc/net/tcp and tcp6.
const sshPortsCommand = `if command -v ss >/dev/null 2>&1; then echo ss; ss -Htlnp 2>/dev/null | grep '"sshd"' || true; ` +
	`else echo proc; sshd -T 2>/dev/null | awk '$1 == "port" { print "port " $2 }'; cat /proc/net/tcp /proc/net/tcp6 2>/dev/null; fi`

// detectSSHPorts returns the TCP ports sshd listens on. Without ss it takes
// port 22 and the ports of sshd -T that a socket listens on; when it finds
// none it returns port 22 and a warning.
func detectSSHPorts(conn connector.Connector) ([]int, string) {
	out, err := conn.ExecuteCommand(sshPortsCommand)
	if err != nil {
		return nil, ""
	}
	if first, rest, _ := strings.Cut(out, "\n"); strings.TrimSpace(first) == "proc" {
		if ports := listeningSSHPorts(rest); len(ports) > 0 {
			return ports, ""
		}
		return []int{22}, "the ports of sshd could not be found (no ss, no listening socket of sshd in /proc/net/tcp): port 22 is assumed"
	}
	return parseSS(out), ""
}

// listeningSSHPorts reads the port lines of sshd -T and the lines of
// /proc/net/tcp*: the ports of sshd (22 and those of its configuration)
// that a socket listens on (state 0A).
func listeningSSHPorts(out string) []int {
	candidates := map[int]bool{22: true}
	listening := map[int]bool{}
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "port" {
			if p, err := strconv.Atoi(f[1]); err == nil {
				candidates[p] = true
			}
			continue
		}
		if len(f) < 4 || f[3] != "0A" {
			continue
		}
		if i := strings.LastIndex(f[1], ":"); i >= 0 {
			if p, err := strconv.ParseInt(f[1][i+1:], 16, 32); err == nil {
				listening[int(p)] = true
			}
		}
	}
	var ports []int
	for p := range candidates {
		if listening[p] {
			ports = append(ports, p)
		}
	}
	sort.Ints(ports)
	return ports
}

// parseSS reads the lines of ss -Htlnp of sshd.
func parseSS(out string) []int {
	var ports []int
	for line := range strings.SplitSeq(out, "\n") {
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
			return "route allow out on " + r.Bridge
		}
		return "route allow out on " + r.Bridge + " from " + r.Source
	}
	if r.Source == "" {
		return "allow " + r.Port + "/" + r.Proto
	}
	return "allow from " + r.Source + " to any port " + r.Port + " proto " + r.Proto
}

// ufwEgress lets allocations in bridge mode reach the network.
func ufwEgress(bridge string) string { return "route allow in on " + bridge }

func configureUFW(conn connector.Connector, s Spec, enable bool) (*Result, error) {
	res := &Result{Backend: "ufw"}
	want := map[string]bool{}
	for _, r := range s.Rules() {
		spec := ufwSpec(r)
		want[spec] = true
		res.run(conn, "ufw "+spec+" comment "+connector.ShellQuote(r.ID()))
	}
	if s.Client {
		egress := ufwEgress(s.bridge())
		want[egress] = true
		res.run(conn, "ufw "+egress+" comment "+connector.ShellQuote(idPrefix+"cni-egress"))
	}

	// Remove the deployer's rules the plan no longer asks for.
	out, _ := conn.ExecuteCommand("ufw show added")
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		spec, comment, ok := splitUFWLine(line)
		if !ok || !(strings.HasPrefix(comment, idPrefix) || strings.HasPrefix(comment, legacyPrefix)) || want[spec] {
			continue
		}
		if after, ok0 := strings.CutPrefix(spec, "route "); ok0 {
			res.run(conn, "ufw route delete "+after)
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
	for _, p := range []int{s.Ports.HTTP, s.Ports.RPC, s.Ports.Serf, s.Ports.Bus, s.Ports.BusCluster, s.Ports.BusLeafNode} {
		if p != 0 {
			managed[strconv.Itoa(p)] = true
		}
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
			// firewalld filters forwarding to the trusted zone of the
			// bridge itself.
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
		res.run(conn, "firewall-cmd --permanent --zone=trusted --add-interface="+s.bridge())
		res.run(conn, "firewall-cmd --permanent --zone=trusted --add-masquerade")
		// The interface earlier deployer versions put there.
		if s.bridge() != staleBridge {
			if _, err := conn.ExecuteCommand("firewall-cmd --permanent --zone=trusted --query-interface=" + staleBridge); err == nil {
				res.run(conn, "firewall-cmd --permanent --zone=trusted --remove-interface="+staleBridge)
			}
		}
	}

	// Remove Norca ports and rich rules the plan no longer asks for.
	out, _ := conn.ExecuteCommand("firewall-cmd --permanent --list-ports")
	for p := range strings.FieldsSeq(out) {
		port, _, _ := strings.Cut(p, "/")
		if managed[port] && !wantPorts[p] {
			res.run(conn, "firewall-cmd --permanent --remove-port="+p)
		}
	}
	out, _ = conn.ExecuteCommand("firewall-cmd --permanent --list-rich-rules")
	for rich := range strings.SplitSeq(out, "\n") {
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
		return fmt.Sprintf("FORWARD -o %s%s -m comment --comment %s -j ACCEPT", r.Bridge, src, r.ID())
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
		rule := "FORWARD -i " + s.bridge() + " -m comment --comment " + id + " -j ACCEPT"
		res.run(conn, "iptables -C "+rule+" 2>/dev/null || iptables -I "+rule)
	}

	// Remove tagged rules the plan no longer asks for, and legacy rules.
	for _, chain := range []string{"INPUT", "FORWARD"} {
		out, _ := conn.ExecuteCommand("iptables -S " + chain)
		for line := range strings.SplitSeq(out, "\n") {
			line = strings.TrimSpace(line)
			i := strings.Index(line, "--comment ")
			if !strings.HasPrefix(line, "-A ") || i < 0 {
				continue
			}
			id := strings.Trim(strings.Fields(line[i+len("--comment "):])[0], `"`)
			// A rule of an earlier deployer version for the wrong bridge
			// has the identifier of the rule that replaces it.
			isStale := s.bridge() != staleBridge &&
				(strings.Contains(line, " -o "+staleBridge+" ") || strings.Contains(line, " -i "+staleBridge+" "))
			if strings.HasPrefix(id, idPrefix) && (!want[id] || isStale) {
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
