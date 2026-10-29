package config

// DeploymentPlan represents the entire cluster deployment plan.
type DeploymentPlan struct {
	Version         string         `yaml:"version" json:"version"`
	Kind            string         `yaml:"kind" json:"kind"`
	Metadata        MetadataConfig `yaml:"metadata" json:"metadata"`
	CredentialsFile string         `yaml:"credentials_file,omitempty" json:"credentials_file,omitempty"`
	Release         ReleaseConfig  `yaml:"release" json:"release"`
	Paths           PlatformPaths  `yaml:"paths,omitempty" json:"paths,omitempty"`
	Tuning          TuningConfig   `yaml:"tuning,omitempty" json:"tuning,omitempty"`
	Network         NetworkConfig  `yaml:"network" json:"network"`
	Security        SecurityConfig `yaml:"security" json:"security"`
	Backup          *BackupConfig  `yaml:"backup,omitempty" json:"backup,omitempty"`
	Packs           *PacksConfig   `yaml:"packs,omitempty" json:"packs,omitempty"`
	CNI             *CNIConfig     `yaml:"cni,omitempty" json:"cni,omitempty"`
	Upgrade         UpgradeConfig  `yaml:"upgrade,omitempty" json:"upgrade,omitempty"`
	Federation      *Federation    `yaml:"federation,omitempty" json:"federation,omitempty"`
	HostPackages    *HostPackages  `yaml:"host_packages,omitempty" json:"host_packages,omitempty"`
	Scripts         []ScriptConfig `yaml:"scripts,omitempty" json:"scripts,omitempty"`
	Secrets         *SecretsConfig `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	// ClientDefaults are merged into the client_settings of every node
	// that runs a client; the node's own settings win.
	ClientDefaults *ClientSettings `yaml:"client_defaults,omitempty" json:"client_defaults,omitempty"`
	// NodePools describes node pools (created after the cluster is up; a
	// pool named by a node is also created when the node registers).
	NodePools []NodePoolConfig `yaml:"node_pools,omitempty" json:"node_pools,omitempty"`
	Inventory InventoryConfig  `yaml:"inventory" json:"inventory"`

	// baseDir is the directory of the loaded configuration file; relative
	// script paths are resolved against it.
	baseDir string
}

// Node roles used by the role filters of host packages and scripts.
const (
	RoleMaster = "master"
	RoleClient = "client"
)

// Script phases, in execution order on a node.
const (
	PhasePreInstall   = "pre_install"   // after connecting, before tuning and directories
	PhasePostPackages = "post_packages" // after the Debian packages are installed
	PhasePostInstall  = "post_install"  // after the Norca service is (re)started
)

// HostPackages installs Debian packages from APT repositories on Debian-based
// Linux nodes before the Norca agent is configured.
type HostPackages struct {
	Repositories []AptRepository `yaml:"repositories,omitempty" json:"repositories,omitempty"`
	Install      []HostPackage   `yaml:"install,omitempty" json:"install,omitempty"`
	// UpdateCache runs apt-get update before installing (default true).
	UpdateCache *bool `yaml:"update_cache,omitempty" json:"update_cache,omitempty"`
	// LockTimeout is the dpkg lock wait in seconds (default 300).
	LockTimeout int `yaml:"lock_timeout,omitempty" json:"lock_timeout,omitempty"`
}

// AptRepository is written as a deb822 source with its own keyring
// (/etc/apt/sources.list.d/norca-<name>.sources, /etc/apt/keyrings/norca-<name>.asc).
type AptRepository struct {
	Name          string   `yaml:"name" json:"name"`
	URI           string   `yaml:"uri" json:"uri"`
	Suite         string   `yaml:"suite" json:"suite"`                                     // e.g. "bookworm" or "stable"; "./" for flat repositories
	Components    []string `yaml:"components,omitempty" json:"components,omitempty"`       // e.g. ["main"]; empty for flat repositories
	Architectures []string `yaml:"architectures,omitempty" json:"architectures,omitempty"` // e.g. ["amd64"]; empty = all
	Key           string   `yaml:"key,omitempty" json:"key,omitempty"`                     // ASCII-armored public key (inline)
	KeyFile       string   `yaml:"key_file,omitempty" json:"key_file,omitempty"`           // ASCII-armored or binary key on the deployer machine
	Roles         []string `yaml:"roles,omitempty" json:"roles,omitempty"`                 // "master", "client"; empty = all nodes

	// PinPriority writes an APT preference for the packages of this
	// repository (/etc/apt/preferences.d/norca-<name>.pref); 0 = no pin.
	// A priority above 1000 also allows downgrades to the versions of
	// this repository.
	PinPriority int      `yaml:"pin_priority,omitempty" json:"pin_priority,omitempty"`
	PinPackages []string `yaml:"pin_packages,omitempty" json:"pin_packages,omitempty"` // default: all packages ("*")
	// PinRelease pins by release fields (e.g. "n=bookworm-backports" or
	// "o=Vendor,a=stable") instead of the origin host. An origin pin applies
	// to every repository on the same host.
	PinRelease string `yaml:"pin_release,omitempty" json:"pin_release,omitempty"`

	// Auth is written to /etc/apt/auth.conf.d/norca-<name>.conf (0600).
	Auth *AptAuth `yaml:"auth,omitempty" json:"auth,omitempty"`
}

type AptAuth struct {
	Username string `yaml:"username" json:"username"`
	Password string `yaml:"password" json:"password"` // may be an !enc:v1 secret
}

// HostPackage is installed from the APT repositories, or from a local .deb
// file that the deployer copies to the node (File).
type HostPackage struct {
	Name    string   `yaml:"name" json:"name"`
	File    string   `yaml:"file,omitempty" json:"file,omitempty"`       // local .deb on the deployer machine ({arch} is replaced)
	SHA256  string   `yaml:"sha256,omitempty" json:"sha256,omitempty"`   // expected digest of File
	Version string   `yaml:"version,omitempty" json:"version,omitempty"` // exact Debian version; empty = candidate version
	Hold    bool     `yaml:"hold,omitempty" json:"hold,omitempty"`       // apt-mark hold after install
	Roles   []string `yaml:"roles,omitempty" json:"roles,omitempty"`     // "master", "client"; empty = all nodes
	Hosts   []string `yaml:"hosts,omitempty" json:"hosts,omitempty"`     // host or hostname; empty = all nodes of the roles
}

// ScriptConfig is a bash script run on Linux nodes at a given phase.
type ScriptConfig struct {
	Name    string            `yaml:"name" json:"name"`
	Phase   string            `yaml:"phase" json:"phase"`                         // pre_install, post_packages, post_install
	Path    string            `yaml:"path,omitempty" json:"path,omitempty"`       // local file, relative to the configuration file
	Inline  string            `yaml:"inline,omitempty" json:"inline,omitempty"`   // script body
	Roles   []string          `yaml:"roles,omitempty" json:"roles,omitempty"`     // "master", "client"; empty = all nodes
	Hosts   []string          `yaml:"hosts,omitempty" json:"hosts,omitempty"`     // host or hostname; empty = all nodes of the roles
	Env     map[string]string `yaml:"env,omitempty" json:"env,omitempty"`         // values may be !enc:v1 secrets
	Always  bool              `yaml:"always,omitempty" json:"always,omitempty"`   // run on every apply, not only when the script or env changed
	Timeout int               `yaml:"timeout,omitempty" json:"timeout,omitempty"` // seconds (default 600)
}

type MetadataConfig struct {
	Name       string `yaml:"name" json:"name"`
	Datacenter string `yaml:"datacenter" json:"datacenter"`
	Region     string `yaml:"region" json:"region"`
}

type ReleaseConfig struct {
	Version         string `yaml:"version" json:"version"`
	Source          string `yaml:"source,omitempty" json:"source,omitempty"` // official, custom-url, local-path, host-package
	ChecksumSHA256  string `yaml:"checksum_sha256,omitempty" json:"checksum_sha256,omitempty"`
	DownloadBaseURL string `yaml:"download_base_url,omitempty" json:"download_base_url,omitempty"`
	// LocalBinaryPath is the norca binary or a release archive (.tar.gz,
	// .zip) on the deployer machine; {os} and {arch} are replaced.
	LocalBinaryPath string `yaml:"local_binary_path,omitempty" json:"local_binary_path,omitempty"`
	// Package is the host package that provides the binary for source
	// host-package (default "norca").
	Package string `yaml:"package,omitempty" json:"package,omitempty"`
	// NATSBridge also installs norca-natsbridge (from the same release
	// archive) on the nodes that run a client, for the NATS bridge job.
	NATSBridge bool `yaml:"natsbridge,omitempty" json:"natsbridge,omitempty"`
	// NATSBridgeLocalPath is the norca-natsbridge binary for source
	// local-path when local_binary_path is a plain binary, not an archive.
	NATSBridgeLocalPath string `yaml:"natsbridge_local_path,omitempty" json:"natsbridge_local_path,omitempty"`
}

// Release sources.
const (
	ReleaseOfficial    = "official"     // GitHub release archive, verified with SHA256SUMS
	ReleaseCustomURL   = "custom-url"   // same layout under download_base_url
	ReleaseLocalPath   = "local-path"   // local binary or archive copied to the nodes
	ReleaseHostPackage = "host-package" // installed by host_packages; only checked
)

// CNIConfig installs the CNI reference plugins on Linux clients.
type CNIConfig struct {
	Install          *bool  `yaml:"install,omitempty" json:"install,omitempty"` // default true
	Version          string `yaml:"version,omitempty" json:"version,omitempty"` // default v1.9.1
	Source           string `yaml:"source,omitempty" json:"source,omitempty"`   // official, custom-url, local-path
	DownloadBaseURL  string `yaml:"download_base_url,omitempty" json:"download_base_url,omitempty"`
	LocalArchivePath string `yaml:"local_archive_path,omitempty" json:"local_archive_path,omitempty"` // {arch} is replaced
	ChecksumSHA256   string `yaml:"checksum_sha256,omitempty" json:"checksum_sha256,omitempty"`
}

// Federation joins the servers of this plan (one region) to the servers of
// other regions. ACLs are bootstrapped in the authoritative region only; the
// other regions replicate them with a management token of that region.
type Federation struct {
	AuthoritativeRegion string `yaml:"authoritative_region" json:"authoritative_region"`
	// Join lists server addresses of other regions (host or host:serf_port).
	Join []string `yaml:"join,omitempty" json:"join,omitempty"`
	// ReplicationTokenFile is the management token file of the
	// authoritative region (its security.acl.management_token_output).
	ReplicationTokenFile string `yaml:"replication_token_file,omitempty" json:"replication_token_file,omitempty"`
}

// UpgradeConfig controls the rolling restart of changed nodes.
type UpgradeConfig struct {
	MaxParallelClients int `yaml:"max_parallel_clients,omitempty" json:"max_parallel_clients,omitempty"` // default 1
	HealthTimeout      int `yaml:"health_timeout,omitempty" json:"health_timeout,omitempty"`             // seconds, default 180
}

// PlatformPaths allows overriding paths for Linux and Windows platforms.
type PlatformPaths struct {
	Linux   *PathConfig `yaml:"linux,omitempty" json:"linux,omitempty"`
	Windows *PathConfig `yaml:"windows,omitempty" json:"windows,omitempty"`
}

type PathConfig struct {
	BinDir       string `yaml:"bin_dir,omitempty" json:"bin_dir,omitempty"`
	ConfigDir    string `yaml:"config_dir,omitempty" json:"config_dir,omitempty"`
	DataDir      string `yaml:"data_dir,omitempty" json:"data_dir,omitempty"`
	LogDir       string `yaml:"log_dir,omitempty" json:"log_dir,omitempty"`
	TLSDir       string `yaml:"tls_dir,omitempty" json:"tls_dir,omitempty"`
	CNIBinDir    string `yaml:"cni_bin_dir,omitempty" json:"cni_bin_dir,omitempty"`
	CNIConfigDir string `yaml:"cni_config_dir,omitempty" json:"cni_config_dir,omitempty"`
	AppsDir      string `yaml:"apps_dir,omitempty" json:"apps_dir,omitempty"`
}

type TuningConfig struct {
	ApplySysctl         bool   `yaml:"apply_sysctl" json:"apply_sysctl"`
	ApplyFirewallRules  bool   `yaml:"apply_firewall_rules" json:"apply_firewall_rules"`
	OpenEphemeralPorts  bool   `yaml:"open_ephemeral_ports" json:"open_ephemeral_ports"`     // 20000-32000
	CNIMTUMode          string `yaml:"cni_mtu_mode,omitempty" json:"cni_mtu_mode,omitempty"` // "auto", "1420", "1500"
	SetupLogrotate      bool   `yaml:"setup_logrotate" json:"setup_logrotate"`
	WindowsLongPaths    bool   `yaml:"windows_long_paths" json:"windows_long_paths"`
	FileDescriptorLimit int    `yaml:"file_descriptor_limit,omitempty" json:"file_descriptor_limit,omitempty"`
	// Firewall restricts the sources of the rules of apply_firewall_rules
	// (Linux nodes only).
	Firewall *FirewallConfig `yaml:"firewall,omitempty" json:"firewall,omitempty"`
}

// FirewallConfig limits who can reach the Norca ports. An empty list allows
// any source.
type FirewallConfig struct {
	// ClusterCIDRs is the network of the nodes: RPC and Serf (and the
	// allocation ports by default). Servers of other regions must be in it.
	ClusterCIDRs []string `yaml:"cluster_cidrs,omitempty" json:"cluster_cidrs,omitempty"`
	// APICIDRs may reach the HTTP API and UI of the servers (operators,
	// load balancers).
	APICIDRs []string `yaml:"api_cidrs,omitempty" json:"api_cidrs,omitempty"`
	// EphemeralCIDRs may reach the allocation ports 20000-32000 on the
	// clients; default cluster_cidrs. Use 0.0.0.0/0 for public services.
	EphemeralCIDRs []string `yaml:"ephemeral_cidrs,omitempty" json:"ephemeral_cidrs,omitempty"`
	// DefaultDeny turns on ufw with incoming traffic denied unless allowed
	// (firewalld denies by default). The SSH ports are always allowed.
	DefaultDeny bool `yaml:"default_deny,omitempty" json:"default_deny,omitempty"`
	// ExtraPorts opens static ports of workloads (e.g. NATS 4222), which
	// are outside the allocation range.
	ExtraPorts []FirewallPort `yaml:"extra_ports,omitempty" json:"extra_ports,omitempty"`
}

// FirewallPort is a static port or range opened on some nodes.
type FirewallPort struct {
	Port  string   `yaml:"port" json:"port"`                       // "4222" or "4222-4223"
	Proto string   `yaml:"proto,omitempty" json:"proto,omitempty"` // tcp (default), udp or both
	CIDRs []string `yaml:"cidrs,omitempty" json:"cidrs,omitempty"` // default cluster_cidrs; empty there too: any
	Roles []string `yaml:"roles,omitempty" json:"roles,omitempty"` // default [client]
}

type NetworkConfig struct {
	VPN             *VPNConfig `yaml:"vpn,omitempty" json:"vpn,omitempty"`
	InterfacePrefix string     `yaml:"interface_prefix,omitempty" json:"interface_prefix,omitempty"` // e.g. "wg", "tun", "tailscale", "eth"
	InterfaceSuffix string     `yaml:"interface_suffix,omitempty" json:"interface_suffix,omitempty"` // e.g. "vpn", "cluster", "0"
	// BindAddress is the listen address of HTTP, RPC and Serf. Empty (the
	// default): each node listens on its cluster address only, and HTTP also
	// on 127.0.0.1 for the local CLI.
	BindAddress   string      `yaml:"bind_address,omitempty" json:"bind_address,omitempty"`
	AdvertiseMode string      `yaml:"advertise_mode" json:"advertise_mode"` // "vpn_interface", "default_interface", "specific_ip"
	Ports         PortsConfig `yaml:"ports" json:"ports"`
	// DynamicPorts is the range of the dynamic allocation ports (default
	// 20000-32000); the firewall opens the same range.
	DynamicPorts *PortRange `yaml:"dynamic_ports,omitempty" json:"dynamic_ports,omitempty"`
}

// PortRange is an inclusive port range.
type PortRange struct {
	Min int `yaml:"min" json:"min"`
	Max int `yaml:"max" json:"max"`
}

type VPNConfig struct {
	Enabled           bool   `yaml:"enabled" json:"enabled"`
	InterfaceName     string `yaml:"interface_name,omitempty" json:"interface_name,omitempty"`     // wg0, tailscale0, tun0
	InterfacePrefix   string `yaml:"interface_prefix,omitempty" json:"interface_prefix,omitempty"` // e.g. "wg", "tun"
	InterfaceSuffix   string `yaml:"interface_suffix,omitempty" json:"interface_suffix,omitempty"` // e.g. "vpn"
	FallbackToDefault bool   `yaml:"fallback_to_default" json:"fallback_to_default"`
	CIDRFilter        string `yaml:"cidr_filter,omitempty" json:"cidr_filter,omitempty"`
}

type PortsConfig struct {
	HTTP int `yaml:"http" json:"http"` // 4646
	RPC  int `yaml:"rpc" json:"rpc"`   // 4647
	Serf int `yaml:"serf" json:"serf"` // 4648
}

type SecurityConfig struct {
	TLSEnabled               bool `yaml:"tls_enabled" json:"tls_enabled"`
	AutoGenerateCertificates bool `yaml:"auto_generate_certificates" json:"auto_generate_certificates"`
	VerifyServerHostname     bool `yaml:"verify_server_hostname" json:"verify_server_hostname"`
	// VerifyHTTPSClient requires a client certificate on the HTTP API; the
	// deployer then issues certs/cli.pem for operators.
	VerifyHTTPSClient bool `yaml:"verify_https_client,omitempty" json:"verify_https_client,omitempty"`
	// GossipEncryptionKey: AUTO_GENERATE (kept in .norca-deployer/), a
	// base64 key (may be an !enc:v1 secret), or empty for no encryption.
	GossipEncryptionKey string     `yaml:"gossip_encryption_key" json:"gossip_encryption_key"`
	PKI                 *PKIConfig `yaml:"pki,omitempty" json:"pki,omitempty"`
	ACL                 *ACLConfig `yaml:"acl,omitempty" json:"acl,omitempty"`
}

type PKIConfig struct {
	CACertPath              string   `yaml:"ca_cert_path,omitempty" json:"ca_cert_path,omitempty"`
	CAKeyPath               string   `yaml:"ca_key_path,omitempty" json:"ca_key_path,omitempty"`
	GenerateWorkerPoolCert  bool     `yaml:"generate_worker_pool_cert" json:"generate_worker_pool_cert"`
	ServerAdditionalDNSSANs []string `yaml:"server_additional_dns_sans,omitempty" json:"server_additional_dns_sans,omitempty"`
	ServerAdditionalIPSANs  []string `yaml:"server_additional_ip_sans,omitempty" json:"server_additional_ip_sans,omitempty"`
	// SPIFFE adds SPIFFE IDs (URI SANs) to the certificates, so that SPIFFE
	// consumers such as OpenZiti can identify the nodes and find the trust
	// domain in the chain.
	SPIFFE *SPIFFEConfig `yaml:"spiffe,omitempty" json:"spiffe,omitempty"`
}

// SPIFFEConfig sets the trust domain of the cluster certificates. The node
// certificates get spiffe://<trust_domain>/norca/<region>/<server|client>/<node>.
type SPIFFEConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// TrustDomain, e.g. "prod.example.com"; default "<metadata.name>.norca".
	// Changing it re-issues every node certificate.
	TrustDomain string `yaml:"trust_domain,omitempty" json:"trust_domain,omitempty"`
}

type ACLConfig struct {
	Enabled               bool   `yaml:"enabled" json:"enabled"`
	AutoBootstrap         bool   `yaml:"auto_bootstrap" json:"auto_bootstrap"`
	ManagementTokenOutput string `yaml:"management_token_output,omitempty" json:"management_token_output,omitempty"`
	// Policies are created or updated after the bootstrap (in the
	// authoritative region; they replicate to the others).
	Policies []ACLPolicyConfig `yaml:"policies,omitempty" json:"policies,omitempty"`
}

// ACLPolicyConfig is an ACL policy, optionally bound to a job so that the
// job's workload identity gets it (e.g. the NATS bridge).
type ACLPolicyConfig struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Rules inline, or RulesFile relative to the plan.
	Rules     string        `yaml:"rules,omitempty" json:"rules,omitempty"`
	RulesFile string        `yaml:"rules_file,omitempty" json:"rules_file,omitempty"`
	Job       *ACLPolicyJob `yaml:"job,omitempty" json:"job,omitempty"`
}

// ACLPolicyJob binds a policy to the workload identities of a job.
type ACLPolicyJob struct {
	Namespace string `yaml:"namespace,omitempty" json:"namespace,omitempty"` // default "default"
	JobID     string `yaml:"job_id" json:"job_id"`
	Group     string `yaml:"group,omitempty" json:"group,omitempty"`
	Task      string `yaml:"task,omitempty" json:"task,omitempty"`
}

// Secret stores of SecretsConfig.Store.
const (
	SecretStoreBuiltin  = "builtin"
	SecretStoreExternal = "external"
)

// SecretsConfig lists secrets and Variables written after the cluster is up
// (before the packs, which may read them).
type SecretsConfig struct {
	// Store: builtin (default) writes the secrets into Norca's built-in
	// secret store. external: the jobs read secrets from an external store
	// (e.g. Vault), which must be set up first; the entries of type secret
	// are skipped, Variables are still written.
	Store     string            `yaml:"store,omitempty" json:"store,omitempty"`
	Bootstrap []BootstrapSecret `yaml:"bootstrap,omitempty" json:"bootstrap,omitempty"`
}

// Kinds of BootstrapSecret.Type.
const (
	SecretTypeSecret   = "secret"
	SecretTypeVariable = "variable"
)

// BootstrapSecret is one secret or Variable. Item values may be plain,
// "!enc:v1:..." (encrypted with the master key), "@path" (a file relative to
// the plan), "!generate:<name>[:<bytes>]" (random, generated once and kept
// in .norca-deployer/generated/), "!nkey-seed:<name>" or
// "!nkey-public:<name>" (a NATS user NKey pair, generated once).
type BootstrapSecret struct {
	Type      string            `yaml:"type,omitempty" json:"type,omitempty"` // secret (default) or variable
	Path      string            `yaml:"path" json:"path"`
	Namespace string            `yaml:"namespace,omitempty" json:"namespace,omitempty"` // default "default"
	Items     map[string]string `yaml:"items" json:"items"`
	// Overwrite writes a new version when the items in the plan changed;
	// without it an existing secret or Variable is never touched.
	Overwrite bool `yaml:"overwrite,omitempty" json:"overwrite,omitempty"`
}

type BackupConfig struct {
	Enabled     bool                    `yaml:"enabled" json:"enabled"`
	Schedule    string                  `yaml:"schedule" json:"schedule"`
	Destination BackupDestinationConfig `yaml:"destination" json:"destination"`
	Encryption  BackupEncryptionConfig  `yaml:"encryption" json:"encryption"`
	Retention   *BackupRetentionConfig  `yaml:"retention,omitempty" json:"retention,omitempty"`
}

type BackupDestinationConfig struct {
	Name           string `yaml:"name" json:"name"`
	Type           string `yaml:"type" json:"type"`                     // s3, directory
	Path           string `yaml:"path,omitempty" json:"path,omitempty"` // absolute path for type "directory"
	Endpoint       string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
	Bucket         string `yaml:"bucket,omitempty" json:"bucket,omitempty"`
	Prefix         string `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	ForcePathStyle bool   `yaml:"force_path_style,omitempty" json:"force_path_style,omitempty"`
}

