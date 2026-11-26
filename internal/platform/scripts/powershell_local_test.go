//go:build windows && pwshlocal

package scripts

// These tests run the PowerShell of a Windows node on this machine, as the
// SSH connector sends it: cmd.exe (the default shell of the Windows
// OpenSSH server) runs powershell.exe -EncodedCommand with the command of
// connector.PowerShellCommand, with pipes for the standard output and
// error. They need Windows PowerShell 5.1 and take about 20 seconds:
//
//	go test -tags pwshlocal -run TestLocal -v ./internal/platform/scripts/

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// result is what the deployer gets back from a command.
type result struct {
	stdout, stderr string
	code           int
	elapsed        time.Duration
}

// startLikeSSH starts a PowerShell command as the Windows OpenSSH server
// runs the command of a session, cmd.exe /c <command>, and returns the wait
// for its result.
func startLikeSSH(t *testing.T, script string) func() result {
	t.Helper()
	c := exec.Command("cmd.exe")
	c.SysProcAttr = &syscall.SysProcAttr{CmdLine: "cmd.exe /c " + connector.PowerShellCommand(script)}
	var stdout, stderr bytes.Buffer
	c.Stdout, c.Stderr = &stdout, &stderr
	start := time.Now()
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	return func() result {
		t.Helper()
		err := c.Wait()
		res := result{stdout: stdout.String(), stderr: stderr.String(), elapsed: time.Since(start)}
		var exit *exec.ExitError
		switch {
		case errors.As(err, &exit):
			res.code = exit.ExitCode()
		case err != nil:
			t.Fatal(err)
		}
		return res
	}
}

// runLikeSSH runs a PowerShell command as startLikeSSH does.
func runLikeSSH(t *testing.T, script string) result {
	t.Helper()
	return startLikeSSH(t, script)()
}

// runScript uploads a script and its environment as Run does and runs it
// (startScript).
func runScript(t *testing.T, body string, env map[string]string, timeout int) result {
	t.Helper()
	return startScript(t, body, env, timeout)()
}

// startScript uploads a script and its environment as Run does (BOM, the
// environment file of PowerShell) into a temporary directory and starts it
// with the command of WindowsCommand.
func startScript(t *testing.T, body string, env map[string]string, timeout int) func() result {
	t.Helper()
	dir := t.TempDir()
	script, envFile := filepath.Join(dir, "s.ps1"), filepath.Join(dir, "s.env.ps1")
	if err := os.WriteFile(script, []byte(withBOM(body)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envFile, []byte(utf8BOM+RenderPowerShellEnvFile(env)), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &Prepared{Config: &config.ScriptConfig{Name: "s", Timeout: timeout}, Body: body, Env: env}
	return startLikeSSH(t, WindowsCommand(p, script, envFile))
}

// The values of the environment arrive in the script as they are in the
// plan: quotes (the typographic ones too), $, the backtick, line breaks
// and letters outside ASCII. An empty value leaves the variable unset.
func TestLocalEnvLiteral(t *testing.T) {
	values := map[string]string{
		"PLAIN":     "/dev/sdb",
		"QUOTE":     "it's ''",
		"TYPO":      "\u2018a\u2019; exit 7; \u2019 \u201ab\u201b",
		"DOLLAR":    "$HOME $(exit 8) ${env:PATH} $env:USERNAME",
		"BACKTICK":  "a`nb` `$x",
		"NEWLINE":   "line1\nline2\n",
		"CRLF":      "line1\r\nline2",
		"NONASCII":  "\u00e1rv\u00edzt\u0171r\u0151 \u65e5\u672c",
		"DOUBLE":    `"quoted" ; exit 9`,
		"SEMICOLON": "'; exit 10; '",
	}
	out := t.TempDir()
	env := map[string]string{"OUT_DIR": out, "EMPTY": ""}
	for k, v := range values {
		env[k] = v
	}
	body := `$utf8 = New-Object System.Text.UTF8Encoding $false
foreach ($name in 'PLAIN', 'QUOTE', 'TYPO', 'DOLLAR', 'BACKTICK', 'NEWLINE', 'CRLF', 'NONASCII', 'DOUBLE', 'SEMICOLON', 'EMPTY') {
  $value = [Environment]::GetEnvironmentVariable($name)
  if ($null -eq $value) { $value = '<unset>' }
  [IO.File]::WriteAllText((Join-Path $env:OUT_DIR $name), $value, $utf8)
}
`
	res := runScript(t, body, env, 60)
	if res.code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	values["EMPTY"] = "<unset>"
	for k, want := range values {
		got, err := os.ReadFile(filepath.Join(out, k))
		if err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	t.Logf("%d values arrived literally (%s)", len(values), res.elapsed.Round(time.Millisecond))
}

// The standard output and error of the script and of its native commands
// reach the deployer on the standard output and error of the command.
func TestLocalOutput(t *testing.T) {
	res := runScript(t, `Write-Output 'out-line'
Write-Host 'host-line'
[Console]::Error.WriteLine('err-line')
cmd.exe /c 'echo native-out'
cmd.exe /c 'echo native-err 1>&2'
`, nil, 60)
	if res.code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
	}
	for _, want := range []string{"out-line", "host-line", "native-out"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("stdout has no %q: %q", want, res.stdout)
		}
	}
	for _, want := range []string{"err-line", "native-err"} {
		if !strings.Contains(res.stderr, want) {
			t.Errorf("stderr has no %q: %q", want, res.stderr)
		}
	}
	t.Logf("stdout %q, stderr %q", res.stdout, res.stderr)
}

// The exit code of the script is the result: exit 3 fails with 3, a throw
// fails with 1, a script that ends normally succeeds.
func TestLocalExitCode(t *testing.T) {
	tests := []struct {
		name, body string
		code       int
		stderr     string
	}{
		{"exit 3", "Write-Output before\nexit 3\nWrite-Output after\n", 3, ""},
		{"throw", "throw 'boom'\n", 1, "boom"},
		{"error under stop", "$ErrorActionPreference = 'Stop'\nGet-Item -LiteralPath 'C:\\no\\such\\file'\nWrite-Output after\n", 1, "no\\such\\file"},
		{"normal end", "Write-Output ok\n", 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := runScript(t, tt.body, nil, 60)
			if res.code != tt.code {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", res.code, tt.code, res.stdout, res.stderr)
			}
			if strings.Contains(res.stdout, "after") {
				t.Errorf("the script went on: %q", res.stdout)
			}
			if !strings.Contains(res.stderr, tt.stderr) {
				t.Errorf("stderr has no %q: %q", tt.stderr, res.stderr)
			}
			t.Logf("exit %d (%s)", res.code, res.elapsed.Round(time.Millisecond))
		})
	}
}

