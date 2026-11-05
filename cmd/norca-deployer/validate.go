package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca-deployer/internal/config"
)

// newValidateCmd builds the `validate` command.
func newValidateCmd() *cobra.Command {
	validateCmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate configuration syntax and logical constraints",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			plan, err := config.LoadFromFile(configFile)
			if err != nil {
				return fmt.Errorf("invalid configuration: %w", err)
			}
			fmt.Fprintf(out, "[OK] Configuration is valid! (cluster: %s, masters: %d, clients: %d)\n",
				plan.Metadata.Name, len(plan.Inventory.Masters), len(plan.Inventory.Clients))
			return nil
		},
	}

	return validateCmd
}
