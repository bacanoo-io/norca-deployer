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
	if err := os.WriteFile(filepath.Join(dir, testApkKeyName), []byte(testApkKey), 0o600); err != nil {
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
	const apkRepoYAML = "      - {name: community, url: https://dl-cdn.alpinelinux.org/alpine/v3.22/community, key_file: " + testApkKeyName + ", tagged: true}\n"
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
		{"apk block", "p.yaml", "host_packages:\n  apk:\n    repositories:\n" + apkRepoYAML + "    install:\n      - {name: containerd, version: 1.7.27-r0, hold: true}\n", "", "apk"},
		{"apk block in hcl", "p.hcl", "host_packages {\n  apk {\n    install {\n      name = \"jq\"\n    }\n  }\n}\n", "", "apk"},
		{"credentials of apk", "p.yaml", "host_packages:\n  apk:\n    repositories:\n      - {name: private, url: https://apk.example.com/v3.22/main, auth: {username: u, password: p}}\n",
			"apk keeps them in /etc/apk/repositories, which every user can read", ""},
		{"suite is no key of apk", "p.yaml", "host_packages:\n  apk:\n    repositories:\n      - {name: x, url: https://apk.example.com/v3.22/main, suite: bookworm}\n",
			"suite is a key of the repositories of apt, not of an apk repository", ""},
		{"tagged is no key of apt", "p.yaml", "host_packages:\n  apt:\n    repositories:\n      - {name: internal, uri: https://apt.example.com/debian, suite: bookworm, components: [main], key_file: repo.asc, tagged: true}\n",
			"tagged is a key of the repositories of apk, not of an apt repository", ""},
		{"repository of a package of apt", "p.yaml", "host_packages:\n  apt:\n    install:\n      - {name: jq, repository: mine}\n",
			"host_packages.apt.install[0]: repository names a tagged repository of apk", ""},
		{"rules of apk in the apk block", "p.yaml", "host_packages:\n  apk:\n    install:\n      - {name: jq, version: 1.7.1-1}\n", "host_packages.apk.install[0]: invalid apk version \"1.7.1-1\"", ""},
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
			case "apk":
				if plan.ApkPackages() != nil {
					n = len(plan.ApkPackages().Install)
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

// The interpreter of a script and its families must name one platform: a
// script is written for Linux or for Windows. A script without an
// interpreter takes the one of each node, so it must not select nodes of
// both platforms.
func TestScriptInterpreterAndFamilies(t *testing.T) {
	windowsClient := func(p *DeploymentPlan) {
		p.Inventory.Clients = append(p.Inventory.Clients, NodeConfig{Host: "10.0.0.9", Hostname: "win-1", Platform: "windows", Arch: "amd64"})
	}
	tests := []struct {
		name   string
		mutate func(p *DeploymentPlan, s *ScriptConfig)
		err    string
	}{
		{"default", func(*DeploymentPlan, *ScriptConfig) {}, ""},
		{"sh", func(_ *DeploymentPlan, s *ScriptConfig) { s.Interpreter = InterpreterSh }, ""},
		{"bash", func(_ *DeploymentPlan, s *ScriptConfig) { s.Interpreter = InterpreterBash }, ""},
		{"powershell", func(_ *DeploymentPlan, s *ScriptConfig) { s.Interpreter = InterpreterPowerShell }, ""},
		{"unknown interpreter", func(_ *DeploymentPlan, s *ScriptConfig) { s.Interpreter = "pwsh" }, "must be bash, sh or powershell"},
		{"linux families", func(_ *DeploymentPlan, s *ScriptConfig) {
			s.Families = []string{FamilyDebian, FamilyRHEL, FamilyAlpine}
		}, ""},
		{"windows family", func(_ *DeploymentPlan, s *ScriptConfig) { s.Families = []string{FamilyWindows} }, ""},
		{"unknown family", func(_ *DeploymentPlan, s *ScriptConfig) { s.Families = []string{"suse"} }, "family \"suse\""},
		{"bash on windows", func(_ *DeploymentPlan, s *ScriptConfig) {
			s.Interpreter, s.Families = InterpreterBash, []string{FamilyWindows}
		}, "scripts[0]: interpreter bash runs on Linux nodes, but families names windows"},
		{"sh on windows and debian", func(_ *DeploymentPlan, s *ScriptConfig) {
			s.Interpreter, s.Families = InterpreterSh, []string{FamilyDebian, FamilyWindows}
		}, "interpreter sh runs on Linux nodes, but families names windows"},
		{"powershell on debian", func(_ *DeploymentPlan, s *ScriptConfig) {
			s.Interpreter, s.Families = InterpreterPowerShell, []string{FamilyDebian}
		}, "interpreter powershell runs on Windows nodes, but families names debian"},
		{"no interpreter for both platforms", func(_ *DeploymentPlan, s *ScriptConfig) {
			s.Families = []string{FamilyWindows, FamilyAlpine}
		}, "families names windows and alpine, but a script is written for Linux or for Windows"},
		{"no interpreter in a mixed cluster", func(p *DeploymentPlan, _ *ScriptConfig) { windowsClient(p) },
			"scripts[0]: the script selects Linux nodes (10.0.0.1) and Windows nodes (10.0.0.9), but names no interpreter"},
		{"bash in a mixed cluster", func(p *DeploymentPlan, s *ScriptConfig) {
			windowsClient(p)
			s.Interpreter = InterpreterBash
		}, ""},
		{"powershell in a mixed cluster", func(p *DeploymentPlan, s *ScriptConfig) {
			windowsClient(p)
			s.Interpreter = InterpreterPowerShell
		}, ""},
		{"families in a mixed cluster", func(p *DeploymentPlan, s *ScriptConfig) {
			windowsClient(p)
			s.Families = []string{FamilyWindows}
		}, ""},
		{"roles in a mixed cluster", func(p *DeploymentPlan, s *ScriptConfig) {
			windowsClient(p)
			s.Roles = []string{RoleMaster}
		}, ""},
		{"hosts in a mixed cluster", func(p *DeploymentPlan, s *ScriptConfig) {
			windowsClient(p)
			s.Hosts = []string{"win-1"}
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validPlan()
			tt.mutate(p, &p.Scripts[0])
			p.SetDefaults()
			err := p.Validate()
			if tt.err == "" && err != nil {
				t.Fatal(err)
			}
			if tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)) {
				t.Fatalf("err %v, want %q", err, tt.err)
			}
		})
	}
}

