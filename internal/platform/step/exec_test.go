package step

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
)

// localHost runs the steps on this machine below a directory: the local
// connector runs the POSIX commands in sh, as on a node.
func localHost(t *testing.T) (*Executor, string, *bytes.Buffer) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the steps run in sh")
	}
	var out bytes.Buffer
	return &Executor{Conn: connector.NewLocalConnector(t.Context()), Host: "10.0.0.1", Out: &out}, t.TempDir(), &out
}

func q(s string) string { return connector.ShellQuote(s) }

// unitStep writes a file and runs Apply when the marker of Apply is
// missing: like a unit and its enable.
func unitStep(dir, content string) Step {
	marker := filepath.Join(dir, "enabled")
	return Step{
		Name:    "unit",
		Dirs:    []Dir{{Path: filepath.Join(dir, "etc"), Mode: 0o755}},
		Files:   []File{{Path: filepath.Join(dir, "etc", "unit"), Mode: 0o644, Content: []byte(content)}},
		Check:   "[ -f " + q(marker) + " ]",
		Apply:   "touch " + q(marker) + "\nlog unit enabled",
		Restart: true,
	}
}

func TestExecutorSecondRunChangesNothing(t *testing.T) {
	e, dir, out := localHost(t)
	s := unitStep(dir, "v1\n")
	res, err := e.Run(t.Context(), &s)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || !res.Restart || !strings.Contains(out.String(), "    -> [10.0.0.1] unit enabled\n") {
		t.Fatalf("first run: %+v, log %q", res, out)
	}
	res, err = e.Run(t.Context(), &s)
	if err != nil || res.Changed || res.Restart {
		t.Fatalf("second run: %+v, %v", res, err)
	}
	// A changed file runs Apply again although the check passes, and marks
	// the restart.
	s = unitStep(dir, "v2\n")
	out.Reset()
	res, err = e.Run(t.Context(), &s)
	if err != nil || !res.Changed || !res.Restart || !strings.Contains(out.String(), "unit enabled") {
		t.Fatalf("changed file: %+v, %v, log %q", res, err, out)
	}
}

func TestExecutorWhenAndRemove(t *testing.T) {
	e, dir, out := localHost(t)
	stale := filepath.Join(dir, "stale.conf")
	if err := os.WriteFile(stale, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Step{Name: "cleanup", When: "[ -e " + q(stale) + " ]", Remove: []string{stale, filepath.Join(dir, "never")}}
	res, err := e.Run(t.Context(), &s)
	if err != nil || !res.Changed {
		t.Fatalf("remove: %+v, %v", res, err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) || !strings.Contains(out.String(), stale+" removed") {
		t.Fatalf("the file is still there (%v), log %q", err, out)
	}
	// The condition is false now: nothing runs.
	s.Files = []File{{Path: filepath.Join(dir, "written"), Mode: 0o644, Content: []byte("x\n")}}
	res, err = e.Run(t.Context(), &s)
	if err != nil || res.Changed {
		t.Fatalf("when false: %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "written")); err == nil {
		t.Fatal("a step whose condition is false wrote a file")
	}
}

// A step with result lines is changed by its lines; its error lines are
// sentences whose subject is the node.
func TestExecutorLines(t *testing.T) {
	e, _, out := localHost(t)
	s := Step{Name: "consul.dns_forwarding", Lines: true, Apply: "echo 'method systemd-resolved'; echo 'changed /etc/x'; echo 'warn no upstream'"}
	res, err := e.Run(t.Context(), &s)
	if err != nil || !res.Changed || !strings.Contains(res.Output, "method systemd-resolved") {
		t.Fatalf("lines: %+v, %v", res, err)
	}
	if !strings.Contains(out.String(), "    [WARN] [10.0.0.1] consul.dns_forwarding: no upstream\n") {
		t.Fatalf("warning not printed: %q", out)
	}
	s.Apply = "echo 'unchanged /etc/x'"
	if res, err := e.Run(t.Context(), &s); err != nil || res.Changed {
		t.Fatalf("unchanged: %+v, %v", res, err)
	}
	s.Apply = "echo 'error has no local resolver'; exit 1"
	if _, err := e.Run(t.Context(), &s); err == nil || err.Error() != "consul.dns_forwarding: 10.0.0.1 has no local resolver" {
		t.Fatalf("error lines: %v", err)
	}
}

func TestExecutorErrors(t *testing.T) {
	e, _, _ := localHost(t)
	s := Step{Name: "apt packages", Apply: "echo 'E: Unable to locate package nothere'\nexit 100"}
	if _, err := e.Run(t.Context(), &s); err == nil || !strings.Contains(err.Error(), "apt packages failed") ||
		!strings.Contains(err.Error(), "Unable to locate package nothere") {
		t.Fatalf("a failed apply: %v", err)
	}
	s = Step{Name: "version check", Apply: "die 'package jq: version 1.6 installed, 1.7 requested'"}
	if _, err := e.Run(t.Context(), &s); err == nil || err.Error() != "version check: package jq: version 1.6 installed, 1.7 requested" {
		t.Fatalf("die: %v", err)
	}
	// set -u: an unset variable stops Apply.
	s = Step{Name: "unset", Apply: "echo \"$not_set\""}
	if _, err := e.Run(t.Context(), &s); err == nil {
		t.Fatal("an unset variable passed")
	}

	// A condition that gives no answer (the connection broke) is an error,
	// not a passed or failed check.
	fake := connector.NewFakeConnector()
	fe := &Executor{Conn: fake, Host: "10.0.0.2", Out: &bytes.Buffer{}}
	s = Step{Name: "with check", Check: "true", Apply: "true"}
	if _, err := fe.Run(t.Context(), &s); err == nil || !strings.Contains(err.Error(), "gave no answer") {
		t.Fatalf("no answer: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := fe.Run(ctx, &s); !errors.Is(err, context.Canceled) || len(fake.Commands) != 1 {
		t.Fatalf("aborted: %v, commands %q", err, fake.Commands)
	}

	bad := Step{Name: "ps", PowerShell: true, Check: "x"}
	if _, err := fe.Run(t.Context(), &bad); err == nil {
		t.Fatal("a PowerShell step with a check")
	}
}

// RunAll stops at the first error and collects the restart.
func TestRunAll(t *testing.T) {
	e, dir, _ := localHost(t)
	steps := []Step{unitStep(dir, "v1\n"), {Name: "fails", Apply: "false"}, {Name: "never", Apply: "touch " + q(filepath.Join(dir, "never"))}}
	changed, restart, err := e.RunAll(t.Context(), steps)
	if err == nil || !changed || !restart {
		t.Fatalf("RunAll: %t %t %v", changed, restart, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "never")); err == nil {
		t.Fatal("a step after the error ran")
	}
}