// A script that outruns its timeout is stopped with every process it
// started: a native child it waits for (ping -n 30), a child of
// Start-Process, and a grandchild (a ping started by a child powershell).
// The command reports the timeout and exits with 124 a few seconds after
// the timeout. The tree of the script is read while it runs; after the
// command, none of its processes runs (a process is its id and its start
// time, as Windows reuses ids quickly).
func TestLocalTimeout(t *testing.T) {
	const timeout = 5
	tests := []struct {
		name, body string
		min        int // processes of the tree at least
	}{
		{"native child", `Set-Content -LiteralPath $env:PIDS -Value $PID
ping.exe -n 30 127.0.0.1
`, 2},
		{"started children", `$bg = Start-Process -FilePath ping.exe -ArgumentList '-n', '60', '127.0.0.1' -WindowStyle Hidden -PassThru
$gc = Start-Process -FilePath powershell.exe -ArgumentList '-NoProfile', '-Command', 'ping.exe -n 60 127.0.0.1' -WindowStyle Hidden -PassThru
Set-Content -LiteralPath $env:PIDS -Value $PID
ping.exe -n 30 127.0.0.1
`, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "pids")
			wait := startScript(t, tt.body, map[string]string{"PIDS": pidFile}, timeout)
			root := waitForPID(t, pidFile)
			// The children of Start-Process need a moment to start
			// their own.
			time.Sleep(1500 * time.Millisecond)
			tree := processTree(t, root)
			res := wait()
			if res.code != timeoutExitCode {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", res.code, timeoutExitCode, res.stdout, res.stderr)
			}
			if !strings.Contains(res.stderr, "the script ran longer than its timeout of 5 s") {
				t.Errorf("stderr: %q", res.stderr)
			}
			if res.elapsed < timeout*time.Second || res.elapsed > (timeout+5)*time.Second {
				t.Errorf("the command took %s with a timeout of %d s", res.elapsed, timeout)
			}
			if len(tree) < tt.min {
				t.Fatalf("the tree of the script has %d processes, want %d at least: %q", len(tree), tt.min, tree)
			}
			if left := stillRunning(t, tree); len(left) > 0 {
				t.Fatalf("still running after the timeout: %q (tree %q)", left, tree)
			}
			t.Logf("exit %d after %s with a timeout of %d s; the %d processes of the tree are gone: %q",
				res.code, res.elapsed.Round(time.Millisecond), timeout, len(tree), tree)
		})
	}
}

// waitForPID returns the process id the script writes to a file.
func waitForPID(t *testing.T, file string) int {
	t.Helper()
	for range 50 {
		data, err := os.ReadFile(file)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(string(data), utf8BOM))); err == nil {
				return pid
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the script wrote no process id to %s", file)
	return 0
}

