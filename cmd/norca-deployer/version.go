package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newVersionCmd builds the `version` command.
func newVersionCmd() *cobra.Command {
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print Norca Deployer version",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Norca Deployer %s (Nomad v1.6.5 compatible orchestrator deployment)\n", version)
			return nil
		},
	}

	return versionCmd
}
