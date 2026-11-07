package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca-deployer/internal/artifacts"
	"github.com/bacanoo-io/norca-deployer/internal/engine"
	"github.com/bacanoo-io/norca-deployer/internal/state"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// newNodeScalingCmd builds the `node-scaling` command group (install-script).
func newNodeScalingCmd() *cobra.Command {
	nsCmd := &cobra.Command{
		Use:   "node-scaling",
		Short: "Prepare the machines of the node scaling operator",
	}
	var arch string
	scriptCmd := &cobra.Command{
		Use:   "install-script",
		Short: "Print the install script of a scaled machine (bash)",
		Long: "Prints a bash script that installs what a machine of the node scaling operator needs before\n" +
			"its bootstrap runs: the norca binary of the release, the CNI plugins, norca-join\n" +
			"(node_scaling.machine.join_download), the directories, the systemd unit of the agent (enabled,\n" +
			"not started), and the host packages and the sysctl tuning of a client. Run it when the image of\n" +
			"the provider is built (node_scaling.machine.install: image); with install: boot, apply writes it\n" +
			"into the bootstrap Variable and the bootstrap runs it at the first boot.\n" +
			"The machine downloads the files itself; the deployer only resolves the URLs and the SHA-256\n" +
			"digests (it downloads the checksum file of a release without a checksum in the plan).\n" +
			"A second run of the script changes nothing.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, err := config.LoadFromFile(configFile)
			if err != nil {
				return fmt.Errorf("invalid configuration: %w", err)
			}
			vault := vaultIfAvailable()
			res := artifacts.NewResolver(plan, state.OpenReadOnly(plan, vault).CacheDir())
			// stdout is the script: the downloads of checksum files are
			// reported on stderr.
			res.SetLog(cmd.ErrOrStderr())
			script, err := engine.GenerateInstallScript(cmd.Context(), plan, res, engine.MachineScriptOptions{Arch: arch, Vault: vault})
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), script)
			return err
		},
	}
	scriptCmd.Flags().StringVar(&arch, "arch", "", "Architecture of the machines: amd64, arm64, arm or 386 (default: node_scaling.machine.arch, amd64)")
	nsCmd.AddCommand(scriptCmd)
	return nsCmd
}
