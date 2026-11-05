package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca-deployer/internal/config"
	"github.com/bacanoo-io/norca-deployer/internal/engine"
)

// newPkiCmd builds the `pki` command group (issue-intermediate, issue-node-ca).
func newPkiCmd() *cobra.Command {
	pkiCmd := &cobra.Command{
		Use:   "pki",
		Short: "Manage certificates of the cluster CA",
	}
	inter := engine.IntermediateOptions{}
	interCmd := &cobra.Command{
		Use:   "issue-intermediate",
		Short: "Issue an intermediate CA signed by the cluster CA (e.g. for OpenZiti)",
		Long: "Issues an intermediate CA signed by the cluster CA, with the SPIFFE trust domain of the plan\n" +
			"as URI SAN, into certs/<name>-ca.pem, <name>-ca-key.pem and <name>-ca-chain.pem.\n" +
			"A PKI that must chain up to the cluster CA (OpenZiti controllers and routers) signs with it.",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			plan, err := config.LoadFromFile(configFile)
			if err != nil {
				return err
			}
			inter.Force = force
			files, err := engine.IssueIntermediate(plan, inter)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "[OK] Intermediate CA %q issued:\n", inter.Name)
			for _, f := range files {
				fmt.Fprintf(out, "  %s\n", f)
			}
			return nil
		},
	}
	interCmd.Flags().StringVar(&inter.Name, "name", "", "Name of the intermediate, e.g. ziti (required)")
	interCmd.Flags().IntVar(&inter.Years, "years", 5, "Validity in years")
	interCmd.Flags().IntVar(&inter.MaxPathLen, "max-path-len", 1, "CA levels allowed below the intermediate")
	interCmd.Flags().BoolVar(&force, "force", false, "Replace an existing intermediate of the same name")
	_ = interCmd.MarkFlagRequired("name")
	pkiCmd.AddCommand(interCmd)

	nodeCA := engine.NodeCAOptions{}
	nodeCACmd := &cobra.Command{
		Use:   "issue-node-ca",
		Short: "Issue the name-constrained intermediate CA of a region for nodes that join by themselves",
		Long: "Issues an intermediate CA signed by the cluster CA that may only sign client certificates of\n" +
			"one region (client.<region>.norca, localhost, 127.0.0.1 and URIs of the SPIFFE trust domain),\n" +
			"into certs/node-ca-<region>.pem, node-ca-<region>-key.pem and node-ca-<region>-chain.pem.\n" +
			"An operator that creates nodes (autoscaling) signs their certificates with it, so the key of\n" +
			"the cluster CA stays with the deployer. It cannot sign a server certificate.\n" +
			"There is no revocation: the validity of the node CA (180 days by default, --days) is the bound\n" +
			"on what a stolen key is worth, and it cannot reach beyond the cluster CA.",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			plan, err := config.LoadFromFile(configFile)
			if err != nil {
				return err
			}
			nodeCA.Force = force
			files, err := engine.IssueNodeCA(plan, nodeCA)
			if err != nil {
				return err
			}
			fmt.Fprintln(out, "[OK] Node CA issued:")
			for _, f := range files {
				fmt.Fprintf(out, "  %s\n", f)
			}
			return nil
		},
	}
	nodeCACmd.Flags().StringVar(&nodeCA.Region, "region", "", "Region of the nodes (default: metadata.region of the plan)")
	nodeCACmd.Flags().IntVar(&nodeCA.Days, "days", 0, fmt.Sprintf("Validity in days (default %d if neither --days nor --years is given)", engine.DefaultNodeCADays))
	nodeCACmd.Flags().IntVar(&nodeCA.Years, "years", 0, "Validity in years (instead of --days)")
	nodeCACmd.Flags().BoolVar(&force, "force", false, "Replace an existing node CA of the region")
	pkiCmd.AddCommand(nodeCACmd)

	return pkiCmd
}
