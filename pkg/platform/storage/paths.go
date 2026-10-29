package storage

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
)

// ResolvedPaths represents the evaluated paths for a specific node and platform.
type ResolvedPaths struct {
	Platform     string
	BinDir       string
	ConfigDir    string
	DataDir      string
	LogDir       string
	TLSDir       string
	CNIBinDir    string
	CNIConfigDir string
	AppsDir      string

	// Native application volume paths
	AppsBinDir    string
	AppsWorkDir   string
	AppsConfigDir string
	AppsDataDir   string
}

// ResolveNodePaths calculates concrete filesystem paths for a given node.
func ResolveNodePaths(node *config.NodeConfig, plan *config.DeploymentPlan) *ResolvedPaths {
	isWin := strings.ToLower(node.Platform) == "windows"

	var base *config.PathConfig
	if isWin {
		base = plan.Paths.Windows
	} else {
		base = plan.Paths.Linux
	}

	rp := &ResolvedPaths{
		Platform: node.Platform,
	}

	// Inherit defaults
	if base != nil {
		rp.BinDir = base.BinDir
		rp.ConfigDir = base.ConfigDir
		rp.DataDir = base.DataDir
		rp.LogDir = base.LogDir
		rp.TLSDir = base.TLSDir
		rp.CNIBinDir = base.CNIBinDir
		rp.CNIConfigDir = base.CNIConfigDir
		rp.AppsDir = base.AppsDir
	}

	// Apply node-specific overrides
	if node.PathOverrides != nil {
		if node.PathOverrides.BinDir != "" {
			rp.BinDir = node.PathOverrides.BinDir
		}
		if node.PathOverrides.ConfigDir != "" {
			rp.ConfigDir = node.PathOverrides.ConfigDir
		}
		if node.PathOverrides.DataDir != "" {
			rp.DataDir = node.PathOverrides.DataDir
		}
		if node.PathOverrides.LogDir != "" {
			rp.LogDir = node.PathOverrides.LogDir
		}
		if node.PathOverrides.TLSDir != "" {
			rp.TLSDir = node.PathOverrides.TLSDir
		}
		if node.PathOverrides.CNIBinDir != "" {
			rp.CNIBinDir = node.PathOverrides.CNIBinDir
		}
		if node.PathOverrides.CNIConfigDir != "" {
			rp.CNIConfigDir = node.PathOverrides.CNIConfigDir
		}
		if node.PathOverrides.AppsDir != "" {
			rp.AppsDir = node.PathOverrides.AppsDir
		}
	}

	// Build native application subdirectories
	if isWin {
		rp.AppsBinDir = filepath.Join(rp.AppsDir, "bin")
		rp.AppsWorkDir = filepath.Join(rp.AppsDir, "work")
		rp.AppsConfigDir = filepath.Join(rp.AppsDir, "config")
		rp.AppsDataDir = filepath.Join(rp.AppsDir, "data")
	} else {
		rp.AppsBinDir = rp.AppsDir + "/bin"
		rp.AppsWorkDir = rp.AppsDir + "/work"
		rp.AppsConfigDir = rp.AppsDir + "/config"
		rp.AppsDataDir = rp.AppsDir + "/data"
	}

	return rp
}

// GenerateHostVolumeBlocks produces HCL host_volume blocks for norca.hcl.
func (rp *ResolvedPaths) GenerateHostVolumeBlocks(client *config.ClientSettings) string {
	var sb strings.Builder

	// 1. Native application predefined volumes
	if client != nil && client.NativeAppVolumes != nil && client.NativeAppVolumes.EnableDefaults {
		sb.WriteString(fmt.Sprintf(`
  host_volume "native-apps-bin" {
    path      = %q
    read_only = false
  }

  host_volume "native-apps-work" {
    path      = %q
    read_only = false
  }

  host_volume "native-apps-config" {
    path      = %q
    read_only = true
  }

  host_volume "native-apps-data" {
    path      = %q
    read_only = false
  }
`, rp.AppsBinDir, rp.AppsWorkDir, rp.AppsConfigDir, rp.AppsDataDir))
	}

	// 2. Custom host volume definitions
	if client != nil {
		for _, hv := range client.CustomHostVolumes {
			sb.WriteString(fmt.Sprintf(`
  host_volume %q {
    path      = %q
    read_only = %t
  }
`, hv.Name, hv.Path, hv.ReadOnly))
		}
	}

	return sb.String()
}
