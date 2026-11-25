package packages

import (
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

// testApkKey is a signing key of Alpine (/etc/apk/keys of alpine:3.22).
const testApkKey = `-----BEGIN PUBLIC KEY-----
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

func testApkPackages() *config.HostPackages {
	return &config.HostPackages{Apk: &config.ApkPackages{
		Repositories: []config.ApkRepository{
			{Name: "community", URL: "https://dl-cdn.alpinelinux.org/alpine/v3.22/community", Roles: []string{config.RoleClient}},
			{Name: "mine", URL: "https://apk.example.com/v3.22/main", Key: testApkKey, KeyName: "packager-6b0763be.rsa.pub", Tagged: true},
		},
		Install: []config.HostPackage{
			{Name: "containerd", Version: "1.7.27-r0", Hold: true, Roles: []string{config.RoleClient}},
			{Name: "norca-agent", Version: "0.3.0-r1", Repository: "mine"},
			{Name: "jq", Hold: true},
			{Name: "tree"},
		},
	}}
}

func TestRenderApkRepositories(t *testing.T) {
	got := RenderApkRepositories(testApkPackages().Apk.Repositories)
	want := "# Managed by norca-deployer. Do not edit: the deployer writes this file whole.\n" +
		"https://dl-cdn.alpinelinux.org/alpine/v3.22/community\n" +
		"@norca-mine https://apk.example.com/v3.22/main\n"
	if got != want {
		t.Fatalf("RenderApkRepositories:\n%s\nwant:\n%s", got, want)
	}
}

// The world entry of a package: its version is its hold; a held package
// without a version is held at the version apk installed; a package at a
// version that is not held is released after the install.
func TestApkPackageOf(t *testing.T) {
	tests := []struct {
		name string
		pkg  config.HostPackage
		want apkPackage
	}{
		{"held at a version", config.HostPackage{Name: "containerd", Version: "1.7.27-r0", Hold: true},
			apkPackage{name: "containerd", version: "1.7.27-r0", spec: "containerd=1.7.27-r0", world: "'containerd=1.7.27-r0'"}},
		{"held at the installed version", config.HostPackage{Name: "jq", Hold: true},
			apkPackage{name: "jq", spec: "jq", world: `"jq=$(apk_version 'jq')"`, fixup: `"jq=$(apk_version 'jq')"`}},
		{"a version, not held", config.HostPackage{Name: "norca-agent", Version: "0.3.0-r1", Repository: "mine"},
			apkPackage{name: "norca-agent", version: "0.3.0-r1", spec: "norca-agent@norca-mine=0.3.0-r1", world: "'norca-agent@norca-mine'", fixup: "'norca-agent@norca-mine'"}},
		{"any version", config.HostPackage{Name: "tree"}, apkPackage{name: "tree", spec: "tree", world: "'tree'"}},
		{"tagged, held at the installed version", config.HostPackage{Name: "x", Repository: "mine", Hold: true},
			apkPackage{name: "x", spec: "x@norca-mine", world: `"x@norca-mine=$(apk_version 'x')"`, fixup: `"x@norca-mine=$(apk_version 'x')"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := apkPackageOf(&tt.pkg); got != tt.want {
				t.Fatalf("apkPackageOf = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// The steps of apk, as the SSH executor runs them and as the install
// script of a machine renders them, against golden files.
func TestApkStepsGolden(t *testing.T) {
	hp := testApkPackages()
	plan := &config.DeploymentPlan{HostPackages: hp}
	steps, err := Apk{}.Steps(SelectApk(hp.Apk, testNode, false), Context{Plan: plan, Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"apk", "apk repositories", "apk keys the plan dropped", "apk packages", "apk versions"}
	if names := stepNames(steps); !slices.Equal(names, want) {
		t.Fatalf("steps %q, want %q", names, want)
	}
	repos := stepNamed(t, steps, "apk repositories")
	if f := fileOf(repos, "/etc/apk/keys/packager-6b0763be.rsa.pub"); f == nil || string(f.Content) != testApkKey || repos.Secret {
		t.Errorf("the key under its own name: %+v, secret %t", f, repos.Secret)
	}
	golden(t, "apk_steps_ssh.golden", dumpSSH(steps))

	script, err := Apk{}.Steps(SelectApk(hp.Apk, testNode, false), Context{Plan: plan, Arch: "amd64", Script: true})
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := step.Render(script, step.RenderOptions{Boot: true, BootHint: "use install: image"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "apk_steps_script.golden", step.Functions(script)+"\n"+rendered+"\n")
}

// Every apk call waits for the lock of apk with the lock timeout of the
// plan; without update_cache the install reads no index.
func TestApkStepsLockTimeout(t *testing.T) {
	hp := testApkPackages()
	hp.LockTimeout = 42
	off := false
	hp.UpdateCache = &off
	steps, err := Apk{}.Steps(SelectApk(hp.Apk, testNode, false), Context{Plan: &config.DeploymentPlan{HostPackages: hp}, Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		for line := range strings.SplitSeq(s.Apply, "\n") {
			if strings.HasPrefix(line, "apk ") && !strings.HasPrefix(line, "apk --wait 42 ") {
				t.Errorf("%s: %q does not wait for the lock", s.Name, line)
			}
		}
	}
	install := stepNamed(t, steps, "apk packages")
	if strings.Contains(install.Apply, " -U") {
		t.Errorf("an index read without update_cache:\n%s", install.Apply)
	}
	wantCheck := "apk_has 'containerd' '1.7.27-r0' &&\n  apk_world 'containerd=1.7.27-r0' &&\n" +
		"  apk_has 'norca-agent' '0.3.0-r1' &&\n  apk_world 'norca-agent@norca-mine' &&\n" +
		"  apk_has 'jq' '' &&\n  apk_world \"jq=$(apk_version 'jq')\" &&\n" +
		"  apk_has 'tree' '' &&\n  apk_world 'tree'"
	if install.Check != wantCheck {
		t.Errorf("check:\n%s\nwant:\n%s", install.Check, wantCheck)
	}
}

// A local .apk is copied over SSH, checked and installed without the
// network; the install script of a machine can only require it. Without
// repositories the file of the deployer's repositories goes, and the keys
// it recorded with it.
func TestApkStepsLocalFile(t *testing.T) {
	dir := t.TempDir()
	apk := filepath.Join(dir, "norca-0.3.0-r0.x86_64.apk")
	if err := os.WriteFile(apk, []byte("fake apk"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, _ := artifacts.FileSHA256(apk)
	hp := &config.HostPackages{Apk: &config.ApkPackages{Install: []config.HostPackage{{
		Name: "norca", File: filepath.Join(dir, "norca-0.3.0-r0.{arch}.apk"), Version: "0.3.0-r0",
	}}}}
	plan := &config.DeploymentPlan{HostPackages: hp}
	sel := SelectApk(hp.Apk, testNode, false)

	steps, err := Apk{}.Steps(sel, Context{Plan: plan, Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"apk", "apk repositories", "apk keys the plan dropped", "apk package file norca-0.3.0-r0.x86_64.apk", "apk package files", "apk versions"}
	if names := stepNames(steps); !slices.Equal(names, want) {
		t.Fatalf("steps %q, want %q", names, want)
	}
	remote := ApkDir + "/norca-0.3.0-r0.x86_64.apk"
	file := stepNamed(t, steps, "apk package file norca-0.3.0-r0.x86_64.apk")
	if f := fileOf(file, remote); f == nil || f.Local != apk || f.SHA256 != sum || file.Apply != "apk_check '"+remote+"' 'norca' '0.3.0-r0'" {
		t.Fatalf("file step: %+v", file)
	}
	install := stepNamed(t, steps, "apk package files")
	if install.Apply != "apk --wait 300 add -q --allow-untrusted --no-network '"+remote+"'" ||
		install.Check != "apk_has 'norca' \"$(apk_field '"+remote+"' pkgver)\"" {
		t.Fatalf("install of the file: %+v", install)
	}
	if repos := stepNamed(t, steps, "apk repositories"); !slices.Equal(repos.Remove, []string{ApkRepositoriesFile}) || repos.Apply != "" || len(repos.Files) > 0 {
		t.Fatalf("without repositories: %+v", repos)
	}
	if dropped := stepNamed(t, steps, "apk keys the plan dropped"); !dropped.Lines || !strings.HasSuffix(dropped.Apply, "rm -f "+ApkKeysRecord) {
		t.Fatalf("without keys the record goes:\n%s", dropped.Apply)
	}

	script, err := Apk{}.Steps(sel, Context{Plan: plan, Arch: "amd64", Script: true})
	if err != nil {
		t.Fatal(err)
	}
	if s := stepNamed(t, script, "apk package norca"); s.Apply != "apk_require 'norca' '0.3.0-r0' 'norca-0.3.0-r0.x86_64.apk'" {
		t.Fatalf("script: %+v", s)
	}
	if slices.Contains(stepNames(script), "apk package files") {
		t.Fatal("the script installs a file it cannot reach")
	}

	hp.Apk.Install[0].File = apk
	hp.Apk.Install[0].SHA256 = strings.Repeat("0", 64)
	if _, err := (Apk{}).Steps(SelectApk(hp.Apk, testNode, false), Context{Plan: plan, Arch: "amd64"}); err == nil || !strings.Contains(err.Error(), "expected 0000") {
		t.Fatalf("a wrong digest: %v", err)
	}
}

// Two repositories signed by one key install it once; the keys are
// recorded for the removal of a dropped one.
func TestApkKeysOnce(t *testing.T) {
	hp := testApkPackages()
	hp.Apk.Repositories[0].Key, hp.Apk.Repositories[0].KeyName = testApkKey, "packager-6b0763be.rsa.pub"
	steps, err := Apk{}.Steps(SelectApk(hp.Apk, testNode, false), Context{Plan: &config.DeploymentPlan{HostPackages: hp}, Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	repos := stepNamed(t, steps, "apk repositories")
	if got := repos.Paths(); !slices.Equal(got, []string{"/etc/apk/keys/packager-6b0763be.rsa.pub", ApkRepositoriesFile}) {
		t.Fatalf("files %q", got)
	}
	if dropped := stepNamed(t, steps, "apk keys the plan dropped"); !strings.HasPrefix(dropped.Apply, "apk_keys_keep='packager-6b0763be.rsa.pub'\n") {
		t.Fatalf("keys kept:\n%s", dropped.Apply)
	}
}

func TestApkArchAndName(t *testing.T) {
	a := Apk{}
	if a.Name() != "apk" || a.Arch("amd64") != "x86_64" || a.Arch("arm64") != "aarch64" {
		t.Fatalf("name %s, arch %s %s", a.Name(), a.Arch("amd64"), a.Arch("arm64"))
	}
}

// The functions of the apk steps in each shell of this machine, on a
// database and a world of the test: versions, world entries, holds, the
// fields of an .apk file and the removal of a dropped key.
func TestApkFunctions(t *testing.T) {
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
	dir := t.TempDir()
	db := filepath.Join(dir, "installed")
	world := filepath.Join(dir, "world")
	files := map[string]string{
		db:    "C:Q1abc=\nP:jq\nV:1.7.1-r0\nA:x86_64\n\nC:Q1def=\nP:jq-doc\nV:1.6-r0\n\nP:norca-agent\nV:0.3.0-r1\nA:x86_64\n",
		world: "alpine-base\njq=1.7.1-r0\nnorca-agent@norca-mine\nnorca><Q1xyz=\ncontainerd@norca-mine=1.7.27-r0\n",
	}
	for p, content := range files {
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// A fake .apk: a tar.gz with a .PKGINFO.
	pkgDir := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(pkgDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, ".PKGINFO"), []byte("# Generated by abuild\npkgname = norca-steptest\npkgver = 1.0.0-r0\narch = noarch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	apkFile := filepath.Join(dir, "steptest.apk")
	if out, err := exec.Command("tar", "-czf", apkFile, "-C", pkgDir, ".PKGINFO").CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, out)
	}
	funcs := strings.NewReplacer("/lib/apk/db/installed", db, apkWorld, world, ApkKeyDir, filepath.Join(dir, "keys")).Replace(apkFunctions)
	stubs := "log() { printf 'log %s\\n' \"$*\"; }\ndie() {\n  printf 'error %s\\n' \"$*\"\n  exit 1\n}\n" +
		// apk info --who-owns: a package owns the keys named alpine-*.
		"apk() { case $4 in */alpine-*) return 0 ;; esac; return 1; }\n"
	if err := os.MkdirAll(filepath.Join(dir, "keys"), 0o750); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		prog string
		want string
	}{
		{name: "the version of a package", prog: "apk_version jq", want: "1.7.1-r0"},
		{name: "not the version of another package", prog: "apk_version jq-doc", want: "1.6-r0"},
		{name: "a package that is not installed", prog: "apk_version tree; echo end", want: "end"},
		{name: "at its version", prog: "apk_has jq 1.7.1-r0 && echo yes", want: "yes"},
		{name: "any version", prog: "apk_has norca-agent '' && echo yes", want: "yes"},
		{name: "another version", prog: "apk_has jq 1.6-r0 || echo no", want: "no"},
		{name: "a world entry", prog: "apk_world 'containerd@norca-mine=1.7.27-r0' && ! apk_world containerd && echo yes", want: "yes"},
		{name: "held at a version", prog: "apk_expect jq 1.7.1-r0", want: "log package jq 1.7.1-r0 (held)"},
		{name: "not held", prog: "apk_expect norca-agent ''", want: "log package norca-agent 0.3.0-r1"},
		{name: "another version installed", prog: "apk_expect jq 1.6-r0", want: "error package jq: version 1.7.1-r0 installed, 1.6-r0 requested"},
		{name: "not installed", prog: "apk_expect tree ''", want: "error package tree is not installed after apk add"},
		{name: "held: a file and a tagged entry", prog: "apk_held norca && apk_held containerd && ! apk_held norca-agent && ! apk_held alpine-base && echo yes", want: "yes"},
		{name: "the fields of a file", prog: "apk_field " + apkFile + " pkgname; apk_field " + apkFile + " pkgver", want: "norca-steptest\n1.0.0-r0"},
		{name: "a file of the package", prog: "apk_check " + apkFile + " norca-steptest 1.0.0-r0 && echo ok", want: "ok"},
		{name: "a file of another package", prog: "apk_check " + apkFile + " norca 1.0.0-r0", want: "error package norca: steptest.apk contains package norca-steptest"},
		{name: "a file of another version", prog: "apk_check " + apkFile + " norca-steptest 2.0-r0", want: "error package norca-steptest: steptest.apk has version 1.0.0-r0, expected 2.0-r0"},
		{name: "no .apk", prog: "apk_check " + db + " norca ''", want: "error package norca: installed is not an .apk file apk 2 or 3 builds (no .PKGINFO)"},
		{name: "a file to require", prog: "apk_require tree '' tree.apk", want: "error package tree is installed from the local file tree.apk on the deployer machine: install it into the image first"},
		{name: "a dropped key", prog: "touch \"$KEYS/k.rsa.pub\"; apk_key_drop k.rsa.pub; [ -e \"$KEYS/k.rsa.pub\" ] || echo gone", want: "removed " + filepath.Join(dir, "keys") + "/k.rsa.pub\ngone"},
		{name: "a key a package owns", prog: "touch \"$KEYS/alpine-x.rsa.pub\"; apk_key_drop alpine-x.rsa.pub; [ -e \"$KEYS/alpine-x.rsa.pub\" ] && echo kept", want: "kept"},
		{name: "a key that is gone", prog: "apk_key_drop none.rsa.pub && echo ok", want: "ok"},
	}
	for shell, argv := range shells {
		t.Run(shell, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					prog := stubs + funcs + "\n" + tt.prog + "\n"
					c := exec.Command(argv[0], append(argv[1:], "-c", prog)...)
					c.Env = []string{"PATH=/usr/bin:/bin", "KEYS=" + filepath.Join(dir, "keys")}
					out, _ := c.CombinedOutput()
					if got := strings.TrimSpace(string(out)); got != tt.want {
						t.Fatalf("got %q, want %q", got, tt.want)
					}
				})
			}
		})
	}
}
