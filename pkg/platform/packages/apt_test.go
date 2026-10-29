package packages

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/pkg/artifacts"
	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/connector"
)

const testKey = "-----BEGIN PGP PUBLIC KEY BLOCK-----\nxyz\n-----END PGP PUBLIC KEY BLOCK-----\n"

func testHostPackages() *config.HostPackages {
	return &config.HostPackages{
		Repositories: []config.AptRepository{{
			Name:          "internal",
			URI:           "https://apt.example.com/debian",
			Suite:         "bookworm",
			Components:    []string{"main"},
			Architectures: []string{"amd64"},
			Key:           testKey,
		}},
		Install: []config.HostPackage{
			{Name: "norca-redis", Version: "8.0.2-1norca1", Hold: true, Roles: []string{config.RoleClient}},
			{Name: "jq"},
		},
	}
}

var testNode = &config.NodeConfig{Host: "10.0.0.1", Arch: "amd64", Platform: "linux"}

func TestRenderSource(t *testing.T) {
	repo := &testHostPackages().Repositories[0]
	want := "# Managed by norca-deployer. Do not edit.\n" +
		"Types: deb\n" +
		"URIs: https://apt.example.com/debian\n" +
		"Suites: bookworm\n" +
		"Components: main\n" +
		"Architectures: amd64\n" +
		"Signed-By: /etc/apt/keyrings/norca-internal.asc\n"
	if got := RenderSource(repo, KeyringPath(repo, true)); got != want {
		t.Fatalf("RenderSource:\n%s\nwant:\n%s", got, want)
	}

	flat := &config.AptRepository{Name: "flat", URI: "https://x.example.com/repo", Suite: "./", Key: testKey}
	if got := RenderSource(flat, KeyringPath(flat, false)); strings.Contains(got, "Components:") || !strings.Contains(got, "norca-flat.gpg") {
		t.Fatalf("flat repository:\n%s", got)
	}
}

func TestRenderPreferencesAndAuth(t *testing.T) {
	repo := &config.AptRepository{
		Name: "vendor", URI: "https://user@pkgs.example.com/debian/", PinPriority: 1001,
		PinPackages: []string{"redis*", "norca-redis"},
		Auth:        &config.AptAuth{Username: "deploy", Password: "!enc:x"},
	}
	pref := RenderPreferences(repo)
	for _, want := range []string{"Package: redis* norca-redis\n", `Pin: origin "pkgs.example.com"`, "Pin-Priority: 1001\n"} {
		if !strings.Contains(pref, want) {
			t.Errorf("preferences lack %q:\n%s", want, pref)
		}
	}
	auth := RenderAuth(repo, "s3cret")
	if !strings.Contains(auth, "machine pkgs.example.com/debian\nlogin deploy\npassword s3cret\n") {
		t.Errorf("auth:\n%s", auth)
	}
}

func TestSelectByRole(t *testing.T) {
	hp := testHostPackages()
	node := &config.NodeConfig{Host: "10.0.0.1"}

	master := Select(hp, node, true)
	if len(master.Packages) != 1 || master.Packages[0].Name != "jq" {
		t.Fatalf("master packages = %+v, want only jq", master.Packages)
	}
	client := Select(hp, node, false)
	if len(client.Packages) != 2 {
		t.Fatalf("client packages = %+v, want 2", client.Packages)
	}

	hp.Install[1].Hosts = []string{"worker-7"}
	if sel := Select(hp, &config.NodeConfig{Host: "10.0.0.2", Hostname: "worker-7"}, false); len(sel.Packages) != 2 {
		t.Fatalf("hostname filter: got %+v", sel.Packages)
	}
	if sel := Select(hp, node, false); len(sel.Packages) != 1 {
		t.Fatalf("host filter must exclude other nodes: got %+v", sel.Packages)
	}
}

