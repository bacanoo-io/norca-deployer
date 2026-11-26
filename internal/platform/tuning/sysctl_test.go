package tuning

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/platform/step"
)

// The OpenRC tuning writes the files of Linux (the modules and sysctl
// services of OpenRC read them at boot), applies the settings with sysctl
// -p, and puts the services of OpenRC into their runlevels.
func TestOpenRCSteps(t *testing.T) {
	steps := OpenRC{}.Steps()
	linux := Linux{}.Steps()
	if len(steps) != 3 || steps[0].Name != "kernel modules" || steps[1].Name != "sysctl" || steps[2].Name != "openrc services" {
		t.Fatalf("steps %+v", steps)
	}
	if steps[0].Files[0].Path != ModulesConfigPath || steps[0].Apply != linux[0].Apply {
		t.Errorf("modules: %+v", steps[0])
	}
	if steps[1].Files[0].Path != SysctlConfigPath || !strings.Contains(steps[1].Apply, "sysctl -p "+SysctlConfigPath) ||
		strings.Contains(steps[1].Apply, "--system") {
		t.Errorf("sysctl:\n%s", steps[1].Apply)
	}
	if linux[1].Apply == steps[1].Apply || !strings.Contains(linux[1].Apply, "sysctl --system") {
		t.Errorf("the sysctl step of Linux changed:\n%s", linux[1].Apply)
	}
}

// testShells are the POSIX shells of this machine, by name.
func testShells(t *testing.T) map[string][]string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the steps run in a POSIX shell")
	}
	shells := map[string][]string{}
	for _, name := range []string{"dash", "bash"} {
		if p, err := exec.LookPath(name); err == nil {
			shells[name] = []string{p}
		}
	}
	if bb := os.Getenv("NORCA_TEST_BUSYBOX"); bb != "" {
		shells["busybox"] = []string{bb, "sh"}
	}
	if len(shells) == 0 {
		t.Skip("no POSIX shell")
	}
	return shells
}

// The sysctl step of OpenRC on the answers of busybox sysctl (exit 0 after
// errors) and of procps: a key the host does not have (outside the first
// network namespace, or before br_netfilter is loaded) is a warning; any
// other error stops the step where /proc/sys is writable.
func TestOpenRCSysctl(t *testing.T) {
	apply := OpenRC{}.Steps()[1].Apply
	tests := []struct {
		name, out string
		rc        int
		want      string
	}{
		{name: "applied", out: "vm.swappiness = 1\nnet.ipv4.ip_forward = 1", want: ""},
		{name: "keys of busybox the host does not have", out: "vm.swappiness = 1\n" +
			"sysctl: error: 'net.ipv4.neigh.default/gc_thresh1' is an unknown key\nsysctl: error: 'net.bridge.bridge-nf-call-iptables' is an unknown key",
			want: "warn the sysctl keys this host does not have were skipped: net.ipv4.neigh.default/gc_thresh1 net.bridge.bridge-nf-call-iptables"},
		{name: "keys of procps the host does not have", rc: 255,
			out:  "sysctl: cannot stat /proc/sys/net/bridge/bridge-nf-call-iptables: No such file or directory",
			want: "warn the sysctl keys this host does not have were skipped: net/bridge/bridge-nf-call-iptables"},
		{name: "keys of busybox that are read-only here", out: "sysctl: error setting key 'vm.swappiness': Read-only file system\nnet.ipv4.ip_forward = 1",
			want: "warn the sysctl keys that are read-only on this host were not set (a container?); they apply at the next boot: vm.swappiness"},
		{name: "keys of procps that are read-only here", rc: 255, out: "sysctl: setting key \"vm.swappiness\": Read-only file system",
			want: "warn the sysctl keys that are read-only on this host were not set (a container?); they apply at the next boot: vm.swappiness"},
		{name: "another error of busybox", out: "sysctl: error setting key 'vm.swappiness': Invalid argument",
			want: "error sysctl -p " + SysctlConfigPath + " failed: sysctl: error setting key 'vm.swappiness': Invalid argument"},
	}
	for shell, argv := range testShells(t) {
		t.Run(shell, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					dir := t.TempDir()
					// /proc/sys is writable here: an error stops the step.
					stubs := "log() { printf 'log %s\\n' \"$*\"; }\nwarn() { printf 'warn %s\\n' \"$*\"; }\n" +
						"die() {\n  printf 'error %s\\n' \"$*\"\n  exit 1\n}\n" +
						"sysctl() {\n  printf '%s\\n' \"$SYSCTL_OUT\" >&2\n  return \"$SYSCTL_RC\"\n}\n"
					prog := stubs + "set -eu\n" + strings.ReplaceAll(apply, "/proc/sys/net/ipv4/ip_forward", filepath.Join(dir, "w")) + "\n"
					if err := os.WriteFile(filepath.Join(dir, "w"), nil, 0o600); err != nil {
						t.Fatal(err)
					}
					c := exec.Command(argv[0], append(argv[1:], "-c", prog)...)
					c.Env = []string{"PATH=/usr/bin:/bin", "SYSCTL_OUT=" + tt.out, "SYSCTL_RC=" + strconv.Itoa(tt.rc)}
					out, _ := c.CombinedOutput()
					if got := strings.TrimSpace(string(out)); got != tt.want {
						t.Fatalf("got %q, want %q", got, tt.want)
					}
				})
			}
		})
	}
}

