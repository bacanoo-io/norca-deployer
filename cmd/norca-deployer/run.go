package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca-deployer/internal/tree"
)

// Exit codes. 1 is a failure while the command ran; 2 is a command line the
// tool could not use (an unknown command or flag, a wrong number of
// arguments), the Unix convention scripts expect.
//
// 130 is 128 plus SIGINT: the command was interrupted and stopped between two
// steps, the usual code for that.
//
// The tree commands add two (design/platform-management-design.md §10): 2
// is also "changes pending" of tree plan --detailed-exitcode, as
// terraform plan -detailed-exitcode has it (a usage error prints its help,
// a plan does not), and 3 is a run that a guard blocked: blocked, not
// failed.
const (
	exitOK          = 0
	exitError       = 1
	exitUsage       = 2
	exitChanges     = 2
	exitBlocked     = 3
	exitInterrupted = 130
)

// errChangesPending ends tree plan --detailed-exitcode when the plan has
// changes: exit code 2, and no error printed.
var errChangesPending = errors.New("changes pending")

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command line and returns the exit code. The commands
// return their errors; this is the one place that prints them (to stderr,
// where diagnostics belong) and decides the code.
func run(args []string, stdout, stderr io.Writer) int {
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.Execute()
	if err == nil {
		return exitOK
	}
	if errors.Is(err, errChangesPending) {
		return exitChanges
	}
	if _, ok := errors.AsType[silentBlocked](err); ok {
		return exitBlocked
	}
	if _, ok := errors.AsType[silentError](err); ok {
		return exitError
	}
	fmt.Fprintf(stderr, "[ERROR] %v\n", err)
	code := exitCode(err)
	if code == exitUsage {
		fmt.Fprintf(stderr, "Run '%s --help' for usage.\n", root.Name())
	}
	return code
}

// exitCode is the code for the error of a command.
func exitCode(err error) int {
	switch {
	case isUsageError(err):
		return exitUsage
	case errors.Is(err, context.Canceled):
		return exitInterrupted
	case errors.Is(err, tree.ErrBlocked):
		return exitBlocked
	}
	return exitError
}

// usageError marks a problem with the command line itself.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

// usageArgs wraps an argument validator so that its error is a usage error.
func usageArgs(v cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := v(cmd, args); err != nil {
			return usageError{err}
		}
		return nil
	}
}

func isUsageError(err error) bool {
	if _, ok := errors.AsType[usageError](err); ok {
		return true
	}
	// cobra reports these two without a hook for them.
	msg := err.Error()
	return strings.HasPrefix(msg, "unknown command") || strings.HasPrefix(msg, "required flag(s)")
}
