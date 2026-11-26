// Package service installs, restarts and removes the Norca agent as a
// service of the init system of a node: systemd on Linux, the service
// manager of Windows. Install and removal are steps; the restart and the
// status stay calls, because the rolling restart waits on them.
package service

import (
	"fmt"
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/platform/step"
	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
)

// UnitPath is the systemd unit of the agent on a Linux node.
const UnitPath = "/etc/systemd/system/norca.service"

// Name is the service of the agent.
const Name = "norca"

// Manager is the driver of an init system.
type Manager interface {
	// InstallSteps register and enable the service; the agent is not
	// started (the rolling restart starts it). A change of the service
	// definition needs the agent restarted.
	InstallSteps(paths *storage.ResolvedPaths, fdLimit int) []step.Step
	// RemoveSteps stop and remove the service (decommission).
	RemoveSteps(paths *storage.ResolvedPaths) []step.Step
	// Restart restarts the agent.
	Restart(conn connector.Connector, paths *storage.ResolvedPaths) error
	// IsRunning reports whether the agent runs.
	IsRunning(conn connector.Connector) bool
}

// LinuxUnit renders the systemd unit. The agent runs as root: clients need
// it for the exec driver, CNI and network policies.
func LinuxUnit(paths *storage.ResolvedPaths, fdLimit int) string {
	if fdLimit <= 0 {
		fdLimit = 65536
	}
	return fmt.Sprintf(`[Unit]
Description=Norca Orchestration Agent
Documentation=https://github.com/bacanoo-io/norca
After=network-online.target
Wants=network-online.target

[Service]
ExecReload=/bin/kill -HUP $MAINPID
ExecStart=%s/norca agent -config=%s/norca.hcl
KillMode=process
KillSignal=SIGINT
LimitNOFILE=%d
LimitNPROC=524288
Restart=on-failure
RestartSec=5s
TasksMax=infinity
OOMScoreAdjust=-1000

[Install]
WantedBy=multi-user.target
`, paths.BinDir, paths.ConfigDir, fdLimit)
}

// Systemd is the driver of systemd.
type Systemd struct{}

var _ Manager = Systemd{}

// wantsDir and wantsLink are where systemctl enable links the unit
// (WantedBy).
const (
	wantsDir  = "/etc/systemd/system/multi-user.target.wants"
	wantsLink = wantsDir + "/" + Name + ".service"
)

// InstallSteps implements Manager: the unit (systemd reloads it when it
// changed) and its enable. Without systemctl (a container while an image
// is built) the unit is enabled the way systemctl does it, with the link
// of its WantedBy target.
func (Systemd) InstallSteps(paths *storage.ResolvedPaths, fdLimit int) []step.Step {
	return []step.Step{
		{
			Name:  "systemd unit",
			Files: []step.File{{Path: UnitPath, Mode: 0o644, Content: []byte(LinuxUnit(paths, fdLimit))}},
			// Nothing to do unless the unit changed.
			Check:   "true",
			Apply:   "if [ -d /run/systemd/system ] && command -v systemctl >/dev/null 2>&1; then\n  systemctl daemon-reload\nfi",
			Restart: true,
		},
		{
			Name: "norca service enabled",
			Check: "if command -v systemctl >/dev/null 2>&1; then\n  systemctl is-enabled --quiet " + Name + " 2>/dev/null\nelse\n" +
				"  [ \"$(readlink " + wantsLink + " 2>/dev/null || true)\" = " + UnitPath + " ]\nfi",
			Apply: "if command -v systemctl >/dev/null 2>&1; then\n  systemctl enable --quiet " + Name + "\n  log \"" + Name + " enabled\"\nelse\n" +
				"  mkdir -p " + wantsDir + "\n  ln -sfn " + UnitPath + " " + wantsLink + "\n" +
				"  log \"systemctl not found: " + Name + " enabled with the link " + wantsLink + "\"\nfi",
		},
	}
}

// RemoveSteps implements Manager. The unit has KillMode=process, so the stop
// leaves the task executors and the log plugins of the agent (norca
// executor, logmon, docker_logger) running in its cgroup: they are killed
// while the unit still exists, so that nothing of the agent outlives it.
func (Systemd) RemoveSteps(*storage.ResolvedPaths) []step.Step {
	return []step.Step{{
		Name: "norca service removed",
		Apply: "systemctl disable --now " + Name + " >/dev/null 2>&1 || true\n" +
			"systemctl kill --kill-whom=all --signal=SIGKILL " + Name + " >/dev/null 2>&1 || true\n" +
			"rm -f " + UnitPath + "\n" +
			"systemctl daemon-reload >/dev/null 2>&1 || true\n" +
			"systemctl reset-failed " + Name + " >/dev/null 2>&1 || true",
	}}
}

