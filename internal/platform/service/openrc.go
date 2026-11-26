package service

import (
	"fmt"
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/platform/step"
	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
)

// InitScriptPath is the OpenRC service of the agent on a Linux node.
const InitScriptPath = "/etc/init.d/" + Name

// runlevelLink is where rc-update add links the service of the default
// runlevel.
const runlevelLink = "/etc/runlevels/default/" + Name

// OpenRCScript renders the OpenRC service of the agent. supervise-daemon
// runs it as systemd's unit does: restarted 5 seconds after it ended,
// stopped with SIGINT (SIGKILL after 90 seconds), only the agent signalled
// (its task executors keep running, as with KillMode=process), the limits
// of the unit, an OOM score of -1000, its output in syslog (systemd: the
// journal), and reload sends SIGHUP.
func OpenRCScript(paths *storage.ResolvedPaths, fdLimit int) string {
	if fdLimit <= 0 {
		fdLimit = 65536
	}
	return fmt.Sprintf(`#!/sbin/openrc-run
# Managed by norca-deployer. Do not edit.
name="norca"
description="Norca Orchestration Agent"

supervisor=supervise-daemon
command="%s/norca"
command_args="agent -config=%s/norca.hcl"
# The output goes to syslog, as systemd sends it to the journal.
output_logger="logger -t norca -p daemon.info"
error_logger="logger -t norca -p daemon.err"
# Restart the agent when it ends (systemd: Restart=on-failure, RestartSec=5s).
respawn_delay=5
respawn_max=0
# Stop with SIGINT (systemd: KillSignal=SIGINT), SIGKILL after 90 seconds.
# Only the agent is signalled: its task executors keep running (systemd:
# KillMode=process), and the cgroup of the service is not emptied.
retry="SIGINT/90/SIGKILL/5"
rc_cgroup_cleanup="no"
# systemd: LimitNOFILE, LimitNPROC.
rc_ulimit="-n %d -u 524288"

extra_started_commands="reload"

depend() {
	need net
	use dns logger
	after firewall norca-nftables
}

start_pre() {
	# systemd: OOMScoreAdjust=-1000; supervise-daemon and the agent inherit it.
	echo -1000 >/proc/self/oom_score_adj
}

reload() {
	ebegin "Reloading ${name}"
	supervise-daemon "${RC_SVCNAME}" --signal HUP
	eend $?
}
`, paths.BinDir, paths.ConfigDir, fdLimit)
}

// OpenRC is the driver of OpenRC (Alpine).
type OpenRC struct{}

var _ Manager = OpenRC{}

// InstallSteps implements Manager: the service (a new or changed one needs
// the agent restarted) and its place in the default runlevel. Without
// rc-update (a container while an image is built) the service is added the
// way rc-update does it, with the link of the runlevel.
func (OpenRC) InstallSteps(paths *storage.ResolvedPaths, fdLimit int) []step.Step {
	return []step.Step{
		{
			Name:    "openrc service",
			Files:   []step.File{{Path: InitScriptPath, Mode: 0o755, Content: []byte(OpenRCScript(paths, fdLimit))}},
			Restart: true,
		},
		{
			Name:  "norca service enabled",
			Check: "[ -e " + runlevelLink + " ]",
			Apply: "if command -v rc-update >/dev/null 2>&1; then\n  rc-update add " + Name + " default >/dev/null\n  log \"" + Name + " enabled\"\nelse\n" +
				"  mkdir -p /etc/runlevels/default\n  ln -sfn " + InitScriptPath + " " + runlevelLink + "\n" +
				"  log \"rc-update not found: " + Name + " enabled with the link " + runlevelLink + "\"\nfi",
		},
	}
}

// serviceCgroupProcs are the process lists of the cgroup OpenRC gives the
// service: cgroup v2 (rc_cgroup_mode unified, the default), hybrid, and the
// named v1 hierarchy of OpenRC.
var serviceCgroupProcs = []string{
	"/sys/fs/cgroup/openrc." + Name + "/cgroup.procs",
	"/sys/fs/cgroup/unified/openrc." + Name + "/cgroup.procs",
	"/sys/fs/cgroup/openrc/" + Name + "/tasks",
}

// RemoveSteps implements Manager. The stop leaves the task executors and the
// log plugins of the agent running in the cgroup of the service
// (rc_cgroup_cleanup="no"): they are killed before the service goes. The
// cgroup_cleanup command of OpenRC is not used: it looks for the cgroup
// under another name than it creates in v2 (openrc.norca) and does nothing
// there.
func (OpenRC) RemoveSteps(*storage.ResolvedPaths) []step.Step {
	return []step.Step{{
		Name: "norca service removed",
		Apply: "rc-service " + Name + " stop >/dev/null 2>&1 || true\n" +
			"for norca_procs in " + strings.Join(serviceCgroupProcs, " ") + "; do\n" +
			"  [ -f \"$norca_procs\" ] || continue\n" +
			"  for norca_pid in $(cat \"$norca_procs\"); do kill -9 \"$norca_pid\" 2>/dev/null || true; done\n" +
			"done\n" +
			"rc-update del " + Name + " default >/dev/null 2>&1 || true\n" +
			"rm -f " + runlevelLink + " " + InitScriptPath,
	}}
}

// Restart implements Manager: rc-service restart starts a stopped service
// too.
func (OpenRC) Restart(conn connector.Connector, _ *storage.ResolvedPaths) error {
	out, err := conn.ExecuteCommand("rc-service " + Name + " restart")
	if err != nil {
		return fmt.Errorf("rc-service norca restart failed: %w (output: %s)", err, out)
	}
	return nil
}

// IsRunning implements Manager: the status is 0 when the service is
// started (3 stopped, 32 crashed, 8 starting).
func (OpenRC) IsRunning(conn connector.Connector) bool {
	_, err := conn.ExecuteCommand("rc-service " + Name + " status >/dev/null 2>&1")
	return err == nil
}
