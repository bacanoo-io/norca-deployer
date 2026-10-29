package engine

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/connector"
	"github.com/bacanoo-io/norca-deployer/pkg/platform/storage"
	"github.com/bacanoo-io/norca-deployer/pkg/state"
)

func installedWith(nodes map[string]string) *state.Installed {
	inst := &state.Installed{Nodes: map[string]*state.InstalledNode{}}
	for name, role := range nodes {
		inst.Nodes[name] = &state.InstalledNode{Role: role, Node: config.NodeConfig{
			Host: "10.9.0." + name[len(name)-1:], Hostname: name,
			Connection: config.ConnectionConfig{Mode: "ssh", SSHUser: "root"},
		}}
	}
	return inst
}

func planWith(masters, clients []string, decom ...config.DecommissionNode) *config.DeploymentPlan {
	p := &config.DeploymentPlan{}
	for _, m := range masters {
		p.Inventory.Masters = append(p.Inventory.Masters, config.NodeConfig{Host: "h-" + m, Hostname: m})
	}
	for _, c := range clients {
		p.Inventory.Clients = append(p.Inventory.Clients, config.NodeConfig{Host: "h-" + c, Hostname: c})
	}
	p.Inventory.Decommission = decom
	p.SetDefaults()
	return p
}

func TestNodeDeletedFromThePlanStopsApply(t *testing.T) {
	inst := installedWith(map[string]string{"s1": "master", "w1": "client", "w2": "client"})
	plan := planWith([]string{"s1"}, []string{"w1"})
	decom, _, err := resolveDecommission(plan, inst)
	if err != nil {
		t.Fatal(err)
	}
	err = checkInventory(plan, inst, decom)
	if err == nil || !strings.Contains(err.Error(), "w2") || !strings.Contains(err.Error(), "decommission") {
		t.Fatalf("expected the missing node w2 to stop the apply, got %v", err)
	}
}

func TestDecommissionByNameUsesTheInstalledNode(t *testing.T) {
	inst := installedWith(map[string]string{"s1": "master", "w2": "client"})
	plan := planWith([]string{"s1"}, nil, config.DecommissionNode{NodeConfig: config.NodeConfig{Hostname: "w2"}, PurgeData: true})
	decom, _, err := resolveDecommission(plan, inst)
	if err != nil {
		t.Fatal(err)
	}
	if len(decom) != 1 || decom[0].Role != "client" || decom[0].Host != "10.9.0.2" || decom[0].Connection.SSHUser != "root" || !decom[0].PurgeData {
		t.Fatalf("decommission entry not completed from the installed list: %+v", decom)
	}
	if err := checkInventory(plan, inst, decom); err != nil {
		t.Fatal(err)
	}
}

func TestDecommissionedEntryIsSkippedLater(t *testing.T) {
	inst := installedWith(map[string]string{"s1": "master"})
	plan := planWith([]string{"s1"}, nil, config.DecommissionNode{NodeConfig: config.NodeConfig{Hostname: "w2"}})
	decom, done, err := resolveDecommission(plan, inst)
	if err != nil || len(decom) != 0 || len(done) != 1 || done[0] != "w2" {
		t.Fatalf("decom=%v done=%v err=%v", decom, done, err)
	}
}

func TestUnknownDecommissionNeedsRole(t *testing.T) {
	inst := installedWith(nil)
	plan := planWith([]string{"s1"}, nil, config.DecommissionNode{NodeConfig: config.NodeConfig{
		Host: "10.0.0.9", Hostname: "old", Connection: config.ConnectionConfig{SSHUser: "root"}}})
	if _, _, err := resolveDecommission(plan, inst); err == nil || !strings.Contains(err.Error(), "set role") {
		t.Fatalf("expected a role error, got %v", err)
	}
}

func TestRoleChangeIsRefused(t *testing.T) {
	inst := installedWith(map[string]string{"s1": "master", "w1": "client"})
	plan := planWith([]string{"s1", "w1"}, nil)
	if err := checkInventory(plan, inst, nil); err == nil || !strings.Contains(err.Error(), "w1 (client -> master)") {
		t.Fatalf("expected a role change error, got %v", err)
	}
}

// fakeAPI answers the API calls of the first server and records them.
type fakeAPI struct {
	mu     sync.Mutex
	events []string
	peers  int
}

