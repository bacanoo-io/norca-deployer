package firewall

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
)

// nftSpec is a client and server with default_deny, IPv4 and IPv6
// sources, an extra UDP port and the DNS of docker0.
func nftSpec() Spec {
	return Spec{
		Ports: ports, Server: true, Client: true, OpenEphemeral: true, DefaultDeny: true, SSHPorts: []int{22, 2222},
		ClusterCIDRs: []string{"10.77.0.0/16", "fd00:77::/64"}, APICIDRs: []string{"10.77.1.0/24"},
		Extra:          []Extra{{Port: "4222:4223", Proto: "udp"}},
		HostDNSSources: []string{"172.17.0.0/16"},
	}
}

func TestRenderNFTables(t *testing.T) {
	got, err := RenderNFTables(nftSpec())
	if err != nil {
		t.Fatal(err)
	}
	want := `# Managed by norca-deployer. Do not edit: the deployer writes the table
# inet norca whole on every apply, and loads this file at boot.
table inet norca
delete table inet norca
table inet norca {
	chain input {
		type filter hook input priority filter; policy drop;
		iifname "lo" accept
		ct state established,related accept
		ct state invalid drop
		meta l4proto { icmp, ipv6-icmp } accept
		udp sport 67 udp dport 68 accept
		udp sport 547 udp dport 546 accept
		tcp dport 22 accept comment "norca:22/tcp:any"
		tcp dport 2222 accept comment "norca:2222/tcp:any"
		ip saddr 10.77.1.0/24 tcp dport 4646 accept comment "norca:4646/tcp:10.77.1.0/24"
		ip saddr 10.77.0.0/16 tcp dport 4647 accept comment "norca:4647/tcp:10.77.0.0/16"
		ip6 saddr fd00:77::/64 tcp dport 4647 accept comment "norca:4647/tcp:fd00:77::/64"
		ip saddr 10.77.0.0/16 tcp dport 4648 accept comment "norca:4648/tcp:10.77.0.0/16"
		ip6 saddr fd00:77::/64 tcp dport 4648 accept comment "norca:4648/tcp:fd00:77::/64"
		ip saddr 10.77.0.0/16 udp dport 4648 accept comment "norca:4648/udp:10.77.0.0/16"
		ip6 saddr fd00:77::/64 udp dport 4648 accept comment "norca:4648/udp:fd00:77::/64"
		ip saddr 10.77.0.0/16 tcp dport 20000-32000 accept comment "norca:20000:32000/tcp:10.77.0.0/16"
		ip6 saddr fd00:77::/64 tcp dport 20000-32000 accept comment "norca:20000:32000/tcp:fd00:77::/64"
		ip saddr 10.77.0.0/16 udp dport 20000-32000 accept comment "norca:20000:32000/udp:10.77.0.0/16"
		ip6 saddr fd00:77::/64 udp dport 20000-32000 accept comment "norca:20000:32000/udp:fd00:77::/64"
		udp dport 4222-4223 accept comment "norca:4222:4223/udp:any"
		ip saddr 172.17.0.0/16 udp dport 53 accept comment "norca:53/udp:172.17.0.0/16"
		ip saddr 172.17.0.0/16 tcp dport 53 accept comment "norca:53/tcp:172.17.0.0/16"
	}
	chain forward {
		type filter hook forward priority filter; policy accept;
		oifname "nomad" ct state established,related accept
		oifname "nomad" iifname "nomad" accept
		oifname "nomad" ip saddr 10.77.0.0/16 accept comment "norca:fwd:10.77.0.0/16"
		oifname "nomad" ip6 saddr fd00:77::/64 accept comment "norca:fwd:fd00:77::/64"
		oifname "nomad" ct state new drop
	}
}
`
	if got != want {
		t.Fatalf("RenderNFTables:\n%s\nwant:\n%s", got, want)
	}
}

// Without default_deny the policies stay accept and nothing is dropped; a
// server without client has no forward chain.
func TestRenderNFTablesWithoutDefaultDeny(t *testing.T) {
	s := nftSpec()
	s.DefaultDeny, s.Client = false, false
	got, err := RenderNFTables(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"drop", "chain forward", "dport 22 ", "ct state"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%q in:\n%s", unwanted, got)
		}
	}
	if !strings.Contains(got, "policy accept;") || !strings.Contains(got, `tcp dport 4646 accept comment "norca:4646/tcp:10.77.1.0/24"`) {
		t.Errorf("ruleset:\n%s", got)
	}
}

