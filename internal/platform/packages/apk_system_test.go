package packages

import (
	"bytes"
	"fmt"
	"net"
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

// The steps of apk run for real on this machine: NORCA_TEST_APK=1 in an
// Alpine container as root, with abuild and the repositories of Alpine
// reachable (tree comes from there). A repository of packages built here
// with a key of their own is served over HTTP (busybox httpd) as a tagged
// repository; a package of a local .apk is signed by a key apk does not
// trust. The test installs, holds, upgrades, releases and downgrades,
// waits for the lock of apk, refuses a file of another package and a
// dropped tagged repository still in use, removes the key of a dropped
// repository and keeps a key a package owns.
func TestApkStepsOnThisSystem(t *testing.T) {
	if os.Getenv("NORCA_TEST_APK") != "1" {
		t.Skip("NORCA_TEST_APK is not set (an Alpine container as root)")
	}
	for _, tool := range []string{"abuild", "abuild-keygen", "busybox", "flock"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s (apk add abuild)", tool)
		}
	}
	dir := t.TempDir()
	repoKey, fileAPK := buildApkRepo(t, dir)
	url := serveDir(t, filepath.Join(dir, "packages"))
	alpineKey := "/etc/apk/keys/alpine-devel@lists.alpinelinux.org-6165ee59.rsa.pub"
	release := strings.TrimSpace(readFile(t, "/etc/alpine-release"))
	branch := "v" + strings.Join(strings.SplitN(release, ".", 3)[:2], ".")
	if strings.Contains(release, "_alpha") {
		branch = "edge"
	}

	hp := &config.HostPackages{LockTimeout: 30, Apk: &config.ApkPackages{
		Repositories: []config.ApkRepository{
			{Name: "steptest", URL: url + "/test", KeyFile: repoKey, Tagged: true},
			// A repository of Alpine with a key of Alpine: the key is a
			// file of the package alpine-keys.
			{Name: "community", URL: "https://dl-cdn.alpinelinux.org/alpine/" + branch + "/community", KeyFile: alpineKey},
		},
		Install: []config.HostPackage{
			{Name: "norca-steptest", Version: "1.0.0-r0", Hold: true, Repository: "steptest"},
			{Name: "tree"},
			{Name: "norca-filetest", File: fileAPK},
		},
	}}
	plan := &config.DeploymentPlan{HostPackages: hp}
	t.Cleanup(func() {
		_ = exec.Command("apk", "del", "-q", "norca-steptest", "norca-filetest", "tree").Run()
		_ = os.Remove(ApkRepositoriesFile)
		_ = os.Remove(filepath.Join(ApkKeyDir, filepath.Base(repoKey)))
	})

	var out bytes.Buffer
	ex := &step.Executor{Conn: connector.NewLocalConnector(t.Context()), Host: "local", Out: &out}
	apply := func(t *testing.T) (bool, error) {
		t.Helper()
		out.Reset()
		steps, err := Apk{}.Steps(SelectApk(hp.Apk, testNode, false), Context{Plan: plan, Arch: "amd64"})
		if err != nil {
			t.Fatal(err)
		}
		changed, _, err := ex.RunAll(t.Context(), steps)
		return changed, err
	}
	version := func(t *testing.T, name string) string {
		t.Helper()
		v, _ := exec.Command("sh", "-c", "awk -v p='P:"+name+"' '$0 == p { f = 1; next } f && /^V:/ { print substr($0, 3); exit }' /lib/apk/db/installed").Output()
		return strings.TrimSpace(string(v))
	}
	world := func(t *testing.T) string { t.Helper(); return readFile(t, "/etc/apk/world") }
	again := func(t *testing.T) {
		t.Helper()
		if changed, err := apply(t); err != nil || changed {
			t.Fatalf("again: changed %t, %v\n%s", changed, err, out.String())
		}
	}

	t.Run("first run installs and holds", func(t *testing.T) {
		changed, err := apply(t)
		if err != nil || !changed {
			t.Fatalf("changed %t, %v\n%s", changed, err, out.String())
		}
		for _, w := range []string{"repositories: indexes read", "package norca-steptest 1.0.0-r0 (held)", "package tree ", "package norca-filetest 2.0.0-r0 (held)"} {
			if !strings.Contains(out.String(), w) {
				t.Errorf("no %q in:\n%s", w, out.String())
			}
		}
		w := world(t)
		for _, e := range []string{"norca-steptest@norca-steptest=1.0.0-r0\n", "tree\n", "norca-filetest><"} {
			if !strings.Contains(w, e) {
				t.Errorf("no %q in the world:\n%s", e, w)
			}
		}
		if _, err := os.Stat(filepath.Join(ApkKeyDir, filepath.Base(repoKey))); err != nil {
			t.Errorf("the key of the repository: %v", err)
		}
		if rec := readFile(t, ApkKeysRecord); !strings.Contains(rec, filepath.Base(repoKey)) || !strings.Contains(rec, filepath.Base(alpineKey)) {
			t.Errorf("record of the keys: %q", rec)
		}
	})
	t.Run("second run changes nothing", func(t *testing.T) { again(t) })
	t.Run("a new version of a held package", func(t *testing.T) {
		hp.Apk.Install[0].Version = "1.1.0-r0"
		if changed, err := apply(t); err != nil || !changed {
			t.Fatalf("changed %t, %v\n%s", changed, err, out.String())
		}
		if v := version(t, "norca-steptest"); v != "1.1.0-r0" || !strings.Contains(world(t), "norca-steptest@norca-steptest=1.1.0-r0\n") {
			t.Fatalf("installed %q, world:\n%s", v, world(t))
		}
		again(t)
	})
	t.Run("the hold is released", func(t *testing.T) {
		hp.Apk.Install[0].Version, hp.Apk.Install[0].Hold = "", false
		if _, err := apply(t); err != nil || !strings.Contains(out.String(), "package norca-steptest 1.1.0-r0\n") {
			t.Fatalf("%v\n%s", err, out.String())
		}
		if !strings.Contains(world(t), "norca-steptest@norca-steptest\n") {
			t.Fatalf("world:\n%s", world(t))
		}
		again(t)
	})
	t.Run("held at the installed version", func(t *testing.T) {
		hp.Apk.Install[0].Hold = true
		if _, err := apply(t); err != nil || !strings.Contains(world(t), "norca-steptest@norca-steptest=1.1.0-r0\n") {
			t.Fatalf("%v\n%s\nworld:\n%s", err, out.String(), world(t))
		}
		again(t)
	})
	t.Run("a version, not held", func(t *testing.T) {
		hp.Apk.Install[0].Version, hp.Apk.Install[0].Hold = "1.0.0-r0", false
		if _, err := apply(t); err != nil {
			t.Fatalf("%v\n%s", err, out.String())
		}
		if v := version(t, "norca-steptest"); v != "1.0.0-r0" || !strings.Contains(world(t), "norca-steptest@norca-steptest\n") {
			t.Fatalf("installed %q, world:\n%s", v, world(t))
		}
		again(t)
	})
	t.Run("the lock of apk", func(t *testing.T) {
		// Another apk holds the lock: the install waits lock_timeout, then
		// fails.
		hp.Apk.Install[0].Version, hp.LockTimeout = "1.1.0-r0", 3
		defer func() { hp.LockTimeout = 30 }()
		lock := exec.Command("flock", "/lib/apk/db/lock", "sleep", "8")
		if err := lock.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = lock.Wait() }()
		time.Sleep(500 * time.Millisecond)
		start := time.Now()
		_, err := apply(t)
		if err == nil || time.Since(start) < 3*time.Second || !strings.Contains(err.Error(), "lock") {
			t.Fatalf("after %s: %v\n%s", time.Since(start), err, out.String())
		}
		_ = lock.Wait()
		if _, err := apply(t); err != nil || version(t, "norca-steptest") != "1.1.0-r0" {
			t.Fatalf("after the lock: %v\n%s", err, out.String())
		}
	})
	t.Run("a file of another package", func(t *testing.T) {
		other := filepath.Join(dir, "norca-other-2.0.0-r0.apk")
		if err := os.WriteFile(other, []byte(readFile(t, fileAPK)), 0o600); err != nil {
			t.Fatal(err)
		}
		hp.Apk.Install = append(hp.Apk.Install, config.HostPackage{Name: "norca-other", File: other})
		defer func() { hp.Apk.Install = hp.Apk.Install[:3] }()
		if _, err := apply(t); err == nil || !strings.Contains(err.Error(), "contains package norca-filetest") {
			t.Fatalf("%v\n%s", err, out.String())
		}
	})
	t.Run("a dropped tagged repository still in use", func(t *testing.T) {
		repos, install := hp.Apk.Repositories, hp.Apk.Install
		hp.Apk.Repositories, hp.Apk.Install = repos[1:], install[1:]
		defer func() { hp.Apk.Repositories, hp.Apk.Install = repos, install }()
		if _, err := apply(t); err == nil || !strings.Contains(err.Error(), "tagged repository @norca-steptest") {
			t.Fatalf("%v\n%s", err, out.String())
		}
	})
	t.Run("the keys of dropped repositories", func(t *testing.T) {
		if out, err := exec.Command("apk", "del", "-q", "norca-steptest").CombinedOutput(); err != nil {
			t.Fatalf("apk del: %v %s", err, out)
		}
		hp.Apk.Repositories = nil
		hp.Apk.Install = []config.HostPackage{{Name: "tree"}}
		changed, err := apply(t)
		if err != nil || !changed {
			t.Fatalf("changed %t, %v\n%s", changed, err, out.String())
		}
		if _, err := os.Stat(ApkRepositoriesFile); !os.IsNotExist(err) {
			t.Errorf("the repositories file stays: %v", err)
		}
		if _, err := os.Stat(filepath.Join(ApkKeyDir, filepath.Base(repoKey))); !os.IsNotExist(err) {
			t.Errorf("the key of the dropped repository stays: %v\n%s", err, out.String())
		}
		if _, err := os.Stat(alpineKey); err != nil {
			t.Errorf("the key of Alpine was removed: %v", err)
		}
		if _, err := os.Stat(ApkKeysRecord); !os.IsNotExist(err) {
			t.Errorf("the record stays: %v", err)
		}
		again(t)
	})
}