// openrcNode is a node of OpenRC below a test directory: its init scripts
// and runlevels, /run/openrc while OpenRC runs, and the commands rc-update
// and rc-service (commands: their names are no function names of ash),
// which record what they are asked; proc/mounts stands for /proc/mounts.
// rc-service cgroups start mounts the cgroups (a cgroup2 line in
// proc/mounts) unless RC_SERVICE_FAIL is set; RC_UPDATE_FAIL makes
// rc-update fail.
type openrcNode struct {
	root, bin string
}

func newOpenRCNode(t *testing.T, files ...string) *openrcNode {
	t.Helper()
	n := &openrcNode{root: t.TempDir()}
	n.bin = filepath.Join(n.root, "bin")
	for _, d := range []string{"bin", "proc", "etc/init.d", "etc/runlevels/boot", "etc/runlevels/sysinit", "etc/runlevels/default"} {
		if err := os.MkdirAll(filepath.Join(n.root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	// Mounted: sysfs, and a cgroup2 elsewhere than on /sys/fs/cgroup.
	if err := os.WriteFile(filepath.Join(n.root, "proc/mounts"), []byte(otherMounts), 0o600); err != nil {
		t.Fatal(err)
	}
	n.add(t, files...)
	// They record to descriptor 3, which the step does not redirect.
	commands := map[string]string{
		"rc-update": "#!/bin/sh\nprintf 'rc-update %s\\n' \"$*\" >&3\nif [ -n \"${RC_UPDATE_FAIL:-}\" ]; then\n  echo 'rc-update: failed' >&2\n  exit 1\nfi\n",
		"rc-service": "#!/bin/sh\nprintf 'rc-service %s\\n' \"$*\" >&3\n[ -z \"${RC_SERVICE_FAIL:-}\" ] || exit 1\n" +
			"if [ \"$*\" = 'cgroups start' ]; then\n  echo '" + cgroupMount + "' >>\"$NODE_ROOT/proc/mounts\"\nfi\n",
	}
	for name, body := range commands {
		if err := os.WriteFile(filepath.Join(n.bin, name), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return n
}

// The lines of proc/mounts: the cgroups the cgroups service mounts, and
// mounts that are no cgroups on /sys/fs/cgroup.
const (
	cgroupMount = "cgroup2 /sys/fs/cgroup cgroup2 rw,nosuid,nodev,noexec,relatime,nsdelegate 0 0"
	otherMounts = "sysfs /sys sysfs rw,nosuid,nodev,noexec,relatime 0 0\n" +
		"cgroup2 /mnt/cgroup cgroup2 rw,relatime 0 0\n"
)

// add creates files of the node (empty), such as an init script, a service
// in a runlevel, run/openrc (OpenRC runs) or mounted (the cgroups are).
func (n *openrcNode) add(t *testing.T, files ...string) {
	t.Helper()
	for _, f := range files {
		switch f {
		case "run/openrc":
			if err := os.MkdirAll(filepath.Join(n.root, f), 0o750); err != nil {
				t.Fatal(err)
			}
			continue
		case "mounted":
			if err := os.WriteFile(filepath.Join(n.root, "proc/mounts"), []byte(otherMounts+cgroupMount+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(filepath.Join(n.root, f), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// run runs a step on the node as the Executor does: its Apply when its
// Check fails ("done" when it holds). The output is what the commands
// recorded and the log of the step.
func (n *openrcNode) run(t *testing.T, argv []string, s step.Step, env ...string) string {
	t.Helper()
	fix := strings.NewReplacer("/etc/", n.root+"/etc/", "/run/openrc", n.root+"/run/openrc", "/proc/mounts", n.root+"/proc/mounts").Replace
	stubs := "log() { printf 'log %s\\n' \"$*\"; }\nwarn() { printf 'warn %s\\n' \"$*\"; }\n"
	prog := stubs + "set -eu\nexec 3>&2\n" + fix(s.Functions) + "\n"
	if s.Check != "" {
		prog += "if {\n" + fix(s.Check) + "\n}; then\n  echo done\n  exit 0\nfi\n"
	}
	prog += fix(s.Apply) + "\n"
	c := exec.Command(argv[0], append(argv[1:], "-c", prog)...)
	c.Env = append([]string{"PATH=" + n.bin + ":/usr/bin:/bin", "NODE_ROOT=" + n.root}, env...)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// The services step of the OpenRC tuning in the shells of this machine:
// the modules and sysctl services go into the boot runlevel when they are
// in none; the cgroups are the init steps' (TestOpenRCCgroupsStep).
func TestOpenRCServicesStep(t *testing.T) {
	s := OpenRC{}.Steps()[2]
	for shell, argv := range testShells(t) {
		t.Run(shell, func(t *testing.T) {
			n := newOpenRCNode(t, "etc/init.d/modules", "etc/init.d/sysctl", "etc/init.d/cgroups", "etc/runlevels/sysinit/modules")
			want := "rc-update add sysctl boot\nlog sysctl added to the boot runlevel"
			if out := n.run(t, argv, s); out != want {
				t.Fatalf("got:\n%s\nwant:\n%s", out, want)
			}
			n.add(t, "etc/runlevels/boot/sysctl")
			if out := n.run(t, argv, s); out != "done" {
				t.Fatalf("second run:\n%s", out)
			}
		})
	}
}

// The init steps of OpenRC: the dependencies of OpenRC read again, then
// the cgroups. Only OpenRC has them.
func TestOpenRCInitSteps(t *testing.T) {
	steps := OpenRC{}.InitSteps()
	if len(steps) != 2 || steps[0].Name != "openrc dependencies" || steps[1].Name != "cgroups" {
		t.Fatalf("steps %+v", steps)
	}
	// Read on every apply, as a refresh of a cache: no check, no change.
	if steps[0].Check != "" || !strings.Contains(steps[0].Apply, "rc-update -u") {
		t.Errorf("dependencies: %+v", steps[0])
	}
	for _, tn := range []Tuner{Linux{}, Windows{}} {
		if s := tn.InitSteps(); len(s) != 0 {
			t.Errorf("%T has init steps %+v", tn, s)
		}
	}
	for _, s := range (OpenRC{}).Steps() {
		if strings.Contains(s.Check+s.Apply, "cgroups") {
			t.Errorf("the tuning step %q handles the cgroups", s.Name)
		}
	}
}

// The step of the dependencies of OpenRC: rc-update -u while OpenRC runs
// (an image built in a container has no /run/openrc), a warning when it
// fails.
func TestOpenRCDependenciesStep(t *testing.T) {
	s := OpenRC{}.InitSteps()[0]
	for shell, argv := range testShells(t) {
		t.Run(shell, func(t *testing.T) {
			n := newOpenRCNode(t)
			if out := n.run(t, argv, s); out != "" {
				t.Fatalf("without OpenRC running: %q", out)
			}
			n.add(t, "run/openrc")
			if out := n.run(t, argv, s); out != "rc-update -u" {
				t.Fatalf("OpenRC runs: %q", out)
			}
			want := "rc-update -u\nwarn rc-update -u failed, so OpenRC may start a service of the host packages without the services it needs: rc-update: failed"
			if out := n.run(t, argv, s, "RC_UPDATE_FAIL=1"); out != want {
				t.Fatalf("got:\n%s\nwant:\n%s", out, want)
			}
		})
	}
}

// The cgroups step: the cgroups service goes into the sysinit runlevel
// when no cgroup file system is mounted, and is started while OpenRC runs,
// so that what starts after it (a post_packages script that starts Docker,
// the agent) finds them mounted. Cgroups mounted by others (a container)
// are left alone.
func TestOpenRCCgroupsStep(t *testing.T) {
	s := OpenRC{}.InitSteps()[1]
	added := "rc-update add cgroups sysinit\nlog cgroups added to the sysinit runlevel\n"
	tests := []struct {
		name  string
		files []string
		env   []string
		want  string
		// again is the output of a second run.
		again string
	}{
		{name: "openrc runs", files: []string{"run/openrc"},
			want: added + "rc-service cgroups start\nlog cgroups mounted", again: "done"},
		{name: "in a runlevel but not started", files: []string{"run/openrc", "etc/runlevels/sysinit/cgroups"},
			want: "rc-service cgroups start\nlog cgroups mounted", again: "done"},
		{name: "an image build, openrc does not run",
			want: added + "warn the cgroups are mounted at the next boot: the task drivers of the agent and Docker need them", again: "done"},
		{name: "the start fails", files: []string{"run/openrc"}, env: []string{"RC_SERVICE_FAIL=1"},
			want: added + "rc-service cgroups start\nwarn the cgroups could not be mounted now (rc-service cgroups start): " +
				"a service that needs them (Docker) fails to start until they are; they are mounted at the next boot",
			again: "rc-service cgroups start\nwarn the cgroups could not be mounted now (rc-service cgroups start): " +
				"a service that needs them (Docker) fails to start until they are; they are mounted at the next boot"},
		{name: "mounted by others", files: []string{"run/openrc", "mounted"}, want: "done", again: "done"},
		{name: "mounted and in a runlevel", files: []string{"run/openrc", "mounted", "etc/runlevels/sysinit/cgroups"}, want: "done", again: "done"},
	}
	for shell, argv := range testShells(t) {
		t.Run(shell, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					n := newOpenRCNode(t, append([]string{"etc/init.d/cgroups"}, tt.files...)...)
					out := n.run(t, argv, s, tt.env...)
					if out != tt.want {
						t.Fatalf("got:\n%s\nwant:\n%s", out, tt.want)
					}
					if strings.Contains(out, "rc-update add cgroups sysinit") {
						// What rc-update add does: the link of the runlevel.
						n.add(t, "etc/runlevels/sysinit/cgroups")
					}
					if out := n.run(t, argv, s, tt.env...); out != tt.again {
						t.Fatalf("second run:\n%s\nwant:\n%s", out, tt.again)
					}
				})
			}
			// No cgroups service: nothing to do.
			if out := newOpenRCNode(t, "run/openrc").run(t, argv, s); out != "done" {
				t.Fatalf("without the cgroups service: %q", out)
			}
		})
	}
}