// A value of the plan that is no word of a rule is refused, not written.
func TestRenderNFTablesRefusesOtherWords(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(s *Spec)
	}{
		{name: "source", mutate: func(s *Spec) { s.ClusterCIDRs = []string{`10.0.0.0/8 accept; }`} }},
		{name: "port", mutate: func(s *Spec) { s.Extra = []Extra{{Port: "1-2", Proto: "tcp"}} }},
		{name: "protocol", mutate: func(s *Spec) { s.Extra = []Extra{{Port: "1", Proto: "sctp"}} }},
		{name: "bridge", mutate: func(s *Spec) { s.Bridge = `nomad" accept` }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := nftSpec()
			tt.mutate(&s)
			if _, err := RenderNFTables(s); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// nftNode is a fake node: which firewall tools it has, whether ufw is
// active and firewalld runs, systemd, and whether nft -f fails.
type nftNode struct {
	tools          []string
	ufwActive      bool
	firewalldOn    bool
	noSystemd      bool
	nftFails       bool
	removeAnswered string
}

func (n nftNode) fake() *connector.FakeConnector {
	c := connector.NewFakeConnector()
	has := func(tool string) bool { return slices.Contains(n.tools, tool) }
	c.Handler = func(cmd string) (string, error, bool) {
		switch {
		case strings.HasPrefix(cmd, "command -v ufw >/dev/null && ufw status"):
			if has("ufw") && n.ufwActive {
				return "Status: active", nil, true
			}
			return "Status: inactive", nil, true
		case strings.HasPrefix(cmd, "command -v firewall-cmd >/dev/null && firewall-cmd --state"):
			if has("firewall-cmd") && n.firewalldOn {
				return "running", nil, true
			}
			return "", errors.New("not running"), true
		case strings.HasPrefix(cmd, "command -v "):
			tool := strings.Fields(cmd)[2]
			if !has(tool) {
				return "", errors.New("not found"), true
			}
			return "/usr/sbin/" + tool + "\n", nil, true
		case strings.HasPrefix(cmd, "nft -f "):
			if n.nftFails {
				return "Error: syntax error", errors.New("exit status 1"), true
			}
			return "", nil, true
		case strings.HasPrefix(cmd, "[ -d /run/systemd/system ]"):
			if n.noSystemd {
				return "", errors.New("exit status 1"), true
			}
			return "", nil, true
		case strings.HasPrefix(cmd, "if command -v nft >/dev/null 2>&1 && nft list table"):
			return n.removeAnswered, nil, true
		}
		return "", nil, true
	}
	return c
}

// The chooser: an active ufw, a running firewalld, nft, with default_deny an
// installed ufw, iptables, none.
func TestChooseBackend(t *testing.T) {
	tests := []struct {
		name    string
		node    nftNode
		deny    bool
		backend string
		err     string
	}{
		{name: "active ufw first", node: nftNode{tools: []string{"ufw", "firewall-cmd", "nft", "iptables"}, ufwActive: true, firewalldOn: true}, backend: "ufw"},
		{name: "running firewalld", node: nftNode{tools: []string{"ufw", "firewall-cmd", "nft", "iptables"}, firewalldOn: true}, backend: "firewalld"},
		{name: "nft before iptables", node: nftNode{tools: []string{"nft", "iptables"}}, backend: "nftables"},
		{name: "nft before an inactive ufw with default_deny", node: nftNode{tools: []string{"ufw", "nft", "iptables"}}, deny: true, backend: "nftables"},
		{name: "an inactive ufw without nft with default_deny", node: nftNode{tools: []string{"ufw", "iptables"}}, deny: true, backend: "ufw"},
		{name: "iptables", node: nftNode{tools: []string{"ufw", "iptables"}}, backend: "iptables"},
		{name: "default_deny with iptables only", node: nftNode{tools: []string{"iptables"}}, deny: true, err: "default_deny needs nftables (nft), ufw or a running firewalld"},
		{name: "none", node: nftNode{}, backend: "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := Spec{Ports: ports, Server: true, DefaultDeny: tt.deny}
			res, err := Configure(tt.node.fake(), s)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("got %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil || res.Backend != tt.backend {
				t.Fatalf("backend %v, %v; want %s", res, err, tt.backend)
			}
		})
	}
}

// The ruleset is written, loaded with nft -f and loaded again at boot by
// the unit, which is enabled once; a second apply loads the same file and
// changes nothing else.
func TestNFTablesApply(t *testing.T) {
	c := nftNode{tools: []string{"nft", "iptables"}}.fake()
	s := nftSpec()
	res, err := Configure(c, s)
	if err != nil || res.Backend != "nftables" {
		t.Fatalf("%v %v", res, err)
	}
	want, _ := RenderNFTables(withDetectedSSH(s))
	if got := string(c.Files[NFTRulesetPath]); got != want {
		t.Fatalf("ruleset:\n%s", got)
	}
	unit := string(c.Files[NFTUnitPath])
	if !strings.Contains(unit, "ExecStart=/usr/sbin/nft -f "+NFTRulesetPath) || !strings.Contains(unit, "WantedBy=sysinit.target") {
		t.Fatalf("unit:\n%s", unit)
	}
	cmds := strings.Join(c.Commands, "\n")
	for _, w := range []string{"nft -f " + NFTRulesetPath, "systemctl daemon-reload", "systemctl is-enabled --quiet norca-nftables.service || systemctl enable --quiet norca-nftables.service"} {
		if !strings.Contains(cmds, w) {
			t.Errorf("no %q in:\n%s", w, cmds)
		}
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings with default_deny: %q", res.Warnings)
	}

	c.Commands = nil
	if _, err := Configure(c, s); err != nil {
		t.Fatal(err)
	}
	cmds = strings.Join(c.Commands, "\n")
	if !strings.Contains(cmds, "nft -f "+NFTRulesetPath) || strings.Contains(cmds, "daemon-reload") || strings.Contains(cmds, "nft delete") {
		t.Fatalf("second apply:\n%s", cmds)
	}
	if again, _ := RenderNFTables(withDetectedSSH(s)); string(c.Files[NFTRulesetPath]) != again {
		t.Fatal("the ruleset of the second apply differs")
	}
}

// withDetectedSSH is the spec as Configure completes it on the fake node
// (no ss: no detected port).
func withDetectedSSH(s Spec) Spec {
	s.SSHPorts = mergePorts(s.SSHPorts, nil)
	return s
}

// A ruleset nft refuses fails the node with default_deny and is a warning
// without it; without systemd the file is not loaded at boot (warning);
// without default_deny the accept policy is named.
func TestNFTablesFailuresAndWarnings(t *testing.T) {
	s := nftSpec()
	if _, err := Configure(nftNode{tools: []string{"nft"}, nftFails: true}.fake(), s); err == nil || !strings.Contains(err.Error(), "default_deny: nft -f") {
		t.Fatalf("default_deny with a failed nft: %v", err)
	}
	s.DefaultDeny = false
	res, err := Configure(nftNode{tools: []string{"nft"}, nftFails: true}.fake(), s)
	if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "Error: syntax error") {
		t.Fatalf("%v %v", res, err)
	}
	res, err = Configure(nftNode{tools: []string{"nft"}, noSystemd: true}.fake(), s)
	if err != nil || !strings.Contains(strings.Join(res.Warnings, "\n"), "no systemd") ||
		!strings.Contains(strings.Join(res.Warnings, "\n"), "policy of the nftables table inet norca is accept") {
		t.Fatalf("%v %v", res, err)
	}
}