type BackupEncryptionConfig struct {
	AutoGenerateAgeKey bool     `yaml:"auto_generate_age_key" json:"auto_generate_age_key"`
	Recipients         []string `yaml:"recipients,omitempty" json:"recipients,omitempty"`
}

type BackupRetentionConfig struct {
	KeepHourly  int `yaml:"keep_hourly,omitempty" json:"keep_hourly,omitempty"`
	KeepDaily   int `yaml:"keep_daily,omitempty" json:"keep_daily,omitempty"`
	KeepWeekly  int `yaml:"keep_weekly,omitempty" json:"keep_weekly,omitempty"`
	KeepMonthly int `yaml:"keep_monthly,omitempty" json:"keep_monthly,omitempty"`
}

type PacksConfig struct {
	// Registry is a single registry (shorthand); Registries lists several.
	Registry   PackRegistryConfig   `yaml:"registry,omitempty" json:"registry,omitempty"`
	Registries []PackRegistryConfig `yaml:"registries,omitempty" json:"registries,omitempty"`
	Deploy     []PackDeployItem     `yaml:"deploy" json:"deploy"`
}

// AllRegistries returns the registries of registry and registries.
func (p *PacksConfig) AllRegistries() []PackRegistryConfig {
	var res []PackRegistryConfig
	if p.Registry.Name != "" {
		res = append(res, p.Registry)
	}
	return append(res, p.Registries...)
}

