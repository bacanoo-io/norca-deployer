//go:build e2e

// End-to-end test against a real Debian node over SSH with password sudo.
//
//	NORCA_E2E_SSH=127.0.0.1:2222 NORCA_E2E_USER=deploy NORCA_E2E_PASSWORD=... \
//	  go test -tags e2e ./pkg/e2e/
//
// The node needs openssh-server and sudo, and internet access for the APT
// part (deb.debian.org).
package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/connector"
	"github.com/bacanoo-io/norca-deployer/pkg/platform/packages"
	"github.com/bacanoo-io/norca-deployer/pkg/platform/scripts"
)

func connect(t *testing.T, knownHosts string) *connector.SSHConnector {
	t.Helper()
	addr := os.Getenv("NORCA_E2E_SSH")
	if addr == "" {
		t.Skip("NORCA_E2E_SSH not set")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)
	cb, err := connector.HostKeyCallback("tofu", "", knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := connector.NewSSHConnector(connector.SSHOptions{
		Host: host, Port: port,
		User:            os.Getenv("NORCA_E2E_USER"),
		Password:        os.Getenv("NORCA_E2E_PASSWORD"),
		Become:          true,
		BecomePassword:  os.Getenv("NORCA_E2E_PASSWORD"),
		HostKeyCallback: cb,
		Timeout:         10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

func TestBecomeFileOperations(t *testing.T) {
	known := filepath.Join(t.TempDir(), "known_hosts")
	conn := connect(t, known)
	defer conn.Close()

	if out, err := conn.ExecuteCommand("id -u"); err != nil || strings.TrimSpace(out) != "0" {
		t.Fatalf("become: id -u = %q, %v", out, err)
	}
	if err := conn.MkdirAll("/etc/norca-e2e/sub", 0750); err != nil {
		t.Fatal(err)
	}
	if err := conn.CopyFile(strings.NewReader("secret\n"), "/etc/norca-e2e/sub/key.pem", 0600); err != nil {
		t.Fatal(err)
	}
	data, err := conn.ReadFile("/etc/norca-e2e/sub/key.pem")
	if err != nil || string(data) != "secret\n" {
		t.Fatalf("ReadFile = %q, %v", data, err)
	}
	out, _ := conn.ExecuteCommand("stat -c '%a %U' /etc/norca-e2e/sub/key.pem")
	if strings.TrimSpace(out) != "600 root" {
		t.Fatalf("mode/owner = %q", out)
	}
	if ok, _ := conn.FileExists("/etc/norca-e2e/missing"); ok {
		t.Fatal("missing file reported as existing")
	}
	changed, err := connector.SyncFile(conn, "/etc/norca-e2e/sub/key.pem", []byte("secret\n"), 0600)
	if err != nil || changed {
		t.Fatalf("SyncFile on identical content: changed=%t err=%v", changed, err)
	}

	// The second connection checks the recorded host key.
	conn2 := connect(t, known)
	conn2.Close()
}

func TestScriptsAndPackages(t *testing.T) {
	conn := connect(t, filepath.Join(t.TempDir(), "known_hosts"))
	defer conn.Close()

	dir := t.TempDir()
	plan := &config.DeploymentPlan{
		Scripts: []config.ScriptConfig{{
			Name: "e2e-marker", Phase: config.PhasePreInstall,
			Inline: "echo \"run $E2E_VALUE\" >> /root/e2e-script.log\n",
			Env:    map[string]string{"E2E_VALUE": "it's ok"},
		}},
	}
	node := &config.NodeConfig{Host: "e2e", Platform: "linux", Arch: "amd64"}
	for i := 0; i < 2; i++ {
		if err := scripts.Run(conn, plan, config.PhasePreInstall, node, false, nil); err != nil {
			t.Fatal(err)
		}
	}
	out, _ := conn.ExecuteCommand("cat /root/e2e-script.log; rm -f /root/e2e-script.log " + scripts.MarkerPath("e2e-marker"))
	if strings.Count(out, "run it's ok") != 1 {
		t.Fatalf("script must run exactly once: %q", out)
	}

	// Binary Debian archive key from the node itself.
	debianKey, err := conn.ReadFile("/usr/share/keyrings/debian-archive-keyring.gpg")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(dir, "debian.gpg")
	os.WriteFile(keyFile, debianKey, 0644)

	deb := filepath.Join(dir, "norca-e2e_1.0-1_amd64.deb")
	if err := os.WriteFile(deb, buildDeb(t, "norca-e2e", "1.0-1", "amd64"), 0644); err != nil {
		t.Fatal(err)
	}

	plan.HostPackages = &config.HostPackages{
		Repositories: []config.AptRepository{{
			Name: "backports", URI: "https://deb.debian.org/debian", Suite: "bookworm-backports",
			Components: []string{"main"}, KeyFile: keyFile,
			PinPriority: 200, PinPackages: []string{"jq"}, PinRelease: "n=bookworm-backports",
			Auth: &config.AptAuth{Username: "nobody", Password: "unused"},
		}},
		Install: []config.HostPackage{
			{Name: "jq"},
			{Name: "norca-e2e", File: filepath.Join(dir, "norca-e2e_1.0-1_{arch}.deb"), Version: "1.0-1", Hold: true},
		},
	}
	sel := packages.Select(plan.HostPackages, node, false)
	if err := packages.Apply(conn, plan, sel, node, nil); err != nil {
		t.Fatal(err)
	}
	// Idempotent second run.
	if err := packages.Apply(conn, plan, sel, node, nil); err != nil {
		t.Fatal(err)
	}

	checks := map[string]string{
		"cat /opt/norca-e2e/hello":                                       "hello from norca-e2e",
		"apt-mark showhold":                                              "norca-e2e",
		"stat -c '%a' /etc/apt/auth.conf.d/norca-backports.conf":         "600",
		"grep Signed-By /etc/apt/sources.list.d/norca-backports.sources": "/etc/apt/keyrings/norca-backports.gpg",
		"grep Pin: /etc/apt/preferences.d/norca-backports.pref":          "Pin: release n=bookworm-backports",
	}
	// The release pin must only hit bookworm-backports; jq comes from the
	// main suites and keeps their priority (an origin pin would raise it).
	if out, _ := conn.ExecuteCommand("apt-cache policy jq"); strings.Contains(out, " 200\n") {
		t.Errorf("release pin leaked to other suites:\n%s", out)
	}
	for cmd, want := range checks {
		out, err := conn.ExecuteCommand(cmd)
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("%s = %q, %v (want %q)", cmd, out, err, want)
		}
	}
}

// buildDeb assembles a minimal binary package: an ar archive with
// debian-binary, control.tar.gz and data.tar.gz.
func buildDeb(t *testing.T, name, version, arch string) []byte {
	t.Helper()
	// Entries ending in "/" are directories; order matters for dpkg.
	tgz := func(entries ...[2]string) []byte {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		for _, e := range entries {
			if strings.HasSuffix(e[0], "/") {
				tw.WriteHeader(&tar.Header{Name: e[0], Mode: 0755, Typeflag: tar.TypeDir, ModTime: time.Unix(0, 0)})
				continue
			}
			tw.WriteHeader(&tar.Header{Name: e[0], Mode: 0644, Size: int64(len(e[1])), Typeflag: tar.TypeReg, ModTime: time.Unix(0, 0)})
			tw.Write([]byte(e[1]))
		}
		tw.Close()
		gz.Close()
		return buf.Bytes()
	}
	control := fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: e2e <e2e@example.com>\nDescription: e2e test package\n", name, version, arch)
	members := []struct {
		name string
		data []byte
	}{
		{"debian-binary", []byte("2.0\n")},
		{"control.tar.gz", tgz([2]string{"./", ""}, [2]string{"./control", control})},
		{"data.tar.gz", tgz([2]string{"./", ""}, [2]string{"./opt/", ""}, [2]string{"./opt/norca-e2e/", ""},
			[2]string{"./opt/norca-e2e/hello", "hello from norca-e2e\n"})},
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
