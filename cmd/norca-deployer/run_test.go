package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const exampleSingleNode = "../../examples/nodes/single-node.yaml"

// execute runs the command line and returns the exit code and what went to
// stdout and stderr.
func execute(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestExitCodes(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	tests := []struct {
		name     string
		args     []string
		code     int
		inStderr string
	}{
		{"success", []string{"validate", "-c", exampleSingleNode}, exitOK, ""},
		{"a failing command", []string{"validate", "-c", missing}, exitError, "[ERROR] invalid configuration"},
		{"unknown command", []string{"frobnicate"}, exitUsage, "unknown command"},
		{"unknown flag", []string{"validate", "--no-such-flag"}, exitUsage, "unknown flag"},
		{"missing argument", []string{"secret", "encrypt"}, exitUsage, "accepts 1 arg"},
		{"missing required flag", []string{"pki", "issue-intermediate"}, exitUsage, "required flag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, stderr := execute(t, tt.args...)
			if code != tt.code {
				t.Fatalf("exit code = %d, want %d (stderr: %q)", code, tt.code, stderr)
			}
			if !strings.Contains(stderr, tt.inStderr) {
				t.Fatalf("stderr = %q, want it to contain %q", stderr, tt.inStderr)
			}
		})
	}
}

// Diagnostics belong to stderr: stdout carries the output of the command only,
// so that it can be piped.
func TestErrorsGoToStderrNotStdout(t *testing.T) {
	code, stdout, stderr := execute(t, "validate", "-c", filepath.Join(t.TempDir(), "missing.yaml"))
	if code != exitError {
		t.Fatalf("exit code = %d", code)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want it empty", stdout)
	}
	if !strings.HasPrefix(stderr, "[ERROR] ") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestValidatePrintsToStdout(t *testing.T) {
	code, stdout, stderr := execute(t, "validate", "-c", exampleSingleNode)
	if code != exitOK || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "[OK] Configuration is valid!") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestPlanPrintsToStdout(t *testing.T) {
	code, stdout, stderr := execute(t, "plan", "-c", exampleSingleNode)
	if code != exitOK {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	for _, want := range []string{"=== Norca Deployment Plan (Dry-Run) ===", "Firewall:", "Master Nodes"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q", want)
		}
	}
}

func TestVersion(t *testing.T) {
	// Set by the linker (make build): it wins over the build information.
	old := version
	t.Cleanup(func() { version = old })
	version = "v9.9.9-test"

	code, stdout, _ := execute(t, "version")
	if code != exitOK || !strings.Contains(stdout, "Norca Deployer v9.9.9-test ") {
		t.Fatalf("code=%d stdout=%q", code, stdout)
	}
	if code, _, stderr := execute(t, "version", "extra"); code != exitUsage {
		t.Fatalf("version with an argument: code=%d stderr=%q", code, stderr)
	}
}

// Without -ldflags the version is never empty: it comes from the build
// information of the binary.
func TestBuildVersionWithoutTheLinker(t *testing.T) {
	old := version
	t.Cleanup(func() { version = old })
	version = ""
	if v := buildVersion(); v == "" {
		t.Fatal("empty version")
	}
}

func TestInitRefusesToOverwrite(t *testing.T) {
	target := filepath.Join(t.TempDir(), "plan.yaml")
	if code, _, stderr := execute(t, "init", "-c", target); code != exitOK {
		t.Fatalf("first init: code=%d stderr=%q", code, stderr)
	}
	code, _, stderr := execute(t, "init", "-c", target)
	if code != exitError || !strings.Contains(stderr, "already exists") {
		t.Fatalf("second init: code=%d stderr=%q", code, stderr)
	}
	if code, _, stderr := execute(t, "init", "-c", target, "--force"); code != exitOK {
		t.Fatalf("init --force: code=%d stderr=%q", code, stderr)
	}
}

// The usage text is for --help and for a wrong command line, not for every
// failure of a command that ran.
func TestAFailingCommandDoesNotPrintTheUsage(t *testing.T) {
	_, stdout, stderr := execute(t, "validate", "-c", filepath.Join(t.TempDir(), "missing.yaml"))
	if strings.Contains(stdout+stderr, "Usage:") {
		t.Fatalf("usage text printed:\n%s%s", stdout, stderr)
	}
}

func TestValidateJSON(t *testing.T) {
	code, stdout, stderr := execute(t, "validate", "-c", exampleSingleNode, "-o", "json")
	if code != exitOK || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	var res struct {
		Valid   bool   `json:"valid"`
		Cluster string `json:"cluster"`
		Masters int    `json:"masters"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if !res.Valid || res.Cluster == "" || res.Masters < 1 {
		t.Fatalf("result = %+v", res)
	}
}

// An invalid plan is a JSON document on stdout too (valid: false), and the
// exit code and the stderr line are those of a failure.
func TestValidateJSONOfAnInvalidPlan(t *testing.T) {
	code, stdout, stderr := execute(t, "validate", "-c", filepath.Join(t.TempDir(), "missing.yaml"), "-o", "json")
	if code != exitError || !strings.Contains(stderr, "[ERROR] invalid configuration") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	var res struct {
		Valid bool   `json:"valid"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if res.Valid || res.Error == "" {
		t.Fatalf("result = %+v", res)
	}
}

func TestPlanJSON(t *testing.T) {
	code, stdout, stderr := execute(t, "plan", "-c", exampleSingleNode, "--output", "json")
	if code != exitOK {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	var res struct {
		Cluster string `json:"cluster"`
		Release struct {
			Version string `json:"version"`
		} `json:"release"`
		Masters []struct {
			Hostname string `json:"hostname"`
		} `json:"masters"`
		Nodes []struct {
			Key    string `json:"key"`
			Status string `json:"status"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if res.Cluster == "" || res.Release.Version == "" || len(res.Masters) == 0 || len(res.Nodes) == 0 {
		t.Fatalf("result = %+v", res)
	}
}

func TestUnknownOutputFormatIsAUsageError(t *testing.T) {
	for _, cmd := range []string{"validate", "plan"} {
		code, stdout, stderr := execute(t, cmd, "-c", exampleSingleNode, "-o", "yaml")
		if code != exitUsage || stdout != "" || !strings.Contains(stderr, "unknown output format") {
			t.Errorf("%s: code=%d stdout=%q stderr=%q", cmd, code, stdout, stderr)
		}
	}
}