// processTree returns the processes of the tree of a root as "id
// start-ticks name": the root and every process whose parent is in the
// tree and that started after its parent.
func processTree(t *testing.T, root int) []string {
	t.Helper()
	res := runLikeSSH(t, `$all = @(Get-CimInstance Win32_Process)
$tree = @{}
foreach ($p in $all) { if ($p.ProcessId -eq `+strconv.Itoa(root)+`) { $tree[$p.ProcessId] = $p } }
do {
  $n = $tree.Count
  foreach ($p in $all) {
    $parent = $tree[$p.ParentProcessId]
    if (-not $tree.ContainsKey($p.ProcessId) -and $parent -and $p.CreationDate -ge $parent.CreationDate) { $tree[$p.ProcessId] = $p }
  }
} while ($tree.Count -ne $n)
$tree.Values | ForEach-Object { '{0} {1} {2}' -f $_.ProcessId, $_.CreationDate.Ticks, $_.Name }`)
	if res.code != 0 {
		t.Fatalf("process tree: %d %s", res.code, res.stderr)
	}
	return lines(res.stdout)
}

// stillRunning returns the processes of a tree that run: the same id with
// the same start time.
func stillRunning(t *testing.T, tree []string) []string {
	t.Helper()
	keys := make([]string, 0, len(tree))
	for _, p := range tree {
		f := strings.Fields(p)
		keys = append(keys, connector.PSQuote(f[0]+" "+f[1]))
	}
	query := "$want = @(" + strings.Join(keys, ", ") + ")\n" +
		"Get-CimInstance Win32_Process | Where-Object { $want -contains ('{0} {1}' -f $_.ProcessId, $_.CreationDate.Ticks) } | " +
		"ForEach-Object { '{0} {1}' -f $_.ProcessId, $_.Name }"
	// taskkill has returned, but a killed process may take a moment to
	// leave the process list.
	var left []string
	for range 10 {
		res := runLikeSSH(t, query)
		if res.code != 0 {
			t.Fatalf("process query: %d %s", res.code, res.stderr)
		}
		if left = lines(res.stdout); len(left) == 0 {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return left
}

// lines returns the lines of an output that are not empty.
func lines(out string) []string {
	return slices.DeleteFunc(strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n"), func(s string) bool { return strings.TrimSpace(s) == "" })
}

// The state directory gets an access list of its own: full control for the
// Administrators and SYSTEM, inherited by its files, nothing from its
// parent. A file that was there before takes the new list. Run twice, the
// command gives the same list.
func TestLocalStateDirACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scripts")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "old.env.ps1")
	if err := os.WriteFile(old, []byte("$env:X = 'secret'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The owner may set the list again, so that the temporary
		// directory can be removed.
		_ = exec.Command("icacls.exe", dir, "/reset", "/T", "/C", "/Q").Run()
	})
	for range 2 {
		if res := runLikeSSH(t, windowsDirCommand(dir)); res.code != 0 {
			t.Fatalf("exit %d\nstdout: %s\nstderr: %s", res.code, res.stdout, res.stderr)
		}
	}
	// The access list of a path, its rules sorted. The .NET call reads it
	// without listing the directory, which the test may no longer do.
	show := func(kind, path string) string {
		t.Helper()
		res := runLikeSSH(t, "$acl = [System.IO."+kind+"]::GetAccessControl("+connector.PSQuote(path)+")\n"+
			"'protected=' + $acl.AreAccessRulesProtected\n"+
			"$acl.Access | ForEach-Object { '{0} {1} {2} inherited={3}' -f $_.IdentityReference.Translate([System.Security.Principal.SecurityIdentifier]).Value, $_.FileSystemRights, $_.InheritanceFlags, $_.IsInherited }")
		if res.code != 0 {
			t.Fatalf("access list of %s: %s", path, res.stderr)
		}
		lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(res.stdout), "\r\n", "\n"), "\n")
		slices.Sort(lines[1:])
		return strings.Join(lines, "\n")
	}
	wantDir := "protected=True\n" +
		"S-1-5-18 FullControl ContainerInherit, ObjectInherit inherited=False\n" +
		"S-1-5-32-544 FullControl ContainerInherit, ObjectInherit inherited=False"
	if got := show("Directory", dir); got != wantDir {
		t.Errorf("directory:\n%s\nwant:\n%s", got, wantDir)
	}
	wantFile := "protected=False\n" +
		"S-1-5-18 FullControl None inherited=True\n" +
		"S-1-5-32-544 FullControl None inherited=True"
	if got := show("File", old); got != wantFile {
		t.Errorf("file in it:\n%s\nwant:\n%s", got, wantFile)
	}
	// A test that does not run elevated is not one of the Administrators:
	// the file is closed to it now.
	elevated := runLikeSSH(t, "([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)")
	if strings.TrimSpace(elevated.stdout) == "True" {
		t.Log("elevated: the read of the file by a user that is not an administrator is not checked")
		return
	}
	if _, err := os.ReadFile(old); !errors.Is(err, os.ErrPermission) {
		t.Errorf("the file is readable by a user that is not an administrator: %v", err)
	}
}