// PackRegistryConfig is created as a cluster registry (POST /v1/pack-registry).
type PackRegistryConfig struct {
	Name         string `yaml:"name" json:"name"`
	Type         string `yaml:"type,omitempty" json:"type,omitempty"` // git (default), oci, dir
	URL          string `yaml:"url" json:"url"`
	Ref          string `yaml:"ref" json:"ref"`
	SyncInterval string `yaml:"sync_interval,omitempty" json:"sync_interval,omitempty"`
}

// PackDeployItem is installed as a release with the server-side deploy
// (POST /v1/release/<name>/deploy-pack).
type PackDeployItem struct {
	Name string `yaml:"name" json:"name"`
	// Registry of the pack; may be empty when the plan has one registry.
	Registry string                 `yaml:"registry,omitempty" json:"registry,omitempty"`
	Pack     string                 `yaml:"pack" json:"pack"`
	Version  string                 `yaml:"version,omitempty" json:"version,omitempty"`
	Enabled  bool                   `yaml:"enabled" json:"enabled"`
	Values   map[string]interface{} `yaml:"values,omitempty" json:"values,omitempty"`
}

type InventoryConfig struct {
	Masters []NodeConfig `yaml:"masters" json:"masters"`
	Clients []NodeConfig `yaml:"clients,omitempty" json:"clients,omitempty"`
	// Decommission lists nodes that apply takes out of the cluster and
	// uninstalls. The deployer keeps no record of installed nodes, so a node
	// that is only deleted from masters/clients keeps running.
	Decommission []DecommissionNode `yaml:"decommission,omitempty" json:"decommission,omitempty"`
}

