//go:build !unix

package connector

import "os/exec"

// killGroup leaves the abort of a command as exec does it: PowerShell is
// killed, and WaitDelay ends the wait for its output.
func killGroup(*exec.Cmd) {}