func (a *fakeAPI) conn() *connector.FakeConnector {
	f := connector.NewFakeConnector()
	f.Handler = func(cmd string) (string, error, bool) {
		a.mu.Lock()
		defer a.mu.Unlock()
		ok := func(body string) (string, error, bool) { return "< HTTP/1.1 200 OK\n" + body, nil, true }
		switch {
		case strings.Contains(cmd, "'/v1/nodes'"):
			return ok(`[{"ID":"n2","Name":"w2"}]`)
		case strings.Contains(cmd, "/v1/node/n2/drain"):
			a.events = append(a.events, "drain")
			return ok(`{}`)
		case strings.Contains(cmd, "'/v1/node/n2'"):
			a.events = append(a.events, "drain-status")
			return ok(`{"ID":"n2","DrainStrategy":null}`)
		case strings.Contains(cmd, "/v1/client/gc"):
			a.events = append(a.events, "gc")
			return ok(`{}`)
		case strings.Contains(cmd, "/v1/node/n2/purge"):
			a.events = append(a.events, "purge")
			return ok(`{}`)
		case strings.Contains(cmd, "/v1/operator/raft/configuration"):
			return ok(`{"Servers":[{"ID":"a","Node":"s1.global"},{"ID":"c","Node":"s3.global"}]}`)
		case strings.Contains(cmd, "/v1/operator/raft/peer"):
			a.events = append(a.events, "remove-peer "+cmd[strings.Index(cmd, "id="):strings.Index(cmd, "id=")+4])
			a.peers--
			return ok(``)
		case strings.Contains(cmd, "/v1/agent/force-leave"):
			a.events = append(a.events, "force-leave")
			return ok(``)
		case strings.Contains(cmd, "/v1/status/peers"):
			peers := make([]string, a.peers)
			for i := range peers {
				peers[i] = `"x"`
			}
			return ok("[" + strings.Join(peers, ",") + "]")
		}
		return "", nil, true
	}
	return f
}

func decomRunner(t *testing.T, plan *config.DeploymentPlan, inst *state.Installed, api *fakeAPI) (*Runner, *target, map[string]*connector.FakeConnector) {
	t.Helper()
	old := healthPollInterval
	healthPollInterval = time.Millisecond
	t.Cleanup(func() { healthPollInterval = old })

	plan.Upgrade.HealthTimeout = 1
	r := NewRunner(plan, nil)
	r.store = &state.Store{Dir: t.TempDir()}
	saved, err := r.store.LoadInstalled(plan.Metadata.Region)
	if err != nil {
		t.Fatal(err)
	}
	saved.Nodes = inst.Nodes // same map: the test sees the removals
	r.installed = saved
	if r.decom, r.decomDone, err = resolveDecommission(plan, inst); err != nil {
		t.Fatal(err)
	}
	nodes := map[string]*connector.FakeConnector{}
	r.connect = func(n *config.NodeConfig) (connector.Connector, error) {
		f := connector.NewFakeConnector()
		nodes[n.Hostname] = f
		api.mu.Lock()
		api.events = append(api.events, "uninstall "+n.Hostname)
		api.mu.Unlock()
		return f, nil
	}
	m := &plan.Inventory.Masters[0]
	first := &target{node: m, isMaster: true, paths: storage.ResolveNodePaths(m, plan), conn: api.conn()}
	return r, first, nodes
}

func TestDecommissionClientDrainsBeforeUninstallAndPurge(t *testing.T) {
	inst := installedWith(map[string]string{"s1": "master", "w2": "client"})
	plan := planWith([]string{"s1"}, nil, config.DecommissionNode{NodeConfig: config.NodeConfig{Hostname: "w2"}, PurgeData: true})
	api := &fakeAPI{}
	r, first, nodes := decomRunner(t, plan, inst, api)

	if err := r.decommission(first, ""); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(api.events, ", ")
	if got != "drain, drain-status, gc, uninstall w2, purge" {
		t.Fatalf("events: %s", got)
	}
	script := strings.Join(nodes["w2"].Commands, "\n")
	for _, want := range []string{"systemctl disable --now norca", "docker rm -f $ids", "rm -f '/usr/local/bin/norca'", "rm -rf '/var/lib/norca'"} {
		if !strings.Contains(script, want) {
			t.Errorf("uninstall script lacks %q:\n%s", want, script)
		}
	}
	if _, e := inst.Lookup("w2"); e != nil {
		t.Fatal("the decommissioned node is still in the installed list")
	}
}

func TestDecommissionServerLeavesRaftAfterStop(t *testing.T) {
	inst := installedWith(map[string]string{"s1": "master", "s3": "master"})
	plan := planWith([]string{"s1"}, nil, config.DecommissionNode{NodeConfig: config.NodeConfig{Hostname: "s3"}})
	api := &fakeAPI{peers: 2}
	r, first, nodes := decomRunner(t, plan, inst, api)

	if err := r.decommission(first, ""); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(api.events, ", ")
	if got != "uninstall s3, remove-peer id=c, force-leave" {
		t.Fatalf("events: %s", got)
	}
	if strings.Contains(strings.Join(nodes["s3"].Commands, "\n"), "rm -rf '/var/lib/norca'") {
		t.Fatal("data removed without purge_data")
	}
}

func TestDecommissionNeedsTokenWithACLs(t *testing.T) {
	inst := installedWith(map[string]string{"s1": "master", "w2": "client"})
	plan := planWith([]string{"s1"}, nil, config.DecommissionNode{NodeConfig: config.NodeConfig{Hostname: "w2"}})
	plan.Security.ACL = &config.ACLConfig{Enabled: true, ManagementTokenOutput: "token"}
	r, first, _ := decomRunner(t, plan, inst, &fakeAPI{})
	if err := r.decommission(first, ""); err == nil || !strings.Contains(err.Error(), "management token") {
		t.Fatalf("expected a token error, got %v", err)
	}
}
