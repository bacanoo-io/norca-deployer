// Package scripts runs the scripts of the plan on the nodes at fixed phases
// of the deployment: with bash (the default) or sh on Linux nodes, with
// Windows PowerShell 5.1 (powershell.exe) on Windows nodes.
package scripts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

const (
	// StateDir keeps the uploaded scripts and the run markers on a Linux
	// node.
	StateDir = "/var/lib/norca-deployer/scripts"

	// WindowsDeployerDir is the directory of the deployer on a Windows
	// node, as /var/lib/norca-deployer is on Linux; the uninstall removes
	// it.
	WindowsDeployerDir = `C:\ProgramData\norca-deployer`
	// WindowsStateDir keeps the uploaded scripts and the run markers on a
	// Windows node. Only the Administrators and SYSTEM can open it.
	WindowsStateDir = WindowsDeployerDir + `\scripts`

	defaultTimeout = 600

	// timeoutExitCode is the exit code of a Windows script that ran out of
	// its timeout: the one of timeout(1), which runs the Linux scripts.
	timeoutExitCode = 124
)

// utf8BOM starts the PowerShell files the deployer writes: Windows
// PowerShell 5.1 reads a file without it in the ANSI code page of the
// node, which would change every value that is not ASCII.
const utf8BOM = "\ufeff"

// Resolver decrypts inline secrets in environment values.
type Resolver func(string) (string, error)

// Prepared is a script ready to run on a node.
type Prepared struct {
	Config *config.ScriptConfig
	Body   string
	Env    map[string]string
	// Digest covers the body and the environment, so that a changed value
	// re-runs the script.
	Digest string
}

// ScriptPath is where the script is stored on a Linux node.
func ScriptPath(name string) string { return StateDir + "/" + name + ".sh" }

// MarkerPath holds the digest of the last successful run on a Linux node.
func MarkerPath(name string) string { return StateDir + "/" + name + ".sha256" }

// files are the paths of a script on its node.
type files struct {
	script, env, marker string
}

