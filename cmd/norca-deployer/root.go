package main

import (
	"github.com/spf13/cobra"
)

var (
	configFile    string
	secretKey     string
	secretKeyFile string
	force         bool
	forceRestart  bool
	version       = "v1.6.0"
)

// newRootCmd builds the command tree. It does not run anything, so a test can
// build it, give it arguments and writers, and look at what comes out.
func newRootCmd() *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "norca-deployer",
		Short: "Idempotent, declarative Norca cluster deployer",
		// The errors are printed once, by run(), to stderr; the usage text is
		// for --help and for a wrong command line only.
		SilenceUsage:  true,
		SilenceErrors: true,
		// No Args here: with Args set cobra stops checking for an unknown
		// command, and a mistyped one would exit with 0 and the help text.
	}
	rootCmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })

	rootCmd.PersistentFlags().StringVarP(&configFile, "config", "c", "cluster-deploy.yaml", "Path to deployment configuration file")
	rootCmd.PersistentFlags().StringVar(&secretKey, "secret-key", "", "Master encryption key string")
	rootCmd.PersistentFlags().StringVar(&secretKeyFile, "secret-key-file", "", "Path to master encryption key file")

	rootCmd.AddCommand(newInitCmd(), newValidateCmd(), newPlanCmd(), newApplyCmd(), newSecretCmd(), newPkiCmd(), newGitOpsCmd(), newVersionCmd())
	return rootCmd
}
