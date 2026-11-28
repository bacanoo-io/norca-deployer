// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"strings"
	"testing"
)

// The tree commands are experimental: without NORCA_DEPLOYER_EXPERIMENTAL
// naming them they refuse to run and are left out of the help; with it they
// show their help.
func TestTreeCommandIsExperimental(t *testing.T) {
	tests := []struct {
		name       string
		env        string
		code       int
		inStdout   string
		inStderr   string
		rootListed bool
	}{
		{name: "off", env: "", code: exitError, inStderr: "the tree commands are experimental: set NORCA_DEPLOYER_EXPERIMENTAL=tree"},
		{name: "another feature", env: "other", code: exitError, inStderr: "experimental"},
		{name: "on", env: "other, tree", code: exitOK, inStdout: "applied as Rigger stacks", rootListed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(experimentalEnv, tt.env)

			code, stdout, stderr := execute(t, "tree")
			if code != tt.code || !strings.Contains(stdout, tt.inStdout) || !strings.Contains(stderr, tt.inStderr) {
				t.Fatalf("tree = %d, stdout %q, stderr %q", code, stdout, stderr)
			}

			_, help, _ := execute(t, "--help")
			if listed := strings.Contains(help, "\n  tree "); listed != tt.rootListed {
				t.Fatalf("tree listed in the help: %v, want %v", listed, tt.rootListed)
			}
		})
	}
}
