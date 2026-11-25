// Package platform chooses the drivers of a node from its facts: the
// package manager, the init system and the tuning. The drivers live in the
// packages below it and return steps (internal/platform/step).
package platform

import (
	"fmt"

	"github.com/bacanoo-io/norca-deployer/internal/host"
	"github.com/bacanoo-io/norca-deployer/internal/platform/packages"
	"github.com/bacanoo-io/norca-deployer/internal/platform/service"
	"github.com/bacanoo-io/norca-deployer/internal/platform/tuning"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// Drivers are the drivers of one node.
type Drivers struct {
	Packages packages.Manager
	Service  service.Manager
	Tuning   tuning.Tuner
}

// For returns the drivers of a node. A package manager the deployer does
// not drive yet (dnf, apk) gets a driver that refuses packages; an init
// system without a driver (OpenRC) is an error.
func For(f *host.Facts) (*Drivers, error) {
	d := &Drivers{}
	switch f.PackageManager {
	case config.PackageManagerApt:
		d.Packages = packages.Apt{}
	case config.PackageManagerNone:
		d.Packages = packages.None{}
	default:
		d.Packages = packages.Unsupported{Manager: f.PackageManager}
	}
	switch f.Init {
	case host.InitSystemd:
		d.Service = service.Systemd{}
	case host.InitWindows:
		d.Service = service.Windows{}
	default:
		return nil, fmt.Errorf("the init system %s is not supported yet: the deployer runs the agent as a systemd service", f.Init)
	}
	if f.Family == host.FamilyWindows {
		d.Tuning = tuning.Windows{}
	} else {
		d.Tuning = tuning.Linux{}
	}
	return d, nil
}
