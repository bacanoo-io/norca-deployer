package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testKey = "-----BEGIN PGP PUBLIC KEY BLOCK-----\nxyz\n-----END PGP PUBLIC KEY BLOCK-----\n"

func validPlan() *DeploymentPlan {
	p := &DeploymentPlan{
		Inventory: InventoryConfig{Masters: []NodeConfig{{Host: "10.0.0.1"}}},
		HostPackages: &HostPackages{
			Repositories: []AptRepository{{
				Name: "internal", URI: "https://apt.example.com/debian", Suite: "bookworm",
				Components: []string{"main"}, Key: testKey,
			}},
			Install: []HostPackage{{Name: "norca-redis", Version: "1:8.0.2-1norca1~bpo12+1"}},
		},
		Scripts: []ScriptConfig{{Name: "prep", Phase: PhasePreInstall, Inline: "true"}},
	}
	p.SetDefaults()
	return p
}

func TestValidateHostPackagesAndScripts(t *testing.T) {
	if err := validPlan().Validate(); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}

	cases := map[string]struct {
		mutate func(p *DeploymentPlan)
		err    string
	}{
		"package name": {func(p *DeploymentPlan) { p.HostPackages.Install[0].Name = "Redis; rm -rf /" }, "invalid Debian package name"},
		"version":      {func(p *DeploymentPlan) { p.HostPackages.Install[0].Version = "8.0 && reboot" }, "invalid Debian version"},
		"duplicate pkg": {func(p *DeploymentPlan) {
			p.HostPackages.Install = append(p.HostPackages.Install, p.HostPackages.Install[0])
		}, "more than once"},
		"uri scheme":      {func(p *DeploymentPlan) { p.HostPackages.Repositories[0].URI = "file:///srv/repo" }, "http(s) URL"},
		"no key":          {func(p *DeploymentPlan) { p.HostPackages.Repositories[0].Key = "" }, "exactly one of key or key_file"},
		"not armored":     {func(p *DeploymentPlan) { p.HostPackages.Repositories[0].Key = "AAAA" }, "ASCII-armored"},
		"no components":   {func(p *DeploymentPlan) { p.HostPackages.Repositories[0].Components = nil }, "components are required"},
		"flat components": {func(p *DeploymentPlan) { p.HostPackages.Repositories[0].Suite = "./" }, "takes no components"},
		"repo role":       {func(p *DeploymentPlan) { p.HostPackages.Repositories[0].Roles = []string{"worker"} }, "unknown role"},
		"script phase":    {func(p *DeploymentPlan) { p.Scripts[0].Phase = "later" }, "phase"},
		"script source":   {func(p *DeploymentPlan) { p.Scripts[0].Path = "x.sh" }, "exactly one of path or inline"},
		"script name":     {func(p *DeploymentPlan) { p.Scripts[0].Name = "../x" }, "name"},
		"script env":      {func(p *DeploymentPlan) { p.Scripts[0].Env = map[string]string{"A-B": "1"} }, "environment variable name"},
		"missing file":    {func(p *DeploymentPlan) { p.Scripts[0].Inline = ""; p.Scripts[0].Path = "missing.sh" }, "failed to read"},
		"backup recipient": {func(p *DeploymentPlan) {
			p.Backup = &BackupConfig{Enabled: true, Destination: BackupDestinationConfig{Name: "s3", Bucket: "b"}}
		}, "age recipient"},
	}
	for name, tc := range cases {
		p := validPlan()
		tc.mutate(p)
		err := p.Validate()
		if err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: got %v, want error containing %q", name, err, tc.err)
		}
	}
}

func TestLoadResolvesPathsRelativeToConfig(t *testing.T) {
	dir := t.TempDir()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(dir, "scripts"), 0755))
	must(os.WriteFile(filepath.Join(dir, "scripts", "prep.sh"), []byte("echo hi\n"), 0644))
	must(os.WriteFile(filepath.Join(dir, "repo.asc"), []byte(testKey), 0644))
	cfg := `
inventory:
  masters:
    - host: 10.0.0.1
host_packages:
  repositories:
    - name: internal
      uri: https://apt.example.com/debian
      suite: bookworm
      components: [main]
      key_file: repo.asc
  install:
    - name: jq
scripts:
  - name: prep
    phase: pre_install
    path: scripts/prep.sh
`
	path := filepath.Join(dir, "cluster-deploy.yaml")
	must(os.WriteFile(path, []byte(cfg), 0644))

	plan, err := LoadFromFile(path)
	must(err)
	body, err := plan.ScriptBody(&plan.Scripts[0])
	must(err)
	if body != "echo hi\n" {
		t.Fatalf("script body = %q", body)
	}
	key, err := plan.RepositoryKey(&plan.HostPackages.Repositories[0])
	must(err)
	if string(key) != testKey {
		t.Fatalf("key = %q", key)
	}
}

func TestSaveToFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.yaml")
	if err := validPlan().SaveToFile(path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows has no Unix permission bits; only check where they exist.
	if filepath.Separator == '/' && info.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v, want 0600", info.Mode().Perm())
	}
}
