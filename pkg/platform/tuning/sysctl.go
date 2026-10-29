package tuning

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/bacanoo-io/norca-deployer/pkg/connector"
)

const SysctlConfigPath = "/etc/sysctl.d/99-norca-production.conf"
const ModulesConfigPath = "/etc/modules-load.d/norca-bridge.conf"

const RecommendedSysctl = `# Norca Production Tuning & CNI Bridge Netfilter
net.ipv4.ip_forward = 1
net.ipv6.conf.all.forwarding = 1
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.bridge.bridge-nf-call-arptables = 1
fs.file-max = 2097152
fs.inotify.max_user_watches = 524288
fs.inotify.max_user_instances = 8192
vm.max_map_count = 262144
vm.overcommit_memory = 1
vm.swappiness = 1
net.core.somaxconn = 32768
net.ipv4.tcp_max_syn_backlog = 16384
net.ipv4.neigh.default.gc_thresh1 = 1024
net.ipv4.neigh.default.gc_thresh2 = 2048
net.ipv4.neigh.default.gc_thresh3 = 4096
`

const RecommendedModules = `# Norca required kernel modules
bridge
br_netfilter
`

// ApplyLinuxTuning configures kernel modules and sysctl parameters on the target Linux host.
func ApplyLinuxTuning(conn connector.Connector) error {
	// 1. Register required kernel modules
	_ = conn.MkdirAll("/etc/modules-load.d", 0755)
	if err := conn.CopyFile(bytes.NewReader([]byte(RecommendedModules)), ModulesConfigPath, 0644); err != nil {
		return fmt.Errorf("failed to write kernel modules configuration: %w", err)
	}

	// Load modules immediately
	_, _ = conn.ExecuteCommand("modprobe bridge && modprobe br_netfilter")

	// 2. Write sysctl settings
	_ = conn.MkdirAll("/etc/sysctl.d", 0755)
	if err := conn.CopyFile(bytes.NewReader([]byte(RecommendedSysctl)), SysctlConfigPath, 0644); err != nil {
		return fmt.Errorf("failed to write sysctl configuration: %w", err)
	}

	// 3. Apply sysctl parameters
	out, err := conn.ExecuteCommand("sysctl --system")
	if err != nil {
		return fmt.Errorf("sysctl apply error: %w (output: %s)", err, out)
	}

	return nil
}

// ApplyWindowsTuning configures LongPathsEnabled and PowerShell ExecutionPolicy on Windows targets.
func ApplyWindowsTuning(conn connector.Connector) error {
	cmd := `Set-ItemProperty -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem' -Name 'LongPathsEnabled' -Value 1 -ErrorAction SilentlyContinue; ` +
		`Set-ExecutionPolicy -ExecutionPolicy RemoteSigned -Scope LocalMachine -Force -ErrorAction SilentlyContinue`

	out, err := conn.ExecuteCommand(cmd)
	if err != nil && !strings.Contains(out, "completed") {
		return fmt.Errorf("Windows environment tuning error: %w (output: %s)", err, out)
	}
	return nil
}
