package tuning

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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

// The functions and the steps of the OpenRC tuning in the shells of this
// machine, on runlevels of the test: a service in a runlevel, one in none.
func TestOpenRCServicesStep(t *testing.T) {
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
	s := OpenRC{}.Steps()[2]
	for shell, argv := range shells {
		t.Run(shell, func(t *testing.T) {
			root := t.TempDir()
			for _, d := range []string{"etc/init.d", "etc/runlevels/boot", "etc/runlevels/sysinit", "etc/runlevels/default"} {
				if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
					t.Fatal(err)
				}
			}
			for _, f := range []string{"etc/init.d/modules", "etc/init.d/sysctl", "etc/init.d/cgroups", "etc/runlevels/sysinit/modules"} {
				if err := os.WriteFile(filepath.Join(root, f), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// The paths of the node below root; rc-update (a command: its
			// name is no function name of ash) records what it is asked; the
			// cgroups are not mounted.
			bin := filepath.Join(root, "bin")
			if err := os.MkdirAll(bin, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bin, "rc-update"), []byte("#!/bin/sh\nprintf 'rc-update %s\\n' \"$*\" >&2\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			fix := strings.NewReplacer("/etc/", root+"/etc/").Replace
			stubs := "log() { printf 'log %s\\n' \"$*\"; }\nwarn() { printf 'warn %s\\n' \"$*\"; }\nstat() { echo sysfs; }\n"
			prog := stubs + fix(s.Functions) + "\nif {\n" + fix(s.Check) + "\n}; then echo done; else\n" + fix(s.Apply) + "\nfi\n"
			run := func() ([]byte, error) {
				c := exec.Command(argv[0], append(argv[1:], "-c", prog)...)
				c.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
				return c.CombinedOutput()
			}
			out, err := run()
			want := "rc-update add sysctl boot\nlog sysctl added to the boot runlevel\n" +
				"rc-update add cgroups sysinit\nlog cgroups added to the sysinit runlevel\n" +
				"warn the cgroups are mounted at the next boot: the task drivers of the agent need them"
			if err != nil || strings.TrimSpace(string(out)) != want {
				t.Fatalf("%v:\n%s\nwant:\n%s", err, out, want)
			}
			// Mounted cgroups and every service in a runlevel: nothing to do.
			for _, f := range []string{"etc/runlevels/boot/sysctl", "etc/runlevels/sysinit/cgroups"} {
				if err := os.WriteFile(filepath.Join(root, f), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, err = run()
			if err != nil || strings.TrimSpace(string(out)) != "done" {
				t.Fatalf("second run %v:\n%s", err, out)
			}
		})
	}
}
