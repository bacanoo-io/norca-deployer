package packages

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/artifacts"
	"github.com/bacanoo-io/norca-deployer/internal/platform/step"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

var update = flag.Bool("update", false, "rewrite the golden files of testdata")

func testDnfPackages() *config.HostPackages {
	return &config.HostPackages{Dnf: &config.DnfPackages{
		Repositories: []config.DnfRepository{{
			Name:     "docker",
			BaseURL:  "https://download.docker.com/linux/rhel/$releasever/$basearch/stable",
			Key:      testKey,
			Priority: 10,
			Include:  []string{"containerd.io", "docker-ce*"},
			Exclude:  []string{"podman*"},
			Roles:    []string{config.RoleClient},
		}},
		Install: []config.HostPackage{
			{Name: "containerd.io", Version: "1.7.27-3.1.el9", Hold: true, Roles: []string{config.RoleClient}},
			{Name: "docker-ce", Version: "3:27.3.1-1.el9", Roles: []string{config.RoleClient}},
			{Name: "jq"},
		},
	}}
}

func TestRenderRepoFile(t *testing.T) {
	repo := &testDnfPackages().Dnf.Repositories[0]
	want := "# Managed by norca-deployer. Do not edit.\n" +
		"[norca-docker]\n" +
		"name=norca-deployer: docker\n" +
		"baseurl=https://download.docker.com/linux/rhel/$releasever/$basearch/stable\n" +
		"enabled=1\n" +
		"gpgcheck=1\n" +
		"gpgkey=file:///etc/pki/rpm-gpg/norca-docker.asc\n" +
		"priority=10\n" +
		"includepkgs=containerd.io docker-ce*\n" +
		"excludepkgs=podman*\n"
	if got := RenderRepoFile(repo, ""); got != want {
		t.Fatalf("RenderRepoFile:\n%s\nwant:\n%s", got, want)
	}

	plain := &config.DnfRepository{Name: "mirror", Metalink: "https://mirrors.example.com/metalink?repo=el9", Key: testKey,
		Auth: &config.RepositoryAuth{Username: "deploy", Password: "!enc:x"}}
	got := RenderRepoFile(plain, "s3cret")
	for _, w := range []string{"metalink=https://mirrors.example.com/metalink?repo=el9\n", "username=deploy\npassword=s3cret\n"} {
		if !strings.Contains(got, w) {
			t.Errorf("no %q in:\n%s", w, got)
		}
	}
	for _, unwanted := range []string{"baseurl=", "priority=", "includepkgs=", "!enc:"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%q in:\n%s", unwanted, got)
		}
	}
}

// Block chooses the block of the node's package manager, by role and host.
func TestBlock(t *testing.T) {
	hp := testDnfPackages()
	hp.Apt = testHostPackages().Apt
	node := &config.NodeConfig{Host: "10.0.0.1"}
	tests := []struct {
		name      string
		manager   string
		isMaster  bool
		repos     int
		packages  []string
		haveBlock bool
	}{
		{name: "dnf client", manager: config.PackageManagerDnf, repos: 1, packages: []string{"containerd.io", "docker-ce", "jq"}, haveBlock: true},
		{name: "dnf master", manager: config.PackageManagerDnf, isMaster: true, packages: []string{"jq"}, haveBlock: true},
		{name: "apt client", manager: config.PackageManagerApt, repos: 1, packages: []string{"norca-redis", "jq"}, haveBlock: true},
		{name: "apk", manager: config.PackageManagerApk},
		{name: "none", manager: config.PackageManagerNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sel, ok := Block(hp, tt.manager, node, tt.isMaster)
			var names []string
			for _, p := range sel.Packages {
				names = append(names, p.Name)
			}
			if ok != tt.haveBlock || len(sel.Repositories)+len(sel.DnfRepositories) != tt.repos || !slices.Equal(names, tt.packages) {
				t.Fatalf("Block = %+v, %t", sel, ok)
			}
		})
	}
	hp.Dnf = nil
	if sel, ok := Block(hp, config.PackageManagerDnf, node, false); ok || !sel.Empty() {
		t.Fatalf("a plan without a dnf block: %+v %t", sel, ok)
	}
	if sel, ok := Block(nil, config.PackageManagerApt, node, false); ok || !sel.Empty() {
		t.Fatalf("a plan without host_packages: %+v %t", sel, ok)
	}
}

