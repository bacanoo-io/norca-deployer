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

// For returns the drivers of a node: apt, dnf or apk; systemd or OpenRC
// (and their tuning); the service manager of Windows. A package manager the
// deployer does not drive gets a driver that refuses packages; an init
// system without a driver is an error.
func For(f *host.Facts) (*Drivers, error) {
	d := &Drivers{}
	switch f.PackageManager {
	case config.PackageManagerApt:
		d.Packages = packages.Apt{}
	case config.PackageManagerDnf:
		d.Packages = packages.Dnf{}
	case config.PackageManagerApk:
		d.Packages = packages.Apk{}
	case config.PackageManagerNone:
		d.Packages = packages.None{}
	default:
		d.Packages = packages.Unsupported{Manager: f.PackageManager}
	}
	switch f.Init {
	case host.InitSystemd:
		d.Service, d.Tuning = service.Systemd{}, tuning.Linux{}
	case host.InitOpenRC:
		d.Service, d.Tuning = service.OpenRC{}, tuning.OpenRC{}
	case host.InitWindows:
		d.Service, d.Tuning = service.Windows{}, tuning.Windows{}
	default:
		return nil, fmt.Errorf("the init system %s is not supported: the deployer runs the agent as a service of systemd or OpenRC", f.Init)
	}
	return d, nil
}
