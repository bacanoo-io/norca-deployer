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

// In a cluster of Linux and Windows nodes, each node runs the scripts of
// its platform: the powershell script on the Windows client, the bash
// script on the Linux server.
func TestScriptsOfEachPlatform(t *testing.T) {
	plan := loadPlan(t, `
metadata: {name: prod, region: eu}
`+officialRelease+`
scripts:
  - {name: lin, phase: pre_install, interpreter: bash, inline: "echo lin"}
  - {name: win, phase: pre_install, interpreter: powershell, inline: "Write-Output win"}
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
	for _, tt := range []struct {
		name     string
		tg       *target
		ran, not string
	}{
		{"windows client", r.targets(plan.Inventory.Clients, false)[0], `C:\ProgramData\norca-deployer\scripts\win.ps1`, "/var/lib/norca-deployer/scripts/lin.sh"},
		{"linux server", r.targets(plan.Inventory.Masters, true)[0], "/var/lib/norca-deployer/scripts/lin.sh", `C:\ProgramData\norca-deployer\scripts\win.ps1`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := &equivHost{files: map[string][]byte{}, modes: map[string]os.FileMode{}, fresh: true, probe: debianProbe}
			r.connect = func(*config.NodeConfig) (connector.Connector, error) { return h, nil }
			if err := r.prepareNode(&target{node: tt.tg.node, paths: tt.tg.paths, isMaster: tt.tg.isMaster}); err != nil {
				t.Fatal(err)
			}
			if _, ok := h.files[tt.ran]; !ok {
				t.Errorf("%s not uploaded", tt.ran)
			}
			if _, ok := h.files[tt.not]; ok {
				t.Errorf("%s uploaded to the wrong platform", tt.not)
			}
		})
	}
}
