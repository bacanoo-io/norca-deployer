package scripts

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

var update = flag.Bool("update", false, "rewrite the golden files of testdata")

func testPlan() *config.DeploymentPlan {
	return &config.DeploymentPlan{
		Scripts: []config.ScriptConfig{
			{Name: "disk", Phase: config.PhasePreInstall, Inline: "echo disk\n", Env: map[string]string{"DEV": "/dev/sdb"}},
			{Name: "redis-tune", Phase: config.PhasePostPackages, Inline: "echo tune\n", Roles: []string{config.RoleClient}},
			{Name: "check", Phase: config.PhasePostInstall, Inline: "echo check\n", Always: true},
		},
	}
}

func TestRunOnceUntilChanged(t *testing.T) {
	plan := testPlan()
	node := &config.NodeConfig{Host: "10.0.0.1", Platform: "linux"}
	conn := connector.NewFakeConnector()

	if err := Run(conn, plan, config.PhasePreInstall, Target{Node: node, IsMaster: false, Family: "debian"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(conn.CommandsWithPrefix("set -a\n")); got != 1 {
		t.Fatalf("first apply: %d runs, want 1", got)
	}
	if string(conn.Files[ScriptPath("disk")]) != "echo disk\n" || conn.Modes[ScriptPath("disk")] != 0700 {
		t.Fatalf("script not uploaded correctly")
	}
	if env := string(conn.Files[StateDir+"/disk.env"]); env != "DEV='/dev/sdb'\n" || conn.Modes[StateDir+"/disk.env"] != 0600 {
		t.Fatalf("env file = %q", env)
	}

	// Second apply with the same script and env: skipped.
	if err := Run(conn, plan, config.PhasePreInstall, Target{Node: node, IsMaster: false, Family: "debian"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(conn.CommandsWithPrefix("set -a\n")); got != 1 {
		t.Fatalf("unchanged script ran again (%d runs)", got)
	}

	// A changed env value re-runs it.
	plan.Scripts[0].Env["DEV"] = "/dev/sdc"
	if err := Run(conn, plan, config.PhasePreInstall, Target{Node: node, IsMaster: false, Family: "debian"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := len(conn.CommandsWithPrefix("set -a\n")); got != 2 {
		t.Fatalf("changed env did not re-run the script (%d runs)", got)
	}
}

func TestAlwaysAndRoles(t *testing.T) {
	plan := testPlan()
	node := &config.NodeConfig{Host: "10.0.0.1", Platform: "linux"}
	conn := connector.NewFakeConnector()

	for range 2 {
		if err := Run(conn, plan, config.PhasePostInstall, Target{Node: node, IsMaster: true, Family: "debian"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(conn.CommandsWithPrefix("set -a\n")); got != 2 {
		t.Fatalf("always script: %d runs, want 2", got)
	}

	// redis-tune is client-only.
	if err := Run(conn, plan, config.PhasePostPackages, Target{Node: node, IsMaster: true, Family: "debian"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := conn.Files[ScriptPath("redis-tune")]; ok {
		t.Fatalf("client-only script ran on a master")
	}
}

func TestFailureKeepsMarkerUnset(t *testing.T) {
	plan := testPlan()
	node := &config.NodeConfig{Host: "10.0.0.1", Platform: "linux"}
	conn := connector.NewFakeConnector()
	conn.Failures["set -a\n"] = errBoom{}

	err := Run(conn, plan, config.PhasePreInstall, Target{Node: node, IsMaster: false, Family: "debian"}, nil)
	if err == nil || !strings.Contains(err.Error(), `script "disk" failed`) {
		t.Fatalf("expected failure, got %v", err)
	}
	if _, ok := conn.Files[MarkerPath("disk")]; ok {
		t.Fatalf("marker written for a failed run")
	}
	if rm := conn.CommandsWithPrefix("rm -f"); len(rm) != 1 {
		t.Fatalf("env file not removed after failure: %v", conn.Commands)
	}
}

// The command is POSIX sh: timeout with -k, which busybox has too.
func TestCommandIsPortable(t *testing.T) {
	p := &Prepared{Config: &config.ScriptConfig{Name: "prep", Timeout: 30}}
	got := Command(p, "/var/lib/norca-deployer/scripts/prep.env")
	want := "set -a\n# shellcheck source=/dev/null\n. '/var/lib/norca-deployer/scripts/prep.env'\nset +a\ntimeout -k 10 30 bash '/var/lib/norca-deployer/scripts/prep.sh'"
	if got != want {
		t.Fatalf("Command = %s\nwant      %s", got, want)
	}
}

// A script of the other platform is skipped without a word: a bash or sh
// script on a Windows node, a powershell script on a Linux node.
func TestOtherPlatformSkipped(t *testing.T) {
	plan := &config.DeploymentPlan{Scripts: []config.ScriptConfig{
		{Name: "bash", Phase: config.PhasePreInstall, Inline: "echo bash\n", Interpreter: config.InterpreterBash},
		{Name: "sh", Phase: config.PhasePreInstall, Inline: "echo sh\n", Interpreter: config.InterpreterSh},
		{Name: "ps", Phase: config.PhasePreInstall, Inline: "Write-Output ps\n", Interpreter: config.InterpreterPowerShell},
	}}
	tests := []struct {
		name, platform, family, runs string
	}{
		{"windows node", "windows", config.FamilyWindows, "ps"},
		{"linux node", "linux", config.FamilyDebian, "bash,sh"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &config.NodeConfig{Host: "10.0.0.9", Platform: tt.platform}
			var names []string
			for _, s := range Select(plan.Scripts, config.PhasePreInstall, Target{Node: node, Family: tt.family}) {
				names = append(names, s.Name)
			}
			if got := strings.Join(names, ","); got != tt.runs {
				t.Fatalf("selected %q, want %q", got, tt.runs)
			}
		})
	}

	// Only scripts of the other platform: nothing is run, and nothing is
	// said.
	linuxOnly := &config.DeploymentPlan{Scripts: plan.Scripts[:2]}
	conn := connector.NewFakeConnector()
	node := &config.NodeConfig{Host: "10.0.0.9", Platform: "windows"}
	out := captureStdout(t, func() {
		if err := Run(conn, linuxOnly, config.PhasePreInstall, Target{Node: node, Family: config.FamilyWindows}, nil); err != nil {
			t.Error(err)
		}
	})
	if len(conn.Commands) != 0 || len(conn.Files) != 0 || out != "" {
		t.Fatalf("bash scripts on a Windows node: commands %q, files %d, output %q", conn.Commands, len(conn.Files), out)
	}
}

// A script without an interpreter runs with powershell on a Windows node:
// the state directory gets its access list first, the script and the
// environment file are uploaded as PowerShell files with a BOM, the
// command runs the script, and the environment file is removed after it.
func TestWindowsRun(t *testing.T) {
	plan := &config.DeploymentPlan{Scripts: []config.ScriptConfig{{
		Name: "prep", Phase: config.PhasePreInstall, Inline: "Write-Output $env:DEV\n", Timeout: 30,
		Env: map[string]string{"DEV": "D:", "TOKEN": "!enc:v1:aes-gcm:abc"},
	}}}
	resolve := func(v string) (string, error) {
		if strings.HasPrefix(v, "!enc:") {
			return "s3cr'et", nil
		}
		return v, nil
	}
	node := &config.NodeConfig{Host: "10.0.0.9", Platform: "windows"}
	conn := connector.NewFakeConnector()
	if err := Run(conn, plan, config.PhasePreInstall, Target{Node: node, Family: config.FamilyWindows}, resolve); err != nil {
		t.Fatal(err)
	}
	const dir = `C:\ProgramData\norca-deployer\scripts`
	if len(conn.Commands) != 3 {
		t.Fatalf("commands: %q", conn.Commands)
	}
	if conn.Commands[0] != windowsDirCommand(dir) {
		t.Errorf("first command %q, want the access list of the directory", conn.Commands[0])
	}
	p, err := Prepare(plan, &plan.Scripts[0], resolve)
	if err != nil {
		t.Fatal(err)
	}
	if want := WindowsCommand(p, dir+`\prep.ps1`, dir+`\prep.env.ps1`); conn.Commands[1] != want {
		t.Errorf("run command %q, want %q", conn.Commands[1], want)
	}
	if want := `Remove-Item -Force -ErrorAction SilentlyContinue -LiteralPath 'C:\ProgramData\norca-deployer\scripts\prep.env.ps1'`; conn.Commands[2] != want {
		t.Errorf("cleanup %q, want %q", conn.Commands[2], want)
	}
	if got := string(conn.Files[dir+`\prep.ps1`]); got != "\ufeffWrite-Output $env:DEV\n" {
		t.Errorf("script %q", got)
	}
	if got := string(conn.Files[dir+`\prep.env.ps1`]); got != "\ufeff$env:DEV = 'D:'\n$env:TOKEN = 's3cr''et'\n" {
		t.Errorf("environment file %q", got)
	}
	if got := string(conn.Files[dir+`\prep.sha256`]); got != p.Digest+"\n" {
		t.Errorf("marker %q", got)
	}
	if strings.Contains(conn.Commands[1], "s3cr") {
		t.Errorf("secret in the command line")
	}

	// The same script again: the marker skips it.
	conn.Commands = nil
	if err := Run(conn, plan, config.PhasePreInstall, Target{Node: node, Family: config.FamilyWindows}, resolve); err != nil {
		t.Fatal(err)
	}
	if len(conn.Commands) != 1 {
		t.Fatalf("unchanged script ran again: %q", conn.Commands)
	}
}

// A failing Windows script fails the node, keeps its marker unset, and its
// environment file is removed all the same.
func TestWindowsFailure(t *testing.T) {
	plan := &config.DeploymentPlan{Scripts: []config.ScriptConfig{{Name: "prep", Phase: config.PhasePreInstall, Inline: "exit 3"}}}
	node := &config.NodeConfig{Host: "10.0.0.9", Platform: "windows"}
	conn := connector.NewFakeConnector()
	conn.Failures[". 'C:"] = errBoom{}
	err := Run(conn, plan, config.PhasePreInstall, Target{Node: node, Family: config.FamilyWindows}, nil)
	if err == nil || !strings.Contains(err.Error(), `script "prep" failed`) {
		t.Fatalf("err %v", err)
	}
	if _, ok := conn.Files[`C:\ProgramData\norca-deployer\scripts\prep.sha256`]; ok {
		t.Fatalf("marker written for a failed run")
	}
	if rm := conn.CommandsWithPrefix("Remove-Item"); len(rm) != 1 {
		t.Fatalf("environment file not removed: %q", conn.Commands)
	}
}

// The PowerShell of a Windows node, against golden files: the access list
// of the state directory, the environment file and the command that runs
// a script with its timeout.
func TestWindowsGolden(t *testing.T) {
	golden(t, "windows_dir.golden", windowsDirCommand(WindowsStateDir)+"\n")
	golden(t, "windows_env.golden", RenderPowerShellEnvFile(map[string]string{
		"PLAIN":    "/dev/sdb",
		"QUOTE":    "it's",
		"DOLLAR":   "$HOME $(Get-Date) ${x}",
		"BACKTICK": "a`nb`",
		"NEWLINE":  "line1\nline2",
		"TYPO":     "\u2018a\u2019 \u201ab\u201b",
		"EMPTY":    "",
	}))
	p := &Prepared{Config: &config.ScriptConfig{Name: "prep", Timeout: 30}}
	golden(t, "windows_command.golden", WindowsCommand(p, WindowsStateDir+`\prep.ps1`, WindowsStateDir+`\prep.env.ps1`)+"\n")
	p.Config.Timeout = 0
	if got := WindowsCommand(p, `C:\s.ps1`, `C:\e.ps1`); !strings.Contains(got, "WaitForExit(600000)") || !strings.Contains(got, "timeout of 600 s") {
		t.Errorf("default timeout: %s", got)
	}
	p.Config.Timeout = 3_000_000
	if got := WindowsCommand(p, `C:\s.ps1`, `C:\e.ps1`); !strings.Contains(got, "WaitForExit(2147483647)") {
		t.Errorf("a timeout beyond Int32 milliseconds: %s", got)
	}
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
		t.Fatalf("%s differs (run with -update after checking the change):\n got: %q\nwant: %q", name, got, want)
	}
}

// captureStdout returns what fn prints.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = stdout
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestSecretEnv(t *testing.T) {
	plan := testPlan()
	plan.Scripts[0].Env["TOKEN"] = "!enc:v1:aes-gcm:abc"
	resolve := func(v string) (string, error) {
		if strings.HasPrefix(v, "!enc:") {
			return "s3cr'et", nil
		}
		return v, nil
	}
	p, err := Prepare(plan, &plan.Scripts[0], resolve)
	if err != nil {
		t.Fatal(err)
	}
	if got := RenderEnvFile(p.Env); !strings.Contains(got, `TOKEN='s3cr'\''et'`) {
		t.Fatalf("env file = %q", got)
	}
	if strings.Contains(Command(p, "/x.env"), "s3cr") {
		t.Fatalf("secret leaked into the command line")
	}
}

type errBoom struct{}

func (errBoom) Error() string { return "boom" }

// A script runs with its interpreter, and only on the nodes of its
// families.
func TestInterpreterAndFamilies(t *testing.T) {
	sh := &Prepared{Config: &config.ScriptConfig{Name: "prep", Interpreter: config.InterpreterSh}}
	if got := Command(sh, "/e"); !strings.HasSuffix(got, "timeout -k 10 600 sh '/var/lib/norca-deployer/scripts/prep.sh'") {
		t.Fatalf("Command = %s", got)
	}
	all := []config.ScriptConfig{
		{Name: "any", Phase: config.PhasePreInstall, Inline: "true"},
		{Name: "rhel-only", Phase: config.PhasePreInstall, Inline: "true", Families: []string{config.FamilyRHEL}},
		{Name: "debian-and-alpine", Phase: config.PhasePreInstall, Inline: "true", Families: []string{config.FamilyDebian, config.FamilyAlpine}},
	}
	node := &config.NodeConfig{Host: "10.0.0.1", Platform: "linux"}
	names := func(family string) string {
		var res []string
		for _, s := range Select(all, config.PhasePreInstall, Target{Node: node, Family: family}) {
			res = append(res, s.Name)
		}
		return strings.Join(res, ",")
	}
	if got := names(config.FamilyDebian); got != "any,debian-and-alpine" {
		t.Errorf("debian: %s", got)
	}
	if got := names(config.FamilyRHEL); got != "any,rhel-only" {
		t.Errorf("rhel: %s", got)
	}
}
