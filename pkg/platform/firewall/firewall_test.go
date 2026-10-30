package firewall

import (
	"errors"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/pkg/connector"
)

var ports = Ports{HTTP: 4646, RPC: 4647, Serf: 4648}

func TestRulesBySourceAndRole(t *testing.T) {
	s := Spec{Ports: ports, Server: true, Client: true, OpenEphemeral: true,
		ClusterCIDRs: []string{"10.8.0.0/24"}, APICIDRs: []string{"192.0.2.0/24"}, EphemeralCIDRs: []string{"10.8.0.0/24"}}
	var ids []string
	for _, r := range s.Rules() {
		ids = append(ids, r.ID())
	}
	got := strings.Join(ids, " ")
	want := "norca:4646/tcp:192.0.2.0/24 norca:4647/tcp:10.8.0.0/24 norca:4648/tcp:10.8.0.0/24 norca:4648/udp:10.8.0.0/24 " +
		"norca:20000:32000/tcp:10.8.0.0/24 norca:20000:32000/udp:10.8.0.0/24 norca:fwd:10.8.0.0/24"
	if got != want {
		t.Fatalf("rules:\n got  %s\n want %s", got, want)
	}

	// A server without client opens no allocation ports; any source without CIDRs.
	s = Spec{Ports: ports, Server: true, OpenEphemeral: true}
	for _, r := range s.Rules() {
		if strings.HasPrefix(r.Port, "20000") || r.Forward || r.Source != "" {
			t.Fatalf("unexpected rule %s", r.ID())
		}
	}
}

func TestSSHPortsOnlyWithDefaultDeny(t *testing.T) {
	s := Spec{Ports: ports, SSHPorts: []int{2222}}
	if len(s.Rules()) != 0 {
		t.Fatal("SSH rule without default_deny")
	}
	s.DefaultDeny = true
	if r := s.Rules(); len(r) != 1 || r[0].ID() != "norca:2222/tcp:any" {
		t.Fatalf("rules = %v", r)
	}
}

func TestDetectSSHPorts(t *testing.T) {
	c := connector.NewFakeConnector()
	c.Responses["ss -Htlnp"] = `LISTEN 0 128 0.0.0.0:2222 0.0.0.0:* users:(("sshd",pid=1,fd=3))
LISTEN 0 128 [::]:2222 [::]:* users:(("sshd",pid=1,fd=4))`
	if got := mergePorts([]int{22}, detectSSHPorts(c)); len(got) != 2 || got[0] != 22 || got[1] != 2222 {
		t.Fatalf("ports = %v", got)
	}
}

// ufwFake simulates ufw with an existing rule set.
func ufwFake(active bool, added string) *connector.FakeConnector {
	c := connector.NewFakeConnector()
	c.Handler = func(cmd string) (string, error, bool) {
		switch {
		case strings.Contains(cmd, "ss -Htlnp"):
			return `LISTEN 0 128 0.0.0.0:22 0.0.0.0:* users:(("sshd",pid=1,fd=3))`, nil, true
		case strings.HasPrefix(cmd, "command -v ufw >/dev/null && ufw status"):
			if active {
				return "Status: active", nil, true
			}
			return "Status: inactive", nil, true
		case strings.HasPrefix(cmd, "command -v firewall-cmd"):
			return "", errors.New("not found"), true
		case cmd == "command -v ufw >/dev/null":
			return "", nil, true
		case cmd == "ufw show added":
			return added, nil, true
		}
		return "", nil, true
	}
	return c
}

