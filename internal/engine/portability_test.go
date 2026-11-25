package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/host"
	"github.com/bacanoo-io/norca-deployer/internal/platform/dns"
	"github.com/bacanoo-io/norca-deployer/internal/platform/firewall"
	"github.com/bacanoo-io/norca-deployer/internal/platform/scripts"
	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// portableScripts are every script and POSIX command the deployer makes for
// a Linux node: the commands prepareNode runs for the example plans (with
// the drivers, the DNS script, the firewall), the probe, the uninstall, the
// command of a script of the plan, and the install scripts of a scaled
// machine of the machine tests.
func portableScripts(t *testing.T) map[string]string {
	t.Helper()
	res := map[string]string{}
	for _, name := range []string{"single-node", "cluster", "multi-datacenter", "local-dev", "add-node"} {
		_, cmds := recordPlan(t, filepath.Join("..", "..", "examples", "nodes", name+".yaml"))
		seen := map[string]bool{}
		for _, c := range cmds {
			if seen[c] {
				continue
			}
			seen[c] = true
			res[fmt.Sprintf("cmd-%s-%03d", name, len(seen))] = c
		}
	}
	// A RHEL node: the steps of dnf and the SELinux labels.
	_, rhel := recordPlanWith(t, loadPlan(t, rhelPlan), rhelProbe)
	seen := map[string]bool{}
	for _, c := range rhel {
		if !seen[c] {
			seen[c] = true
			res[fmt.Sprintf("cmd-rhel-%03d", len(seen))] = c
		}
	}
	res["probe"] = host.ProbeCommand
	res["nftables-remove"] = firewall.RemoveNFTablesCommand
	res["consul-dns"] = dns.Script()
	paths := storage.ResolveNodePaths(&config.NodeConfig{Platform: "linux"}, loadPlan(t, machinePlan(officialRelease, "")))
	res["uninstall"] = linuxUninstallScript(paths, true)
	res["script-command"] = scripts.Command(&scripts.Prepared{Config: &config.ScriptConfig{Name: "prep", Interpreter: "sh"}}, "/var/lib/norca-deployer/scripts/prep.env")
	for name, yaml := range machineScriptPlans() {
		res["machine-"+name] = generate(t, loadPlan(t, yaml), MachineScriptOptions{})
	}
	return res
}

// Every script and command is POSIX sh: dash, bash (/bin/sh of RHEL) and
// busybox sh read it (NORCA_TEST_BUSYBOX names a static busybox), and
// shellcheck -s sh finds nothing when it is installed.
// NORCA_SHELL_SCRIPTS=<dir> writes them there, for a shellcheck of its own
// (koalaman/shellcheck -s sh).
func TestShellPortability(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the scripts run on Linux")
	}
	all := portableScripts(t)
	checkers := map[string][]string{}
	for _, shell := range []string{"dash", "bash"} {
		if p, err := exec.LookPath(shell); err == nil {
			checkers[shell] = []string{p, "-n"}
		}
	}
	if bb := os.Getenv("NORCA_TEST_BUSYBOX"); bb != "" {
		checkers["busybox"] = []string{bb, "sh", "-n"}
	}
	if len(checkers) == 0 {
		t.Skip("neither dash nor busybox")
	}
	dir := os.Getenv("NORCA_SHELL_SCRIPTS")
	if dir == "" {
		dir = t.TempDir()
	}
	for name, script := range all {
		file := filepath.Join(dir, name+".sh")
		if !strings.HasPrefix(script, "#!") {
			script = "#!/bin/sh\n" + script + "\n"
		}
		if err := os.WriteFile(file, []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		for shell, argv := range checkers {
			if out, err := exec.Command(argv[0], append(argv[1:], file)...).CombinedOutput(); err != nil {
				t.Errorf("%s does not read %s: %v\n%s", shell, name, err, out)
			}
		}
	}
	if sc, err := exec.LookPath("shellcheck"); err == nil {
		files, _ := filepath.Glob(filepath.Join(dir, "*.sh"))
		if out, err := exec.Command(sc, append([]string{"-s", "sh"}, files...)...).CombinedOutput(); err != nil {
			t.Errorf("shellcheck -s sh: %v\n%s", err, out)
		}
	}
	t.Logf("%d scripts and commands checked with %d shells", len(all), len(checkers))
}
