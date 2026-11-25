package step

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// helpers are small versions of the helpers of the install script, for
// running rendered steps.
const helpers = `changed=0
wrote=0
log() { printf 'LOG %s\n' "$*"; }
warn() { printf 'WARN %s\n' "$*"; }
die() {
  printf 'DIE %s\n' "$*"
  exit 1
}
put_file() {
  tmp=$(mktemp)
  if [ "${3:-}" = base64 ]; then base64 -d >"$tmp"; else cat >"$tmp"; fi
  wrote=0
  if [ -f "$1" ] && cmp -s "$tmp" "$1"; then
    rm -f "$tmp"
    return 0
  fi
  mkdir -p "$(dirname "$1")"
  mv -f "$tmp" "$1"
  chmod "$2" "$1"
  wrote=1
  changed=1
}
remove_file() {
  [ -e "$1" ] || return 1
  rm -f "$1"
  changed=1
}
ensure_dir() { mkdir -p "$1"; }
step_lines() {
  while IFS= read -r l; do
    case $l in
    changed\ *) changed=1 ;;
    error\ *) die "$1: this machine ${l#error }" ;;
    esac
  done <<EOF
$3
EOF
  [ "$2" = 0 ] || die "$1: failed"
}
`

// shells are the POSIX shells of this machine; NORCA_TEST_BUSYBOX names a
// static busybox.
func shells(t *testing.T) map[string][]string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the script runs on Linux")
	}
	res := map[string][]string{}
	for _, name := range []string{"dash", "bash"} {
		if p, err := exec.LookPath(name); err == nil {
			res[name] = []string{p}
		}
	}
	if bb := os.Getenv("NORCA_TEST_BUSYBOX"); bb != "" {
		res["busybox"] = []string{bb, "sh"}
	}
	if len(res) == 0 {
		t.Skip("no POSIX shell")
	}
	return res
}

func TestRenderRunsTwice(t *testing.T) {
	for name, argv := range shells(t) {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			stale := filepath.Join(dir, "stale")
			if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			steps := []Step{
				unitStep(dir, "v1\n"),
				{Name: "binary key", Files: []File{{Path: filepath.Join(dir, "key.gpg"), Mode: 0o644, Content: []byte{0x99, 0x01, 0x0d}}},
					Remove: []string{stale}},
				{Name: "guarded", When: "[ -e " + q(stale) + " ]", Apply: "touch " + q(filepath.Join(dir, "guarded"))},
				{Name: "lines", Lines: true, Apply: "echo 'unchanged x'"},
			}
			body, err := Render(steps, RenderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			script := "set -eu\n" + helpers + Functions(steps) + body + "\necho \"changed=$changed\"\n"
			run := func() string {
				c := exec.Command(argv[0], append(argv[1:], "-c", script)...)
				c.Env = []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + t.TempDir()}
				out, err := c.CombinedOutput()
				if err != nil {
					t.Fatalf("%v:\n%s\nscript:\n%s", err, out, script)
				}
				return string(out)
			}
			if out := run(); !strings.HasSuffix(out, "changed=1\n") || !strings.Contains(out, "LOG unit enabled") {
				t.Fatalf("first run:\n%s", out)
			}
			if data, err := os.ReadFile(filepath.Join(dir, "key.gpg")); err != nil || string(data) != "\x99\x01\x0d" {
				t.Fatalf("binary file %q, %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(dir, "guarded")); err == nil {
				t.Fatal("the guarded step ran after its file was removed")
			}
			if out := run(); !strings.HasSuffix(out, "changed=0\n") {
				t.Fatalf("second run:\n%s", out)
			}
		})
	}
}

func TestRenderRefuses(t *testing.T) {
	tests := []struct {
		name string
		step Step
		opts RenderOptions
		err  string
	}{
		{"secret at boot", Step{Name: "apt credentials docker", Secret: true, Files: []File{{Path: "/a", Content: []byte("x\n")}}},
			RenderOptions{Boot: true, BootHint: "use install: image"}, "apt credentials docker has credentials: at boot they would travel in the user data of the machine; use install: image"},
		{"local file", Step{Name: "deb", Files: []File{{Path: "/a", Local: "/tmp/a.deb"}}}, RenderOptions{}, "cannot reach"},
		{"windows", Step{Name: "service", PowerShell: true, Apply: "x"}, RenderOptions{}, "Windows"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Render([]Step{tt.step}, tt.opts); err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Fatalf("err %v, want %q", err, tt.err)
			}
		})
	}
	if _, err := Render([]Step{{Name: "apt credentials docker", Secret: true}}, RenderOptions{}); err != nil {
		t.Fatalf("a secret in an image: %v", err)
	}
}

func TestShellFile(t *testing.T) {
	if got := ShellFile("/a", "0644", []byte("x\ny\n")); got != "put_file '/a' 0644 <<'NORCA_EOF'\nx\ny\nNORCA_EOF" {
		t.Fatalf("text: %q", got)
	}
	for _, content := range [][]byte{[]byte("no newline"), {0x99, 0x01, 0x0d, 0x04}, []byte("a\nNORCA_EOF\nb\n"), []byte("dos\r\n")} {
		if got := ShellFile("/a", "0644", content); !strings.HasPrefix(got, "put_file '/a' 0644 base64 <<'NORCA_EOF'\n") {
			t.Errorf("%q: %q", content, got)
		}
	}
}