// Restart implements Manager.
func (Systemd) Restart(conn connector.Connector, _ *storage.ResolvedPaths) error {
	out, err := conn.ExecuteCommand("systemctl restart " + Name)
	if err != nil {
		return fmt.Errorf("systemctl restart norca failed: %w (output: %s)", err, out)
	}
	return nil
}

// IsRunning implements Manager.
func (Systemd) IsRunning(conn connector.Connector) bool {
	out, err := conn.ExecuteCommand("systemctl is-active " + Name)
	return err == nil && strings.TrimSpace(out) == "active"
}

// WindowsImagePath is the quoted service command line; the quotes keep paths
// with spaces (C:\Program Files) working and avoid the unquoted service path
// problem.
func WindowsImagePath(paths *storage.ResolvedPaths) string {
	return fmt.Sprintf(`"%s\norca.exe" agent -config="%s\norca.hcl"`, paths.BinDir, paths.ConfigDir)
}

// Windows is the driver of the service manager of Windows.
type Windows struct{}

var _ Manager = Windows{}

// InstallSteps implements Manager: the service with its command line and
// its restart on failure; a new or changed command line is a change.
func (Windows) InstallSteps(paths *storage.ResolvedPaths, _ int) []step.Step {
	image := connector.PSQuote(WindowsImagePath(paths))
	return []step.Step{{
		Name:       "windows service",
		PowerShell: true,
		Lines:      true,
		Restart:    true,
		Apply: `$svc = Get-Service -Name norca -ErrorAction SilentlyContinue
if (-not $svc) {
  New-Service -Name norca -DisplayName 'Norca Orchestration Agent' -BinaryPathName ` + image + ` -StartupType Automatic | Out-Null
  'changed norca service'
} else {
  $key = 'HKLM:\SYSTEM\CurrentControlSet\Services\norca'
  if ((Get-ItemProperty -Path $key).ImagePath -ne ` + image + `) {
    Set-ItemProperty -Path $key -Name ImagePath -Value ` + image + `
    'changed norca service'
  }
  Set-Service -Name norca -StartupType Automatic
}
sc.exe failure norca reset= 86400 actions= restart/5000/restart/10000// | Out-Null`,
	}}
}

// RemoveSteps implements Manager.
func (Windows) RemoveSteps(*storage.ResolvedPaths) []step.Step {
	return []step.Step{{
		Name:       "windows service removed",
		PowerShell: true,
		Apply: "Stop-Service -Name norca -ErrorAction SilentlyContinue; " +
			"if (Get-Service -Name norca -ErrorAction SilentlyContinue) { sc.exe delete norca | Out-Null }",
	}}
}

// Restart implements Manager: a staged binary (norca.exe.new) is swapped in
// while the service is stopped.
func (Windows) Restart(conn connector.Connector, paths *storage.ResolvedPaths) error {
	exe := paths.BinDir + `\norca.exe`
	staged := exe + connector.PendingSuffix
	script := `Stop-Service -Name norca -ErrorAction SilentlyContinue
(Get-Service -Name norca).WaitForStatus('Stopped', '00:01:00')
if (Test-Path -LiteralPath ` + connector.PSQuote(staged) + `) { Move-Item -Force -LiteralPath ` + connector.PSQuote(staged) + ` -Destination ` + connector.PSQuote(exe) + ` }
Start-Service -Name norca`
	if out, err := conn.ExecuteCommand(script); err != nil {
		return fmt.Errorf("failed to restart the Windows service: %w (output: %s)", err, out)
	}
	return nil
}

// IsRunning implements Manager.
func (Windows) IsRunning(conn connector.Connector) bool {
	out, err := conn.ExecuteCommand("(Get-Service -Name norca -ErrorAction SilentlyContinue).Status")
	return err == nil && strings.TrimSpace(out) == "Running"
}

// For returns the driver of a node the apply did not probe: Windows or
// systemd by the platform of the plan.
func For(windows bool) Manager {
	if windows {
		return Windows{}
	}
	return Systemd{}
}