// buildApkRepo builds the packages of the test with abuild: norca-steptest
// 1.0.0-r0 and 1.1.0-r0 in the repository dir/packages/test, signed by a
// key of their own, and norca-filetest 2.0.0-r0 signed by another key. It
// returns the public key of the repository and the .apk of filetest; no
// key of the test stays in /etc/apk/keys.
func buildApkRepo(t *testing.T, dir string) (string, string) {
	t.Helper()
	script := `set -e
build() {
  mkdir -p "$DIR/aports/test/$1-$2"
  cat >"$DIR/aports/test/$1-$2/APKBUILD" <<EOF
pkgname=$1
pkgver=$2
pkgrel=0
pkgdesc="a package of the tests of norca-deployer"
url="https://example.org"
arch="noarch"
license="MIT"
options="!check"
package() {
  mkdir -p "\$pkgdir/usr/share/$1"
  echo "$2" >"\$pkgdir/usr/share/$1/version"
}
EOF
  (cd "$DIR/aports/test/$1-$2" && REPODEST="$DIR/$3" abuild -F rootpkg >/dev/null 2>&1 && REPODEST="$DIR/$3" abuild -F index >/dev/null 2>&1)
}
# A key is named after its packager and the second it was made: two
# packagers, two names.
for k in repo file; do
  HOME=$DIR/home-$k PACKAGER="Norca test <$k@example.org>" abuild-keygen -a -n >/dev/null 2>&1
done
export PACKAGER="Norca test <repo@example.org>"
# ~/.abuild, or ~/.config/abuild since abuild 3.16 (Alpine edge).
repo_key=$(find "$DIR/home-repo" -name '*.rsa.pub')
file_key=$(find "$DIR/home-file" -name '*.rsa.pub')
# abuild indexes with the keys of /etc/apk/keys: trusted while it builds.
cp "$repo_key" "$file_key" /etc/apk/keys/
HOME=$DIR/home-repo build norca-steptest 1.0.0 packages
HOME=$DIR/home-repo build norca-steptest 1.1.0 packages
HOME=$DIR/home-file build norca-filetest 2.0.0 files
rm -f /etc/apk/keys/"${repo_key##*/}" /etc/apk/keys/"${file_key##*/}"
echo "$repo_key"
find "$DIR/files" -name 'norca-filetest-2.0.0-r0.apk'
`
	c := exec.Command("sh", "-c", script)
	c.Env = append(os.Environ(), "DIR="+dir)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("abuild: %v\n%s", err, out)
	}
	lines := strings.Fields(string(out))
	if len(lines) != 2 {
		t.Fatalf("abuild: %q", out)
	}
	return lines[0], lines[1]
}

// serveDir serves a directory over HTTP on the loopback with busybox
// httpd and returns its URL.
func serveDir(t *testing.T, dir string) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	// Alpine has httpd in busybox-extras.
	bb := "busybox"
	if p, err := exec.LookPath("busybox-extras"); err == nil {
		bb = p
	}
	httpd := exec.Command(bb, "httpd", "-f", "-p", addr, "-h", dir)
	if err := httpd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = httpd.Process.Kill()
		_ = httpd.Wait()
	})
	for range 50 {
		if c, err := net.Dial("tcp", addr); err == nil {
			_ = c.Close()
			return fmt.Sprintf("http://%s", addr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("busybox httpd does not listen on %s", addr)
	return ""
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
