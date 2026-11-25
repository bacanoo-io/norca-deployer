package step

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
)

// Prelude defines the helpers of Apply over SSH: each prints a result line
// that the Executor reads (the install script defines its own, which print
// to the console).
const Prelude = `log() { printf 'log %s\n' "$*"; }
warn() { printf 'warn %s\n' "$*"; }
die() {
  printf 'error %s\n' "$*"
  exit 1
}
`

// The answers of a condition (When, Check) over SSH: a condition that
// cannot be run (the connection broke) gives neither.
const (
	answerYes = "norca-step yes"
	answerNo  = "norca-step no"
	removed   = "norca-removed "
)

// Executor runs steps on a node over a connector: the SSH path of apply.
type Executor struct {
	Conn connector.Connector
	// Host names the node in the log and in the errors.
	Host string
	// Windows copies a local file as <path>.new for the restart to swap
	// in (a running .exe cannot be replaced).
	Windows bool
	// Out receives the log of the steps; nil is os.Stdout.
	Out io.Writer
}

// Result is what a step did on a node.
type Result struct {
	// Changed: a file was written or removed, Apply ran because the check
	// failed, or a result line reported a change.
	Changed bool
	// Restart: the step changed and its change needs the agent restarted.
	Restart bool
	// Output is the output of Apply, for a caller that reads its result
	// lines.
	Output string
}

func (e *Executor) out() io.Writer {
	if e.Out == nil {
		return os.Stdout
	}
	return e.Out
}

// RunAll runs steps in order and stops at the first error. It reports
// whether a step changed something and whether a change needs the agent
// restarted.
func (e *Executor) RunAll(ctx context.Context, steps []Step) (changed, restart bool, err error) {
	for i := range steps {
		res, err := e.Run(ctx, &steps[i])
		if err != nil {
			return changed, restart, err
		}
		changed = changed || res.Changed
		restart = restart || res.Restart
	}
	return changed, restart, nil
}

// Run runs one step. The context is looked at before the step: a command
// that runs is not cut (the connector ends it on an abort).
func (e *Executor) Run(ctx context.Context, s *Step) (*Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", s.Name, err)
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	if s.When != "" {
		ok, err := e.condition(WhenCommand(s))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.Name, err)
		}
		if !ok {
			return &Result{}, nil
		}
	}
	res := &Result{}
	for _, d := range s.Dirs {
		if err := e.Conn.MkdirAll(d.Path, d.Mode); err != nil {
			return nil, fmt.Errorf("%s: failed to create %s: %w", s.Name, d.Path, err)
		}
	}
	for _, f := range s.Files {
		changed, err := e.sync(f)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.Name, err)
		}
		res.Changed = res.Changed || changed
	}
	filesChanged := res.Changed
	if len(s.Remove) > 0 {
		out, err := e.Conn.ExecuteCommand(RemoveCommand(s))
		if err != nil {
			return nil, fmt.Errorf("%s: failed to remove %s: %w", s.Name, strings.Join(s.Remove, ", "), err)
		}
		for line := range strings.SplitSeq(out, "\n") {
			if p, ok := strings.CutPrefix(strings.TrimSpace(line), removed); ok {
				fmt.Fprintf(e.out(), "    -> [%s] %s removed\n", e.Host, p)
				res.Changed = true
			}
		}
	}

	if s.Apply == "" {
		return e.finish(s, res), nil
	}
	// Apply with a check runs when the check fails or a file changed, and
	// is a change then; without a check it runs every time.
	if s.Check != "" && !filesChanged {
		done, err := e.condition(CheckCommand(s))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.Name, err)
		}
		if done {
			return e.finish(s, res), nil
		}
	}
	for _, f := range s.Inputs {
		if _, err := e.sync(f); err != nil {
			return nil, fmt.Errorf("%s: %w", s.Name, err)
		}
	}
	cmd := s.Apply
	if !s.PowerShell {
		cmd = ApplyCommand(s)
	}
	out, runErr := e.Conn.ExecuteCommand(cmd)
	res.Output = out
	lines := parseLines(out)
	for _, l := range lines.logs {
		fmt.Fprintf(e.out(), "    -> [%s] %s\n", e.Host, l)
	}
	for _, w := range lines.warnings {
		fmt.Fprintf(e.out(), "    [WARN] [%s] %s: %s\n", e.Host, s.Name, w)
	}
	switch {
	case s.Lines && len(lines.errors) > 0:
		return res, fmt.Errorf("%s: %s %s", s.Name, e.Host, strings.Join(lines.errors, "; "))
	case runErr != nil && len(lines.errors) > 0:
		return res, fmt.Errorf("%s: %s", s.Name, strings.Join(lines.errors, "; "))
	case runErr != nil:
		return res, fmt.Errorf("%s failed: %w (output: %s)", s.Name, runErr, lastLines(out, 30))
	}
	if s.Check != "" || (s.Lines && lines.changed) {
		res.Changed = true
	}
	return e.finish(s, res), nil
}

