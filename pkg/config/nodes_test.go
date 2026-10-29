package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestClientDefaultsMerge(t *testing.T) {
	p := &DeploymentPlan{
		ClientDefaults: &ClientSettings{
			NodeClass:       "general",
			NodePool:        "apps",
			Drivers:         []string{"docker", "exec"},
			Meta:            map[string]string{"zone": "a", "storage": "hdd"},
			Reserved:        &ReservedConfig{CPU: 200, Memory: 256},
			DrainOnShutdown: &DrainOnShutdownConfig{Deadline: "30m"},
		},
		Inventory: InventoryConfig{
			Masters: []NodeConfig{
				{Host: "10.0.0.1"}, // server only: no client settings
				{Host: "10.0.0.2", ClientSettings: &ClientSettings{}}, // server with client
			},
			Clients: []NodeConfig{
				{Host: "10.0.1.1"},
				{Host: "10.0.1.2", ClientSettings: &ClientSettings{
					NodeClass: "gpu",
					Drivers:   []string{"exec"},
					Meta:      map[string]string{"storage": "ssd", "gpu": "a100"},
				}},
			},
		},
	}
	p.SetDefaults()

	if p.Inventory.Masters[0].ClientSettings != nil {
		t.Fatal("a server without client_settings got a client")
	}
	if cs := p.Inventory.Masters[1].ClientSettings; cs.NodePool != "apps" || cs.Reserved == nil {
		t.Fatalf("server with client: %+v", cs)
	}
	plain := p.Inventory.Clients[0].ClientSettings
	if plain.NodeClass != "general" || !reflect.DeepEqual(plain.Drivers, []string{"docker", "exec"}) ||
		plain.Meta["zone"] != "a" || plain.DrainOnShutdown.Deadline != "30m" {
		t.Fatalf("defaults not applied: %+v", plain)
	}
	gpu := p.Inventory.Clients[1].ClientSettings
	want := map[string]string{"zone": "a", "storage": "ssd", "gpu": "a100"}
	if gpu.NodeClass != "gpu" || gpu.NodePool != "apps" || !reflect.DeepEqual(gpu.Drivers, []string{"exec"}) || !reflect.DeepEqual(gpu.Meta, want) {
		t.Fatalf("node values must win, meta merged: %+v", gpu)
	}
	// The defaults themselves are not changed by the merge.
	if p.ClientDefaults.Meta["storage"] != "hdd" {
		t.Fatal("client_defaults.meta was modified")
	}
}

func TestNodeSettingsValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		cs   ClientSettings
		pool []NodePoolConfig
		dyn  *PortRange
		want string
	}{
		"meta key":        {cs: ClientSettings{Meta: map[string]string{"bad key": "x"}}, want: "meta key"},
		"pool all":        {cs: ClientSettings{NodePool: "all"}, want: "node_pool"},
		"host network":    {cs: ClientSettings{HostNetworks: []HostNetworkConfig{{Name: "public"}}}, want: "interface or cidr"},
		"bad cidr":        {cs: ClientSettings{HostNetworks: []HostNetworkConfig{{Name: "p", CIDR: "10.0.0.0/33"}}}, want: "host network p"},
		"reserved ports":  {cs: ClientSettings{Reserved: &ReservedConfig{ReservedPorts: "22;80"}}, want: "reserved_ports"},
		"drain deadline":  {cs: ClientSettings{DrainOnShutdown: &DrainOnShutdownConfig{Deadline: "soon"}}, want: "deadline"},
		"pool definition": {pool: []NodePoolConfig{{Name: "all"}}, want: "node_pools[0]"},
		"port range":      {dyn: &PortRange{Min: 30000, Max: 20000}, want: "dynamic_ports"},
	} {
		cs := tc.cs
		p := &DeploymentPlan{
			Network:   NetworkConfig{BindAddress: "0.0.0.0", DynamicPorts: tc.dyn},
			NodePools: tc.pool,
			Inventory: InventoryConfig{Masters: []NodeConfig{{Host: "10.0.0.1"}}, Clients: []NodeConfig{{Host: "10.0.1.1", ClientSettings: &cs}}},
		}
		p.SetDefaults()
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
}
