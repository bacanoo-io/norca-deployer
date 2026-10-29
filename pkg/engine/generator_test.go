package engine

import (
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/platform/storage"
)

func TestGenerateBackupBlock(t *testing.T) {
	plan := &config.DeploymentPlan{
		Inventory: config.InventoryConfig{Masters: []config.NodeConfig{{Host: "10.0.0.1", Hostname: "m1"}}},
		Backup: &config.BackupConfig{
			Enabled:     true,
			Schedule:    "0 * * * *",
			Destination: config.BackupDestinationConfig{Name: "local", Type: "directory", Path: "/srv/backups"},
			Encryption:  config.BackupEncryptionConfig{Recipients: []string{"age1abc"}},
			Retention:   &config.BackupRetentionConfig{KeepDaily: 7, KeepMonthly: 6},
		},
	}
	plan.SetDefaults()
	node := &plan.Inventory.Masters[0]
	hcl := GenerateNorcaConfig(node, plan, storage.ResolveNodePaths(node, plan), true, GenOptions{})

	for _, want := range []string{
		`path             = "/srv/backups"`,
		`recipients = [`,
		`"age1abc",`,
		`keep_daily   = 7`,
		`keep_monthly = 6`,
	} {
		if !strings.Contains(hcl, want) {
			t.Errorf("generated config lacks %q:\n%s", want, hcl)
		}
	}
	for _, unwanted := range []string{"keep_hourly", "keep_weekly", "force_path_style"} {
		if strings.Contains(hcl, unwanted) {
			t.Errorf("generated config has %q for a directory destination with unset values", unwanted)
		}
	}
}

func TestGenerateBindAddresses(t *testing.T) {
	gen := func(net config.NetworkConfig) string {
		plan := &config.DeploymentPlan{
			Network:   net,
			Inventory: config.InventoryConfig{Masters: []config.NodeConfig{{Host: "203.0.113.10", ClusterAddress: "10.8.0.10", Hostname: "m1"}}},
		}
		plan.SetDefaults()
		node := &plan.Inventory.Masters[0]
		return GenerateNorcaConfig(node, plan, storage.ResolveNodePaths(node, plan), true, GenOptions{})
	}

	// Default: cluster address only, HTTP also on loopback.
	hcl := gen(config.NetworkConfig{})
	for _, want := range []string{`http = "127.0.0.1 10.8.0.10"`, `rpc  = "10.8.0.10"`, `serf = "10.8.0.10"`} {
		if !strings.Contains(hcl, want) {
			t.Errorf("default bind lacks %q:\n%s", want, hcl)
		}
	}
	if strings.Contains(hcl, "bind_addr") {
		t.Errorf("default bind must not set bind_addr:\n%s", hcl)
	}

	// Interface match: the same template as the advertise address.
	hcl = gen(config.NetworkConfig{InterfacePrefix: "wg"})
	if !strings.Contains(hcl, `http = "127.0.0.1 {{ GetAllInterfaces | include \"name\" \"^wg\"`) {
		t.Errorf("interface bind missing:\n%s", hcl)
	}

	// Explicit bind_address keeps the old behavior.
	hcl = gen(config.NetworkConfig{BindAddress: "0.0.0.0"})
	if !strings.Contains(hcl, `bind_addr  = "0.0.0.0"`) || strings.Contains(hcl, "addresses {") {
		t.Errorf("explicit bind_address not used:\n%s", hcl)
	}
}

func TestGenerateClientLabels(t *testing.T) {
	plan := &config.DeploymentPlan{
		Network: config.NetworkConfig{BindAddress: "0.0.0.0", DynamicPorts: &config.PortRange{Min: 21000, Max: 29000}},
		ClientDefaults: &config.ClientSettings{
			Meta:     map[string]string{"zone": "a", "rack": "r1"},
			NodePool: "apps",
		},
		Inventory: config.InventoryConfig{
			Masters: []config.NodeConfig{{Host: "10.0.0.1", Hostname: "s1", ServerSettings: &config.ServerSettings{ExtraHCL: "num_schedulers = 2"}}},
			Clients: []config.NodeConfig{{Host: "10.0.1.1", Hostname: "w1", ClientSettings: &config.ClientSettings{
				NodeClass:       "gpu",
				HostNetworks:    []config.HostNetworkConfig{{Name: "public", Interface: "eth0", ReservedPorts: "22"}},
				Reserved:        &config.ReservedConfig{CPU: 200, Memory: 256, ReservedPorts: "22"},
				DrainOnShutdown: &config.DrainOnShutdownConfig{Force: true},
				ExtraHCL:        "max_kill_timeout = \"60s\"",
			}}},
		},
	}
	plan.SetDefaults()
	node := &plan.Inventory.Clients[0]
	hcl := GenerateNorcaConfig(node, plan, storage.ResolveNodePaths(node, plan), false, GenOptions{})
	for _, want := range []string{
		`node_class = "gpu"`,
		`node_pool  = "apps"`,
		"min_dynamic_port = 21000",
		"max_dynamic_port = 29000",
		"meta {\n    \"rack\" = \"r1\"\n    \"zone\" = \"a\"\n  }",
		"host_network \"public\" {\n    interface = \"eth0\"\n    reserved_ports = \"22\"\n  }",
		"reserved {\n    cpu = 200\n    memory = 256\n    reserved_ports = \"22\"\n  }",
		`deadline           = "1h"`,
		`force              = true`,
		`  max_kill_timeout = "60s"`,
	} {
		if !strings.Contains(hcl, want) {
			t.Errorf("client config lacks %q:\n%s", want, hcl)
		}
	}

	server := &plan.Inventory.Masters[0]
	hcl = GenerateNorcaConfig(server, plan, storage.ResolveNodePaths(server, plan), true, GenOptions{})
	if !strings.Contains(hcl, "  num_schedulers = 2") || strings.Contains(hcl, "client {") {
		t.Errorf("server config: extra_hcl missing or a client block on a server only node:\n%s", hcl)
	}

	// Without network.dynamic_ports the agent default stays (no restart of
	// existing clusters).
	plan.Network.DynamicPorts = nil
	hcl = GenerateNorcaConfig(node, plan, storage.ResolveNodePaths(node, plan), false, GenOptions{})
	if strings.Contains(hcl, "dynamic_port") {
		t.Errorf("dynamic ports written without a configured range")
	}
}
