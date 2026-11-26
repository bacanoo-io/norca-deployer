package service

import (
	"strings"
	"testing"
)

// inOrder fails the test when a part of want is missing from script or comes
// before the part ahead of it.
func inOrder(t *testing.T, script string, want ...string) {
	t.Helper()
	at := 0
	for _, w := range want {
		i := strings.Index(script[at:], w)
		if i < 0 {
			t.Fatalf("no %q (in this order) in:\n%s", w, script)
		}
		at += i + len(w)
	}
}

// The unit has KillMode=process: the removal kills what the stop leaves in
// its cgroup (executors, logmon, docker_logger) while the unit still exists.
func TestSystemdRemoveStepsKillTheCgroupOfTheUnit(t *testing.T) {
	inOrder(t, Systemd{}.RemoveSteps(testPaths)[0].Apply,
		"systemctl disable --now norca",
		"systemctl kill --kill-whom=all --signal=SIGKILL norca",
		"rm -f "+UnitPath,
		"systemctl daemon-reload",
	)
}
