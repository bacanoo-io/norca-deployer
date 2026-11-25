package engine

import (
	"os"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/artifacts"
	"github.com/bacanoo-io/norca-deployer/internal/connector"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// A new norca binary on a Windows node is staged as norca.exe.new (a
// running .exe cannot be replaced) and needs the restart that swaps it in:
// the node is changed although norca.exe itself is not.
func TestWindowsBinaryNeedsTheRestart(t *testing.T) {
	plan := loadPlan(t, `
metadata: {name: prod, region: eu}
`+officialRelease+`
inventory:
  masters:
    - host: 10.0.0.1
      hostname: s1
  clients:
    - host: 10.0.0.9
      hostname: w1
      platform: windows
`)
	equivArtifacts(t, plan)
	r := NewRunner(plan, nil)
	r.artifacts = artifacts.NewResolver(plan, t.TempDir())
	h := &equivHost{files: map[string][]byte{}, modes: map[string]os.FileMode{}}
	r.connect = func(*config.NodeConfig) (connector.Connector, error) { return h, nil }
	tg := r.targets(plan.Inventory.Clients, false)[0]

	for run := 1; run <= 2; run++ {
		h.fresh = run == 1
		node := &target{node: tg.node, paths: tg.paths}
		if err := r.prepareNode(node); err != nil {
			t.Fatal(err)
		}
		if !node.changed {
			t.Fatalf("run %d: the staged binary does not restart the agent", run)
		}
	}
	if _, ok := h.files[tg.paths.BinDir+`\norca.exe`+connector.PendingSuffix]; !ok {
		t.Fatalf("no staged binary: %v", h.files)
	}
}
