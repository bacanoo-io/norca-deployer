package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// fakeCluster answers the commands of the rolling restart for several nodes
// and records them in one global order.
type fakeCluster struct {
	mu        sync.Mutex
	events    []string
	unhealthy map[string]bool
	peers     int
	// onHealth runs when a node answers the health check (a test interrupts there).
	onHealth func(host string)
}

func (c *fakeCluster) conn(host string) *connector.FakeConnector {
	f := connector.NewFakeConnector()
	f.Handler = func(cmd string) (string, error, bool) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case strings.HasPrefix(cmd, "systemctl restart norca"):
			c.events = append(c.events, "restart "+host)
			return "", nil, true
		case strings.Contains(cmd, "/v1/agent/health"):
			c.events = append(c.events, "health "+host)
			if c.onHealth != nil && !c.unhealthy[host] {
				c.onHealth(host)
			}
			if c.unhealthy[host] {
				return "< HTTP/1.1 500 Internal Server Error\n{\"server\":{\"ok\":false,\"message\":\"down\"}}", nil, true
			}
			return "< HTTP/1.1 200 OK\n{\"server\":{\"ok\":true},\"client\":{\"ok\":true}}", nil, true
		case strings.Contains(cmd, "/v1/status/leader"):
			return "< HTTP/1.1 200 OK\n\"10.0.0.1:4647\"", nil, true
		case strings.Contains(cmd, "/v1/status/peers"):
			var peers []string
			for i := range c.peers {
				peers = append(peers, fmt.Sprintf("\"10.0.0.%d:4647\"", i+1))
			}
			return "< HTTP/1.1 200 OK\n[" + strings.Join(peers, ",") + "]", nil, true
		}
		return "", nil, true
	}
	return f
}

func testRunner(t *testing.T, masters, clients int) (*Runner, []*target, []*target, *fakeCluster) {
	t.Helper()
	old := healthPollInterval
	healthPollInterval = time.Millisecond
	t.Cleanup(func() { healthPollInterval = old })

	plan := &config.DeploymentPlan{}
	for i := range masters {
		plan.Inventory.Masters = append(plan.Inventory.Masters, config.NodeConfig{Host: fmt.Sprintf("10.0.0.%d", i+1)})
	}
	for i := range clients {
		plan.Inventory.Clients = append(plan.Inventory.Clients, config.NodeConfig{Host: fmt.Sprintf("10.0.1.%d", i+1)})
	}
	plan.SetDefaults()
	plan.Upgrade.HealthTimeout = 1

	r := NewRunner(plan, nil)
	cluster := &fakeCluster{unhealthy: map[string]bool{}, peers: masters}
	mk := func(nodes []config.NodeConfig, isMaster bool) []*target {
		var ts []*target
		for i := range nodes {
			n := &nodes[i]
			ts = append(ts, &target{node: n, isMaster: isMaster, paths: storage.ResolveNodePaths(n, plan), conn: cluster.conn(n.Host)})
		}
		return ts
	}
	return r, mk(plan.Inventory.Masters, true), mk(plan.Inventory.Clients, false), cluster
}

func TestRollingRestartOneServerAtATime(t *testing.T) {
	r, masters, _, cluster := testRunner(t, 3, 0)
	for _, m := range masters {
		m.wasRunning = true
	}
	masters[0].changed = true
	masters[2].changed = true

	if err := r.restartMasters(t.Context(), masters); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(cluster.events, ", ")
	want := "restart 10.0.0.1, health 10.0.0.1, restart 10.0.0.3, health 10.0.0.3"
	if got != want {
		t.Fatalf("events:\n got  %s\n want %s", got, want)
	}
}

func TestBootstrapStartsAllServersFirst(t *testing.T) {
	r, masters, _, cluster := testRunner(t, 3, 0)
	if err := r.restartMasters(t.Context(), masters); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(cluster.events[:3], ", ")
	if got != "restart 10.0.0.1, restart 10.0.0.2, restart 10.0.0.3" {
		t.Fatalf("a new cluster must start all servers before waiting: %v", cluster.events)
	}
}

