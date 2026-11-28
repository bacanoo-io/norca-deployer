// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// experimentalEnv names the experimental command groups that are turned on,
// separated by commas ("tree").
const experimentalEnv = "NORCA_DEPLOYER_EXPERIMENTAL"

// experimental reports whether the experimental feature name is turned on.
func experimental(name string) bool {
	return slices.ContainsFunc(strings.Split(os.Getenv(experimentalEnv), ","), func(f string) bool {
		return strings.TrimSpace(f) == name
	})
}

// newTreeCmd is the command group tree: the objects of a running platform
// from a directory tree, applied as Rigger stacks
// (design/platform-management-design.md). Until its first release it is
// hidden, and refuses to run unless NORCA_DEPLOYER_EXPERIMENTAL names it.
func newTreeCmd() *cobra.Command {
	on := experimental("tree")

	return &cobra.Command{
		Use:    "tree",
		Short:  "Manage the objects of a running platform from a directory tree (experimental)",
		Hidden: !on,
		Long: `Manage the objects of a running Norca platform (namespaces, ACL policies,
jobs, Variables, secrets, objects, ...) from a directory tree, usually a Git
repository. The tree is applied as Rigger stacks. Experimental: turned on
with ` + experimentalEnv + `=tree.`,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if !on {
				return fmt.Errorf("the tree commands are experimental: set %s=tree to use them", experimentalEnv)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
}