func TestDnfInstallCommand(t *testing.T) {
	got := DnfInstallCommand([]string{"containerd.io-1.7.27-3.1.el9", "/var/lib/norca-deployer/rpms/x.rpm"}, true)
	want := "dnf -y -q --setopt=install_weak_deps=False --refresh install 'containerd.io-1.7.27-3.1.el9' '/var/lib/norca-deployer/rpms/x.rpm'"
	if got != want {
		t.Fatalf("DnfInstallCommand:\n%s\nwant\n%s", got, want)
	}
	if got := DnfInstallCommand([]string{"jq"}, false); strings.Contains(got, "--refresh") {
		t.Fatalf("no refresh: %s", got)
	}
}

// stepNames lists the names of steps.
func stepNames(steps []step.Step) []string {
	names := make([]string, 0, len(steps))
	for _, s := range steps {
		names = append(names, s.Name)
	}
	return names
}

// dumpSSH writes the commands the SSH executor runs for steps, and the
// files they write: the form a golden file compares.
func dumpSSH(steps []step.Step) string {
	var b strings.Builder
	for i := range steps {
		s := &steps[i]
		fmt.Fprintf(&b, "=== %s (secret %t, lines %t)\n", s.Name, s.Secret, s.Lines)
		for _, d := range s.Dirs {
			fmt.Fprintf(&b, "dir %s %04o\n", d.Path, d.Mode.Perm())
		}
		for _, f := range s.Files {
			if f.Local != "" {
				fmt.Fprintf(&b, "file %s %04o local %s\n", f.Path, f.Mode.Perm(), filepath.Base(f.Local))
				continue
			}
			fmt.Fprintf(&b, "file %s %04o:\n%s", f.Path, f.Mode.Perm(), f.Content)
		}
		if s.Check != "" {
			fmt.Fprintf(&b, "--- check\n%s\n", step.CheckCommand(s))
		}
		if s.Apply != "" {
			fmt.Fprintf(&b, "--- apply\n%s\n", step.ApplyCommand(s))
		}
	}
	return b.String()
}

// golden compares got with testdata/<name>, or writes it with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	file := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%v (run the test with -update to write it)", err)
	}
	if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
		gotLines, wantLines := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := range min(len(gotLines), len(wantLines)) {
			if gotLines[i] != wantLines[i] {
				t.Fatalf("%s differs at line %d:\n got: %s\nwant: %s\n(run with -update after checking the change)", name, i+1, gotLines[i], wantLines[i])
			}
		}
		t.Fatalf("%s differs in length: %d lines, want %d", name, len(gotLines), len(wantLines))
	}
}

// The steps of dnf, as the SSH executor runs them and as the install
// script of a machine renders them, against golden files.
func TestDnfStepsGolden(t *testing.T) {
	hp := testDnfPackages()
	hp.Dnf.Repositories[0].Auth = &config.RepositoryAuth{Username: "u", Password: "!enc:secret"}
	plan := &config.DeploymentPlan{HostPackages: hp}
	resolve := func(v string) (string, error) { return strings.TrimPrefix(v, "!enc:"), nil }

	steps, err := Dnf{}.Steps(SelectDnf(hp.Dnf, testNode, false), Context{Plan: plan, Arch: "amd64", Resolve: resolve})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"dnf", "dnf repository docker (with credentials)", "dnf repositories the plan dropped", "dnf packages",
		"dnf versionlock", "dnf holds", "dnf versions"}
	if names := stepNames(steps); !slices.Equal(names, want) {
		t.Fatalf("steps %q, want %q", names, want)
	}
	repo := stepNamed(t, steps, "dnf repository docker (with credentials)")
	if f := fileOf(repo, "/etc/yum.repos.d/norca-docker.repo"); f == nil || f.Mode != 0o600 || !strings.Contains(string(f.Content), "password=secret\n") || !repo.Secret {
		t.Errorf("credentials: %+v, secret %t", f, repo.Secret)
	}
	golden(t, "dnf_steps_ssh.golden", dumpSSH(steps))

	hp.Dnf.Repositories[0].Auth = nil
	script, err := Dnf{}.Steps(SelectDnf(hp.Dnf, testNode, false), Context{Plan: plan, Arch: "amd64", Script: true})
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := step.Render(script, step.RenderOptions{Boot: true, BootHint: "use install: image"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "dnf_steps_script.golden", step.Functions(script)+"\n"+rendered+"\n")
}

