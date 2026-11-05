package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca-deployer/internal/config"
)

// newValidateCmd builds the `validate` command.
func newValidateCmd() *cobra.Command {
	var format *string // set below, read when the command runs
	validateCmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate configuration syntax and logical constraints",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := checkFormat(*format); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			plan, err := config.LoadFromFile(configFile)
			if err != nil {
				if *format == formatJSON {
					_ = writeJSON(out, validateResult{Error: err.Error()})
				}
				return fmt.Errorf("invalid configuration: %w", err)
			}
			if *format == formatJSON {
				return writeJSON(out, validateResult{
					Valid: true, Cluster: plan.Metadata.Name, Region: plan.Metadata.Region,
					Masters: len(plan.Inventory.Masters), Clients: len(plan.Inventory.Clients),
				})
			}
			fmt.Fprintf(out, "[OK] Configuration is valid! (cluster: %s, masters: %d, clients: %d)\n",
				plan.Metadata.Name, len(plan.Inventory.Masters), len(plan.Inventory.Clients))
			return nil
		},
	}
	format = addOutputFlag(validateCmd)

	return validateCmd
}