// DecommissionNode is a node to remove: clients are drained and purged,
// servers are removed from Raft and Serf; then Norca is uninstalled.
type DecommissionNode struct {
	NodeConfig `yaml:",inline"`
	// Role is the role the node had: master or client.
	Role string `yaml:"role" json:"role"`
	// DrainDeadline is the drain deadline of a client in seconds (default
	// 600); allocations still running then are stopped.
	DrainDeadline int `yaml:"drain_deadline,omitempty" json:"drain_deadline,omitempty"`
	// PurgeData also deletes data_dir, log_dir and apps_dir.
	PurgeData bool `yaml:"purge_data,omitempty" json:"purge_data,omitempty"`
}

type NodeConfig struct {
	Host     string `yaml:"host" json:"host"`
	Hostname string `yaml:"hostname" json:"hostname"`
	Platform string `yaml:"platform" json:"platform"` // linux, windows
	Arch     string `yaml:"arch" json:"arch"`         // amd64, arm64, arm
	// ClusterAddress is the address other nodes reach this node on (e.g.
	// its VPN address) when it differs from the SSH host; used for
	// retry_join and the server certificate.
	ClusterAddress string `yaml:"cluster_address,omitempty" json:"cluster_address,omitempty"`
	// Datacenter overrides metadata.datacenter for this node.
	Datacenter     string           `yaml:"datacenter,omitempty" json:"datacenter,omitempty"`
	Connection     ConnectionConfig `yaml:"connection" json:"connection"`
	ServerSettings *ServerSettings  `yaml:"server_settings,omitempty" json:"server_settings,omitempty"`
	ClientSettings *ClientSettings  `yaml:"client_settings,omitempty" json:"client_settings,omitempty"`
	PathOverrides  *PathConfig      `yaml:"path_overrides,omitempty" json:"path_overrides,omitempty"`
}