func (e *Executor) finish(s *Step, res *Result) *Result {
	res.Restart = res.Changed && s.Restart
	return res
}

// sync writes a file of a step unless the node has it.
func (e *Executor) sync(f File) (bool, error) {
	if f.Local != "" {
		return connector.SyncLocalFile(e.Conn, f.Local, f.SHA256, f.Path, f.Mode, e.Windows)
	}
	return connector.SyncFile(e.Conn, f.Path, f.Content, f.Mode)
}

// condition runs a When or a Check command and reads its answer.
func (e *Executor) condition(cmd string) (bool, error) {
	out, err := e.Conn.ExecuteCommand(cmd)
	if err != nil {
		return false, fmt.Errorf("the condition could not be checked: %w", err)
	}
	for line := range strings.SplitSeq(out, "\n") {
		switch strings.TrimSpace(line) {
		case answerYes:
			return true, nil
		case answerNo:
			return false, nil
		}
	}
	return false, fmt.Errorf("the condition gave no answer (output: %s)", lastLines(out, 10))
}

// header is the first line of a command: the node shows what runs.
func header(kind string, s *Step) string {
	return "# " + kind + ": " + oneLine(s.Name) + "\n"
}

// WhenCommand is the POSIX command of the When of a step.
func WhenCommand(s *Step) string { return conditionCommand("when", s, s.When) }

// CheckCommand is the POSIX command of the Check of a step.
func CheckCommand(s *Step) string { return conditionCommand("check", s, s.Check) }

func conditionCommand(kind string, s *Step, cond string) string {
	return header(kind, s) + UsedFunctions(s.Functions, cond) + "if {\n" + strings.TrimRight(cond, "\n") + "\n}; then echo '" + answerYes +
		"'; else echo '" + answerNo + "'; fi"
}

// RemoveCommand is the POSIX command that removes the files of Remove that
// exist and prints each it removed.
func RemoveCommand(s *Step) string {
	quoted := make([]string, 0, len(s.Remove))
	for _, p := range s.Remove {
		quoted = append(quoted, connector.ShellQuote(p))
	}
	return header("remove", s) + "for f in " + strings.Join(quoted, " ") +
		"; do if [ -e \"$f\" ] || [ -L \"$f\" ]; then rm -f \"$f\" && echo \"" + removed + "$f\"; fi; done"
}

// ApplyCommand is the POSIX command of Apply over SSH: set -eu, the
// helpers and the functions of the step that Apply calls, and Apply.
func ApplyCommand(s *Step) string {
	return header("apply", s) + "set -eu\n" + UsedFunctions(Prelude+s.Functions, s.Apply) + strings.TrimRight(s.Apply, "\n")
}

// lines are the result lines of an Apply.
type lines struct {
	logs, warnings, errors []string
	changed                bool
}

func parseLines(out string) lines {
	var res lines
	for line := range strings.SplitSeq(out, "\n") {
		kind, rest, _ := strings.Cut(strings.TrimRight(line, "\r"), " ")
		switch kind {
		case "log":
			res.logs = append(res.logs, rest)
		case "warn":
			res.warnings = append(res.warnings, rest)
		case "error":
			res.errors = append(res.errors, rest)
		case "changed", "removed", "restarted":
			res.changed = true
		}
	}
	return res
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// oneLine keeps a name on one line (it goes into a comment).
func oneLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}