// Another backend removes the table of an earlier nftables apply: its
// policy drop would still filter what ufw lets in.
func TestNFTablesRemovedByAnotherBackend(t *testing.T) {
	c := nftNode{tools: []string{"ufw", "nft"}, ufwActive: true, removeAnswered: "removed\nremoved\n"}.fake()
	res, err := Configure(c, Spec{Ports: ports, Server: true})
	if err != nil || res.Backend != "ufw" {
		t.Fatalf("%v %v", res, err)
	}
	if len(c.CommandsWithPrefix("if command -v nft >/dev/null 2>&1 && nft list table inet norca")) != 1 ||
		!strings.Contains(strings.Join(res.Warnings, "\n"), "of an earlier apply was removed: ufw filters the node now") {
		t.Fatalf("warnings %q, commands:\n%s", res.Warnings, strings.Join(c.Commands, "\n"))
	}
}

// The firewalld backend against a running firewalld (RHEL uses it as
// before): NORCA_TEST_FIREWALLD=1 as root where firewalld runs (a test
// container that started dbus and firewalld). The rules are in the
// permanent configuration after a reload, a second apply adds nothing new,
// and the nftables backend is not chosen.
func TestFirewalldOnThisSystem(t *testing.T) {
	if os.Getenv("NORCA_TEST_FIREWALLD") != "1" {
		t.Skip("NORCA_TEST_FIREWALLD is not set (a running firewalld)")
	}
	conn := connector.NewLocalConnector(t.Context())
	s := Spec{Ports: ports, Server: true, Client: true, OpenEphemeral: true, ClusterCIDRs: []string{"10.77.0.0/16"}}
	for run := 1; run <= 2; run++ {
		res, err := Configure(conn, s)
		if err != nil || res.Backend != "firewalld" {
			t.Fatalf("run %d: %v %v", run, res, err)
		}
		t.Logf("run %d warnings: %q", run, res.Warnings)
	}
	out, err := conn.ExecuteCommand("firewall-cmd --list-rich-rules; firewall-cmd --zone=trusted --list-interfaces")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, w := range []string{`rule family="ipv4" source address="10.77.0.0/16" port port="4647" protocol="tcp" accept`,
		`port port="20000-32000" protocol="udp" accept`, "nomad"} {
		if !strings.Contains(out, w) {
			t.Errorf("no %q in:\n%s", w, out)
		}
	}
	if strings.Count(out, `port port="4647"`) != 1 {
		t.Errorf("a rule twice:\n%s", out)
	}
	if out, err := conn.ExecuteCommand("nft list table " + NFTTable); err == nil {
		t.Errorf("the nftables table exists next to firewalld:\n%s", out)
	}
	t.Logf("firewalld:\n%s", out)
}