// filesOf returns the paths of a script in the state directory of a Linux
// or a Windows node. The names of scripts are letters, digits, '_', '.'
// and '-' (validated with the plan), so they need no quoting in a path.
func filesOf(name string, windows bool) files {
	if windows {
		base := WindowsStateDir + `\` + name
		return files{script: base + ".ps1", env: base + ".env.ps1", marker: base + ".sha256"}
	}
	return files{script: ScriptPath(name), env: StateDir + "/" + name + ".env", marker: MarkerPath(name)}
}

// Prepare reads the body and resolves the environment of a script.
func Prepare(plan *config.DeploymentPlan, s *config.ScriptConfig, resolve Resolver) (*Prepared, error) {
	body, err := plan.ScriptBody(s)
	if err != nil {
		return nil, err
	}
	env := make(map[string]string, len(s.Env))
	for k, v := range s.Env {
		if resolve != nil {
			rv, err := resolve(v)
			if err != nil {
				return nil, fmt.Errorf("script %q: env %s: %w", s.Name, k, err)
			}
			v = rv
		}
		env[k] = v
	}
	return &Prepared{Config: s, Body: body, Env: env, Digest: digest(body, env)}, nil
}

func digest(body string, env map[string]string) string {
	h := sha256.New()
	h.Write([]byte(body))
	for _, k := range sortedKeys(env) {
		fmt.Fprintf(h, "\x00%s=%s", k, env[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sortedKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// timeoutOf returns the timeout of a script in seconds.
func timeoutOf(s *config.ScriptConfig) int {
	if s.Timeout <= 0 {
		return defaultTimeout
	}
	return s.Timeout
}

// Command builds the shell command that runs the uploaded script on a
// Linux node. The environment is written to a private file next to the
// script instead of the command line, so that secret values do not show up
// in the process list.
func Command(p *Prepared, envFile string) string {
	return fmt.Sprintf("set -a\n# shellcheck source=/dev/null\n. %s\nset +a\ntimeout -k 10 %d %s %s",
		connector.ShellQuote(envFile), timeoutOf(p.Config), config.ScriptInterpreter(p.Config, false), connector.ShellQuote(ScriptPath(p.Config.Name)))
}

// RenderEnvFile renders KEY='value' lines for the environment file of a
// Linux node.
func RenderEnvFile(env map[string]string) string {
	var sb strings.Builder
	for _, k := range sortedKeys(env) {
		sb.WriteString(k + "=" + connector.ShellQuote(env[k]) + "\n")
	}
	return sb.String()
}

// RenderPowerShellEnvFile renders the environment file of a Windows node: a
// PowerShell script of $env:KEY = 'value' lines. The values are
// single-quoted literals (connector.PSQuote), so $, the backtick, quotes
// and line breaks reach the script as they are. The names are validated
// with the plan (letters, digits and '_'). Windows has no empty variables:
// an empty value leaves the variable unset.
func RenderPowerShellEnvFile(env map[string]string) string {
	var sb strings.Builder
	for _, k := range sortedKeys(env) {
		sb.WriteString("$env:" + k + " = " + connector.PSQuote(env[k]) + "\n")
	}
	return sb.String()
}

// windowsDirCommand creates the state directory of a Windows node and sets
// its access list: full control for the Administrators (S-1-5-32-544) and
// SYSTEM (S-1-5-18), inherited by every file in it, and nothing inherited
// from ProgramData (which lets every user read). The groups are named by
// their SIDs, which do not depend on the language of Windows. A file the
// deployer uploads later takes this list, so the environment file with its
// secrets is never readable by others.
func windowsDirCommand(dir string) string {
	return "$dir = " + connector.PSQuote(dir) + `
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$acl = New-Object System.Security.AccessControl.DirectorySecurity
$acl.SetAccessRuleProtection($true, $false)
foreach ($sid in 'S-1-5-32-544', 'S-1-5-18') {
  $id = New-Object System.Security.Principal.SecurityIdentifier $sid
  $acl.AddAccessRule((New-Object System.Security.AccessControl.FileSystemAccessRule $id, 'FullControl', 'ContainerInherit, ObjectInherit', 'None', 'Allow'))
}
(Get-Item -LiteralPath $dir).SetAccessControl($acl)`
}

// WindowsCommand builds the PowerShell that runs an uploaded script on a
// Windows node; the connector sends it with -EncodedCommand.
//
// It reads the environment file into its own process and starts the
// script in a child powershell.exe (-File), which inherits that
// environment and the standard output and error of the command, so the
// output of the script and of its own children reaches the deployer as it
// is written. The exit code of the child is the result: the number of
// exit N, 1 after an error that ends the script (throw, or any error under
// $ErrorActionPreference = 'Stop'), else 0.
//
// When the timeout runs out first, taskkill /T /F stops the child and every
// process it started (the tree of its parent process ids), the command
// says so on its error output and exits with 124, as timeout(1) does on
// Linux. taskkill runs through Start-Process -NoNewWindow, which hands it
// the standard handles directly: an error line of taskkill (a process of
// the tree that ended meanwhile) must not become a PowerShell error under
// the 'Stop' of the connector.
func WindowsCommand(p *Prepared, scriptFile, envFile string) string {
	timeout := timeoutOf(p.Config)
	// WaitForExit takes an Int32 of milliseconds: 24.8 days at most.
	wait := min(int64(timeout)*1000, math.MaxInt32)
	args := `-NoProfile -NonInteractive -ExecutionPolicy Bypass -File "` + scriptFile + `"`
	stopped := fmt.Sprintf("the script ran longer than its timeout of %d s: it and the processes it started were stopped", timeout)
	return fmt.Sprintf(`. %s
$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName = "$PSHOME\powershell.exe"
$psi.Arguments = %s
$psi.UseShellExecute = $false
$run = [System.Diagnostics.Process]::Start($psi)
if (-not $run.WaitForExit(%d)) {
  Start-Process -FilePath taskkill.exe -ArgumentList '/T', '/F', '/PID', $run.Id -NoNewWindow -Wait
  [void]$run.WaitForExit(10000)
  [Console]::Error.WriteLine(%s)
  exit %d
}
exit $run.ExitCode`, connector.PSQuote(envFile), connector.PSQuote(args), wait, connector.PSQuote(stopped), timeoutExitCode)
}

// Target is the node the scripts of a phase run on.
type Target struct {
	Node     *config.NodeConfig
	IsMaster bool
	// Family is the family of the operating system of the node (its
	// facts); a script with families runs only on the nodes of those.
	Family string
}

func (t Target) isWindows() bool { return strings.EqualFold(t.Node.Platform, "windows") }

// Select returns the scripts of a phase that apply to the node, in the order
// of the configuration. A script of the other platform (a powershell
// script on a Linux node, a bash or sh script on a Windows node) does not
// apply: it is written for the other platform.
func Select(all []config.ScriptConfig, phase string, t Target) []*config.ScriptConfig {
	var res []*config.ScriptConfig
	for i := range all {
		s := &all[i]
		if s.Phase != phase || !config.AppliesTo(s.Roles, s.Hosts, t.Node, t.IsMaster) {
			continue
		}
		if !config.ScriptRunsOn(s, t.isWindows()) {
			continue
		}
		if len(s.Families) > 0 && !slices.Contains(s.Families, t.Family) {
			continue
		}
		res = append(res, s)
	}
	return res
}

// Run uploads and runs the scripts of a phase. A script runs when its digest
// differs from the marker of its last successful run, or when it is marked
// always. A failing script stops the deployment of the node.
func Run(conn connector.Connector, plan *config.DeploymentPlan, phase string, t Target, resolve Resolver) error {
	node := t.Node
	selected := Select(plan.Scripts, phase, t)
	if len(selected) == 0 {
		return nil
	}
	windows := t.isWindows()

	if windows {
		if out, err := conn.ExecuteCommand(windowsDirCommand(WindowsStateDir)); err != nil {
			return fmt.Errorf("failed to create %s: %w (output: %s)", WindowsStateDir, err, out)
		}
	} else if err := conn.MkdirAll(StateDir, 0700); err != nil {
		return fmt.Errorf("failed to create %s: %w", StateDir, err)
	}

	for _, s := range selected {
		p, err := Prepare(plan, s, resolve)
		if err != nil {
			return err
		}
		f := filesOf(s.Name, windows)

		if !s.Always {
			if prev, err := conn.ReadFile(f.marker); err == nil && strings.TrimSpace(string(prev)) == p.Digest {
				fmt.Printf("    -> [%s] Script %s unchanged (skipped)\n", node.Host, s.Name)
				continue
			}
		}

		body, envBody := p.Body, RenderEnvFile(p.Env)
		command := Command(p, f.env)
		cleanup := "rm -f " + connector.ShellQuote(f.env)
		if windows {
			body, envBody = withBOM(body), utf8BOM+RenderPowerShellEnvFile(p.Env)
			command = WindowsCommand(p, f.script, f.env)
			cleanup = "Remove-Item -Force -ErrorAction SilentlyContinue -LiteralPath " + connector.PSQuote(f.env)
		}

		if err := conn.CopyFile(bytes.NewReader([]byte(body)), f.script, 0700); err != nil {
			return fmt.Errorf("script %q: upload failed: %w", s.Name, err)
		}
		if err := conn.CopyFile(bytes.NewReader([]byte(envBody)), f.env, 0600); err != nil {
			return fmt.Errorf("script %q: env upload failed: %w", s.Name, err)
		}

		fmt.Printf("    -> [%s] Running script %s (%s)...\n", node.Host, s.Name, phase)
		out, runErr := conn.ExecuteCommand(command)
		_, _ = conn.ExecuteCommand(cleanup)
		if runErr != nil {
			return fmt.Errorf("script %q failed: %w", s.Name, runErr)
		}
		if trimmed := strings.TrimSpace(out); trimmed != "" {
			for line := range strings.SplitSeq(trimmed, "\n") {
				fmt.Printf("       | %s\n", strings.TrimSuffix(line, "\r"))
			}
		}

		if err := conn.CopyFile(bytes.NewReader([]byte(p.Digest+"\n")), f.marker, 0600); err != nil {
			return fmt.Errorf("script %q: failed to record run: %w", s.Name, err)
		}
	}
	return nil
}

// withBOM starts a PowerShell script with the UTF-8 byte order mark, unless
// it has one.
func withBOM(body string) string {
	if strings.HasPrefix(body, utf8BOM) {
		return body
	}
	return utf8BOM + body
}