func TestInstallCommand(t *testing.T) {
	cmd := InstallCommand([]string{"norca-redis=8.0.2-1norca1", "jq", "/var/lib/norca-deployer/debs/x_1_amd64.deb"}, 0)
	for _, want := range []string{
		"DEBIAN_FRONTEND=noninteractive apt-get install -y",
		"--allow-change-held-packages",
		"DPkg::Lock::Timeout=300",
		"'norca-redis=8.0.2-1norca1'",
		"'jq'",
		"'/var/lib/norca-deployer/debs/x_1_amd64.deb'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("install command %q lacks %q", cmd, want)
		}
	}
}

func TestApply(t *testing.T) {
	hp := testHostPackages()
	hp.Repositories[0].PinPriority = 900
	hp.Repositories[0].Auth = &config.AptAuth{Username: "u", Password: "!enc:secret"}
	plan := &config.DeploymentPlan{HostPackages: hp}
	conn := connector.NewFakeConnector()
	conn.Responses["dpkg-query"] = "norca-redis 8.0.2-1norca1\njq 1.6-2.1\n"
	resolve := func(v string) (string, error) { return strings.TrimPrefix(v, "!enc:"), nil }

	sel := Select(hp, testNode, false)
	if err := Apply(conn, plan, sel, testNode, resolve); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if string(conn.Files["/etc/apt/keyrings/norca-internal.asc"]) != testKey {
		t.Errorf("keyring not written")
	}
	if !strings.Contains(string(conn.Files["/etc/apt/sources.list.d/norca-internal.sources"]), "Signed-By: /etc/apt/keyrings/norca-internal.asc") {
		t.Errorf("source not written")
	}
	if !strings.Contains(string(conn.Files["/etc/apt/preferences.d/norca-internal.pref"]), "Pin-Priority: 900") {
		t.Errorf("pin not written")
	}
	if auth := conn.Files["/etc/apt/auth.conf.d/norca-internal.conf"]; !strings.Contains(string(auth), "password secret") ||
		conn.Modes["/etc/apt/auth.conf.d/norca-internal.conf"] != 0600 {
		t.Errorf("auth = %q mode %v", auth, conn.Modes["/etc/apt/auth.conf.d/norca-internal.conf"])
	}
	if len(conn.CommandsWithPrefix("apt-get update")) != 1 {
		t.Errorf("apt-get update not run")
	}
	if hold := conn.CommandsWithPrefix("apt-mark hold"); len(hold) != 1 || !strings.Contains(hold[0], "'norca-redis'") {
		t.Errorf("hold = %v", hold)
	}
}

func TestBinaryKey(t *testing.T) {
	hp := testHostPackages()
	binKey := []byte{0x99, 0x01, 0x0d, 0x04} // old-format public key packet
	hp.Repositories[0].Key = string(binKey)
	hp.Install = nil
	conn := connector.NewFakeConnector()

	sel := Select(hp, testNode, false)
	if err := Apply(conn, &config.DeploymentPlan{HostPackages: hp}, sel, testNode, nil); err != nil {
		t.Fatal(err)
	}
	if string(conn.Files["/etc/apt/keyrings/norca-internal.gpg"]) != string(binKey) {
		t.Fatalf("binary key must go to a .gpg keyring: %v", conn.Files)
	}
	if !strings.Contains(string(conn.Files["/etc/apt/sources.list.d/norca-internal.sources"]), "norca-internal.gpg") {
		t.Fatalf("source must reference the .gpg keyring")
	}
	if rm := conn.CommandsWithPrefix("rm -f '/etc/apt/keyrings/norca-internal.asc'"); len(rm) != 1 {
		t.Fatalf("stale armored keyring not removed: %v", conn.Commands)
	}
}