type ConnectionConfig struct {
	Mode             string `yaml:"mode" json:"mode"` // local, ssh
	SSHUser          string `yaml:"ssh_user,omitempty" json:"ssh_user,omitempty"`
	SSHPassword      string `yaml:"ssh_password,omitempty" json:"ssh_password,omitempty"`
	SSHKeyPath       string `yaml:"ssh_key_path,omitempty" json:"ssh_key_path,omitempty"`
	SSHKeyPassphrase string `yaml:"ssh_key_passphrase,omitempty" json:"ssh_key_passphrase,omitempty"`
	SSHPrivateKey    string `yaml:"ssh_private_key,omitempty" json:"ssh_private_key,omitempty"`
	SSHPort          int    `yaml:"ssh_port,omitempty" json:"ssh_port,omitempty"`
	Become           bool   `yaml:"become,omitempty" json:"become,omitempty"`
	BecomePassword   string `yaml:"become_password,omitempty" json:"become_password,omitempty"`
	// HostKeyChecking: tofu (default; records unknown keys in
	// .norca-deployer/known_hosts and rejects changed ones), strict (the
	// key must already be known), off.
	HostKeyChecking string `yaml:"host_key_checking,omitempty" json:"host_key_checking,omitempty"`
	KnownHostsFile  string `yaml:"known_hosts_file,omitempty" json:"known_hosts_file,omitempty"`
}