// Without an interpreter in the plan, a script runs with bash on Linux and
// with powershell on Windows; a script runs only on the platform of its
// interpreter.
func TestScriptInterpreterOfNode(t *testing.T) {
	tests := []struct {
		interpreter    string
		linux, windows string
		onLinux, onWin bool
	}{
		{"", InterpreterBash, InterpreterPowerShell, true, true},
		{InterpreterBash, InterpreterBash, InterpreterBash, true, false},
		{InterpreterSh, InterpreterSh, InterpreterSh, true, false},
		{InterpreterPowerShell, InterpreterPowerShell, InterpreterPowerShell, false, true},
	}
	for _, tt := range tests {
		s := &ScriptConfig{Interpreter: tt.interpreter}
		if got := ScriptInterpreter(s, false); got != tt.linux {
			t.Errorf("%q on Linux: %q, want %q", tt.interpreter, got, tt.linux)
		}
		if got := ScriptInterpreter(s, true); got != tt.windows {
			t.Errorf("%q on Windows: %q, want %q", tt.interpreter, got, tt.windows)
		}
		if got := ScriptRunsOn(s, false); got != tt.onLinux {
			t.Errorf("%q runs on Linux: %t", tt.interpreter, got)
		}
		if got := ScriptRunsOn(s, true); got != tt.onWin {
			t.Errorf("%q runs on Windows: %t", tt.interpreter, got)
		}
	}
}

