// Package step is the model of the changes the deployer makes on a host:
// idempotent steps, each with its directories and files, a check and a
// change. The drivers of packages, service, tuning, directories, binary and
// CNI return steps; the SSH path runs them with an Executor, and the install
// script of a scaled machine renders them into POSIX sh with Render. So both
// paths share one implementation.
//
// Every POSIX command and script of a step must be POSIX sh, without bash
// features: it runs in dash, bash and busybox ash.
package step

import (
	"fmt"
	"os"
	"slices"
)

// Step is one idempotent change of a host.
//
// The order of a step: When decides whether the step applies to the node;
// the Dirs are created, the Files written when their content differs and
// the files of Remove removed; then Apply runs when Check fails or a File
// changed (Check empty: Apply runs every time and is no change by itself).
type Step struct {
	// Name is the step in the log and in the errors: "apt repository
	// docker", "systemd unit".
	Name string
	// When, when set, is a POSIX condition: the step applies to the node
	// only when it is true (nothing is written, nothing runs otherwise).
	When string
	// Dirs are created (with their mode) before the files.
	Dirs []Dir
	// Files are written when their content (or digest) differs.
	Files []File
	// Remove are files that an earlier plan wrote and this one does not:
	// removed when they exist, which counts as a change.
	Remove []string
	// Check is a POSIX condition: true means nothing is to be done. Empty:
	// Apply runs every time.
	Check string
	// Inputs are files Apply reads (an archive to unpack): copied only when
	// Apply runs.
	Inputs []File
	// Apply is the change, a POSIX script that runs with set -eu and the
	// helpers log, warn and die.
	Apply string
	// Functions are POSIX sh functions that Check and Apply call; a script
	// holds each set of functions once.
	Functions string
	// Lines says that Apply reports its changes by result lines (changed,
	// removed, restarted, warn, error, unchanged, each with its subject);
	// the error lines are sentences whose subject is the node.
	Lines bool
	// Restart marks a step whose change needs the agent restarted (the
	// binary, the unit, the configuration).
	Restart bool
	// Secret marks a step that carries a credential: the install script of
	// a scaled machine refuses it at boot (the user data is not a place for
	// secrets).
	Secret bool
	// PowerShell marks a step of a Windows node: Apply is PowerShell, and
	// When, Check, Remove and Inputs are not used.
	PowerShell bool
}

// Dir is a directory of a step.
type Dir struct {
	Path string
	Mode os.FileMode
}

// File is a file of a step: Content, or a file of the deployer machine
// (Local, with its SHA256) that is copied when the node has another one.
type File struct {
	Path    string
	Mode    os.FileMode
	Content []byte
	// Local is a file on the deployer machine (a .deb, the norca binary);
	// SHA256 is its digest. The install script of a scaled machine cannot
	// reach it: Render refuses it.
	Local  string
	SHA256 string
}

// validate checks the parts of a step that do not go together.
func (s *Step) validate() error {
	if s.Name == "" {
		return fmt.Errorf("a step without a name")
	}
	if s.PowerShell && (s.When != "" || s.Check != "" || len(s.Remove) > 0 || len(s.Inputs) > 0 || s.Functions != "") {
		return fmt.Errorf("step %q: a PowerShell step has only directories, files and Apply", s.Name)
	}
	if s.Lines && s.Check != "" {
		return fmt.Errorf("step %q: a step with result lines runs every time (no Check)", s.Name)
	}
	if len(s.Inputs) > 0 && s.Apply == "" {
		return fmt.Errorf("step %q: inputs without Apply", s.Name)
	}
	for _, f := range slices.Concat(s.Files, s.Inputs) {
		if f.Path == "" {
			return fmt.Errorf("step %q: a file without a path", s.Name)
		}
		if f.Local != "" && f.Content != nil {
			return fmt.Errorf("step %q: %s has content and a local file", s.Name, f.Path)
		}
	}
	return nil
}

// Paths returns the paths of the files a step writes, for a test or a
// listing.
func (s *Step) Paths() []string {
	res := make([]string, 0, len(s.Files))
	for _, f := range s.Files {
		res = append(res, f.Path)
	}
	return res
}
