package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/crypto"
	"github.com/bacanoo-io/norca-deployer/pkg/engine"
)

var (
	configFile    string
	secretKey     string
	secretKeyFile string
	force         bool
	forceRestart  bool
	version       = "v1.6.0"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "norca-deployer",
		Short: "Idempotent, declarative Norca cluster deployer",
	}

	rootCmd.PersistentFlags().StringVarP(&configFile, "config", "c", "cluster-deploy.yaml", "Path to deployment configuration file")
	rootCmd.PersistentFlags().StringVar(&secretKey, "secret-key", "", "Master encryption key string")
	rootCmd.PersistentFlags().StringVar(&secretKeyFile, "secret-key-file", "", "Path to master encryption key file")

	// 1. Init command (Generate sample configuration)
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Generate a sample deployment configuration file",
		Run: func(cmd *cobra.Command, args []string) {
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
				fmt.Printf("[ERROR] %v\n", err)
				os.Exit(1)
			}
			if err := plan.SaveToFile(target); err != nil {
				fmt.Printf("Error saving configuration file: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Sample deployment configuration successfully created: %s\n", target)
		},
	}

	// 2. Validate command
	validateCmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate configuration syntax and logical constraints",
		Run: func(cmd *cobra.Command, args []string) {
			plan, err := config.LoadFromFile(configFile)
			if err != nil {
				fmt.Printf("[ERROR] Invalid configuration: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("[OK] Configuration is valid! (cluster: %s, masters: %d, clients: %d)\n",
				plan.Metadata.Name, len(plan.Inventory.Masters), len(plan.Inventory.Clients))
		},
	}

	// 3. Plan command (Dry-Run)
	planCmd := &cobra.Command{
		Use:   "plan",
		Short: "Display execution plan without modifying any infrastructure (Dry-Run)",
		Run: func(cmd *cobra.Command, args []string) {
			plan, err := config.LoadFromFile(configFile)
			if err != nil {
				fmt.Printf("[ERROR] %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("=== Norca Deployment Plan (Dry-Run) ===\n")
			fmt.Printf("Cluster Name:    %s (Region: %s, DC: %s)\n", plan.Metadata.Name, plan.Metadata.Region, plan.Metadata.Datacenter)
			fmt.Printf("Norca Release:   %s (%s)\n", plan.Release.Version, plan.Release.Source)
			if plan.CNI != nil && plan.CNI.Install != nil && *plan.CNI.Install {
				fmt.Printf("CNI Plugins:     %s (%s)\n", plan.CNI.Version, plan.CNI.Source)
			}
			fmt.Printf("Rolling Restart: servers one by one, clients %d at a time (health timeout %ds)\n",
				plan.Upgrade.MaxParallelClients, plan.Upgrade.HealthTimeout)
			fmt.Printf("Gossip Encrypt:  %t\n", plan.Security.GossipEncryptionKey != "")
			fmt.Printf("Interface Match: Prefix: %q, Suffix: %q\n", plan.Network.InterfacePrefix, plan.Network.InterfaceSuffix)
			if plan.Network.BindAddress != "" {
				fmt.Printf("Listen Address:  %s\n", plan.Network.BindAddress)
			} else {
				fmt.Printf("Listen Address:  cluster address of each node (HTTP also on 127.0.0.1)\n")
			}
			printFirewall(plan)
			fmt.Printf("mTLS Security:   %t (Dynamic Worker Pool cert: %t)\n", plan.Security.TLSEnabled, plan.Security.PKI != nil && plan.Security.PKI.GenerateWorkerPoolCert)
			if td, _ := plan.SPIFFETrustDomain(); td != nil {
				fmt.Printf("SPIFFE IDs:      %s/norca/%s/<server|client>/<node>\n", td, plan.Metadata.Region)
			}
			fmt.Printf("Master Nodes (%d nodes):\n", len(plan.Inventory.Masters))
			for _, m := range plan.Inventory.Masters {
				fmt.Printf("  - %s (%s, %s, mode: %s)%s\n", m.Hostname, m.Host, m.Platform, m.Connection.Mode, nodeLabels(&m, plan))
			}
			fmt.Printf("Client Nodes (%d nodes):\n", len(plan.Inventory.Clients))
			for _, c := range plan.Inventory.Clients {
				fmt.Printf("  - %s (%s, %s, mode: %s)%s\n", c.Hostname, c.Host, c.Platform, c.Connection.Mode, nodeLabels(&c, plan))
			}
			if len(plan.NodePools) > 0 {
				fmt.Printf("Node Pools (%d):\n", len(plan.NodePools))
				for _, np := range plan.NodePools {
					fmt.Printf("  - %s: %s\n", np.Name, np.Description)
				}
			}
			if plan.Network.DynamicPorts != nil {
				lo, hi := plan.DynamicPortRange()
				fmt.Printf("Dynamic Ports:   %d-%d\n", lo, hi)
			}
			if hp := plan.HostPackages; hp != nil {
				fmt.Printf("APT Repositories (%d):\n", len(hp.Repositories))
				for _, r := range hp.Repositories {
					fmt.Printf("  - %s: %s %s %v %s\n", r.Name, r.URI, r.Suite, r.Components, rolesLabel(r.Roles, nil))
				}
				fmt.Printf("Host Packages (%d):\n", len(hp.Install))
				for _, p := range hp.Install {
					version := p.Version
					if version == "" {
						version = "candidate"
					}
					fmt.Printf("  - %s=%s hold=%t %s\n", p.Name, version, p.Hold, rolesLabel(p.Roles, p.Hosts))
				}
			}
			if len(plan.Scripts) > 0 {
				fmt.Printf("Scripts (%d):\n", len(plan.Scripts))
				for _, s := range plan.Scripts {
					source := s.Path
					if source == "" {
						source = "inline"
					}
					fmt.Printf("  - %s [%s] %s always=%t %s\n", s.Name, s.Phase, source, s.Always, rolesLabel(s.Roles, s.Hosts))
				}
			}
			if acl := plan.Security.ACL; acl != nil && len(acl.Policies) > 0 {
				fmt.Printf("ACL Policies (%d):\n", len(acl.Policies))
				for _, p := range acl.Policies {
					bound := ""
					if p.Job != nil {
						bound = fmt.Sprintf(" (job %s/%s)", p.Job.Namespace, p.Job.JobID)
					}
					fmt.Printf("  - %s%s\n", p.Name, bound)
				}
			}
			if s := plan.Secrets; s != nil && len(s.Bootstrap) > 0 {
				fmt.Printf("Bootstrap Secrets (%d, store %s):\n", len(s.Bootstrap), s.Store)
				for _, b := range s.Bootstrap {
					mode := "create once"
					if b.Overwrite {
						mode = "overwrite on change"
					}
					if b.Type == config.SecretTypeSecret && s.Store == config.SecretStoreExternal {
						mode = "skipped: external store"
					}
					fmt.Printf("  - %s %s/%s [%s] (%s)\n", b.Type, b.Namespace, b.Path, strings.Join(sortedKeys(b.Items), ", "), mode)
				}
			}
			if pk := plan.Packs; pk != nil && len(pk.Deploy) > 0 {
				fmt.Printf("Packs (%d):\n", len(pk.Deploy))
				for _, d := range pk.Deploy {
					reg := d.Registry
					if reg == "" {
						reg = pk.AllRegistries()[0].Name
					}
					fmt.Printf("  - %s: %s/%s enabled=%t\n", d.Name, reg, d.Pack, d.Enabled)
				}
			}

			// Plan compared with the nodes installed by earlier applies.
			report, err := engine.InventoryReport(plan, vaultIfAvailable())
			if err != nil {
				fmt.Printf("[ERROR] %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Node Status (%d):\n", len(report))
			for _, n := range report {
				fmt.Printf("  - %-20s %-15s %-6s %s\n", n.Key, n.Host, n.Role, n.Status)
			}
		},
	}

	// 4. Apply command
	applyCmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply configuration and provision cluster nodes idempotently",
		Run: func(cmd *cobra.Command, args []string) {
			plan, err := config.LoadFromFile(configFile)
			if err != nil {
				fmt.Printf("[ERROR] Failed to load configuration: %v\n", err)
				os.Exit(1)
			}

			runner := engine.NewRunner(plan, vaultIfAvailable())
			runner.ForceRestart = forceRestart
			if err := runner.Apply(); err != nil {
				fmt.Printf("[ERROR] Deployment failed: %v\n", err)
				os.Exit(1)
			}
		},
	}

	// 5. Secret commands
	secretCmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage encrypted credentials and Master Key",
	}

	secretCmd.AddCommand(&cobra.Command{
		Use:   "create-key",
		Short: "Generate a new 256-bit AES Master Key file (.norca-deployer.key)",
		Run: func(cmd *cobra.Command, args []string) {
			hexKey, _, err := crypto.GenerateMasterKey()
			if err != nil {
				fmt.Printf("Key generation error: %v\n", err)
				os.Exit(1)
			}
			if _, err := os.Stat(".norca-deployer.key"); err == nil {
				fmt.Printf("[ERROR] .norca-deployer.key already exists; it decrypts your existing secrets\n")
				os.Exit(1)
			}
			err = os.WriteFile(".norca-deployer.key", []byte(hexKey), 0600)
			if err != nil {
				fmt.Printf("Failed to save key file: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("[OK] New Master Key saved to .norca-deployer.key\n")
		},
	})

	secretCmd.AddCommand(&cobra.Command{
		Use:   "encrypt <plaintext>",
		Short: "Encrypt text into inline secret format (!enc:v1:aes-gcm:...)",
		Args:  cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			key, err := crypto.ResolveMasterKey(secretKey, secretKeyFile)
			if err != nil {
				fmt.Printf("[ERROR] %v\n", err)
				os.Exit(1)
			}
			v, err := crypto.NewVault(key)
			if err != nil {
				fmt.Printf("[ERROR] %v\n", err)
				os.Exit(1)
			}
			encrypted, err := v.Encrypt(args[0])
			if err != nil {
				fmt.Printf("[ERROR] Encryption failed: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("Encrypted value:\n%s\n", encrypted)
		},
	})

	// 6. PKI commands
	pkiCmd := &cobra.Command{
		Use:   "pki",
		Short: "Manage certificates of the cluster CA",
	}
	inter := engine.IntermediateOptions{}
	interCmd := &cobra.Command{
		Use:   "issue-intermediate",
		Short: "Issue an intermediate CA signed by the cluster CA (e.g. for OpenZiti)",
		Long: "Issues an intermediate CA signed by the cluster CA, with the SPIFFE trust domain of the plan\n" +
			"as URI SAN, into certs/<name>-ca.pem, <name>-ca-key.pem and <name>-ca-chain.pem.\n" +
			"A PKI that must chain up to the cluster CA (OpenZiti controllers and routers) signs with it.",
		Run: func(cmd *cobra.Command, args []string) {
			plan, err := config.LoadFromFile(configFile)
			if err != nil {
				fmt.Printf("[ERROR] %v\n", err)
				os.Exit(1)
			}
			inter.Force = force
			files, err := engine.IssueIntermediate(plan, inter)
			if err != nil {
				fmt.Printf("[ERROR] %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("[OK] Intermediate CA %q issued:\n", inter.Name)
			for _, f := range files {
				fmt.Printf("  %s\n", f)
			}
		},
	}
	interCmd.Flags().StringVar(&inter.Name, "name", "", "Name of the intermediate, e.g. ziti (required)")
	interCmd.Flags().IntVar(&inter.Years, "years", 5, "Validity in years")
	interCmd.Flags().IntVar(&inter.MaxPathLen, "max-path-len", 1, "CA levels allowed below the intermediate")
	interCmd.Flags().BoolVar(&force, "force", false, "Replace an existing intermediate of the same name")
	_ = interCmd.MarkFlagRequired("name")
	pkiCmd.AddCommand(interCmd)

	// 7. Version command
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print Norca Deployer version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("Norca Deployer %s (Nomad v1.6.5 compatible orchestrator deployment)\n", version)
		},
	}

	initCmd.Flags().BoolVar(&force, "force", false, "Overwrite an existing configuration file")
	applyCmd.Flags().BoolVar(&forceRestart, "force-restart", false, "Restart every agent (rolling), even when nothing changed")

	rootCmd.AddCommand(initCmd, validateCmd, planCmd, applyCmd, secretCmd, pkiCmd, versionCmd)

	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
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

func rolesLabel(roles, hosts []string) string {
	label := "(all nodes)"
	if len(roles) > 0 {
		label = fmt.Sprintf("(roles: %s)", strings.Join(roles, ", "))
	}
	if len(hosts) > 0 {
		label += fmt.Sprintf(" (hosts: %s)", strings.Join(hosts, ", "))
	}
	return label
}

// vaultIfAvailable returns the vault of the master key, or nil without a key.
func vaultIfAvailable() *crypto.Vault {
	key, err := crypto.ResolveMasterKey(secretKey, secretKeyFile)
	if err != nil {
		return nil
	}
	v, _ := crypto.NewVault(key)
	return v
}

// printFirewall summarizes who can reach the Norca ports.
func printFirewall(plan *config.DeploymentPlan) {
	if !plan.Tuning.ApplyFirewallRules {
		fmt.Printf("Firewall:        not managed\n")
		return
	}
	fw := plan.Tuning.Firewall
	if fw == nil {
		fw = &config.FirewallConfig{}
	}
	from := func(list []string) string {
		if len(list) == 0 {
			return "ANY source"
		}
		return strings.Join(list, ", ")
	}
	eph := fw.EphemeralCIDRs
	if len(eph) == 0 {
		eph = fw.ClusterCIDRs
	}
	fmt.Printf("Firewall:        Linux nodes, default deny: %t\n", fw.DefaultDeny)
	fmt.Printf("  HTTP API:      %s\n", from(fw.APICIDRs))
	fmt.Printf("  RPC, Serf:     %s\n", from(fw.ClusterCIDRs))
	if plan.Tuning.OpenEphemeralPorts {
		fmt.Printf("  Allocations:   %s\n", from(eph))
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// nodeLabels describes the scheduling labels of a node that runs a client.
func nodeLabels(n *config.NodeConfig, plan *config.DeploymentPlan) string {
	cs := n.ClientSettings
	if cs == nil {
		return ""
	}
	var parts []string
	parts = append(parts, "dc="+plan.NodeDatacenter(n))
	if cs.NodePool != "" {
		parts = append(parts, "pool="+cs.NodePool)
	}
	if cs.NodeClass != "" {
		parts = append(parts, "class="+cs.NodeClass)
	}
	for _, k := range sortedKeys(cs.Meta) {
		parts = append(parts, "meta."+k+"="+cs.Meta[k])
	}
	return " [" + strings.Join(parts, " ") + "]"
}
