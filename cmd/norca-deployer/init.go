package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
)

// newInitCmd builds the `init` command.
func newInitCmd() *cobra.Command {
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Generate a sample deployment configuration file",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			plan := &config.DeploymentPlan{
				Metadata: config.MetadataConfig{
					Name:       "norca-demo-cluster",
					Datacenter: "dc1",
					Region:     "global",
				},
				Release: config.ReleaseConfig{
					Version: "v0.1.0",
				},
				Network: config.NetworkConfig{
					BindAddress:     "0.0.0.0",
					InterfacePrefix: "wg",
					VPN: &config.VPNConfig{
						Enabled:         true,
						InterfacePrefix: "wg",
					},
				},
				Security: config.SecurityConfig{
					TLSEnabled:               true,
					AutoGenerateCertificates: true,
					VerifyServerHostname:     true,
					GossipEncryptionKey:      "AUTO_GENERATE",
					PKI: &config.PKIConfig{
						GenerateWorkerPoolCert: true,
					},
					ACL: &config.ACLConfig{
						Enabled:       true,
						AutoBootstrap: true,
					},
				},
				Inventory: config.InventoryConfig{
					Masters: []config.NodeConfig{
						{
							Host:     "127.0.0.1",
							Hostname: "norca-master-01",
							Platform: "linux",
							Arch:     "amd64",
							Connection: config.ConnectionConfig{
								Mode: "local",
							},
						},
					},
				},
			}
			plan.SetDefaults()
			target := configFile
			if err := refuseOverwrite(target); err != nil {
				return err
			}
			if err := plan.SaveToFile(target); err != nil {
				return fmt.Errorf("error saving configuration file: %w", err)
			}
			fmt.Fprintf(out, "Sample deployment configuration successfully created: %s\n", target)
			return nil
		},
	}

	initCmd.Flags().BoolVar(&force, "force", false, "Overwrite an existing configuration file")

	return initCmd
}

// refuseOverwrite protects an existing file unless --force is given.
func refuseOverwrite(path string) error {
	if force {
		return nil
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists (use --force to overwrite)", path)
	}
	return nil
}
