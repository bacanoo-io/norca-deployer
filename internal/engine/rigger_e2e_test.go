// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package engine

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
	"github.com/bacanoo-io/norca-deployer/internal/state"
)

// localState runs the commands of the deployer on this machine, with the
// state directory of the CLI moved into a directory of the test.
type localState struct {
	*connector.LocalConnector

	dir string
}

func (l *localState) move(s string) string { return strings.ReplaceAll(s, cliStateDir, l.dir) }

func (l *localState) ExecuteCommand(cmd string) (string, error) {
	return l.LocalConnector.ExecuteCommand(l.move(cmd))
}

func (l *localState) CopyFile(src io.Reader, dest string, mode os.FileMode) error {
	return l.LocalConnector.CopyFile(src, l.move(dest), mode)
}

func (l *localState) MkdirAll(dir string, mode os.FileMode) error {
	return l.LocalConnector.MkdirAll(l.move(dir), mode)
}

// TestDeployRiggerAgainstAgent applies a stack with the rigger program through
// the proxy and the norca CLI to a running agent without ACLs (norca agent
// -dev): NORCA_DEPLOYER_RIGGER_E2E=1, with norca and rigger on the PATH.
func TestDeployRiggerAgainstAgent(t *testing.T) {
	if os.Getenv("NORCA_DEPLOYER_RIGGER_E2E") == "" {
		t.Skip("NORCA_DEPLOYER_RIGGER_E2E is not set")
	}

	norca, err := exec.LookPath("norca")
	if err != nil {
		t.Fatal(err)
	}

	riggerBin, err := exec.LookPath("rigger")
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	plan := `
metadata: {name: dev, region: global}
release: {version: v0.2.0}
rigger:
  binary: ` + riggerBin + `
  stack_hcl: |
    rigger {
      name      = "deployer-e2e"
      namespace = "default"
    }

    module "cache" {
      source  = "builtin/redis"
      version = "~> 0.3"
      wait    = false
    }
inventory:
  masters:
    - {host: 127.0.0.1, hostname: s1}
`

	path := filepath.Join(dir, "plan.yaml")
	if err := os.WriteFile(path, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}

	p := loadPlanFile(t, path)
	r := NewRunner(p, nil)

	if r.store, err = state.Open(p, nil); err != nil {
		t.Fatal(err)
	}

	m := &p.Inventory.Masters[0]
	paths := storage.ResolveNodePaths(m, p)
	paths.BinDir = filepath.Dir(norca)
	conn := &localState{LocalConnector: connector.NewLocalConnector(context.Background()), dir: filepath.Join(dir, "cli")}
	first := &target{node: m, isMaster: true, paths: paths, conn: conn}

	t.Cleanup(func() {
		cmd := exec.Command(riggerBin, "stack", "destroy", r.store.Path("rigger-stack"))
		cmd.Env = os.Environ()
		out, _ := cmd.CombinedOutput()
		t.Logf("destroy: %s", out)
	})

	if err := r.deployRigger(first, ""); err != nil {
		t.Fatal(err)
	}

	// A second apply finds the release unchanged.
	if err := r.deployRigger(first, ""); err != nil {
		t.Fatal(err)
	}
}