type ServerSettings struct {
	BootstrapExpect int    `yaml:"bootstrap_expect" json:"bootstrap_expect"`
	RaftMultiplier  int    `yaml:"raft_multiplier,omitempty" json:"raft_multiplier,omitempty"`
	LogLevel        string `yaml:"log_level,omitempty" json:"log_level,omitempty"`
	// ExtraHCL is appended inside the server block of norca.hcl.
	ExtraHCL string `yaml:"extra_hcl,omitempty" json:"extra_hcl,omitempty"`
}

type ClientSettings struct {
	NetworkInterface     string             `yaml:"network_interface,omitempty" json:"network_interface,omitempty"`
	CNISubnet            string             `yaml:"cni_subnet,omitempty" json:"cni_subnet,omitempty"`
	CNIPlugins           []string           `yaml:"cni_plugins,omitempty" json:"cni_plugins,omitempty"`
	Drivers              []string           `yaml:"drivers,omitempty" json:"drivers,omitempty"` // docker, exec, raw_exec
	EnableRawExec        bool               `yaml:"enable_raw_exec" json:"enable_raw_exec"`
	GCDiskUsageThreshold int                `yaml:"gc_disk_usage_threshold,omitempty" json:"gc_disk_usage_threshold,omitempty"`
	NativeAppVolumes     *NativeAppVolumes  `yaml:"native_app_volumes,omitempty" json:"native_app_volumes,omitempty"`
	CustomHostVolumes    []HostVolumeConfig `yaml:"custom_host_volumes,omitempty" json:"custom_host_volumes,omitempty"`

	// NodeClass and Meta are the scheduling labels of the node: jobs select
	// nodes with constraint, affinity, spread or separate on
	// ${node.class} and ${meta.<key>}.
	NodeClass string            `yaml:"node_class,omitempty" json:"node_class,omitempty"`
	NodePool  string            `yaml:"node_pool,omitempty" json:"node_pool,omitempty"`
	Meta      map[string]string `yaml:"meta,omitempty" json:"meta,omitempty"`
	// HostNetworks are named networks that job ports can be bound to.
	HostNetworks []HostNetworkConfig `yaml:"host_networks,omitempty" json:"host_networks,omitempty"`
	// Reserved resources are kept for the operating system and the
	// services outside Norca.
	Reserved        *ReservedConfig        `yaml:"reserved,omitempty" json:"reserved,omitempty"`
	DrainOnShutdown *DrainOnShutdownConfig `yaml:"drain_on_shutdown,omitempty" json:"drain_on_shutdown,omitempty"`
	// ExtraHCL is appended inside the client block of norca.hcl, for
	// settings the deployer does not model.
	ExtraHCL string `yaml:"extra_hcl,omitempty" json:"extra_hcl,omitempty"`
}

