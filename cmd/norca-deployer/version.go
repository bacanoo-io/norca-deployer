package main

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version is set at build time (go build -ldflags "-X main.version=v1.2.3",
// which `make build` does from the git tag). Without it the version comes from
// the build information of the binary.
var version string

// buildVersion returns the version to show: the one set by the linker, else
// the module version of `go install ...@vX.Y.Z`, else the commit the binary
// was built from, else "dev".
func buildVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if rev != "" {
		return "dev-" + rev + dirty
	}
	return "dev"
}

// newVersionCmd builds the `version` command.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print Norca Deployer version",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "Norca Deployer %s (Nomad v1.6.5 compatible orchestrator deployment)\n", buildVersion())
			return nil
		},
	}
}
