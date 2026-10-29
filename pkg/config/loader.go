package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadFromFile reads and validates a deployment plan configuration file:
// YAML (.yaml, .yml) or HCL (.hcl). Both formats use the same keys.
func LoadFromFile(path string) (*DeploymentPlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read configuration file (%s): %w", path, err)
	}

	plan := &DeploymentPlan{}
	ext := strings.ToLower(filepath.Ext(path))

	switch ext {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(data, plan); err != nil {
			return nil, fmt.Errorf("invalid YAML syntax: %w", err)
		}
	case ".hcl":
		if err := decodeHCL(data, plan); err != nil {
			return nil, fmt.Errorf("invalid HCL syntax: %w", err)
		}
	default:
		// Default fallback to YAML parser
		if err := yaml.Unmarshal(data, plan); err != nil {
			return nil, fmt.Errorf("unsupported format or syntax error (%s): %w", ext, err)
		}
	}

	plan.baseDir = filepath.Dir(path)
	plan.SetDefaults()

	if err := plan.Validate(); err != nil {
		return nil, fmt.Errorf("configuration validation error: %w", err)
	}

	return plan, nil
}

// SaveToFile serializes the plan to YAML and writes it to disk. The file is
// private to the user because it can hold SSH and become credentials.
func (p *DeploymentPlan) SaveToFile(path string) error {
	data, err := yaml.Marshal(p)
	if err != nil {
		return fmt.Errorf("failed to serialize YAML: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write configuration file (%s): %w", path, err)
	}
	return nil
}

