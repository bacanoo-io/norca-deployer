package scripts

import (
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/connector"
)

func testPlan() *config.DeploymentPlan {
	return &config.DeploymentPlan{
		Scripts: []config.ScriptConfig{
			{Name: "disk", Phase: config.PhasePreInstall, Inline: "echo disk\n", Env: map[string]string{"DEV": "/dev/sdb"}},
			{Name: "redis-tune", Phase: config.PhasePostPackages, Inline: "echo tune\n", Roles: []string{config.RoleClient}},
			{Name: "check", Phase: config.PhasePostInstall, Inline: "echo check\n", Always: true},
		},
	}
}

func TestRunOnceUntilChanged(t *testing.T) {
	plan := testPlan()
	node := &config.NodeConfig{Host: "10.0.0.1", Platform: "linux"}
	conn := connector.NewFakeConnector()

	if err := Run(conn, plan, config.PhasePreInstall, node, false, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(conn.CommandsWithPrefix("set -a;")); got != 1 {
		t.Fatalf("first apply: %d runs, want 1", got)
	}
	if string(conn.Files[ScriptPath("disk")]) != "echo disk\n" || conn.Modes[ScriptPath("disk")] != 0700 {
		t.Fatalf("script not uploaded correctly")
	}
	if env := string(conn.Files[StateDir+"/disk.env"]); env != "DEV='/dev/sdb'\n" || conn.Modes[StateDir+"/disk.env"] != 0600 {
		t.Fatalf("env file = %q", env)
	}

	// Second apply with the same script and env: skipped.
	if err := Run(conn, plan, config.PhasePreInstall, node, false, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(conn.CommandsWithPrefix("set -a;")); got != 1 {
		t.Fatalf("unchanged script ran again (%d runs)", got)
	}

	// A changed env value re-runs it.
	plan.Scripts[0].Env["DEV"] = "/dev/sdc"
	if err := Run(conn, plan, config.PhasePreInstall, node, false, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(conn.CommandsWithPrefix("set -a;")); got != 2 {
		t.Fatalf("changed env did not re-run the script (%d runs)", got)
	}
}

func TestAlwaysAndRoles(t *testing.T) {
	plan := testPlan()
	node := &config.NodeConfig{Host: "10.0.0.1", Platform: "linux"}
	conn := connector.NewFakeConnector()

	for i := 0; i < 2; i++ {
		if err := Run(conn, plan, config.PhasePostInstall, node, true, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(conn.CommandsWithPrefix("set -a;")); got != 2 {
		t.Fatalf("always script: %d runs, want 2", got)
	}

	// redis-tune is client-only.
	if err := Run(conn, plan, config.PhasePostPackages, node, true, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := conn.Files[ScriptPath("redis-tune")]; ok {
		t.Fatalf("client-only script ran on a master")
	}
}

func TestFailureKeepsMarkerUnset(t *testing.T) {
	plan := testPlan()
	node := &config.NodeConfig{Host: "10.0.0.1", Platform: "linux"}
	conn := connector.NewFakeConnector()
	conn.Failures["set -a;"] = errBoom{}

	err := Run(conn, plan, config.PhasePreInstall, node, false, nil)
	if err == nil || !strings.Contains(err.Error(), `script "disk" failed`) {
		t.Fatalf("expected failure, got %v", err)
	}
	if _, ok := conn.Files[MarkerPath("disk")]; ok {
		t.Fatalf("marker written for a failed run")
	}
	if rm := conn.CommandsWithPrefix("rm -f"); len(rm) != 1 {
		t.Fatalf("env file not removed after failure: %v", conn.Commands)
	}
}

func TestWindowsSkipped(t *testing.T) {
	conn := connector.NewFakeConnector()
	node := &config.NodeConfig{Host: "10.0.0.9", Platform: "windows"}
	if err := Run(conn, testPlan(), config.PhasePreInstall, node, false, nil); err != nil {
		t.Fatal(err)
	}
	if len(conn.Commands) != 0 || len(conn.Files) != 0 {
		t.Fatalf("bash script touched a Windows node")
	}
}

func TestSecretEnv(t *testing.T) {
	plan := testPlan()
	plan.Scripts[0].Env["TOKEN"] = "!enc:v1:aes-gcm:abc"
	resolve := func(v string) (string, error) {
		if strings.HasPrefix(v, "!enc:") {
			return "s3cr'et", nil
		}
		return v, nil
	}
	p, err := Prepare(plan, &plan.Scripts[0], resolve)
	if err != nil {
		t.Fatal(err)
	}
	if got := RenderEnvFile(p.Env); !strings.Contains(got, `TOKEN='s3cr'\''et'`) {
		t.Fatalf("env file = %q", got)
	}
	if strings.Contains(Command(p, "/x.env"), "s3cr") {
		t.Fatalf("secret leaked into the command line")
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }
