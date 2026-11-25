// Package tuning tunes the host for the agent: the kernel modules and the
// sysctl settings of a Linux node, long paths on Windows. Its drivers
// return steps.
package tuning

import (
	"github.com/bacanoo-io/norca-deployer/internal/platform/step"
)

// The files of the Linux tuning.
const (
	SysctlConfigPath = "/etc/sysctl.d/99-norca-production.conf"
	// ModulesConfigPath persists the kernel modules: systemd-modules-load
	// loads them at boot.
	ModulesConfigPath = "/etc/modules-load.d/norca-bridge.conf"
)

// RecommendedSysctl are the sysctl settings of a Linux node.
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

// RecommendedModules are the kernel modules of the CNI bridge.
const RecommendedModules = `# Norca required kernel modules
bridge
br_netfilter
`

// Tuner is the driver of the tuning of a node.
type Tuner interface {
	Steps() []step.Step
}

// Linux tunes a Linux node with systemd-modules-load and sysctl.
type Linux struct{}

var _ Tuner = Linux{}

// Steps implements Tuner: the kernel modules are persisted and loaded (a
// module that cannot be loaded now is a warning: it is loaded at boot); the
// sysctl settings are written and applied. A failed sysctl --system is an
// error where /proc/sys can be written, and a warning where it cannot (a
// container that builds an image): the settings apply at the next boot.
func (Linux) Steps() []step.Step {
	return []step.Step{
		{
			Name:  "kernel modules",
			Dirs:  []step.Dir{{Path: "/etc/modules-load.d", Mode: 0o755}},
			Files: []step.File{{Path: ModulesConfigPath, Mode: 0o644, Content: []byte(RecommendedModules)}},
			Apply: "if ! { modprobe bridge && modprobe br_netfilter; } >/dev/null 2>&1; then\n" +
				"  warn \"the modules bridge and br_netfilter could not be loaded now (they are loaded at boot)\"\nfi",
		},
		{
			Name:  "sysctl",
			Dirs:  []step.Dir{{Path: "/etc/sysctl.d", Mode: 0o755}},
			Files: []step.File{{Path: SysctlConfigPath, Mode: 0o644, Content: []byte(RecommendedSysctl)}},
			Apply: "if ! sysctl_out=$(sysctl --system 2>&1); then\n" +
				"  if [ -w /proc/sys/net/ipv4/ip_forward ]; then\n" +
				"    die \"sysctl --system failed: $(printf '%s\\n' \"$sysctl_out\" | tail -n 3 | tr '\\n' ' ')\"\n" +
				"  fi\n" +
				"  warn \"sysctl --system failed: the settings apply at the next boot\"\nfi",
		},
	}
}

// Windows tunes a Windows node: long paths and the execution policy of
// PowerShell.
type Windows struct{}

var _ Tuner = Windows{}

// Steps implements Tuner. A failure whose message says that the change was
// completed (an execution policy that a policy of a wider scope overrides)
// is no failure.
func (Windows) Steps() []step.Step {
	return []step.Step{{
		Name:       "windows tuning",
		PowerShell: true,
		Apply: `try { ` +
			`Set-ItemProperty -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem' -Name 'LongPathsEnabled' -Value 1 -ErrorAction SilentlyContinue; ` +
			`Set-ExecutionPolicy -ExecutionPolicy RemoteSigned -Scope LocalMachine -Force -ErrorAction SilentlyContinue ` +
			`} catch { if ("$_" -notlike '*completed*') { throw } }`,
	}}
}