// SetDefaults populates missing fields with recommended default values.
func (p *DeploymentPlan) SetDefaults() {
	if p.Version == "" {
		p.Version = "norca.io/v1alpha1"
	}
	if p.Kind == "" {
		p.Kind = "DeploymentPlan"
	}
	if p.Metadata.Name == "" {
		p.Metadata.Name = "norca-cluster"
	}
	if p.Metadata.Datacenter == "" {
		p.Metadata.Datacenter = "dc1"
	}
	if p.Metadata.Region == "" {
		p.Metadata.Region = "global"
	}

	// Release defaults
	if p.Release.Version == "" {
		p.Release.Version = "v0.1.0"
	}
	if p.Release.Source == "" {
		p.Release.Source = "official"
	}
	if p.Release.DownloadBaseURL == "" {
		p.Release.DownloadBaseURL = "https://github.com/bacanoo-io/norca/releases/download"
	}
	if p.Release.Source == ReleaseHostPackage && p.Release.Package == "" {
		p.Release.Package = "norca"
	}

	// CNI plugin defaults
	if p.CNI == nil {
		p.CNI = &CNIConfig{}
	}
	if p.CNI.Install == nil {
		install := true
		p.CNI.Install = &install
	}
	if p.CNI.Version == "" {
		p.CNI.Version = DefaultCNIVersion
	}
	if p.CNI.Source == "" {
		p.CNI.Source = ReleaseOfficial
	}
	if p.CNI.DownloadBaseURL == "" {
		p.CNI.DownloadBaseURL = "https://github.com/containernetworking/plugins/releases/download"
	}

	// Rolling restart defaults
	if p.Upgrade.MaxParallelClients <= 0 {
		p.Upgrade.MaxParallelClients = 1
	}
	if p.Upgrade.HealthTimeout <= 0 {
		p.Upgrade.HealthTimeout = 180
	}

	if p.Security.ACL != nil && p.Security.ACL.ManagementTokenOutput == "" {
		p.Security.ACL.ManagementTokenOutput = StateDirName + "/management-token"
	}
	if s := p.Security.PKI; s != nil && s.SPIFFE != nil && s.SPIFFE.Enabled && s.SPIFFE.TrustDomain == "" {
		s.SPIFFE.TrustDomain = strings.ToLower(p.Metadata.Name) + ".norca"
	}
	if p.Packs != nil {
		if p.Packs.Registry.Name != "" && p.Packs.Registry.Type == "" {
			p.Packs.Registry.Type = "git"
		}
		for i := range p.Packs.Registries {
			if p.Packs.Registries[i].Type == "" {
				p.Packs.Registries[i].Type = "git"
			}
		}
	}
	if p.Secrets != nil {
		if p.Secrets.Store == "" {
			p.Secrets.Store = SecretStoreBuiltin
		}
		for i := range p.Secrets.Bootstrap {
			b := &p.Secrets.Bootstrap[i]
			if b.Type == "" {
				b.Type = SecretTypeSecret
			}
			if b.Namespace == "" {
				b.Namespace = "default"
			}
		}
	}
	if a := p.Security.ACL; a != nil {
		for i := range a.Policies {
			if j := a.Policies[i].Job; j != nil && j.Namespace == "" {
				j.Namespace = "default"
			}
		}
	}

	// Network defaults
	if p.Network.Ports.HTTP == 0 {
		p.Network.Ports.HTTP = 4646
	}
	if p.Network.Ports.RPC == 0 {
		p.Network.Ports.RPC = 4647
	}
	if p.Network.Ports.Serf == 0 {
		p.Network.Ports.Serf = 4648
	}
	if p.Network.AdvertiseMode == "" {
		p.Network.AdvertiseMode = "vpn_interface"
	}

	// Platform Path defaults
	if p.Paths.Linux == nil {
		p.Paths.Linux = &PathConfig{}
	}
	p.fillLinuxPathDefaults(p.Paths.Linux)

	if p.Paths.Windows == nil {
		p.Paths.Windows = &PathConfig{}
	}
	p.fillWindowsPathDefaults(p.Paths.Windows)

	// Tuning defaults
	if p.Tuning.CNIMTUMode == "" {
		p.Tuning.CNIMTUMode = "auto"
	}
	if p.Tuning.FileDescriptorLimit == 0 {
		p.Tuning.FileDescriptorLimit = 65536
	}

	// Inventory defaults; client_defaults first, so that the driver default
	// below only applies when neither sets drivers.
	p.applyClientDefaults()
	masterCount := len(p.Inventory.Masters)
	for i := range p.Inventory.Masters {
		m := &p.Inventory.Masters[i]
		if m.Platform == "" {
			m.Platform = "linux"
		}
		if m.Arch == "" {
			m.Arch = "amd64"
		}
		if m.Connection.Mode == "" {
			m.Connection.Mode = "ssh"
		}
		if m.Connection.SSHPort == 0 {
			m.Connection.SSHPort = 22
		}
		if m.Connection.HostKeyChecking == "" {
			m.Connection.HostKeyChecking = HostKeyTOFU
		}
		if m.ServerSettings == nil {
			m.ServerSettings = &ServerSettings{}
		}
		if m.ServerSettings.BootstrapExpect == 0 {
			m.ServerSettings.BootstrapExpect = masterCount
		}
		if m.ServerSettings.RaftMultiplier == 0 {
			m.ServerSettings.RaftMultiplier = 1
		}
		if m.ServerSettings.LogLevel == "" {
			m.ServerSettings.LogLevel = "INFO"
		}
	}

	for i := range p.Inventory.Decommission {
		d := &p.Inventory.Decommission[i]
		if d.Platform == "" {
			d.Platform = "linux"
		}
		if d.Arch == "" {
			d.Arch = "amd64"
		}
		if d.Connection.Mode == "" {
			d.Connection.Mode = "ssh"
		}
		if d.Connection.SSHPort == 0 {
			d.Connection.SSHPort = 22
		}
		if d.Connection.HostKeyChecking == "" {
			d.Connection.HostKeyChecking = HostKeyTOFU
		}
		if d.DrainDeadline <= 0 {
			d.DrainDeadline = 600
		}
	}

	for i := range p.Inventory.Clients {
		c := &p.Inventory.Clients[i]
		if c.Platform == "" {
			c.Platform = "linux"
		}
		if c.Arch == "" {
			c.Arch = "amd64"
		}
		if c.Connection.Mode == "" {
			c.Connection.Mode = "ssh"
		}
		if c.Connection.SSHPort == 0 {
			c.Connection.SSHPort = 22
		}
		if c.Connection.HostKeyChecking == "" {
			c.Connection.HostKeyChecking = HostKeyTOFU
		}
		if c.ClientSettings == nil {
			c.ClientSettings = &ClientSettings{}
		}
		if len(c.ClientSettings.Drivers) == 0 {
			c.ClientSettings.Drivers = []string{"docker", "exec"}
		}
	}
}

