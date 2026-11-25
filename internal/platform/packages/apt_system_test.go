package packages

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

// The steps of apt run for real on this machine: NORCA_TEST_APT=1 in a
// Debian test container as root (they install and hold a package of a
// local .deb file, offline).
func TestAptStepsOnThisSystem(t *testing.T) {
	if os.Getenv("NORCA_TEST_APT") != "1" {
		t.Skip("NORCA_TEST_APT is not set (a Debian container as root)")
	}
	if _, err := exec.LookPath("dpkg-deb"); err != nil {
		t.Skip("no dpkg")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "norca-steptest_1.0-1_amd64.deb"), buildDeb(t, "norca-steptest", "1.0-1"), 0o600); err != nil {
		t.Fatal(err)
	}
	off := false
	hp := &config.HostPackages{UpdateCache: &off, Apt: &config.AptPackages{Install: []config.HostPackage{
		{Name: "norca-steptest", File: filepath.Join(dir, "norca-steptest_1.0-1_{arch}.deb"), Version: "1.0-1", Hold: true},
	}}}
	plan := &config.DeploymentPlan{HostPackages: hp}
	t.Cleanup(func() { _ = exec.Command("dpkg", "--purge", "norca-steptest").Run() })

	var out bytes.Buffer
	ex := &step.Executor{Conn: connector.NewLocalConnector(t.Context()), Host: "local", Out: &out}
	apply := func() bool {
		t.Helper()
		steps, err := Apt{}.Steps(Select(hp.Apt, testNode, false), Context{Plan: plan, Arch: "amd64"})
		if err != nil {
			t.Fatal(err)
		}
		changed, _, err := ex.RunAll(t.Context(), steps)
		if err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
		return changed
	}
	if !apply() || !strings.Contains(out.String(), "package norca-steptest 1.0-1") || !strings.Contains(out.String(), "package norca-steptest held") {
		t.Fatalf("first run:\n%s", out.String())
	}
	if held, _ := exec.Command("apt-mark", "showhold").Output(); !strings.Contains(string(held), "norca-steptest") {
		t.Fatalf("not held: %s", held)
	}
	out.Reset()
	if apply() || strings.Contains(out.String(), "held") {
		t.Fatalf("second run changed something:\n%s", out.String())
	}
	// The plan no longer asks for the hold: it is released.
	hp.Apt.Install[0].Hold = false
	out.Reset()
	apply()
	if !strings.Contains(out.String(), "package norca-steptest released") {
		t.Fatalf("hold not released:\n%s", out.String())
	}
	// A file with another package is refused before anything is installed.
	hp.Apt.Install[0].Name = "norca-other"
	steps, err := Apt{}.Steps(Select(hp.Apt, testNode, false), Context{Plan: plan, Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ex.RunAll(t.Context(), steps); err == nil || !strings.Contains(err.Error(), "contains package norca-steptest") {
		t.Fatalf("another package: %v", err)
	}
}

// buildDeb assembles a minimal binary package: an ar archive with
// debian-binary, control.tar.gz and data.tar.gz.
func buildDeb(t *testing.T, name, version string) []byte {
	t.Helper()
	tgz := func(entries ...[2]string) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for _, e := range entries {
			if strings.HasSuffix(e[0], "/") {
				_ = tw.WriteHeader(&tar.Header{Name: e[0], Mode: 0o755, Typeflag: tar.TypeDir, ModTime: time.Unix(0, 0)})
				continue
			}
			_ = tw.WriteHeader(&tar.Header{Name: e[0], Mode: 0o644, Size: int64(len(e[1])), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)})
			_, _ = tw.Write([]byte(e[1]))
		}
		_ = tw.Close()
		_ = gz.Close()
		return buf.Bytes()
	}
	control := fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: all\nMaintainer: test <test@example.com>\nDescription: test package\n", name, version)
	members := []struct {
		name string
		data []byte
	}{
		{"debian-binary", []byte("2.0\n")},
		{"control.tar.gz", tgz([2]string{"./", ""}, [2]string{"./control", control})},
		{"data.tar.gz", tgz([2]string{"./", ""}, [2]string{"./opt/", ""}, [2]string{"./opt/" + name + "/", ""},
			[2]string{"./opt/" + name + "/hello", "hello\n"})},
	}
	var ar bytes.Buffer
	ar.WriteString("!<arch>\n")
	for _, m := range members {
		fmt.Fprintf(&ar, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", m.name, 0, 0, 0, "100644", len(m.data))
		ar.Write(m.data)
		if len(m.data)%2 == 1 {
			ar.WriteByte('\n')
		}
	}
	return ar.Bytes()
}
