package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca-deployer/internal/config"
	"github.com/bacanoo-io/norca-deployer/internal/engine"
)

// newApplyCmd builds the `apply` command.
func newApplyCmd() *cobra.Command {
	applyCmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply configuration and provision cluster nodes idempotently",
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, err := config.LoadFromFile(configFile)
			if err != nil {
				return fmt.Errorf("failed to load configuration: %w", err)
			}

			runner := engine.NewRunner(plan, vaultIfAvailable())
			runner.ForceRestart = forceRestart
			if err := runner.Apply(); err != nil {
				return fmt.Errorf("deployment failed: %w", err)
			}
			return nil
		},
	}

	applyCmd.Flags().BoolVar(&forceRestart, "force-restart", false, "Restart every agent (rolling), even when nothing changed")

	return applyCmd
}
