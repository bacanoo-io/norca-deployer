package plan

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// planFile writes a plan of one server with the given body and loads it.
func loadBody(t *testing.T, name, body string) (*DeploymentPlan, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "repo.asc"), []byte(testKey), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadFromFile(path)
}

// The packages of the nodes of each package manager are a block of their
// own; the lists of earlier plans and the block that follows later are
// refused with their reason, in YAML and in HCL alike, and a key of the
// other manager's repositories is an error, not a key that is dropped.
func TestHostPackagesBlocks(t *testing.T) {
	const inventoryYAML = "inventory:\n  masters:\n    - host: 10.0.0.1\n"
	const inventoryHCL = "inventory {\n  masters {\n    host = \"10.0.0.1\"\n  }\n}\n"
	const dnfRepoYAML = "      - {name: docker, baseurl: 'https://download.docker.com/linux/rhel/$releasever/$basearch/stable', key_file: repo.asc}\n"
	tests := []struct {
		name  string
		file  string
		body  string
		err   string // "" is valid
		block string // the block that must have one package
	}{
		{"apt block", "p.yaml", "host_packages:\n  apt:\n    repositories:\n      - {name: internal, uri: https://apt.example.com/debian, suite: bookworm, components: [main], key_file: repo.asc}\n    install:\n      - {name: jq, hold: true}\n", "", "apt"},
		{"apt block in hcl", "p.hcl", "host_packages {\n  apt {\n    repository {\n    }\n  }\n}\n", "unknown key \"repository\"", ""},
		{"apt block in hcl with lists", "p.hcl", "host_packages {\n  apt {\n    repositories {\n      name = \"internal\"\n      uri = \"https://apt.example.com/debian\"\n      suite = \"bookworm\"\n      components = [\"main\"]\n      key_file = \"repo.asc\"\n    }\n    install {\n      name = \"jq\"\n    }\n  }\n}\n", "", "apt"},
		{"old repositories", "p.yaml", "host_packages:\n  repositories:\n    - {name: internal}\n", "moved into host_packages.apt", ""},
		{"old install", "p.yaml", "host_packages:\n  install:\n    - name: jq\n", "moved into host_packages.apt", ""},
		{"old install in hcl", "p.hcl", "host_packages {\n  install {\n    name = \"jq\"\n  }\n}\n", "moved into host_packages.apt", ""},
		{"dnf block", "p.yaml", "host_packages:\n  dnf:\n    repositories:\n" + dnfRepoYAML + "    install:\n      - {name: containerd.io, version: 1.7.27-3.1.el9, hold: true}\n", "", "dnf"},
		{"dnf block in hcl", "p.hcl", "host_packages {\n  dnf {\n    repositories {\n      name = \"docker\"\n      baseurl = \"https://download.docker.com/linux/rhel/$releasever/$basearch/stable\"\n      key_file = \"repo.asc\"\n      priority = 10\n      include = [\"containerd.io\"]\n    }\n    install {\n      name = \"containerd.io\"\n    }\n  }\n}\n", "", "dnf"},
		{"apk in hcl", "p.hcl", "host_packages {\n  apk {\n    install {\n      name = \"jq\"\n    }\n  }\n}\n", "host_packages.apk is not supported yet", ""},
		{"rules of debian in the apt block", "p.yaml", "host_packages:\n  apt:\n    install:\n      - {name: Not_Debian}\n", "host_packages.apt.install[0]: invalid Debian package name", ""},
		{"rules of rpm in the dnf block", "p.yaml", "host_packages:\n  dnf:\n    install:\n      - {name: jq, version: '1:1.6'}\n", "host_packages.dnf.install[0]: invalid RPM version \"1:1.6\"", ""},
		{"baseurl is no key of apt", "p.yaml", "host_packages:\n  apt:\n    repositories:\n      - {name: internal, baseurl: https://x.example.com, key_file: repo.asc}\n", "baseurl is a key of the repositories of dnf, not of an apt repository", ""},
		{"suite is no key of dnf", "p.yaml", "host_packages:\n  dnf:\n    repositories:\n      - {name: docker, baseurl: https://x.example.com/el9, suite: bookworm, key_file: repo.asc}\n", "suite is a key of the repositories of apt, not of a dnf repository", ""},
		{"unknown key of dnf", "p.yaml", "host_packages:\n  dnf:\n    repositories:\n      - {name: docker, baseurl: https://x.example.com/el9, gpgcheck: 0, key_file: repo.asc}\n", "unknown key \"gpgcheck\" in a dnf repository", ""},
		{"suite is no key of dnf in hcl", "p.hcl", "host_packages {\n  dnf {\n    repositories {\n      name = \"docker\"\n      suite = \"bookworm\"\n    }\n  }\n}\n", "unknown key \"suite\"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := inventoryYAML
			if strings.HasSuffix(tt.file, ".hcl") {
				inv = inventoryHCL
			}
			plan, err := loadBody(t, tt.file, tt.body+inv)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var n int
			switch tt.block {
			case "apt":
				if plan.AptPackages() != nil {
					n = len(plan.AptPackages().Install)
				}
			case "dnf":
				if plan.DnfPackages() != nil {
					n = len(plan.DnfPackages().Install)
				}
			}
			if n != 1 {
				t.Fatalf("%s block %+v", tt.block, plan.HostPackages)
			}
		})
	}
}