func (p *DeploymentPlan) fillLinuxPathDefaults(pc *PathConfig) {
	if pc.BinDir == "" {
		pc.BinDir = "/usr/local/bin"
	}
	if pc.ConfigDir == "" {
		pc.ConfigDir = "/etc/norca.d"
	}
	if pc.DataDir == "" {
		pc.DataDir = "/var/lib/norca"
	}
	if pc.LogDir == "" {
		pc.LogDir = "/var/log/norca"
	}
	if pc.TLSDir == "" {
		pc.TLSDir = "/etc/norca.d/tls"
	}
	if pc.CNIBinDir == "" {
		pc.CNIBinDir = "/opt/cni/bin"
	}
	if pc.CNIConfigDir == "" {
		pc.CNIConfigDir = "/opt/cni/config"
	}
	if pc.AppsDir == "" {
		pc.AppsDir = "/opt/norca/apps"
	}
}

func (p *DeploymentPlan) fillWindowsPathDefaults(pc *PathConfig) {
	if pc.BinDir == "" {
		pc.BinDir = `C:\Program Files\norca\bin`
	}
	if pc.ConfigDir == "" {
		pc.ConfigDir = `C:\ProgramData\norca\config`
	}
	if pc.DataDir == "" {
		pc.DataDir = `C:\ProgramData\norca\data`
	}
	if pc.LogDir == "" {
		pc.LogDir = `C:\ProgramData\norca\logs`
	}
	if pc.TLSDir == "" {
		pc.TLSDir = `C:\ProgramData\norca\config\tls`
	}
	if pc.CNIBinDir == "" {
		pc.CNIBinDir = `C:\Program Files\norca\cni`
	}
	if pc.CNIConfigDir == "" {
		pc.CNIConfigDir = `C:\ProgramData\norca\cni\config`
	}
	if pc.AppsDir == "" {
		pc.AppsDir = `C:\norca\apps`
	}
}

// Validate verifies logical and structural requirements of the plan.
func (p *DeploymentPlan) Validate() error {
	if len(p.Inventory.Masters) == 0 {
		return fmt.Errorf("at least one master (server) node must be specified in the inventory")
	}

	masterCount := len(p.Inventory.Masters)
	for idx, m := range p.Inventory.Masters {
		if m.Host == "" {
			return fmt.Errorf("master[%d]: 'host' field is required", idx)
		}
		if m.ServerSettings != nil && m.ServerSettings.BootstrapExpect != masterCount {
			return fmt.Errorf("master[%s]: bootstrap_expect (%d) does not match total masters count (%d)",
				m.Host, m.ServerSettings.BootstrapExpect, masterCount)
		}
	}

	for idx, c := range p.Inventory.Clients {
		if c.Host == "" {
			return fmt.Errorf("client[%d]: 'host' field is required", idx)
		}
	}

	if err := p.validateBackup(); err != nil {
		return err
	}
	if err := p.validateHostPackages(); err != nil {
		return err
	}
	if err := p.validateScripts(); err != nil {
		return err
	}
	if err := p.validateCluster(); err != nil {
		return err
	}
	if err := p.validateNetwork(); err != nil {
		return err
	}
	if err := p.validateSPIFFE(); err != nil {
		return err
	}
	if err := p.validateNodeSettings(); err != nil {
		return err
	}

	return nil
}

// validateBackup mirrors the checks of the Norca server, which refuses to
// start with an invalid backup block.
func (p *DeploymentPlan) validateBackup() error {
	b := p.Backup
	if b == nil || !b.Enabled {
		return nil
	}
	if b.Destination.Name == "" {
		return fmt.Errorf("backup.destination.name is required")
	}
	switch b.Destination.Type {
	case "", "s3":
		if b.Destination.Bucket == "" {
			return fmt.Errorf("backup.destination.bucket is required for an s3 destination")
		}
	case "directory":
		if !strings.HasPrefix(b.Destination.Path, "/") && !filepath.IsAbs(b.Destination.Path) {
			return fmt.Errorf("backup.destination.path must be an absolute path for a directory destination")
		}
	default:
		return fmt.Errorf("backup.destination.type %q must be s3 or directory", b.Destination.Type)
	}
	if len(b.Encryption.Recipients) == 0 {
		return fmt.Errorf("backup.encryption.recipients: at least one age recipient is required (Norca encrypts every backup)")
	}
	return nil
}
