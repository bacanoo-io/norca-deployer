package packages

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/platform/step"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// The steps of dnf run for real on this machine: NORCA_TEST_DNF=1 in a
// RHEL-like container as root, with rpmbuild and a reachable distribution
// repository (the versionlock plugin of dnf 4 and the package tree come
// from there). They install, hold, upgrade and release a package of local
// .rpm files built here, install a package of the repository, wait for
// another package manager process and refuse a file of another package.
func TestDnfStepsOnThisSystem(t *testing.T) {
	if os.Getenv("NORCA_TEST_DNF") != "1" {
		t.Skip("NORCA_TEST_DNF is not set (a RHEL-like container as root)")
	}
	if _, err := exec.LookPath("rpmbuild"); err != nil {
		t.Skip("no rpmbuild")
	}
	dir := t.TempDir()
	rpm1 := buildRPM(t, dir, "norca-steptest", "1.0", "1")
	rpm2 := buildRPM(t, dir, "norca-steptest", "2.0", "1")
	hp := &config.HostPackages{Dnf: &config.DnfPackages{Install: []config.HostPackage{
		{Name: "norca-steptest", File: rpm1, Version: "1.0-1", Hold: true},
		{Name: "tree"},
	}}}
	plan := &config.DeploymentPlan{HostPackages: hp}
	t.Cleanup(func() {
		_ = exec.Command("dnf", "-q", "--disablerepo=*", "versionlock", "delete", "norca-steptest").Run()
		_ = exec.Command("rpm", "-e", "norca-steptest", "tree").Run()
	})

	var out bytes.Buffer
	ex := &step.Executor{Conn: connector.NewLocalConnector(t.Context()), Host: "local", Out: &out}
	apply := func(t *testing.T) (bool, error) {
		t.Helper()
		out.Reset()
		steps, err := Dnf{}.Steps(SelectDnf(hp.Dnf, testNode, false), Context{Plan: plan, Arch: "amd64"})
		if err != nil {
			t.Fatal(err)
		}
		changed, _, err := ex.RunAll(t.Context(), steps)
		return changed, err
	}
	installed := func(t *testing.T, name string) string {
		t.Helper()
		v, _ := exec.Command("rpm", "-q", "--qf", "%{VERSION}-%{RELEASE}", name).Output()
		return string(v)
	}
	locks := func(t *testing.T) string {
		t.Helper()
		v, _ := exec.Command("dnf", "-q", "--disablerepo=*", "versionlock", "list").CombinedOutput()
		return string(v)
	}

	t.Run("first run installs and holds", func(t *testing.T) {
		changed, err := apply(t)
		if err != nil || !changed {
			t.Fatalf("changed %t, %v\n%s", changed, err, out.String())
		}
		for _, w := range []string{"package norca-steptest 1.0-1", "package norca-steptest held", "package tree "} {
			if !strings.Contains(out.String(), w) {
				t.Errorf("no %q in:\n%s", w, out.String())
			}
		}
		if v := installed(t, "norca-steptest"); v != "1.0-1" {
			t.Fatalf("installed %q", v)
		}
		if l := locks(t); !strings.Contains(l, "norca-steptest") || !strings.Contains(l, "1.0-1") {
			t.Fatalf("not held at 1.0-1: %s", l)
		}
	})
	t.Run("second run changes nothing", func(t *testing.T) {
		changed, err := apply(t)
		if err != nil || changed || strings.Contains(out.String(), "held") {
			t.Fatalf("changed %t, %v\n%s", changed, err, out.String())
		}
	})
	t.Run("a new version of a held package", func(t *testing.T) {
		hp.Dnf.Install[0].File, hp.Dnf.Install[0].Version = rpm2, "2.0-1"
		changed, err := apply(t)
		if err != nil || !changed {
			t.Fatalf("changed %t, %v\n%s", changed, err, out.String())
		}
		if v := installed(t, "norca-steptest"); v != "2.0-1" {
			t.Fatalf("installed %q\n%s", v, out.String())
		}
		if l := locks(t); !strings.Contains(l, "2.0-1") || strings.Contains(l, "1.0-1") {
			t.Fatalf("the hold did not move to 2.0-1: %s", l)
		}
		if changed, err := apply(t); err != nil || changed {
			t.Fatalf("again: changed %t, %v\n%s", changed, err, out.String())
		}
	})
	t.Run("the hold is released", func(t *testing.T) {
		hp.Dnf.Install[0].Hold = false
		if _, err := apply(t); err != nil || !strings.Contains(out.String(), "package norca-steptest released") {
			t.Fatalf("%v\n%s", err, out.String())
		}
		if l := locks(t); strings.Contains(l, "norca-steptest") {
			t.Fatalf("still held: %s", l)
		}
	})
	t.Run("another package manager process", func(t *testing.T) {
		// A process named rpm holds the package manager: the steps wait
		// for it at most lock_timeout.
		bin := filepath.Join(dir, "bin")
		if err := os.MkdirAll(bin, 0o750); err != nil {
			t.Fatal(err)
		}
		sleep, err := exec.LookPath("sleep")
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(sleep)
		if err != nil {
			t.Fatal(err)
		}
		fake := filepath.Join(bin, "rpm")
		if err := os.WriteFile(fake, data, 0o700); err != nil {
			t.Fatal(err)
		}
		holder := exec.Command(fake, "60")
		if err := holder.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()
		hp.LockTimeout = 4
		hp.Dnf.Install[0].File, hp.Dnf.Install[0].Version = rpm1, "1.0-1"
		start := time.Now()
		_, err = apply(t)
		if err == nil || !strings.Contains(err.Error(), "still runs after 4 seconds (host_packages.lock_timeout)") ||
			!strings.Contains(out.String(), "waiting for rpm (process ") || time.Since(start) < 4*time.Second {
			t.Fatalf("after %s: %v\n%s", time.Since(start), err, out.String())
		}
		_ = holder.Process.Kill()
		_ = holder.Wait()
		// Without the other process the downgrade to 1.0-1 goes on.
		if _, err := apply(t); err != nil || installed(t, "norca-steptest") != "1.0-1" {
			t.Fatalf("%v, installed %q\n%s", err, installed(t, "norca-steptest"), out.String())
		}
	})
	t.Run("a file of another package", func(t *testing.T) {
		hp.Dnf.Install[0].Name = "norca-other"
		_, err := apply(t)
		if err == nil || !strings.Contains(err.Error(), "contains package norca-steptest") {
			t.Fatalf("another package: %v\n%s", err, out.String())
		}
	})
}

// buildRPM builds a noarch package with one file with rpmbuild and returns
// its path.
func buildRPM(t *testing.T, dir, name, version, release string) string {
	t.Helper()
	top := filepath.Join(dir, "rpmbuild-"+version)
	spec := filepath.Join(dir, name+"-"+version+".spec")
	body := fmt.Sprintf("Name: %[1]s\nVersion: %[2]s\nRelease: %[3]s\nSummary: test package\nLicense: MIT\nBuildArch: noarch\n\n"+
		"%%description\ntest package\n\n%%install\nmkdir -p %%{buildroot}/opt/%[1]s\necho %[2]s >%%{buildroot}/opt/%[1]s/version\n\n"+
		"%%files\n/opt/%[1]s/version\n", name, version, release)
	if err := os.WriteFile(spec, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("rpmbuild", "-bb", "--define", "_topdir "+top, spec).CombinedOutput(); err != nil {
		t.Fatalf("rpmbuild: %v\n%s", err, out)
	}
	return filepath.Join(top, "RPMS", "noarch", fmt.Sprintf("%s-%s-%s.noarch.rpm", name, version, release))
}