func TestLocalDebWithoutRepository(t *testing.T) {
	dir := t.TempDir()
	deb := filepath.Join(dir, "norca-redis_8.0.2-1norca1_amd64.deb")
	if err := os.WriteFile(deb, []byte("fake deb"), 0644); err != nil {
		t.Fatal(err)
	}
	sum, _ := artifacts.FileSHA256(deb)

	hp := &config.HostPackages{Install: []config.HostPackage{{
		Name: "norca-redis", File: filepath.Join(dir, "norca-redis_8.0.2-1norca1_{arch}.deb"), Version: "8.0.2-1norca1",
	}}}
	plan := &config.DeploymentPlan{HostPackages: hp}
	conn := connector.NewFakeConnector()
	conn.Responses["dpkg-deb -f"] = "Package: norca-redis\nVersion: 8.0.2-1norca1\n"
	conn.Responses["dpkg-query"] = "norca-redis 8.0.2-1norca1\n"

	sel := Select(hp, testNode, false)
	if err := Apply(conn, plan, sel, testNode, nil); err != nil {
		t.Fatal(err)
	}
	remote := DebDir + "/norca-redis_8.0.2-1norca1_amd64.deb"
	if string(conn.Files[remote]) != "fake deb" {
		t.Fatalf(".deb not copied: %v", conn.Files)
	}
	if len(conn.CommandsWithPrefix("apt-get update")) != 0 {
		t.Fatalf("offline install must not run apt-get update")
	}
	if inst := conn.CommandsWithPrefix("DEBIAN_FRONTEND"); len(inst) != 1 || !strings.Contains(inst[0], "'"+remote+"'") {
		t.Fatalf("install = %v", inst)
	}

	// Second apply: same digest on the node, no upload.
	conn2 := connector.NewFakeConnector()
	conn2.Responses["sha256sum"] = sum + "  " + remote + "\n"
	conn2.Responses["dpkg-deb -f"] = "Package: norca-redis\nVersion: 8.0.2-1norca1\n"
	conn2.Responses["dpkg-query"] = "norca-redis 8.0.2-1norca1\n"
	if err := Apply(conn2, plan, sel, testNode, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := conn2.Files[remote]; ok {
		t.Fatalf("unchanged .deb uploaded again")
	}
}

func TestLocalDebWrongPackage(t *testing.T) {
	dir := t.TempDir()
	deb := filepath.Join(dir, "x.deb")
	os.WriteFile(deb, []byte("x"), 0644)
	hp := &config.HostPackages{Install: []config.HostPackage{{Name: "norca-redis", File: deb}}}
	conn := connector.NewFakeConnector()
	conn.Responses["dpkg-deb -f"] = "Package: other\nVersion: 1\n"

	err := Apply(conn, &config.DeploymentPlan{HostPackages: hp}, Select(hp, testNode, false), testNode, nil)
	if err == nil || !strings.Contains(err.Error(), `contains package "other"`) {
		t.Fatalf("expected a package name error, got %v", err)
	}
	if len(conn.CommandsWithPrefix("DEBIAN_FRONTEND")) != 0 {
		t.Fatalf("a mismatching .deb must not be installed")
	}
}

func TestApplyVersionMismatch(t *testing.T) {
	hp := testHostPackages()
	conn := connector.NewFakeConnector()
	conn.Responses["dpkg-query"] = "norca-redis 8.0.1-1norca1\njq 1.6-2.1\n"

	err := Apply(conn, &config.DeploymentPlan{HostPackages: hp}, Select(hp, testNode, false), testNode, nil)
	if err == nil || !strings.Contains(err.Error(), "8.0.1-1norca1 installed") {
		t.Fatalf("expected version mismatch error, got %v", err)
	}
}

func TestApplyNotDebian(t *testing.T) {
	hp := testHostPackages()
	conn := connector.NewFakeConnector()
	conn.Failures["command -v apt-get"] = errors.New("exit 1")

	err := Apply(conn, &config.DeploymentPlan{HostPackages: hp}, Select(hp, testNode, false), testNode, nil)
	if err == nil || !strings.Contains(err.Error(), "Debian-based") {
		t.Fatalf("expected a Debian check error, got %v", err)
	}
	if len(conn.Files) != 0 {
		t.Fatalf("nothing may be written on a non-Debian node: %v", conn.Files)
	}
}
