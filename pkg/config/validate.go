package config

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

const (
	// StateDirName holds the local state of the deployer next to the
	// configuration file: gossip key, management token, known_hosts and the
	// download cache.
	StateDirName = ".norca-deployer"

	DefaultCNIVersion = "v1.9.1"

	// GossipAutoGenerate generates the gossip key once and keeps it in the
	// state directory.
	GossipAutoGenerate = "AUTO_GENERATE"
)

// SSH host key checking modes.
const (
	HostKeyTOFU   = "tofu"
	HostKeyStrict = "strict"
	HostKeyOff    = "off"
)

var (
	releaseNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	versionTagRe  = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$`)
	sha256Re      = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// validateCluster checks the inventory, release, CNI, security and packs
// settings that the original validation did not cover.
func (p *DeploymentPlan) validateCluster() error {
	seen := map[string]string{}
	check := func(role string, idx int, n *NodeConfig) error {
		what := fmt.Sprintf("%s[%d] (%s)", role, idx, n.Host)
		switch n.Platform {
		case "linux", "windows":
		default:
			return fmt.Errorf("%s: platform %q must be linux or windows", what, n.Platform)
		}
		switch n.Arch {
		case "amd64", "arm64", "arm", "386":
		default:
			return fmt.Errorf("%s: arch %q must be amd64, arm64, arm or 386", what, n.Arch)
		}
		switch n.Connection.Mode {
		case "local", "ssh":
		default:
			return fmt.Errorf("%s: connection.mode %q must be local or ssh", what, n.Connection.Mode)
		}
		switch n.Connection.HostKeyChecking {
		case HostKeyTOFU, HostKeyStrict, HostKeyOff:
		default:
			return fmt.Errorf("%s: connection.host_key_checking %q must be tofu, strict or off", what, n.Connection.HostKeyChecking)
		}
		if n.Platform == "windows" && n.Connection.Become {
			return fmt.Errorf("%s: become is not supported on Windows nodes (connect as an administrator)", what)
		}
		for _, key := range []string{n.Host, n.Hostname} {
			if key == "" {
				continue
			}
			if prev, ok := seen[key]; ok {
				return fmt.Errorf("%s: %q is also used by %s", what, key, prev)
			}
			seen[key] = what
		}
		return nil
	}
	for i := range p.Inventory.Masters {
		if err := check("master", i, &p.Inventory.Masters[i]); err != nil {
			return err
		}
	}
	for i := range p.Inventory.Clients {
		if err := check("client", i, &p.Inventory.Clients[i]); err != nil {
			return err
		}
	}
	// A node to decommission must not be in masters or clients any more
	// (the seen map reports it), and needs its hostname: the cluster knows
	// nodes by name.
	for i := range p.Inventory.Decommission {
		d := &p.Inventory.Decommission[i]
		what := fmt.Sprintf("decommission[%d] (%s)", i, NodeKey(&d.NodeConfig))
		if d.Host == "" && d.Hostname == "" {
			return fmt.Errorf("decommission[%d]: host or hostname is required", i)
		}
		// Role and connection may come from the list of installed nodes;
		// the engine checks that they are known.
		if d.Role != "" && d.Role != RoleMaster && d.Role != RoleClient {
			return fmt.Errorf("%s: role %q must be master or client", what, d.Role)
		}
		if err := check("decommission", i, &d.NodeConfig); err != nil {
			return err
		}
	}

	// Release
	r := &p.Release
	switch r.Source {
	case ReleaseOfficial, ReleaseCustomURL:
		if !versionTagRe.MatchString(r.Version) {
			return fmt.Errorf("release.version %q must look like v1.2.3", r.Version)
		}
		if u, err := url.Parse(r.DownloadBaseURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("release.download_base_url %q must be an http(s) URL", r.DownloadBaseURL)
		}
	case ReleaseLocalPath:
		if r.LocalBinaryPath == "" {
			return fmt.Errorf("release.local_binary_path is required for source local-path")
		}
	case ReleaseHostPackage:
		for _, n := range append(append([]NodeConfig{}, p.Inventory.Masters...), p.Inventory.Clients...) {
			if n.Platform == "windows" {
				return fmt.Errorf("release.source host-package cannot install Windows nodes (%s)", n.Host)
			}
		}
		if !p.hostPackageCoversAllNodes(r.Package) {
			return fmt.Errorf("release.source host-package: host_packages.install must install %q on every node", r.Package)
		}
	default:
		return fmt.Errorf("release.source %q must be official, custom-url, local-path or host-package", r.Source)
	}
	if r.ChecksumSHA256 != "" && !sha256Re.MatchString(strings.ToLower(r.ChecksumSHA256)) {
		return fmt.Errorf("release.checksum_sha256 must be 64 hex characters")
	}

	// CNI
	if c := p.CNI; c != nil && c.Install != nil && *c.Install {
		switch c.Source {
		case ReleaseOfficial, ReleaseCustomURL:
			if !versionTagRe.MatchString(c.Version) {
				return fmt.Errorf("cni.version %q must look like v1.9.1", c.Version)
			}
		case ReleaseLocalPath:
			if c.LocalArchivePath == "" {
				return fmt.Errorf("cni.local_archive_path is required for source local-path")
			}
		default:
			return fmt.Errorf("cni.source %q must be official, custom-url or local-path", c.Source)
		}
		if c.ChecksumSHA256 != "" && !sha256Re.MatchString(strings.ToLower(c.ChecksumSHA256)) {
			return fmt.Errorf("cni.checksum_sha256 must be 64 hex characters")
		}
	}

	// Gossip key
	switch k := p.Security.GossipEncryptionKey; {
	case k == "", k == GossipAutoGenerate, strings.HasPrefix(k, "!enc:"):
	default:
		raw, err := base64.StdEncoding.DecodeString(k)
		if err != nil || (len(raw) != 16 && len(raw) != 24 && len(raw) != 32) {
			return fmt.Errorf("security.gossip_encryption_key must be AUTO_GENERATE, empty, or a base64 key of 16, 24 or 32 bytes")
		}
	}
	if p.Security.VerifyHTTPSClient && !p.Security.TLSEnabled {
		return fmt.Errorf("security.verify_https_client needs tls_enabled")
	}

	// Federation
	if f := p.Federation; f != nil {
		if f.AuthoritativeRegion == "" {
			return fmt.Errorf("federation.authoritative_region is required")
		}
		for i, j := range f.Join {
			if strings.TrimSpace(j) == "" || strings.ContainsAny(j, " \t\"") {
				return fmt.Errorf("federation.join[%d] %q is invalid", i, j)
			}
		}
		if p.IsReplicaRegion() && p.Security.ACL != nil && p.Security.ACL.Enabled && p.Federation.ReplicationTokenFile == "" {
			return fmt.Errorf("federation.replication_token_file is required for region %q with ACLs (the authoritative region is %q)",
				p.Metadata.Region, f.AuthoritativeRegion)
		}
	}

	if err := p.validatePacks(); err != nil {
		return err
	}
	if err := p.validateSecrets(); err != nil {
		return err
	}
	if err := p.validateACLPolicies(); err != nil {
		return err
	}
	if r.NATSBridge && r.Source == ReleaseLocalPath && !isArchivePath(r.LocalBinaryPath) && r.NATSBridgeLocalPath == "" {
		return fmt.Errorf("release.natsbridge with a plain local_binary_path needs release.natsbridge_local_path")
	}
	return nil
}

// IsReplicaRegion reports whether the region of the plan replicates ACLs from
// another (authoritative) region.
func (p *DeploymentPlan) IsReplicaRegion() bool {
	return p.Federation != nil && p.Federation.AuthoritativeRegion != "" &&
		p.Federation.AuthoritativeRegion != p.Metadata.Region
}

// NodeKey identifies a node in the list of installed nodes: its hostname
// (the node name in the cluster), or its host when it has no hostname.
func NodeKey(n *NodeConfig) string {
	if n.Hostname != "" {
		return n.Hostname
	}
	return n.Host
}

// HasConnection reports whether the node entry sets how to connect to it
// (beyond the defaults).
func (c *ConnectionConfig) HasConnection() bool {
	return c.Mode == "local" || c.SSHUser != "" || c.SSHKeyPath != "" || c.SSHPassword != "" || c.SSHPrivateKey != ""
}

// NodeDatacenter is the datacenter of a node.
func (p *DeploymentPlan) NodeDatacenter(n *NodeConfig) string {
	if n.Datacenter != "" {
		return n.Datacenter
	}
	return p.Metadata.Datacenter
}

// hostPackageCoversAllNodes reports whether a host package is installed on
// every node of the inventory.
func (p *DeploymentPlan) hostPackageCoversAllNodes(name string) bool {
	if p.HostPackages == nil {
		return false
	}
	covers := func(n *NodeConfig, isMaster bool) bool {
		for _, hp := range p.HostPackages.Install {
			if hp.Name == name && AppliesTo(hp.Roles, hp.Hosts, n, isMaster) {
				return true
			}
		}
		return false
	}
	for i := range p.Inventory.Masters {
		if !covers(&p.Inventory.Masters[i], true) {
			return false
		}
	}
	for i := range p.Inventory.Clients {
		if !covers(&p.Inventory.Clients[i], false) {
			return false
		}
	}
	return true
}
