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
	// ModulesConfigPath persists the kernel modules: systemd-modules-load,
	// or the modules service of OpenRC (Alpine), loads them at boot.
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

// OpenRC tunes a Linux node whose init system is OpenRC (Alpine). The
// files are those of Linux: the modules and sysctl services of OpenRC read
// /etc/modules-load.d and /etc/sysctl.d at boot. The settings are applied
// now with sysctl -p (busybox has no sysctl --system, and its sysctl ends
// with 0 after an error, so its output is read). The modules and sysctl
// services are put into the boot runlevel when they are in none, and the
// cgroups service into sysinit when no cgroup file system is mounted (the
// task drivers of the agent need one; cgroup v1 and v2 both serve).
type OpenRC struct{}

var _ Tuner = OpenRC{}

// openrcFunctions are the shell functions of the OpenRC tuning.
const openrcFunctions = `# openrc_enabled SERVICE is true when a runlevel of OpenRC has the service.
openrc_enabled() {
  for openrc_l in /etc/runlevels/*/"$1"; do
    [ -e "$openrc_l" ] && return 0
  done
  return 1
}

# openrc_cgroups is true when a cgroup file system is mounted (cgroup2fs:
# v2; tmpfs: the hierarchies of v1).
openrc_cgroups() {
  case $(stat -fc %T /sys/fs/cgroup 2>/dev/null) in
  cgroup2fs | tmpfs) return 0 ;;
  esac
  return 1
}
`

// Steps implements Tuner.
func (OpenRC) Steps() []step.Step {
	linux := Linux{}.Steps()
	modules, sysctl := linux[0], linux[1]
	// A key the host does not have (the neighbour table outside the first
	// network namespace, net.bridge before br_netfilter is loaded) is a
	// warning, as sysctl --system leaves it; busybox calls it an unknown
	// key, procps cannot stat it. A key that is read-only here (a
	// container) is the warning of a read-only /proc/sys.
	sysctl.Apply = "sysctl_rc=0\n" +
		"sysctl_out=$(sysctl -p " + SysctlConfigPath + " 2>&1) || sysctl_rc=$?\n" +
		"sysctl_unknown=$(printf '%s\\n' \"$sysctl_out\" | sed -n -e \"s/.*error: '\\(.*\\)' is an unknown key.*/\\1/p\" -e 's|.*cannot stat /proc/sys/\\([^:]*\\):.*|\\1|p' | tr '\\n' ' ')\n" +
		"sysctl_ro=$(printf '%s\\n' \"$sysctl_out\" | sed -n \"s/.*setting key [\\\"']\\(.*\\)[\\\"']: Read-only file system.*/\\1/p\" | tr '\\n' ' ')\n" +
		"sysctl_bad=$(printf '%s\\n' \"$sysctl_out\" | grep -i -e error -e cannot | grep -v -e 'is an unknown key' -e 'cannot stat /proc/sys/' -e 'Read-only file system' || true)\n" +
		"if [ -n \"$sysctl_unknown\" ]; then\n" +
		"  warn \"sysctl: keys this host does not have were skipped: ${sysctl_unknown% }\"\n" +
		"fi\n" +
		"if [ -n \"$sysctl_ro\" ]; then\n" +
		"  warn \"sysctl: keys that are read-only on this host were not set (a container?); they apply at the next boot: ${sysctl_ro% }\"\n" +
		"fi\n" +
		"if [ -n \"$sysctl_bad\" ] || { [ \"$sysctl_rc\" != 0 ] && [ -z \"$sysctl_unknown$sysctl_ro\" ]; }; then\n" +
		"  if [ -w /proc/sys/net/ipv4/ip_forward ]; then\n" +
		"    die \"sysctl -p " + SysctlConfigPath + " failed: $(printf '%s\\n' \"${sysctl_bad:-$sysctl_out}\" | tail -n 3 | tr '\\n' ' ')\"\n" +
		"  fi\n" +
		"  warn \"sysctl -p failed: the settings apply at the next boot\"\nfi"
	services := step.Step{
		Name:      "openrc services",
		Functions: openrcFunctions,
		Check: "{ [ ! -e /etc/init.d/modules ] || openrc_enabled modules; } &&\n" +
			"  { [ ! -e /etc/init.d/sysctl ] || openrc_enabled sysctl; } &&\n" +
			"  { [ ! -e /etc/init.d/cgroups ] || openrc_enabled cgroups || openrc_cgroups; }",
		Apply: "for openrc_svc in modules sysctl; do\n" +
			"  if [ -e \"/etc/init.d/$openrc_svc\" ] && ! openrc_enabled \"$openrc_svc\"; then\n" +
			"    rc-update add \"$openrc_svc\" boot >/dev/null\n" +
			"    log \"$openrc_svc added to the boot runlevel\"\n" +
			"  fi\n" +
			"done\n" +
			"if [ -e /etc/init.d/cgroups ] && ! openrc_enabled cgroups && ! openrc_cgroups; then\n" +
			"  rc-update add cgroups sysinit >/dev/null\n" +
			"  log \"cgroups added to the sysinit runlevel\"\n" +
			"  if [ -d /run/openrc ] && rc-service cgroups start >/dev/null 2>&1; then\n" +
			"    log \"cgroups mounted\"\n" +
			"  else\n" +
			"    warn \"the cgroups are mounted at the next boot: the task drivers of the agent need them\"\n" +
			"  fi\n" +
			"fi",
	}
	return []step.Step{modules, sysctl, services}
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
