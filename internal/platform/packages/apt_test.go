package packages

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/artifacts"
	"github.com/bacanoo-io/norca-deployer/internal/platform/step"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

const testKey = "-----BEGIN PGP PUBLIC KEY BLOCK-----\nxyz\n-----END PGP PUBLIC KEY BLOCK-----\n"

func testHostPackages() *config.HostPackages {
	return &config.HostPackages{Apt: &config.AptPackages{
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
	}}
}

var testNode = &config.NodeConfig{Host: "10.0.0.1", Arch: "amd64", Platform: "linux"}

func TestRenderSource(t *testing.T) {
	repo := &testHostPackages().Apt.Repositories[0]
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

	master := Select(hp.Apt, node, true)
	if len(master.Packages) != 1 || master.Packages[0].Name != "jq" {
		t.Fatalf("master packages = %+v, want only jq", master.Packages)
	}
	client := Select(hp.Apt, node, false)
	if len(client.Packages) != 2 {
		t.Fatalf("client packages = %+v, want 2", client.Packages)
	}

	hp.Apt.Install[1].Hosts = []string{"worker-7"}
	if sel := Select(hp.Apt, &config.NodeConfig{Host: "10.0.0.2", Hostname: "worker-7"}, false); len(sel.Packages) != 2 {
		t.Fatalf("hostname filter: got %+v", sel.Packages)
	}
	if sel := Select(hp.Apt, node, false); len(sel.Packages) != 1 {
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

// stepNamed returns the step of a name.
func stepNamed(t *testing.T, steps []step.Step, name string) *step.Step {
	t.Helper()
	for i := range steps {
		if steps[i].Name == name {
			return &steps[i]
		}
	}
	var names []string
	for _, s := range steps {
		names = append(names, s.Name)
	}
	t.Fatalf("no step %q in %q", name, names)
	return nil
}

func fileOf(s *step.Step, path string) *step.File {
	for i := range s.Files {
		if s.Files[i].Path == path {
			return &s.Files[i]
		}
	}
	return nil
}

func TestSteps(t *testing.T) {
	hp := testHostPackages()
	hp.Apt.Repositories[0].PinPriority = 900
	hp.Apt.Repositories[0].Auth = &config.AptAuth{Username: "u", Password: "!enc:secret"}
	plan := &config.DeploymentPlan{HostPackages: hp}
	resolve := func(v string) (string, error) { return strings.TrimPrefix(v, "!enc:"), nil }

	steps, err := Apt{}.Steps(Select(hp.Apt, testNode, false), Context{Plan: plan, Arch: "amd64", Resolve: resolve})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range steps {
		names = append(names, s.Name)
	}
	want := []string{"apt", "apt repository internal (with credentials)", "apt packages", "apt holds", "apt versions"}
	if !slices.Equal(names, want) {
		t.Fatalf("steps %q, want %q", names, want)
	}

	repo := stepNamed(t, steps, "apt repository internal (with credentials)")
	if f := fileOf(repo, "/etc/apt/keyrings/norca-internal.asc"); f == nil || string(f.Content) != testKey {
		t.Errorf("keyring: %+v", f)
	}
	if f := fileOf(repo, "/etc/apt/sources.list.d/norca-internal.sources"); f == nil ||
		!strings.Contains(string(f.Content), "Signed-By: /etc/apt/keyrings/norca-internal.asc") {
		t.Errorf("source: %+v", f)
	}
	if f := fileOf(repo, "/etc/apt/preferences.d/norca-internal.pref"); f == nil || !strings.Contains(string(f.Content), "Pin-Priority: 900") {
		t.Errorf("pin: %+v", f)
	}
	if f := fileOf(repo, "/etc/apt/auth.conf.d/norca-internal.conf"); f == nil || f.Mode != 0o600 ||
		!strings.Contains(string(f.Content), "password secret") || !repo.Secret {
		t.Errorf("credentials: %+v, secret %t", f, repo.Secret)
	}
	if !slices.Equal(repo.Remove, []string{"/etc/apt/keyrings/norca-internal.gpg"}) {
		t.Errorf("remove %q", repo.Remove)
	}

	install := stepNamed(t, steps, "apt packages")
	for _, w := range []string{"apt-get update -q -o DPkg::Lock::Timeout=300\n", "DEBIAN_FRONTEND=noninteractive apt-get install", "'norca-redis=8.0.2-1norca1' 'jq'"} {
		if !strings.Contains(install.Apply, w) {
			t.Errorf("install has no %q:\n%s", w, install.Apply)
		}
	}
	if install.Check != "" {
		t.Errorf("with update_cache the install runs every time, check %q", install.Check)
	}
	holds := stepNamed(t, steps, "apt holds").Apply
	if !strings.Contains(holds, "apt-mark hold 'norca-redis'") || !strings.Contains(holds, "apt-mark unhold 'jq'") {
		t.Errorf("holds:\n%s", holds)
	}
	if v := stepNamed(t, steps, "apt versions").Apply; v != "expect_package 'norca-redis' '8.0.2-1norca1'\nexpect_package 'jq' ''" {
		t.Errorf("versions:\n%s", v)
	}

	// Without a pin and credentials their files of an earlier plan go.
	hp.Apt.Repositories[0].PinPriority, hp.Apt.Repositories[0].Auth = 0, nil
	steps, err = Apt{}.Steps(Select(hp.Apt, testNode, true), Context{Plan: plan, Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	repo = stepNamed(t, steps, "apt repository internal")
	if repo.Secret || !slices.Equal(repo.Remove, []string{"/etc/apt/keyrings/norca-internal.gpg", "/etc/apt/preferences.d/norca-internal.pref", "/etc/apt/auth.conf.d/norca-internal.conf"}) {
		t.Errorf("repository without pin and credentials: %+v", repo)
	}
}

func TestStepsBinaryKey(t *testing.T) {
	hp := testHostPackages()
	hp.Apt.Repositories[0].Key = string([]byte{0x99, 0x01, 0x0d, 0x04}) // old-format public key packet
	hp.Apt.Install = nil
	steps, err := Apt{}.Steps(Select(hp.Apt, testNode, false), Context{Plan: &config.DeploymentPlan{HostPackages: hp}, Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	repo := stepNamed(t, steps, "apt repository internal")
	if fileOf(repo, "/etc/apt/keyrings/norca-internal.gpg") == nil ||
		!strings.Contains(string(fileOf(repo, "/etc/apt/sources.list.d/norca-internal.sources").Content), "norca-internal.gpg") ||
		repo.Remove[0] != "/etc/apt/keyrings/norca-internal.asc" {
		t.Fatalf("a binary key goes to a .gpg keyring: %+v", repo)
	}
	if len(steps) != 2 {
		t.Fatalf("repositories only: %d steps", len(steps))
	}
}

// A local .deb is copied over SSH and checked; the install script of a
// machine can only require it to be installed.
func TestStepsLocalDeb(t *testing.T) {
	dir := t.TempDir()
	deb := filepath.Join(dir, "norca-redis_8.0.2-1norca1_amd64.deb")
	if err := os.WriteFile(deb, []byte("fake deb"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, _ := artifacts.FileSHA256(deb)
	hp := &config.HostPackages{Apt: &config.AptPackages{Install: []config.HostPackage{{
		Name: "norca-redis", File: filepath.Join(dir, "norca-redis_8.0.2-1norca1_{arch}.deb"), Version: "8.0.2-1norca1",
	}}}}
	plan := &config.DeploymentPlan{HostPackages: hp}
	sel := Select(hp.Apt, testNode, false)

	steps, err := Apt{}.Steps(sel, Context{Plan: plan, Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	remote := DebDir + "/norca-redis_8.0.2-1norca1_amd64.deb"
	file := stepNamed(t, steps, "apt package file norca-redis_8.0.2-1norca1_amd64.deb")
	if f := fileOf(file, remote); f == nil || f.Local != deb || f.SHA256 != sum ||
		file.Apply != "deb_check '"+remote+"' 'norca-redis' '8.0.2-1norca1'" {
		t.Fatalf("file step: %+v", file)
	}
	install := stepNamed(t, steps, "apt packages")
	if strings.Contains(install.Apply, "apt-get update") || !strings.Contains(install.Apply, "'"+remote+"'") {
		t.Fatalf("an offline node gets no apt-get update:\n%s", install.Apply)
	}

	script, err := Apt{}.Steps(sel, Context{Plan: plan, Arch: "amd64", Script: true})
	if err != nil {
		t.Fatal(err)
	}
	if s := stepNamed(t, script, "apt package norca-redis"); s.Apply != "require_package 'norca-redis' '8.0.2-1norca1' 'norca-redis_8.0.2-1norca1_amd64.deb'" {
		t.Fatalf("script: %+v", s)
	}
	for _, s := range script {
		if s.Name == "apt packages" {
			t.Fatal("the script installs a file it cannot reach")
		}
	}

	hp.Apt.Install[0].File = deb
	hp.Apt.Install[0].SHA256 = strings.Repeat("0", 64)
	if _, err := (Apt{}).Steps(Select(hp.Apt, testNode, false), Context{Plan: plan, Arch: "amd64"}); err == nil || !strings.Contains(err.Error(), "expected 0000") {
		t.Fatalf("a wrong digest: %v", err)
	}
}

// Without update_cache only a missing package (or one not at its version)
// is installed.
func TestStepsWithoutUpdateCache(t *testing.T) {
	hp := testHostPackages()
	off := false
	hp.UpdateCache = &off
	steps, err := Apt{}.Steps(Select(hp.Apt, testNode, false), Context{Plan: &config.DeploymentPlan{HostPackages: hp}, Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	install := stepNamed(t, steps, "apt packages")
	if strings.Contains(install.Apply, "apt-get update") || install.Check != "apt_has 'norca-redis' '8.0.2-1norca1' &&\n  apt_has 'jq' ''" {
		t.Fatalf("install: %+v", install)
	}
}

func TestNoPackageManager(t *testing.T) {
	sel := Select(testHostPackages().Apt, testNode, false)
	for _, m := range []Manager{None{}, Unsupported{Manager: "dnf"}} {
		if steps, err := m.Steps(&Selection{}, Context{}); err != nil || steps != nil {
			t.Errorf("%s with nothing: %v %v", m.Name(), steps, err)
		}
		if _, err := m.Steps(sel, Context{}); err == nil {
			t.Errorf("%s with packages: no error", m.Name())
		}
	}
	if (Apt{}).Arch("arm") != "armhf" || (Apt{}).Arch("amd64") != "amd64" {
		t.Error("the Debian names of the architectures")
	}
}