func TestRollingRestartStopsOnUnhealthyServer(t *testing.T) {
	r, masters, _, cluster := testRunner(t, 3, 0)
	for _, m := range masters {
		m.wasRunning = true
		m.changed = true
	}
	cluster.unhealthy["10.0.0.1"] = true

	err := r.restartMasters(t.Context(), masters)
	if err == nil || !strings.Contains(err.Error(), "rolling restart stopped") {
		t.Fatalf("expected the rollout to stop, got %v", err)
	}
	for _, e := range cluster.events {
		if e == "restart 10.0.0.2" {
			t.Fatalf("restarted the next server after an unhealthy one: %v", cluster.events)
		}
	}
}

func TestServerWaitsForAllPeers(t *testing.T) {
	r, masters, _, cluster := testRunner(t, 3, 0)
	for _, m := range masters {
		m.wasRunning = true
	}
	masters[0].changed = true
	cluster.peers = 2 // one server missing

	if err := r.restartMasters(t.Context(), masters); err == nil || !strings.Contains(err.Error(), "2 of 3 raft peers") {
		t.Fatalf("expected a peer count error, got %v", err)
	}
}

func TestClientBatches(t *testing.T) {
	r, _, clients, cluster := testRunner(t, 1, 4)
	r.plan.Upgrade.MaxParallelClients = 2
	for _, c := range clients {
		c.wasRunning = true
		c.changed = true
	}
	clients[3].changed = false

	if err := r.restartClients(t.Context(), clients); err != nil {
		t.Fatal(err)
	}
	restarts := 0
	for _, e := range cluster.events {
		if strings.HasPrefix(e, "restart") {
			restarts++
		}
	}
	if restarts != 3 {
		t.Fatalf("restarts = %d, want 3 (unchanged client kept running): %v", restarts, cluster.events)
	}
}

func TestClientBatchStopsAfterFailure(t *testing.T) {
	r, _, clients, cluster := testRunner(t, 1, 4)
	r.plan.Upgrade.MaxParallelClients = 2
	for _, c := range clients {
		c.wasRunning = true
		c.changed = true
	}
	cluster.unhealthy["10.0.1.1"] = true

	if err := r.restartClients(t.Context(), clients); err == nil {
		t.Fatal("expected an error")
	}
	for _, e := range cluster.events {
		if e == "restart 10.0.1.3" || e == "restart 10.0.1.4" {
			t.Fatalf("second batch restarted after a failed batch: %v", cluster.events)
		}
	}
}

func TestAPIParsesVerboseOutput(t *testing.T) {
	plan := &config.DeploymentPlan{Inventory: config.InventoryConfig{Masters: []config.NodeConfig{{Host: "10.0.0.1"}}}}
	plan.SetDefaults()
	plan.Security.TLSEnabled = true
	node := &plan.Inventory.Masters[0]
	conn := connector.NewFakeConnector()
	conn.Handler = func(cmd string) (string, error, bool) {
		return "> GET /v1/release/x HTTP/1.1\n< HTTP/1.1 403 Forbidden\n< Content-Type: text/plain\nPermission denied", nil, true
	}
	cli := &remoteCLI{conn: conn, plan: plan, node: node, paths: storage.ResolveNodePaths(node, plan), isMaster: true, token: "secret-token"}

	status, body, err := cli.api("GET", "/v1/release/x", nil)
	if err != nil || status != 403 || body != "Permission denied" {
		t.Fatalf("api = %d %q %v", status, body, err)
	}

	env := string(conn.Files[cliStateDir+"/cli.env"])
	for _, want := range []string{"NORCA_ADDR='https://127.0.0.1:4646'", "NORCA_CLIENT_CERT='/etc/norca.d/tls/server.pem'", "NORCA_TOKEN='secret-token'"} {
		if !strings.Contains(env, want) {
			t.Errorf("env file lacks %q:\n%s", want, env)
		}
	}
	for _, c := range conn.Commands {
		if strings.Contains(c, "secret-token") {
			t.Fatalf("token leaked into the command line: %s", c)
		}
	}
}

