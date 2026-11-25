package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/platform/dns"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// dnsNode answers the run of the DNS script with out (and err) and ufw as
// active, so that the firewall rules show in the commands.
func dnsNode(out string, err error) *connector.FakeConnector {
	f := connector.NewFakeConnector()
	f.Handler = func(cmd string) (string, error, bool) {
		switch {
		case strings.HasSuffix(cmd, " sh "+dns.ScriptPath):
			return out, err, true
		case strings.HasPrefix(cmd, "if [ -e "):
			return "no\n", nil, true
		case strings.HasPrefix(cmd, "command -v ufw >/dev/null && ufw status"):
			return "Status: active", nil, true
		}
		return "", nil, true
	}
	return f
}

func dnsRunner(t *testing.T, fwd *config.ConsulDNSForwarding) *Runner {
	t.Helper()
	plan := &config.DeploymentPlan{Inventory: config.InventoryConfig{Masters: []config.NodeConfig{{Host: "10.0.0.1"}}}}
	plan.Consul = &config.ConsulConfig{DNSForwarding: fwd}
	plan.Tuning.ApplyFirewallRules = true
	plan.SetDefaults()
	return NewRunner(plan, nil)
}

func TestConfigureHostDNS(t *testing.T) {
	r := dnsRunner(t, &config.ConsulDNSForwarding{Enabled: true, Docker: true})
	conn := dnsNode("method systemd-resolved\nunchanged "+dns.ResolvedDropIn+"\nbridge 172.17.0.1/16\n", nil)
	tg := &target{node: &r.plan.Inventory.Masters[0], isMaster: true, conn: conn}
	if err := r.configureHostDNS(tg); err != nil {
		t.Fatal(err)
	}
	if tg.changed {
		t.Fatal("the DNS step marked the agent for a restart")
	}
	if len(conn.CommandsWithPrefix("NCD_ENABLED='1' ")) != 1 {
		t.Fatalf("commands = %q, want one run of the script", conn.Commands)
	}

	// The firewall lets the containers of docker0 ask the stub.
	if err := r.configureFirewall(tg); err != nil {
		t.Fatal(err)
	}
	cmds := strings.Join(conn.Commands, "\n")
	for _, want := range []string{
		"ufw allow from 172.17.0.0/16 to any port 53 proto udp comment 'norca:53/udp:172.17.0.0/16'",
		"ufw allow from 172.17.0.0/16 to any port 53 proto tcp comment 'norca:53/tcp:172.17.0.0/16'",
	} {
		if !strings.Contains(cmds, want) {
			t.Errorf("missing %q:\n%s", want, cmds)
		}
	}
}

func TestConfigureHostDNSError(t *testing.T) {
	r := dnsRunner(t, &config.ConsulDNSForwarding{Enabled: true})
	conn := dnsNode("error has no local resolver the deployer can forward the consul domain from: systemd-resolved not installed\n", errors.New("exit status 1"))
	tg := &target{node: &r.plan.Inventory.Masters[0], isMaster: true, conn: conn}
	err := r.configureHostDNS(tg)
	if err == nil || !strings.Contains(err.Error(), "consul.dns_forwarding: 10.0.0.1 has no local resolver") {
		t.Fatalf("configureHostDNS() = %v", err)
	}
}

// Without the block a node that never had the forwarding only gets the look
// for old files; a Windows node gets nothing.
func TestConfigureHostDNSOff(t *testing.T) {
	r := dnsRunner(t, nil)
	conn := dnsNode("", nil)
	tg := &target{node: &r.plan.Inventory.Masters[0], isMaster: true, conn: conn}
	if err := r.configureHostDNS(tg); err != nil {
		t.Fatal(err)
	}
	if len(conn.Commands) != 1 || len(conn.Files) != 0 || len(tg.hostDNSSources) != 0 {
		t.Fatalf("commands %q, files %v", conn.Commands, conn.Files)
	}

	r = dnsRunner(t, &config.ConsulDNSForwarding{Enabled: true})
	r.plan.Inventory.Masters[0].Platform = "windows"
	conn = dnsNode("", nil)
	tg = &target{node: &r.plan.Inventory.Masters[0], isMaster: true, conn: conn}
	if err := r.configureHostDNS(tg); err != nil {
		t.Fatal(err)
	}
	if len(conn.Commands) != 0 {
		t.Fatalf("commands on a Windows node: %q", conn.Commands)
	}
}