// The ruleset loads into a real nftables and loads again the same:
// NORCA_TEST_NFT=1 as root with CAP_NET_ADMIN (a test container), nft
// installed. The listing of the table is compared before and after the
// second load; then another backend removes it.
func TestNFTablesOnThisSystem(t *testing.T) {
	if os.Getenv("NORCA_TEST_NFT") != "1" {
		t.Skip("NORCA_TEST_NFT is not set (a container with CAP_NET_ADMIN and nft)")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("no nft")
	}
	conn := connector.NewLocalConnector(t.Context())
	t.Cleanup(func() { _, _ = conn.ExecuteCommand(RemoveNFTablesCommand) })
	list := func(t *testing.T) string {
		t.Helper()
		out, err := conn.ExecuteCommand("nft list table " + NFTTable)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return out
	}
	for _, deny := range []bool{true, false} {
		s := nftSpec()
		s.DefaultDeny = deny
		res, err := Configure(conn, s)
		if err != nil || res.Backend != "nftables" {
			t.Fatalf("default_deny %t: %v %v", deny, res, err)
		}
		first := list(t)
		for _, w := range []string{"policy " + map[bool]string{true: "drop", false: "accept"}[deny], `comment "norca:4647/tcp:10.77.0.0/16"`, "20000-32000"} {
			if !strings.Contains(first, w) {
				t.Errorf("default_deny %t: no %q in:\n%s", deny, w, first)
			}
		}
		if _, err := Configure(conn, s); err != nil {
			t.Fatal(err)
		}
		if second := list(t); second != first {
			t.Fatalf("the second load differs:\n%s\nfirst:\n%s", second, first)
		}
		t.Logf("default_deny %t:\n%s", deny, first)
	}
	res := &Result{Backend: "ufw"}
	removeNFTables(conn, res)
	if out, err := conn.ExecuteCommand("nft list table " + NFTTable); err == nil || len(res.Warnings) != 1 {
		t.Fatalf("the table is still there: %s, warnings %q", out, res.Warnings)
	}
}