func TestGenerateGossipAndJoin(t *testing.T) {
	plan := &config.DeploymentPlan{
		Inventory: config.InventoryConfig{
			Masters: []config.NodeConfig{{Host: "192.168.1.10", ClusterAddress: "10.8.0.1", Hostname: "m1"}},
			Clients: []config.NodeConfig{{Host: "192.168.1.20", Hostname: "w1",
				ClientSettings: &config.ClientSettings{Drivers: []string{"docker", "exec", "raw_exec"}, EnableRawExec: false}}},
		},
	}
	plan.SetDefaults()
	m := &plan.Inventory.Masters[0]
	server := GenerateNorcaConfig(m, plan, storage.ResolveNodePaths(m, plan), true, GenOptions{GossipKey: "a2V5"})
	for _, want := range []string{`encrypt          = "a2V5"`, `"10.8.0.1:4648",`, `log_level  = "INFO"`} {
		if !strings.Contains(server, want) {
			t.Errorf("server config lacks %q:\n%s", want, server)
		}
	}

	c := &plan.Inventory.Clients[0]
	client := GenerateNorcaConfig(c, plan, storage.ResolveNodePaths(c, plan), false, GenOptions{GossipKey: "a2V5"})
	for _, want := range []string{`"10.8.0.1:4647",`, `"driver.allowlist" = "docker,exec"`} {
		if !strings.Contains(client, want) {
			t.Errorf("client config lacks %q:\n%s", want, client)
		}
	}
	for _, unwanted := range []string{"encrypt", "raw_exec", "driver.exec.enable"} {
		if strings.Contains(client, unwanted) {
			t.Errorf("client config has %q:\n%s", unwanted, client)
		}
	}

	c.ClientSettings.EnableRawExec = true
	client = GenerateNorcaConfig(c, plan, storage.ResolveNodePaths(c, plan), false, GenOptions{})
	if !strings.Contains(client, "plugin \"raw_exec\" {\n  config {\n    enabled = true") {
		t.Errorf("raw_exec plugin block missing:\n%s", client)
	}
}

// An interrupt while a server restarts: that server is finished (restarted and
// checked), the next one is not touched.
func TestRollingRestartStopsBetweenServersWhenInterrupted(t *testing.T) {
	r, masters, _, cluster := testRunner(t, 3, 0)
	for _, m := range masters {
		m.wasRunning = true
		m.changed = true
	}
	ctx, interrupt := context.WithCancel(t.Context())
	cluster.onHealth = func(host string) {
		if host == "10.0.0.1" {
			interrupt()
		}
	}

	err := r.restartMasters(ctx, masters)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "were not restarted") {
		t.Fatalf("err = %v", err)
	}
	got := strings.Join(cluster.events, ", ")
	if got != "restart 10.0.0.1, health 10.0.0.1" {
		t.Fatalf("events = %s, want only the first server, restarted and checked", got)
	}
}

func TestClientBatchesStopWhenInterrupted(t *testing.T) {
	r, _, clients, cluster := testRunner(t, 1, 4)
	r.plan.Upgrade.MaxParallelClients = 2
	for _, c := range clients {
		c.wasRunning = true
		c.changed = true
	}
	ctx, interrupt := context.WithCancel(t.Context())
	cluster.onHealth = func(string) { interrupt() }

	err := r.restartClients(ctx, clients)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	for _, e := range cluster.events {
		if e == "restart 10.0.1.3" || e == "restart 10.0.1.4" {
			t.Fatalf("the second batch started after the interrupt: %v", cluster.events)
		}
	}
}

// The second interrupt cuts the wait for a node to become healthy.
func TestWaitHealthyIsAbortable(t *testing.T) {
	r, masters, _, cluster := testRunner(t, 1, 0)
	cluster.unhealthy["10.0.0.1"] = true
	r.plan.Upgrade.HealthTimeout = 60
	abort, cut := context.WithCancel(t.Context())
	r.Abort = abort
	cluster.onHealth = nil
	go func() {
		time.Sleep(20 * time.Millisecond)
		cut()
	}()

	start := time.Now()
	err := r.waitHealthy(masters[0], 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the wait did not end with the abort")
	}
}

// Nodes that were not started when the interrupt came are skipped.
func TestPrepareAllSkipsNodesAfterAnInterrupt(t *testing.T) {
	r, masters, _, _ := testRunner(t, 2, 0)
	ctx, interrupt := context.WithCancel(t.Context())
	interrupt()
	connected := 0
	r.connect = func(*config.NodeConfig) (connector.Connector, error) {
		connected++
		return connector.NewFakeConnector(), nil
	}

	r.prepareAll(ctx, masters)
	if connected != 0 {
		t.Fatalf("%d nodes were connected after the interrupt", connected)
	}
	if err := collect(masters); !errors.Is(err, context.Canceled) {
		t.Fatalf("collect = %v, want it to wrap context.Canceled", err)
	}
}
