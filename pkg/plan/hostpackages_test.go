package plan

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
		HostPackages: &HostPackages{Apt: &AptPackages{
			Repositories: []AptRepository{{
				Name: "internal", URI: "https://apt.example.com/debian", Suite: "bookworm",
				Components: []string{"main"}, Key: testKey,
			}},
			Install: []HostPackage{{Name: "norca-redis", Version: "1:8.0.2-1norca1~bpo12+1"}},
		}},
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
		"package name": {func(p *DeploymentPlan) { p.HostPackages.Apt.Install[0].Name = "Redis; rm -rf /" }, "invalid Debian package name"},
		"version":      {func(p *DeploymentPlan) { p.HostPackages.Apt.Install[0].Version = "8.0 && reboot" }, "invalid Debian version"},
		"duplicate pkg": {func(p *DeploymentPlan) {
			p.HostPackages.Apt.Install = append(p.HostPackages.Apt.Install, p.HostPackages.Apt.Install[0])
		}, "more than once"},
		"uri scheme":      {func(p *DeploymentPlan) { p.HostPackages.Apt.Repositories[0].URI = "file:///srv/repo" }, "http(s) URL"},
		"no key":          {func(p *DeploymentPlan) { p.HostPackages.Apt.Repositories[0].Key = "" }, "exactly one of key or key_file"},
		"not armored":     {func(p *DeploymentPlan) { p.HostPackages.Apt.Repositories[0].Key = "AAAA" }, "ASCII-armored"},
		"no components":   {func(p *DeploymentPlan) { p.HostPackages.Apt.Repositories[0].Components = nil }, "components are required"},
		"flat components": {func(p *DeploymentPlan) { p.HostPackages.Apt.Repositories[0].Suite = "./" }, "takes no components"},
		"repo role":       {func(p *DeploymentPlan) { p.HostPackages.Apt.Repositories[0].Roles = []string{"worker"} }, "unknown role"},
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
  apt:
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
	key, err := plan.RepositoryKey(&plan.HostPackages.Apt.Repositories[0])
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

// validate warns of host_packages that work only if every Linux node uses
// the package managers the plan has blocks of: a block whose packages or
// repositories every Linux node of the inventory takes, while another
// manager has no block. The plan stays valid: apply decides with the
// package manager it finds on each node.
func TestHostPackagesWarnings(t *testing.T) {
	const head = "metadata: {name: prod, region: eu}\nrelease: {version: v0.3.0}\n"
	const servers = "inventory:\n  masters:\n    - {host: 10.0.0.1}\n"
	const serverAndClient = servers + "  clients:\n    - {host: 10.0.0.2}\n"
	const windowsClient = serverAndClient + "    - {host: 10.0.0.3, platform: windows, connection: {mode: ssh, ssh_user: admin}}\n"
	apkOnly := "  apk:\n    install:\n      - {name: jq}\n"
	tests := []struct {
		name, packages, inventory string
		// want are the parts of the one warning; none: no warning.
		want []string
	}{
		{name: "apk for every node", packages: apkOnly, inventory: serverAndClient, want: []string{
			"host_packages: every Linux node of the inventory takes packages or repositories of host_packages.apk, " +
				"and the plan has no block for apt or dnf: it works only if every Linux node uses apk. " +
				"apply checks this against the package manager it finds on each node, before it changes anything"}},
		{name: "apk for the clients", packages: "  apk:\n    install:\n      - {name: jq, roles: [client]}\n", inventory: serverAndClient},
		{name: "apk for one host", packages: "  apk:\n    install:\n      - {name: jq, hosts: [10.0.0.2]}\n", inventory: serverAndClient},
		{name: "apk for the servers of an inventory of servers", packages: "  apk:\n    install:\n      - {name: jq, roles: [master]}\n",
			inventory: servers, want: []string{"of host_packages.apk", "works only if every Linux node uses apk"}},
		{name: "a repository for every node", inventory: servers,
			packages: "  apk:\n    repositories:\n      - {name: community, url: https://dl-cdn.alpinelinux.org/alpine/v3.22/community}\n",
			want:     []string{"of host_packages.apk", "no block for apt or dnf"}},
		{name: "apt and apk for every node", packages: "  apt:\n    install:\n      - {name: jq}\n" + apkOnly, inventory: serverAndClient,
			want: []string{"of host_packages.apt and host_packages.apk", "no block for dnf", "uses apt or apk"}},
		{name: "apt for every node and dnf for the clients", inventory: serverAndClient,
			packages: "  apt:\n    install:\n      - {name: jq}\n  dnf:\n    install:\n      - {name: jq, roles: [client]}\n",
			want:     []string{"of host_packages.apt,", "no block for apk", "uses apt or dnf"}},
		{name: "every block", inventory: serverAndClient,
			packages: "  apt:\n    install:\n      - {name: jq}\n  dnf:\n    install:\n      - {name: jq}\n" + apkOnly},
		{name: "windows nodes take no packages", packages: "  apk:\n    install:\n      - {name: jq, hosts: [10.0.0.1, 10.0.0.2]}\n",
			inventory: windowsClient, want: []string{"works only if every Linux node uses apk"}},
		{name: "no packages", inventory: serverAndClient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := head + tt.inventory
			if tt.packages != "" {
				src += "host_packages:\n" + tt.packages
			}
			p, err := Parse([]byte(src), FormatYAML, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			got := p.Warnings()
			if len(tt.want) == 0 {
				if len(got) != 0 {
					t.Fatalf("warnings %q", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("warnings %q, want one", got)
			}
			for _, w := range tt.want {
				if !strings.Contains(got[0], w) {
					t.Errorf("the warning has no %q:\n%s", w, got[0])
				}
			}
		})
	}
}
