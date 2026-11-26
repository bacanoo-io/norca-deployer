package service

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/platform/step"
	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
)

var testPaths = &storage.ResolvedPaths{BinDir: "/opt/norca/bin", ConfigDir: "/opt/norca/etc"}

// The service runs the agent of the paths of the node under
// supervise-daemon, with the settings of the systemd unit.
func TestOpenRCScript(t *testing.T) {
	got := OpenRCScript(testPaths, 0)
	for _, w := range []string{
		"#!/sbin/openrc-run\n",
		"supervisor=supervise-daemon\n",
		`command="/opt/norca/bin/norca"` + "\n",
		`command_args="agent -config=/opt/norca/etc/norca.hcl"` + "\n",
		"respawn_delay=5\nrespawn_max=0\n",
		`retry="SIGINT/90/SIGKILL/5"` + "\n",
		`rc_cgroup_cleanup="no"` + "\n",
		`rc_ulimit="-n 65536 -u 524288"` + "\n",
		"\tneed net\n",
		"echo -1000 >/proc/self/oom_score_adj\n",
		`supervise-daemon "${RC_SVCNAME}" --signal HUP`,
	} {
		if !strings.Contains(got, w) {
			t.Errorf("no %q in:\n%s", w, got)
		}
	}
	if got := OpenRCScript(testPaths, 1048576); !strings.Contains(got, `rc_ulimit="-n 1048576 -u 524288"`) {
		t.Errorf("file descriptor limit:\n%s", got)
	}
}

// openrcNode is a fake node for the steps of the service: whether the
// runlevel has the service, whether rc-update is there, and the state of
// the service.
type openrcNode struct {
	enabled, noRCUpdate, started bool
}

func (n *openrcNode) fake() *connector.FakeConnector {
	c := connector.NewFakeConnector()
	c.Handler = func(cmd string) (string, error, bool) {
		switch {
		case strings.HasPrefix(cmd, "# check: norca service enabled"):
			if n.enabled {
				return "norca-step yes", nil, true
			}
			return "norca-step no", nil, true
		case strings.HasPrefix(cmd, "# apply: norca service enabled"):
			n.enabled = true
			if n.noRCUpdate {
				return "log rc-update not found: norca enabled with the link " + runlevelLink, nil, true
			}
			return "log norca enabled", nil, true
		case cmd == "rc-service norca restart":
			n.started = true
			return " * Starting norca ... [ ok ]", nil, true
		case cmd == "rc-service norca status >/dev/null 2>&1":
			if n.started {
				return "", nil, true
			}
			return "", errors.New("exit status 3"), true
		}
		return "", nil, true
	}
	return c
}

// The service is written with mode 0755 and added to the default runlevel
// once; a changed service needs the agent restarted. A second run changes
// nothing.
func TestOpenRCInstallSteps(t *testing.T) {
	steps := OpenRC{}.InstallSteps(testPaths, 0)
	if len(steps) != 2 || steps[0].Files[0].Path != InitScriptPath || steps[0].Files[0].Mode != 0o755 || !steps[0].Restart {
		t.Fatalf("steps %+v", steps)
	}
	node := &openrcNode{}
	c := node.fake()
	var out bytes.Buffer
	ex := &step.Executor{Conn: c, Host: "alpine", Out: &out}
	changed, restart, err := ex.RunAll(t.Context(), steps)
	if err != nil || !changed || !restart || !node.enabled || string(c.Files[InitScriptPath]) != OpenRCScript(testPaths, 0) {
		t.Fatalf("first run: changed %t restart %t enabled %t: %v\n%s", changed, restart, node.enabled, err, out.String())
	}
	changed, restart, err = ex.RunAll(t.Context(), OpenRC{}.InstallSteps(testPaths, 0))
	if err != nil || changed || restart {
		t.Fatalf("second run: changed %t restart %t: %v\n%s", changed, restart, err, out.String())
	}
	if !strings.Contains(steps[1].Apply, "ln -sfn "+InitScriptPath+" "+runlevelLink) || !strings.Contains(steps[1].Apply, "rc-update add norca default") {
		t.Fatalf("enable:\n%s", steps[1].Apply)
	}
}

// Restart starts a stopped service too; the status is the exit code of
// rc-service status.
func TestOpenRCRestartAndStatus(t *testing.T) {
	node := &openrcNode{}
	c := node.fake()
	if (OpenRC{}).IsRunning(c) {
		t.Fatal("a stopped service runs")
	}
	if err := (OpenRC{}).Restart(c, testPaths); err != nil {
		t.Fatal(err)
	}
	if !(OpenRC{}).IsRunning(c) {
		t.Fatal("the restarted service does not run")
	}
	remove := OpenRC{}.RemoveSteps(testPaths)[0].Apply
	for _, w := range []string{"rc-service norca stop", "rc-update del norca default", "rm -f " + runlevelLink + " " + InitScriptPath} {
		if !strings.Contains(remove, w) {
			t.Errorf("no %q in the removal:\n%s", w, remove)
		}
	}
}

// The stop leaves the executors and log plugins of the agent in the cgroup
// of the service (rc_cgroup_cleanup="no"): the removal kills them, in the
// cgroup of every mode of OpenRC, before the service goes.
func TestOpenRCRemoveStepsKillTheCgroupOfTheService(t *testing.T) {
	inOrder(t, OpenRC{}.RemoveSteps(testPaths)[0].Apply,
		"rc-service norca stop",
		"for norca_procs in /sys/fs/cgroup/openrc.norca/cgroup.procs /sys/fs/cgroup/unified/openrc.norca/cgroup.procs /sys/fs/cgroup/openrc/norca/tasks; do",
		`kill -9 "$norca_pid"`,
		"rc-update del norca default",
		"rm -f "+runlevelLink+" "+InitScriptPath,
	)
}
