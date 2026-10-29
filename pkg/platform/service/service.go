package service

import (
	"fmt"
	"strings"

	"github.com/bacanoo-io/norca-deployer/pkg/connector"
	"github.com/bacanoo-io/norca-deployer/pkg/platform/storage"
)

const unitPath = "/etc/systemd/system/norca.service"

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

// InstallService registers and enables the Norca service. It reports whether
// the service definition changed (the agent then needs a restart).
func InstallService(conn connector.Connector, paths *storage.ResolvedPaths, isWindows bool, fdLimit int) (bool, error) {
	if isWindows {
		return installWindowsService(conn, paths)
	}
	return installLinuxService(conn, paths, fdLimit)
}

func installLinuxService(conn connector.Connector, paths *storage.ResolvedPaths, fdLimit int) (bool, error) {
	changed, err := connector.SyncFile(conn, unitPath, []byte(LinuxUnit(paths, fdLimit)), 0644)
	if err != nil {
		return false, err
	}
	cmd := "systemctl enable norca >/dev/null 2>&1"
	if changed {
		cmd = "systemctl daemon-reload && " + cmd
	}
	if out, err := conn.ExecuteCommand(cmd); err != nil {
		return false, fmt.Errorf("systemctl enable failed: %w (output: %s)", err, out)
	}
	return changed, nil
}

// WindowsImagePath is the quoted service command line; the quotes keep paths
// with spaces (C:\Program Files) working and avoid the unquoted service path
// problem.
func WindowsImagePath(paths *storage.ResolvedPaths) string {
	return fmt.Sprintf(`"%s\norca.exe" agent -config="%s\norca.hcl"`, paths.BinDir, paths.ConfigDir)
}

func installWindowsService(conn connector.Connector, paths *storage.ResolvedPaths) (bool, error) {
	image := connector.PSQuote(WindowsImagePath(paths))
	script := `$svc = Get-Service -Name norca -ErrorAction SilentlyContinue
if (-not $svc) {
  New-Service -Name norca -DisplayName 'Norca Orchestration Agent' -BinaryPathName ` + image + ` -StartupType Automatic | Out-Null
  'changed'
} else {
  $key = 'HKLM:\SYSTEM\CurrentControlSet\Services\norca'
  if ((Get-ItemProperty -Path $key).ImagePath -ne ` + image + `) {
    Set-ItemProperty -Path $key -Name ImagePath -Value ` + image + `
    'changed'
  }
  Set-Service -Name norca -StartupType Automatic
}
sc.exe failure norca reset= 86400 actions= restart/5000/restart/10000// | Out-Null`
	out, err := conn.ExecuteCommand(script)
	if err != nil {
		return false, fmt.Errorf("Windows service registration failed: %w (output: %s)", err, out)
	}
	return strings.Contains(out, "changed"), nil
}

// RestartService restarts the agent. On Windows a staged binary
// (norca.exe.new) is swapped in while the service is stopped.
func RestartService(conn connector.Connector, paths *storage.ResolvedPaths, isWindows bool) error {
	if isWindows {
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

	out, err := conn.ExecuteCommand("systemctl restart norca")
	if err != nil {
		return fmt.Errorf("systemctl restart norca failed: %w (output: %s)", err, out)
	}
	return nil
}

// IsServiceRunning checks if the service process is currently active.
func IsServiceRunning(conn connector.Connector, isWindows bool) bool {
	if isWindows {
		out, err := conn.ExecuteCommand("(Get-Service -Name norca -ErrorAction SilentlyContinue).Status")
		return err == nil && strings.TrimSpace(out) == "Running"
	}
	out, err := conn.ExecuteCommand("systemctl is-active norca")
	return err == nil && strings.TrimSpace(out) == "active"
}