// Every step of dnf that runs dnf waits for the other package manager
// processes with the lock timeout of the plan.
func TestDnfStepsLockTimeout(t *testing.T) {
	hp := testDnfPackages()
	hp.LockTimeout = 42
	steps, err := Dnf{}.Steps(SelectDnf(hp.Dnf, testNode, false), Context{Plan: &config.DeploymentPlan{HostPackages: hp}, Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dnf repository docker", "dnf packages", "dnf versionlock", "dnf holds"} {
		if s := stepNamed(t, steps, name); !strings.HasPrefix(s.Apply, "dnf_wait 42\n") {
			t.Errorf("%s does not wait first:\n%s", name, s.Apply)
		}
	}
}

// Without a held package there is no versionlock step, and the holds of an
// earlier plan are released.
func TestDnfStepsWithoutHold(t *testing.T) {
	hp := testDnfPackages()
	hp.Dnf.Install[0].Hold = false
	off := false
	hp.UpdateCache = &off
	steps, err := Dnf{}.Steps(SelectDnf(hp.Dnf, testNode, false), Context{Plan: &config.DeploymentPlan{HostPackages: hp}, Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(stepNames(steps), "dnf versionlock") {
		t.Fatal("a versionlock step without a held package")
	}
	holds := stepNamed(t, steps, "dnf holds").Apply
	if !strings.Contains(holds, "if dnf_held 'containerd.io'; then\n  dnf_unlock 'containerd.io'") {
		t.Errorf("holds:\n%s", holds)
	}
	install := stepNamed(t, steps, "dnf packages")
	if strings.Contains(install.Apply, "--refresh") || install.Check != "rpm_has 'containerd.io' '1.7.27-3.1.el9' &&\n  rpm_has 'docker-ce' '3:27.3.1-1.el9' &&\n  rpm_has 'jq' ''" {
		t.Errorf("install without update_cache: %+v", install)
	}
}

// A local .rpm is copied over SSH and checked; the install script of a
// machine can only require it to be installed. The plan's repositories
// that a node no longer has are removed, the others kept.
func TestDnfStepsLocalRPM(t *testing.T) {
	dir := t.TempDir()
	rpm := filepath.Join(dir, "norca-0.3.0-1.x86_64.rpm")
	if err := os.WriteFile(rpm, []byte("fake rpm"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, _ := artifacts.FileSHA256(rpm)
	hp := &config.HostPackages{Dnf: &config.DnfPackages{Install: []config.HostPackage{{
		Name: "norca", File: filepath.Join(dir, "norca-0.3.0-1.{arch}.rpm"), Version: "0.3.0-1",
	}}}}
	plan := &config.DeploymentPlan{HostPackages: hp}
	sel := SelectDnf(hp.Dnf, testNode, false)

	steps, err := Dnf{}.Steps(sel, Context{Plan: plan, Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	remote := RPMDir + "/norca-0.3.0-1.x86_64.rpm"
	file := stepNamed(t, steps, "dnf package file norca-0.3.0-1.x86_64.rpm")
	if f := fileOf(file, remote); f == nil || f.Local != rpm || f.SHA256 != sum || file.Apply != "rpm_check '"+remote+"' 'norca' '0.3.0-1'" {
		t.Fatalf("file step: %+v", file)
	}
	install := stepNamed(t, steps, "dnf packages")
	if strings.Contains(install.Apply, "--refresh") || !strings.Contains(install.Apply, "'"+remote+"'") ||
		!strings.Contains(install.Check, "rpm_has 'norca' \"$(rpm_field '"+remote+"' '%{EPOCHNUM}:%{VERSION}-%{RELEASE}')\"") {
		t.Fatalf("an offline node gets no --refresh, and the version of the file is checked:\n%+v", install)
	}
	dropped := stepNamed(t, steps, "dnf repositories the plan dropped")
	if !dropped.Lines || strings.Contains(dropped.Apply, "case") || !strings.Contains(dropped.Apply, "echo \"removed $f\"") {
		t.Fatalf("without repositories every repository of the deployer goes:\n%s", dropped.Apply)
	}

	script, err := Dnf{}.Steps(sel, Context{Plan: plan, Arch: "amd64", Script: true})
	if err != nil {
		t.Fatal(err)
	}
	if s := stepNamed(t, script, "dnf package norca"); s.Apply != "dnf_require 'norca' '0.3.0-1' 'norca-0.3.0-1.x86_64.rpm'" {
		t.Fatalf("script: %+v", s)
	}
	if slices.Contains(stepNames(script), "dnf packages") {
		t.Fatal("the script installs a file it cannot reach")
	}

	hp.Dnf.Install[0].File = rpm
	hp.Dnf.Install[0].SHA256 = strings.Repeat("0", 64)
	if _, err := (Dnf{}).Steps(SelectDnf(hp.Dnf, testNode, false), Context{Plan: plan, Arch: "amd64"}); err == nil || !strings.Contains(err.Error(), "expected 0000") {
		t.Fatalf("a wrong digest: %v", err)
	}
}

// A repository with credentials is a secret step that the install script
// refuses at boot; a resolved password of two lines is refused.
func TestDnfRepositoryCredentials(t *testing.T) {
	hp := testDnfPackages()
	hp.Dnf.Repositories[0].Auth = &config.RepositoryAuth{Username: "u", Password: "!enc:x"}
	plan := &config.DeploymentPlan{HostPackages: hp}
	steps, err := Dnf{}.Steps(SelectDnf(hp.Dnf, testNode, false), Context{Plan: plan, Arch: "amd64", Script: true,
		Resolve: func(string) (string, error) { return "pw", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := step.Render(steps, step.RenderOptions{Boot: true, BootHint: "use install: image"}); err == nil || !strings.Contains(err.Error(), "use install: image") {
		t.Fatalf("boot with credentials: %v", err)
	}
	if _, err := (Dnf{}).Steps(SelectDnf(hp.Dnf, testNode, false), Context{Plan: plan, Arch: "amd64",
		Resolve: func(string) (string, error) { return "pw\ngpgcheck=0", nil }}); err == nil || !strings.Contains(err.Error(), "one line") {
		t.Fatalf("a password of two lines: %v", err)
	}
}

func TestDnfArchAndName(t *testing.T) {
	d := Dnf{}
	if d.Name() != "dnf" || d.Arch("amd64") != "x86_64" || d.Arch("arm64") != "aarch64" {
		t.Fatalf("name %s, arch %s %s", d.Name(), d.Arch("amd64"), d.Arch("arm64"))
	}
}

// dnfStubs are commands of a node for the functions of the dnf steps: rpm
// answers from $RPM_DB (NAME EPOCH:VERSION-RELEASE lines) and dnf prints
// $DNF_LOCKS for versionlock list.
const dnfStubs = `rpm() {
  if [ "$1" = -qp ]; then
    case $3 in
    %{NAME}) echo norca-steptest ;;
    *) echo 0:1.0-1 ;;
    esac
    return 0
  fi
  name=$4
  found=$(awk -v n="$name" '$1 == n { print $2 }' "$RPM_DB")
  if [ -z "$found" ]; then
    echo "package $name is not installed"
    return 1
  fi
  printf '%s\n' "$found"
}
dnf() {
  case "$*" in
  *"versionlock list"*) printf '%s' "$DNF_LOCKS" ;;
  esac
}
log() { printf 'log %s\n' "$*"; }
die() {
  printf 'error %s\n' "$*"
  exit 1
}
`

// The functions of the dnf steps in each shell of this machine: versions
// with and without an epoch, the lists of versionlock of dnf 4 and dnf5,
// and the wait for another package manager.
func TestDnfFunctions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the functions run in a POSIX shell")
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
	db := filepath.Join(t.TempDir(), "rpmdb")
	if err := os.WriteFile(db, []byte("docker-ce 3:27.3.1-1.el9\njq 0:1.6-21.el9\njq 0:1.6-21.el9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		locks string
		prog  string
		want  string
	}{
		{name: "version without its epoch", prog: "rpm_has docker-ce 27.3.1-1.el9 && echo yes", want: "yes"},
		{name: "version with its epoch", prog: "rpm_has docker-ce 3:27.3.1-1.el9 && echo yes", want: "yes"},
		{name: "another epoch", prog: "rpm_has docker-ce 2:27.3.1-1.el9 || echo no", want: "no"},
		{name: "another version", prog: "rpm_has docker-ce 27.3.0-1.el9 || echo no", want: "no"},
		{name: "any version", prog: "rpm_has jq '' && echo yes", want: "yes"},
		{name: "not installed", prog: "rpm_has tree '' || echo no", want: "no"},
		{name: "expect logs without the epoch 0", prog: "dnf_expect jq 1.6-21.el9", want: "log package jq 1.6-21.el9"},
		{name: "expect another version", prog: "dnf_expect docker-ce 1-1", want: "error package docker-ce: version 3:27.3.1-1.el9 installed, 1-1 requested"},
		{name: "a file of another package", prog: "rpm_check /x/other.rpm norca 0.3.0-1", want: "error package norca: other.rpm contains package norca-steptest"},
		{name: "the locks of dnf 4", locks: "docker-ce-3:27.3.1-1.el9.*\ncontainerd.io-0:1.7.27-3.1.el9.*\n!podman-0:4.9-1.*\n",
			prog: "dnf_locks | tr '\\n' ' '", want: "docker-ce containerd.io"},
		{name: "the locks of dnf5", locks: "# Added by 'versionlock add' command on 2026-11-25 18:31:55\nPackage name: docker-ce\nevr = 3:27.3.1-1.el9\n\nPackage name: jq\nevr = 1.6-21.el9\n",
			prog: "dnf_locks | tr '\\n' ' '", want: "docker-ce jq"},
		{name: "held", locks: "docker-ce-3:27.3.1-1.el9.*\n", prog: "dnf_held_list=$(dnf_locks); dnf_held docker-ce && ! dnf_held docker && echo yes", want: "yes"},
		// dnf_busy runs in a command substitution: its count is in a file.
		{name: "wait for another process", prog: "dnf_busy() { n=$(($(cat \"$COUNT\" 2>/dev/null || echo 0) + 1)); echo $n >\"$COUNT\"; [ $n -le 2 ] && echo 'dnf (process 7)'; }; sleep() { :; }; dnf_wait 10 && echo waited",
			want: "log waiting for dnf (process 7) to finish\nwaited"},
		{name: "the lock timeout", prog: "dnf_busy() { echo 'rpm (process 9)'; }; sleep() { :; }; dnf_wait 4",
			want: "log waiting for rpm (process 9) to finish\nerror another package manager process, rpm (process 9), still runs after 4 seconds (host_packages.lock_timeout)"},
	}
	for shell, argv := range shells {
		t.Run(shell, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					prog := dnfStubs + dnfFunctions + "\n" + tt.prog + "\n"
					c := exec.Command(argv[0], append(argv[1:], "-c", prog)...)
					c.Env = []string{"PATH=/usr/bin:/bin", "RPM_DB=" + db, "DNF_LOCKS=" + tt.locks, "COUNT=" + filepath.Join(t.TempDir(), "count")}
					out, _ := c.CombinedOutput()
					if got := strings.TrimSpace(string(out)); got != tt.want {
						t.Fatalf("got %q, want %q", got, tt.want)
					}
				})
			}
		})
	}
}