func TestUFWReplacesStaleRulesAfterAddingNewOnes(t *testing.T) {
	added := `Added user rules (see 'ufw status' for running firewall):
ufw allow 22/tcp
ufw allow 4647/tcp comment 'Norca RPC'
ufw allow from 10.8.0.0/24 to any port 4648 proto tcp comment 'norca:4648/tcp:10.8.0.0/24'
ufw allow from 10.9.0.0/24 to any port 4648 proto tcp comment 'norca:4648/tcp:10.9.0.0/24'`
	c := ufwFake(true, added)
	s := Spec{Ports: ports, Server: true, ClusterCIDRs: []string{"10.8.0.0/24"}}
	res, err := Configure(c, s)
	if err != nil || res.Backend != "ufw" {
		t.Fatalf("res=%v err=%v", res, err)
	}
	cmds := strings.Join(c.Commands, "\n")
	addRPC := strings.Index(cmds, "ufw allow from 10.8.0.0/24 to any port 4647 proto tcp comment 'norca:4647/tcp:10.8.0.0/24'")
	delOld := strings.Index(cmds, "ufw delete allow 4647/tcp")
	if addRPC < 0 || delOld < 0 || delOld < addRPC {
		t.Fatalf("expected the restricted RPC rule before deleting the open legacy rule:\n%s", cmds)
	}
	if !strings.Contains(cmds, "ufw delete allow from 10.9.0.0/24 to any port 4648 proto tcp") {
		t.Fatalf("stale CIDR rule not removed:\n%s", cmds)
	}
	for _, keep := range []string{"ufw delete allow 22/tcp", "ufw delete allow from 10.8.0.0/24 to any port 4648"} {
		if strings.Contains(cmds, keep) {
			t.Fatalf("removed a rule that must stay (%s):\n%s", keep, cmds)
		}
	}
	if strings.Contains(cmds, "ufw --force enable") {
		t.Fatal("enabled ufw without default_deny")
	}
}

func TestDefaultDenyEnablesUFWWithSSH(t *testing.T) {
	c := ufwFake(false, "")
	s := Spec{Ports: ports, Client: true, OpenEphemeral: true, ClusterCIDRs: []string{"10.8.0.0/24"},
		DefaultDeny: true, SSHPorts: []int{2200}}
	if _, err := Configure(c, s); err != nil {
		t.Fatal(err)
	}
	cmds := strings.Join(c.Commands, "\n")
	ssh := strings.Index(cmds, "ufw allow 2200/tcp")
	ssh22 := strings.Index(cmds, "ufw allow 22/tcp")
	deny := strings.Index(cmds, "ufw default deny incoming")
	enable := strings.Index(cmds, "ufw --force enable")
	if ssh < 0 || ssh22 < 0 || deny < ssh || enable < deny {
		t.Fatalf("SSH must be allowed before deny and enable:\n%s", cmds)
	}
	for _, want := range []string{
		"ufw route allow out on cni0 from 10.8.0.0/24 comment 'norca:fwd:10.8.0.0/24'",
		"ufw route allow in on cni0",
	} {
		if !strings.Contains(cmds, want) {
			t.Fatalf("missing %q:\n%s", want, cmds)
		}
	}
}

func TestDefaultDenyNeedsUFWOrFirewalld(t *testing.T) {
	c := connector.NewFakeConnector()
	c.Failures["command -v"] = errors.New("not found")
	if _, err := Configure(c, Spec{Ports: ports, Server: true, DefaultDeny: true}); err == nil || !strings.Contains(err.Error(), "ufw") {
		t.Fatalf("expected an error, got %v", err)
	}
}

func TestIPTablesTaggedRulesAndWarning(t *testing.T) {
	c := connector.NewFakeConnector()
	c.Handler = func(cmd string) (string, error, bool) {
		switch {
		case strings.HasPrefix(cmd, "command -v ufw"), strings.HasPrefix(cmd, "command -v firewall-cmd"):
			return "", errors.New("not found"), true
		case cmd == "iptables -S INPUT":
			return "-P INPUT ACCEPT\n-A INPUT -s 10.9.0.0/24 -p tcp -m tcp --dport 4647 -m comment --comment norca:4647/tcp:10.9.0.0/24 -j ACCEPT", nil, true
		case cmd == "iptables -S INPUT | head -1":
			return "-P INPUT ACCEPT", nil, true
		}
		return "", nil, true
	}
	res, err := Configure(c, Spec{Ports: ports, Server: true, ClusterCIDRs: []string{"10.8.0.0/24"}})
	if err != nil || res.Backend != "iptables" {
		t.Fatalf("res=%v err=%v", res, err)
	}
	cmds := strings.Join(c.Commands, "\n")
	if !strings.Contains(cmds, "iptables -I INPUT -s 10.8.0.0/24 -p tcp --dport 4647 -m comment --comment norca:4647/tcp:10.8.0.0/24 -j ACCEPT") {
		t.Fatalf("missing restricted rule:\n%s", cmds)
	}
	if !strings.Contains(cmds, "iptables -D INPUT -s 10.9.0.0/24 -p tcp -m tcp --dport 4647 -m comment --comment norca:4647/tcp:10.9.0.0/24 -j ACCEPT") {
		t.Fatalf("stale rule not removed:\n%s", cmds)
	}
	if len(res.Warnings) == 0 || !strings.Contains(strings.Join(res.Warnings, " "), "INPUT policy is ACCEPT") {
		t.Fatalf("expected the ACCEPT policy warning, got %v", res.Warnings)
	}
}

