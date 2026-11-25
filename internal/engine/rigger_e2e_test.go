// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package engine

import (
	"cmp"
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
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
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

func (l *localState) ReadFile(p string) ([]byte, error) {
	return l.LocalConnector.ReadFile(l.move(p))
}

func (l *localState) FileExists(p string) (bool, error) {
	return l.LocalConnector.FileExists(l.move(p))
}

func (l *localState) MkdirAll(dir string, mode os.FileMode) error {
	return l.LocalConnector.MkdirAll(l.move(dir), mode)
}

// TestDeployRiggerAgainstAgent applies stacks to a running agent without
// ACLs (norca agent -dev), in the four ways of the rigger section: with
// norca rigger of the first server, which is this machine here, on a copy
// of the stack, and with the rigger program through the proxy and the norca
// CLI; applied by the servers and by the program. Each way applies a stack
// written in the plan, and a stack directory with a value file from beside
// it and a variable. NORCA_DEPLOYER_RIGGER_E2E=1, with norca and rigger on
// the PATH; for an agent of another region than global (norca agent -dev
// -region eu), NORCA_REGION names it, and the stacks name it as well.
func TestDeployRiggerAgainstAgent(t *testing.T) {
	if os.Getenv("NORCA_DEPLOYER_RIGGER_E2E") == "" {
		t.Skip("NORCA_DEPLOYER_RIGGER_E2E is not set")
	}

	region := cmp.Or(os.Getenv("NORCA_REGION"), "global")

	norca, err := exec.LookPath("norca")
	if err != nil {
		t.Fatal(err)
	}

	riggerBin, err := exec.LookPath("rigger")
	if err != nil {
		t.Fatal(err)
	}

	for _, runOn := range []string{config.RiggerOnServer, config.RiggerOnLocal} {
		for _, mode := range []string{config.RiggerEmbedded, config.RiggerStandalone} {
			section := "  run_on: " + runOn + "\n  mode: " + mode + "\n"
			if runOn == config.RiggerOnLocal {
				section += "  binary: " + riggerBin + "\n"
			}

			name := "deployer-e2e-" + runOn + "-" + mode

			t.Run(runOn+"/"+mode+"/stack in the plan", func(t *testing.T) {
				section := section + `  stack: |
    rigger {
      name      = "` + name + `"
      region    = "` + region + `"
      namespace = "default"
    }

    module "cache" {
      source  = "builtin/redis"
      version = "~> 0.3"
      wait    = false

      values = {
        job_name = "` + name + `"
      }
    }
`
				r, first, conn := agentRunner(t, norca, region, section)
				stack := r.store.Path("rigger-stack")

				applyTwice(t, r, first, norca, stack, name)

				// The lock file of the sync is in the state, wherever the
				// program ran.
				if data, err := os.ReadFile(filepath.Join(stack, riggerLockFile)); err != nil || !strings.Contains(string(data), "builtin/redis") {
					t.Errorf("the lock file in the state = %q, %v", data, err)
				}

				if _, err := os.Stat(filepath.Join(conn.dir, "rigger")); err == nil {
					t.Errorf("the copy of the stack is left in %s", conn.dir)
				}
			})

			t.Run(runOn+"/"+mode+"/stack directory", func(t *testing.T) {
				name := name + "-dir"
				section := section + "  stack_dir: stacks/platform\n  values: [site/e2e.vars.hcl]\n  vars: {node_pool: default}\n  prune: true\n"

				r, first, conn := agentRunner(t, norca, region, section)
				stack := filepath.Join(r.plan.BaseDir(), "stacks", "platform")

				writeFiles(t, r.plan.BaseDir(), map[string]string{
					"stacks/platform/stack.hcl": `rigger {
  name      = "` + name + `"
  region    = "` + region + `"
  namespace = "default"
}

module "cache" {
  source  = "builtin/redis"
  version = "~> 0.3"
  wait    = false

  values = {
    job_name  = var.job_name
    node_pool = var.node_pool
  }
}
`,
					"stacks/platform/variables.hcl": "variable \"job_name\" {\n  type = string\n}\n\nvariable \"node_pool\" {\n  type = string\n}\n",
					"site/e2e.vars.hcl":             "job_name = \"" + name + "\"\n",
				})

				// The lock file of a stack directory is its user's.
				if out, err := exec.Command(norca, "rigger", "stack", "sync", stack).CombinedOutput(); err != nil {
					t.Fatalf("stack sync: %v: %s", err, out)
				}

				applyTwice(t, r, first, norca, stack, name)

				if _, err := os.Stat(filepath.Join(conn.dir, "rigger")); err == nil {
					t.Errorf("the copy of the stack is left in %s", conn.dir)
				}
			})
		}
	}
}

// agentRunner is a runner of a plan of the region with the rigger section,
// whose first server is this machine with the agent.
func agentRunner(t *testing.T, norca, region, section string) (*Runner, *target, *localState) {
	t.Helper()

	dir := t.TempDir()
	plan := "metadata: {name: dev, region: " + region + "}\nrelease: {version: v0.2.0}\nrigger:\n" + section +
		"inventory:\n  masters:\n    - {host: 127.0.0.1, hostname: s1}\n"

	path := filepath.Join(dir, "plan.yaml")
	if err := os.WriteFile(path, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}

	p := loadPlanFile(t, path)
	r := NewRunner(p, nil)

	var err error
	if r.store, err = state.Open(p, nil); err != nil {
		t.Fatal(err)
	}

	m := &p.Inventory.Masters[0]
	paths := storage.ResolveNodePaths(m, p)
	paths.BinDir = filepath.Dir(norca)
	conn := &localState{LocalConnector: connector.NewLocalConnector(context.Background()), dir: filepath.Join(dir, "cli")}

	return r, &target{node: m, isMaster: true, paths: paths, conn: conn}, conn
}

// applyTwice applies the stack of the plan, checks that the job of its
// release is registered under the name its values give, and applies again,
// which finds the release unchanged. The stack is destroyed after the test.
func applyTwice(t *testing.T, r *Runner, first *target, norca, stack, job string) {
	t.Helper()

	t.Cleanup(func() {
		out, _ := exec.Command(norca, "rigger", "stack", "destroy", stack).CombinedOutput()
		t.Logf("destroy: %s", out)
	})

	if err := r.deployRigger(first, ""); err != nil {
		t.Fatal(err)
	}

	if out, err := exec.Command(norca, "operator", "api", "/v1/job/"+job).CombinedOutput(); err != nil || !strings.Contains(string(out), `"ID":"`+job+`"`) {
		t.Fatalf("the job %s of the release: %v: %.300s", job, err, out)
	}

	if err := r.deployRigger(first, ""); err != nil {
		t.Fatal(err)
	}
}