// interpreter: powershell and families load from YAML and HCL alike.
func TestScriptPowerShellYAMLAndHCL(t *testing.T) {
	const yamlBody = "scripts:\n  - {name: win-prep, phase: pre_install, interpreter: powershell, families: [windows], inline: \"Write-Output 1\"}\n" +
		"inventory:\n  masters:\n    - host: 10.0.0.1\n  clients:\n    - {host: 10.0.0.9, platform: windows}\n"
	const hclBody = "scripts {\n  name = \"win-prep\"\n  phase = \"pre_install\"\n  interpreter = \"powershell\"\n  families = [\"windows\"]\n  inline = \"Write-Output 1\"\n}\n" +
		"inventory {\n  masters {\n    host = \"10.0.0.1\"\n  }\n  clients {\n    host = \"10.0.0.9\"\n    platform = \"windows\"\n  }\n}\n"
	for file, body := range map[string]string{"p.yaml": yamlBody, "p.hcl": hclBody} {
		t.Run(file, func(t *testing.T) {
			plan, err := loadBody(t, file, body)
			if err != nil {
				t.Fatal(err)
			}
			s := plan.Scripts[0]
			if s.Interpreter != InterpreterPowerShell || !reflect.DeepEqual(s.Families, []string{FamilyWindows}) {
				t.Fatalf("script %+v", s)
			}
		})
	}
}

// testApkKeyName and testApkKey are a signing key of Alpine (from
// /etc/apk/keys of alpine:3.22).
const (
	testApkKeyName = "alpine-devel@lists.alpinelinux.org-6165ee59.rsa.pub"
	testApkKey     = `-----BEGIN PUBLIC KEY-----
MIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEAutQkua2CAig4VFSJ7v54
ALyu/J1WB3oni7qwCZD3veURw7HxpNAj9hR+S5N/pNeZgubQvJWyaPuQDm7PTs1+
tFGiYNfAsiibX6Rv0wci3M+z2XEVAeR9Vzg6v4qoofDyoTbovn2LztaNEjTkB+oK
tlvpNhg1zhou0jDVYFniEXvzjckxswHVb8cT0OMTKHALyLPrPOJzVtM9C1ew2Nnc
3848xLiApMu3NBk0JqfcS3Bo5Y2b1FRVBvdt+2gFoKZix1MnZdAEZ8xQzL/a0YS5
Hd0wj5+EEKHfOd3A75uPa/WQmA+o0cBFfrzm69QDcSJSwGpzWrD1ScH3AK8nWvoj
v7e9gukK/9yl1b4fQQ00vttwJPSgm9EnfPHLAtgXkRloI27H6/PuLoNvSAMQwuCD
hQRlyGLPBETKkHeodfLoULjhDi1K2gKJTMhtbnUcAA7nEphkMhPWkBpgFdrH+5z4
Lxy+3ek0cqcI7K68EtrffU8jtUj9LFTUC8dERaIBs7NgQ/LfDbDfGh9g6qVj1hZl
k9aaIPTm/xsi8v3u+0qaq7KzIBc9s59JOoA8TlpOaYdVgSQhHHLBaahOuAigH+VI
isbC9vmqsThF2QdDtQt37keuqoda2E6sL7PUvIyVXDRfwX7uMDjlzTxHTymvq2Ck
htBqojBnThmjJQFgZXocHG8CAwEAAQ==
-----END PUBLIC KEY-----
`
)