// HostNetworkConfig is a client host_network block.
type HostNetworkConfig struct {
	Name          string `yaml:"name" json:"name"`
	Interface     string `yaml:"interface,omitempty" json:"interface,omitempty"`
	CIDR          string `yaml:"cidr,omitempty" json:"cidr,omitempty"`
	ReservedPorts string `yaml:"reserved_ports,omitempty" json:"reserved_ports,omitempty"`
}

// ReservedConfig is the client reserved block.
type ReservedConfig struct {
	CPU           int    `yaml:"cpu,omitempty" json:"cpu,omitempty"`       // MHz
	Memory        int    `yaml:"memory,omitempty" json:"memory,omitempty"` // MB
	Disk          int    `yaml:"disk,omitempty" json:"disk,omitempty"`     // MB
	ReservedPorts string `yaml:"reserved_ports,omitempty" json:"reserved_ports,omitempty"`
}

// DrainOnShutdownConfig drains the node when the agent stops (e.g. on a
// reboot), so that its allocations move first.
type DrainOnShutdownConfig struct {
	Deadline         string `yaml:"deadline,omitempty" json:"deadline,omitempty"` // e.g. "1h" (default 1h)
	Force            bool   `yaml:"force,omitempty" json:"force,omitempty"`
	IgnoreSystemJobs bool   `yaml:"ignore_system_jobs,omitempty" json:"ignore_system_jobs,omitempty"`
}

// NodePoolConfig describes a node pool.
type NodePoolConfig struct {
	Name        string            `yaml:"name" json:"name"`
	Description string            `yaml:"description,omitempty" json:"description,omitempty"`
	Meta        map[string]string `yaml:"meta,omitempty" json:"meta,omitempty"`
}

type NativeAppVolumes struct {
	EnableDefaults bool `yaml:"enable_defaults" json:"enable_defaults"` // bin, work, config, data
}

type HostVolumeConfig struct {
	Name     string `yaml:"name" json:"name"`
	Path     string `yaml:"path" json:"path"`
	ReadOnly bool   `yaml:"read_only" json:"read_only"`
	Owner    string `yaml:"owner,omitempty" json:"owner,omitempty"`
	Mode     string `yaml:"mode,omitempty" json:"mode,omitempty"`
}