// The same dnf block in YAML and in HCL loads to the same plan.
func TestDnfBlockYAMLHCLParity(t *testing.T) {
	_, err := loadBody(t, "p.yaml", `host_packages:
  lock_timeout: 60
  dnf:
    repositories:
      - name: docker
        baseurl: https://download.docker.com/linux/rhel/$releasever/$basearch/stable
        key_file: repo.asc
        priority: 10
        include: [containerd.io, docker-ce]
        exclude: [podman*]
        auth: {username: deploy, password: secret}
        roles: [client]
    install:
      - {name: containerd.io, version: "1.7.27-3.1.el9", hold: true, roles: [client]}
      - {name: norca, file: "rpms/norca-{arch}.rpm"}
inventory:
  masters:
    - host: 10.0.0.1
`)
	if err == nil || !strings.Contains(err.Error(), "norca-x86_64.rpm") {
		t.Fatalf("the missing .rpm of the x86_64 nodes is not named: %v", err)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "rpms"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"repo.asc", "rpms/norca-x86_64.rpm"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(testKey), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	yamlBody := `host_packages:
  lock_timeout: 60
  dnf:
    repositories:
      - name: docker
        baseurl: https://download.docker.com/linux/rhel/$releasever/$basearch/stable
        key_file: repo.asc
        priority: 10
        include: [containerd.io, docker-ce]
        exclude: [podman*]
        auth: {username: deploy, password: secret}
        roles: [client]
    install:
      - {name: containerd.io, version: "1.7.27-3.1.el9", hold: true, roles: [client]}
      - {name: norca, file: "rpms/norca-{arch}.rpm"}
inventory:
  masters:
    - {host: 10.0.0.1, arch: amd64}
`
	hclBody := `host_packages {
  lock_timeout = 60
  dnf {
    repositories {
      name     = "docker"
      baseurl  = "https://download.docker.com/linux/rhel/$releasever/$basearch/stable"
      key_file = "repo.asc"
      priority = 10
      include  = ["containerd.io", "docker-ce"]
      exclude  = ["podman*"]
      auth {
        username = "deploy"
        password = "secret"
      }
      roles = ["client"]
    }
    install {
      name    = "containerd.io"
      version = "1.7.27-3.1.el9"
      hold    = true
      roles   = ["client"]
    }
    install {
      name = "norca"
      file = "rpms/norca-{arch}.rpm"
    }
  }
}
inventory {
  masters {
    host = "10.0.0.1"
    arch = "amd64"
  }
}
`
	fromYAML, err := Parse([]byte(yamlBody), FormatYAML, dir)
	if err != nil {
		t.Fatal(err)
	}
	fromHCL, err := Parse([]byte(hclBody), FormatHCL, dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromYAML.HostPackages, fromHCL.HostPackages) {
		t.Fatalf("yaml %+v != hcl %+v", fromYAML.HostPackages.Dnf, fromHCL.HostPackages.Dnf)
	}
	r := fromYAML.HostPackages.Dnf.Repositories[0]
	if r.Priority != 10 || len(r.Include) != 2 || r.Auth == nil || r.Auth.Password != "secret" {
		t.Fatalf("repository %+v", r)
	}
	if got := fromYAML.RPMFile(&fromYAML.HostPackages.Dnf.Install[1], "arm64"); !strings.HasSuffix(got, filepath.Join("rpms", "norca-aarch64.rpm")) {
		t.Fatalf("RPMFile arm64 = %s", got)
	}
}

// The rules of RPM in the dnf block.
func TestValidateDnfPackages(t *testing.T) {
	valid := func() *DeploymentPlan {
		p := validPlan()
		p.HostPackages.Dnf = &DnfPackages{
			Repositories: []DnfRepository{{
				Name: "docker", BaseURL: "https://download.docker.com/linux/rhel/$releasever/$basearch/stable",
				Key: testKey, Priority: 10, Include: []string{"containerd.io", "docker-ce*"},
			}},
			Install: []HostPackage{{Name: "containerd.io", Version: "1.7.27-3.1.el9", Hold: true}, {Name: "docker-ce", Version: "3:27.3.1-1.el9"}},
		}
		return p
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(d *DnfPackages)
		err    string
	}{
		{"metalink", func(d *DnfPackages) {
			d.Repositories[0].BaseURL, d.Repositories[0].Metalink = "", "https://mirrors.example.com/metalink?repo=x"
		}, ""},
		{"release with a tilde", func(d *DnfPackages) { d.Install[0].Version = "1.7.27~rc1-1.el9" }, ""},
		{"upper case name", func(d *DnfPackages) { d.Install[0].Name = "NetworkManager-ovs" }, ""},
		{"no repository location", func(d *DnfPackages) { d.Repositories[0].BaseURL = "" }, "exactly one of baseurl or metalink"},
		{"baseurl and metalink", func(d *DnfPackages) { d.Repositories[0].Metalink = "https://m.example.com/x" }, "exactly one of baseurl or metalink"},
		{"file baseurl", func(d *DnfPackages) { d.Repositories[0].BaseURL = "file:///srv/repo" }, "must be an http(s) URL"},
		{"credentials in the url", func(d *DnfPackages) { d.Repositories[0].BaseURL = "https://u:p@x.example.com/el9" }, "put credentials into auth"},
		{"no key", func(d *DnfPackages) { d.Repositories[0].Key = "" }, "exactly one of key or key_file"},
		{"binary key", func(d *DnfPackages) { d.Repositories[0].Key = "\x99\x01\x0d" }, "ASCII-armored"},
		{"priority", func(d *DnfPackages) { d.Repositories[0].Priority = 100 }, "between 1 and 99"},
		{"include with a space", func(d *DnfPackages) { d.Repositories[0].Include = []string{"a b"} }, "invalid include or exclude entry"},
		{"exclude with a comma", func(d *DnfPackages) { d.Repositories[0].Exclude = []string{"a,b"} }, "invalid include or exclude entry"},
		{"auth without password", func(d *DnfPackages) { d.Repositories[0].Auth = &RepositoryAuth{Username: "u"} }, "auth needs username and password"},
		{"password of two lines", func(d *DnfPackages) {
			d.Repositories[0].Auth = &RepositoryAuth{Username: "u", Password: "p\ngpgcheck=0"}
		}, "auth.password must be one line"},
		{"repository name", func(d *DnfPackages) { d.Repositories[0].Name = "Docker CE" }, "must be lower case letters"},
		{"duplicate repository", func(d *DnfPackages) { d.Repositories = append(d.Repositories, d.Repositories[0]) }, "duplicate repository name"},
		{"repository role", func(d *DnfPackages) { d.Repositories[0].Roles = []string{"worker"} }, "unknown role"},
		{"package name", func(d *DnfPackages) { d.Install[0].Name = "-y; reboot" }, "invalid RPM package name"},
		{"version without release", func(d *DnfPackages) { d.Install[0].Version = "1.7.27" }, "invalid RPM version"},
		{"version with a space", func(d *DnfPackages) { d.Install[0].Version = "1.7.27-1 x" }, "invalid RPM version"},
		{"debian file", func(d *DnfPackages) { d.Install[0].File = "x.deb" }, "must be a .rpm"},
		{"duplicate package", func(d *DnfPackages) { d.Install = append(d.Install, d.Install[0]) }, "more than once"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := valid()
			tt.mutate(p.HostPackages.Dnf)
			err := p.Validate()
			if tt.err == "" {
				if err != nil {
					t.Fatalf("valid plan rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Fatalf("got %v, want an error with %q", err, tt.err)
			}
		})
	}
}

// release.source host-package: the package may be listed in the dnf block.
func TestHostPackageListedInDnf(t *testing.T) {
	p := validPlan()
	p.Release = ReleaseConfig{Source: ReleaseHostPackage, Package: "norca"}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "host_packages.dnf.install") {
		t.Fatalf("got %v, want the blocks named", err)
	}
	p.HostPackages.Dnf = &DnfPackages{Install: []HostPackage{{Name: "norca", Version: "0.3.0-1.el9"}}}
	if err := p.Validate(); err != nil {
		t.Fatalf("the package of the dnf block is not found: %v", err)
	}
}

func TestScriptInterpreterAndFamilies(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(s *ScriptConfig)
		err    string
	}{
		{"default bash", func(*ScriptConfig) {}, ""},
		{"sh", func(s *ScriptConfig) { s.Interpreter = InterpreterSh }, ""},
		{"bash", func(s *ScriptConfig) { s.Interpreter = InterpreterBash }, ""},
		{"powershell follows", func(s *ScriptConfig) { s.Interpreter = "powershell" }, "not supported yet"},
		{"unknown interpreter", func(s *ScriptConfig) { s.Interpreter = "zsh" }, "must be bash or sh"},
		{"families", func(s *ScriptConfig) { s.Families = []string{FamilyDebian, FamilyRHEL, FamilyAlpine, FamilyWindows} }, ""},
		{"unknown family", func(s *ScriptConfig) { s.Families = []string{"suse"} }, "family \"suse\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validPlan()
			tt.mutate(&p.Scripts[0])
			err := p.Validate()
			if tt.err == "" && err != nil {
				t.Fatal(err)
			}
			if tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)) {
				t.Fatalf("err %v, want %q", err, tt.err)
			}
		})
	}
	if got := ScriptInterpreter(&ScriptConfig{}); got != InterpreterBash {
		t.Fatalf("the default interpreter is %q", got)
	}
}