func TestFirewalldRichRules(t *testing.T) {
	c := connector.NewFakeConnector()
	c.Handler = func(cmd string) (string, error, bool) {
		switch {
		case strings.HasPrefix(cmd, "command -v ufw"):
			return "", errors.New("not found"), true
		case strings.HasPrefix(cmd, "command -v firewall-cmd"):
			return "running", nil, true
		case cmd == "firewall-cmd --permanent --list-ports":
			return "22/tcp 4647/tcp 20000-32000/tcp", nil, true
		}
		return "", nil, true
	}
	if _, err := Configure(c, Spec{Ports: ports, Server: true, ClusterCIDRs: []string{"10.8.0.0/24"}}); err != nil {
		t.Fatal(err)
	}
	cmds := strings.Join(c.Commands, "\n")
	for _, want := range []string{
		`--add-rich-rule='rule family="ipv4" source address="10.8.0.0/24" port port="4647" protocol="tcp" accept'`,
		"firewall-cmd --permanent --remove-port=4647/tcp",
		"firewall-cmd --permanent --remove-port=20000-32000/tcp",
	} {
		if !strings.Contains(cmds, want) {
			t.Fatalf("missing %q:\n%s", want, cmds)
		}
	}
	if strings.Contains(cmds, "--remove-port=22/tcp") {
		t.Fatal("removed a port the deployer does not own")
	}
}

func TestExtraPorts(t *testing.T) {
	s := Spec{Ports: ports, Client: true, Extra: []Extra{
		{Port: "4222", Proto: "tcp", Sources: []string{"10.8.0.0/24"}},
		{Port: "7422:7423", Proto: "udp"},
	}}
	var ids []string
	for _, r := range s.Rules() {
		ids = append(ids, r.ID())
	}
	if got := strings.Join(ids, " "); got != "norca:4222/tcp:10.8.0.0/24 norca:7422:7423/udp:any" {
		t.Fatalf("rules: %s", got)
	}
}

func TestCustomDynamicRange(t *testing.T) {
	s := Spec{Ports: ports, Client: true, OpenEphemeral: true, DynamicMin: 21000, DynamicMax: 29000}
	if r := s.Rules(); r[0].ID() != "norca:21000:29000/tcp:any" {
		t.Fatalf("rules: %v", r)
	}
}

func TestRulesBusPorts(t *testing.T) {
	// Servers open the bus ports to the cluster; clients don't.
	p := ports
	p.Bus, p.BusCluster = 4649, 4650
	s := Spec{Ports: p, Server: true, ClusterCIDRs: []string{"10.8.0.0/24"}}
	var ids []string
	for _, r := range s.Rules() {
		ids = append(ids, r.ID())
	}
	got := strings.Join(ids, " ")
	for _, want := range []string{"norca:4649/tcp:10.8.0.0/24", "norca:4650/tcp:10.8.0.0/24"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rules %s lack %s", got, want)
		}
	}
	s = Spec{Ports: p, Client: true, ClusterCIDRs: []string{"10.8.0.0/24"}}
	for _, r := range s.Rules() {
		if r.Port == "4649" || r.Port == "4650" {
			t.Fatalf("client opens the bus port: %s", r.ID())
		}
	}
}
