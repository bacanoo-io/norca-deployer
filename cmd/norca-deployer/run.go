package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

// Exit codes. 1 is a failure while the command ran; 2 is a command line the
// tool could not use (an unknown command or flag, a wrong number of
// arguments), the Unix convention scripts expect.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

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
	fmt.Fprintf(stderr, "[ERROR] %v\n", err)
	if isUsageError(err) {
		fmt.Fprintf(stderr, "Run '%s --help' for usage.\n", root.Name())
		return exitUsage
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