// The same apk block in YAML and in HCL loads to the same plan; {arch} of
// an .apk file is the name of Alpine (x86_64, aarch64), and a key file is
// installed under its own name.
func TestApkBlockYAMLHCLParity(t *testing.T) {
	dir := t.TempDir()
	for f, content := range map[string]string{"keys/" + testApkKeyName: testApkKey, "apks/norca-x86_64.apk": "x"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	yamlBody := `host_packages:
  lock_timeout: 60
  apk:
    repositories:
      - name: community
        url: https://dl-cdn.alpinelinux.org/alpine/v3.22/community
        roles: [client]
      - name: mine
        url: https://apk.example.com/v3.22/main
        key_file: keys/` + testApkKeyName + `
        tagged: true
    install:
      - {name: containerd, version: "1.7.27-r0", hold: true, roles: [client]}
      - {name: norca-agent, repository: mine}
      - {name: norca, file: "apks/norca-{arch}.apk"}
inventory:
  masters:
    - {host: 10.0.0.1, arch: amd64}
`
	hclBody := `host_packages {
  lock_timeout = 60
  apk {
    repositories {
      name  = "community"
      url   = "https://dl-cdn.alpinelinux.org/alpine/v3.22/community"
      roles = ["client"]
    }
    repositories {
      name     = "mine"
      url      = "https://apk.example.com/v3.22/main"
      key_file = "keys/` + testApkKeyName + `"
      tagged   = true
    }
    install {
      name    = "containerd"
      version = "1.7.27-r0"
      hold    = true
      roles   = ["client"]
    }
    install {
      name       = "norca-agent"
      repository = "mine"
    }
    install {
      name = "norca"
      file = "apks/norca-{arch}.apk"
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
		t.Fatalf("yaml %+v != hcl %+v", fromYAML.HostPackages.Apk, fromHCL.HostPackages.Apk)
	}
	apk := fromYAML.HostPackages.Apk
	if !apk.Repositories[1].Tagged || apk.Install[1].Repository != "mine" {
		t.Fatalf("apk block %+v", apk)
	}
	if got := fromYAML.ApkFile(&apk.Install[2], "arm64"); !strings.HasSuffix(got, filepath.Join("apks", "norca-aarch64.apk")) {
		t.Fatalf("ApkFile arm64 = %s", got)
	}
	key, name, err := fromYAML.ApkRepositoryKey(&apk.Repositories[1])
	if err != nil || name != testApkKeyName || string(key) != testApkKey {
		t.Fatalf("ApkRepositoryKey = %q, %v", name, err)
	}
	if _, name, _ := fromYAML.ApkRepositoryKey(&apk.Repositories[0]); name != "" {
		t.Fatalf("a repository without a key has the key %q", name)
	}
}

// The rules of apk in the apk block: repositories with an RSA key under its
// own name (or none), no credentials, tagged repositories for the packages
// that name them, versions as version-rN.
func TestValidateApkPackages(t *testing.T) {
	valid := func() *DeploymentPlan {
		p := validPlan()
		p.HostPackages.Apk = &ApkPackages{
			Repositories: []ApkRepository{
				{Name: "community", URL: "https://dl-cdn.alpinelinux.org/alpine/v3.22/community"},
				{Name: "mine", URL: "https://apk.example.com/v3.22/main", Key: testApkKey, KeyName: "packager-6b0763be.rsa.pub", Tagged: true},
			},
			Install: []HostPackage{
				{Name: "containerd", Version: "1.7.27-r0", Hold: true},
				{Name: "norca-agent", Version: "0.3.0_rc1-r2", Repository: "mine"},
				{Name: "py3-yaml", Version: "6.0.2_git20240101-r1"},
			},
		}
		return p
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(a *ApkPackages)
		err    string
	}{
		{"version with a letter", func(a *ApkPackages) { a.Install[0].Version = "1.1.1w-r0" }, ""},
		{"http url", func(a *ApkPackages) { a.Repositories[0].URL = "http://mirror.example.com/alpine/v3.22/main" }, ""},
		{"no url", func(a *ApkPackages) { a.Repositories[0].URL = "" }, `url "" must be an http(s) URL`},
		{"file url", func(a *ApkPackages) { a.Repositories[0].URL = "/srv/repo" }, "must be an http(s) URL"},
		{"credentials in the url", func(a *ApkPackages) { a.Repositories[0].URL = "https://u:p@apk.example.com/main" },
			"no credentials: apk keeps them in /etc/apk/repositories"},
		{"auth", func(a *ApkPackages) { a.Repositories[0].Auth = &RepositoryAuth{Username: "u", Password: "p"} },
			"a private repository is reached through local .apk files (file) or a proxy"},
		{"key without a name", func(a *ApkPackages) { a.Repositories[1].KeyName = "" }, "key needs key_name"},
		{"key name without a key", func(a *ApkPackages) { a.Repositories[0].KeyName = "x.rsa.pub" }, "key_name needs key"},
		{"key and key file", func(a *ApkPackages) { a.Repositories[1].KeyFile = "x.rsa.pub" }, "at most one of key or key_file"},
		{"key name with a slash", func(a *ApkPackages) { a.Repositories[1].KeyName = "../x.rsa.pub" }, "invalid key name"},
		{"key name without .pub", func(a *ApkPackages) { a.Repositories[1].KeyName = "packager.rsa" }, "invalid key name"},
		{"openpgp key", func(a *ApkPackages) { a.Repositories[1].Key = testKey }, "an RSA public key in PEM"},
		{"repository name", func(a *ApkPackages) { a.Repositories[0].Name = "Community" }, "must be lower case letters"},
		{"duplicate repository", func(a *ApkPackages) { a.Repositories = append(a.Repositories, a.Repositories[0]) }, "duplicate repository name"},
		{"repository role", func(a *ApkPackages) { a.Repositories[0].Roles = []string{"worker"} }, "unknown role"},
		{"package name", func(a *ApkPackages) { a.Install[0].Name = "jq@edge" }, "invalid apk package name"},
		{"version without the release", func(a *ApkPackages) { a.Install[0].Version = "1.7.27" }, "invalid apk version"},
		{"debian version", func(a *ApkPackages) { a.Install[0].Version = "1.7.27-1" }, "invalid apk version"},
		{"unknown repository", func(a *ApkPackages) { a.Install[1].Repository = "other" }, `repository "other" is no repository of host_packages.apk`},
		{"untagged repository", func(a *ApkPackages) { a.Install[1].Repository = "community" }, "repository community is not tagged"},
		{"repository of a file", func(a *ApkPackages) { a.Install[1].File = "x.apk" }, "a package of a local file has no repository"},
		{"rpm file", func(a *ApkPackages) { a.Install[0].File = "x.rpm" }, "must be a .apk"},
		{"duplicate package", func(a *ApkPackages) { a.Install = append(a.Install, a.Install[0]) }, "more than once"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := valid()
			tt.mutate(p.HostPackages.Apk)
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

// Two repositories may name the same key (the keys of Alpine sign main and
// community), but not two different keys under one name.
func TestApkKeysOfOneName(t *testing.T) {
	dir := t.TempDir()
	other := strings.Replace(testApkKey, "MIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEAutQkua2CAig4VFSJ7v54",
		"MIICIjANBgkqhkiG9w0BAQEFAAOCAg8AMIICCgKCAgEAutQkua2CAig4VFSJ7v55", 1)
	for name, content := range map[string]string{"a/" + testApkKeyName: testApkKey, "b/" + testApkKeyName: testApkKey, "c/" + testApkKeyName: other} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	plan := func(second string) *DeploymentPlan {
		p := validPlan()
		p.baseDir = dir
		p.HostPackages.Apk = &ApkPackages{Repositories: []ApkRepository{
			{Name: "main", URL: "https://dl-cdn.alpinelinux.org/alpine/v3.22/main", KeyFile: "a/" + testApkKeyName},
			{Name: "community", URL: "https://dl-cdn.alpinelinux.org/alpine/v3.22/community", KeyFile: second + "/" + testApkKeyName},
		}}
		return p
	}
	if err := plan("b").Validate(); err != nil {
		t.Fatalf("the same key twice: %v", err)
	}
	if err := plan("c").Validate(); err == nil || !strings.Contains(err.Error(), "two different keys named "+testApkKeyName) {
		t.Fatalf("got %v, want the two keys of one name refused", err)
	}
}

// release.source host-package: the package may be listed in the apk block.
func TestHostPackageListedInApk(t *testing.T) {
	p := validPlan()
	p.Release = ReleaseConfig{Source: ReleaseHostPackage, Package: "norca"}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "host_packages.apk.install") {
		t.Fatalf("got %v, want the blocks named", err)
	}
	p.HostPackages.Apk = &ApkPackages{Install: []HostPackage{{Name: "norca", Version: "0.3.0-r0"}}}
	if err := p.Validate(); err != nil {
		t.Fatalf("the package of the apk block is not found: %v", err)
	}
}
