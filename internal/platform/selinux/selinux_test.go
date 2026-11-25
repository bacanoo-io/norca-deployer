package selinux

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/platform/service"
	"github.com/bacanoo-io/norca-deployer/internal/platform/step"
	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
)

func TestNodePaths(t *testing.T) {
	p := NodePaths(&storage.ResolvedPaths{
		BinDir: "/usr/local/bin", ConfigDir: "/etc/norca.d", DataDir: "/var/lib/norca", LogDir: "/var/log/norca",
		TLSDir: "/etc/norca.d/tls", CNIBinDir: "/opt/cni/bin", CNIConfigDir: "/opt/cni/config",
	})
	for _, want := range []string{"/usr/local/bin/norca", service.UnitPath, "/etc/norca.d", "/opt/cni/bin", "/var/lib/norca-deployer/rpms"} {
		if !slices.Contains(p.Recursive, want) {
			t.Errorf("%s is not restored with what is below it: %q", want, p.Recursive)
		}
	}
	// The data of the allocations keeps the labels of the runtimes.
	if !slices.Equal(p.Self, []string{"/var/lib/norca", "/var/log/norca"}) || slices.Contains(p.Recursive, "/var/lib/norca") {
		t.Errorf("self %q, recursive %q", p.Self, p.Recursive)
	}
	if slices.Contains(p.Recursive, "") || slices.Contains(NodePaths(&storage.ResolvedPaths{}).Recursive, "/norca") {
		t.Error("an empty path")
	}
}

// The step runs restorecon -R on what exists of the recursive paths and
// plain restorecon on the directories; without restorecon it warns. In the
// install script it runs only after a change, with SELinux on.
func TestRestoreStep(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the step runs in a POSIX shell")
	}
	root := t.TempDir()
	for _, d := range []string{"etc/norca.d", "var/lib/norca"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	paths := Paths{
		Recursive: []string{filepath.Join(root, "etc/norca.d"), filepath.Join(root, "missing")},
		Self:      []string{filepath.Join(root, "var/lib/norca")},
	}
	bin := t.TempDir()
	stub := "#!/bin/sh\necho \"restorecon $*\"\n"
	if err := os.WriteFile(filepath.Join(bin, "restorecon"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	helpers := "log() { echo \"log $*\"; }\nwarn() { echo \"warn $*\"; }\n"
	tests := []struct {
		name    string
		path    string
		script  bool
		prelude string
		want    []string
		unwant  []string
	}{
		{name: "over ssh", path: bin + ":/usr/bin:/bin",
			want:   []string{"restorecon -R " + root + "/etc/norca.d\n", "restorecon " + root + "/var/lib/norca\n", "log SELinux labels restored"},
			unwant: []string{"missing", "norca-*"}},
		{name: "without restorecon", path: "/usr/bin:/bin", want: []string{"warn SELinux is on, but restorecon (policycoreutils) is missing"}},
		{name: "script after a change", path: bin + ":/usr/bin:/bin", script: true,
			prelude: "changed=1\ngetenforce() { echo Enforcing; }\n", want: []string{"restorecon -R " + root + "/etc/norca.d\n"}},
		{name: "script without a change", path: bin + ":/usr/bin:/bin", script: true,
			prelude: "changed=0\ngetenforce() { echo Enforcing; }\n", unwant: []string{"restorecon"}},
		{name: "script with selinux disabled", path: bin + ":/usr/bin:/bin", script: true,
			prelude: "changed=1\ngetenforce() { echo Disabled; }\n", unwant: []string{"restorecon"}},
	}
	for _, shell := range []string{"dash", "bash"} {
		sh, err := exec.LookPath(shell)
		if err != nil {
			continue
		}
		t.Run(shell, func(t *testing.T) {
			for _, tt := range tests {
				t.Run(tt.name, func(t *testing.T) {
					s := RestoreStep(paths, tt.script)
					prog := helpers + tt.prelude + s.Apply
					if tt.script {
						rendered, err := step.Render([]step.Step{s}, step.RenderOptions{})
						if err != nil {
							t.Fatal(err)
						}
						prog = helpers + tt.prelude + rendered
					} else if s.When != "" {
						t.Fatalf("over SSH the caller decides: when %q", s.When)
					}
					c := exec.Command(sh, "-c", prog)
					c.Env = []string{"PATH=" + tt.path}
					out, err := c.CombinedOutput()
					if err != nil {
						t.Fatalf("%v\n%s", err, out)
					}
					for _, w := range tt.want {
						if !strings.Contains(string(out), w) {
							t.Errorf("no %q in:\n%s", w, out)
						}
					}
					for _, u := range tt.unwant {
						if strings.Contains(string(out), u) {
							t.Errorf("%q in:\n%s", u, out)
						}
					}
				})
			}
		})
	}
}
