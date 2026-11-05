package main

import (
	"errors"
	"fmt"
	"io"
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

	// 1. Init command (Generate sample configuration)
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Generate a sample deployment configuration file",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
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
				return err
			}
			if err := plan.SaveToFile(target); err != nil {
				return fmt.Errorf("error saving configuration file: %w", err)
			}
			fmt.Fprintf(out, "Sample deployment configuration successfully created: %s\n", target)
			return nil
		},
	}

	// 2. Validate command
	validateCmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate configuration syntax and logical constraints",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			plan, err := config.LoadFromFile(configFile)
			if err != nil {
				return fmt.Errorf("invalid configuration: %w", err)
			}
			fmt.Fprintf(out, "[OK] Configuration is valid! (cluster: %s, masters: %d, clients: %d)\n",
				plan.Metadata.Name, len(plan.Inventory.Masters), len(plan.Inventory.Clients))
			return nil
		},
	}

	// 3. Plan command (Dry-Run)
	planCmd := &cobra.Command{
		Use:   "plan",
		Short: "Display execution plan without modifying any infrastructure (Dry-Run)",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			plan, err := config.LoadFromFile(configFile)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "=== Norca Deployment Plan (Dry-Run) ===\n")
			fmt.Fprintf(out, "Cluster Name:    %s (Region: %s, DC: %s)\n", plan.Metadata.Name, plan.Metadata.Region, plan.Metadata.Datacenter)
			fmt.Fprintf(out, "Norca Release:   %s (%s)\n", plan.Release.Version, plan.Release.Source)
			if plan.CNI != nil && plan.CNI.Install != nil && *plan.CNI.Install {
				fmt.Fprintf(out, "CNI Plugins:     %s (%s)\n", plan.CNI.Version, plan.CNI.Source)
			}
			fmt.Fprintf(out, "Rolling Restart: servers one by one, clients %d at a time (health timeout %ds)\n",
				plan.Upgrade.MaxParallelClients, plan.Upgrade.HealthTimeout)
			fmt.Fprintf(out, "Gossip Encrypt:  %t\n", plan.Security.GossipEncryptionKey != "")
			fmt.Fprintf(out, "Interface Match: Prefix: %q, Suffix: %q\n", plan.Network.InterfacePrefix, plan.Network.InterfaceSuffix)
			if plan.Network.BindAddress != "" {
				fmt.Fprintf(out, "Listen Address:  %s\n", plan.Network.BindAddress)
			} else {
				fmt.Fprintf(out, "Listen Address:  cluster address of each node (HTTP also on 127.0.0.1)\n")
			}
			printFirewall(out, plan)
			fmt.Fprintf(out, "mTLS Security:   %t (Dynamic Worker Pool cert: %t)\n", plan.Security.TLSEnabled, plan.Security.PKI != nil && plan.Security.PKI.GenerateWorkerPoolCert)
			if td, _ := plan.SPIFFETrustDomain(); td != nil {
				fmt.Fprintf(out, "SPIFFE IDs:      %s/norca/%s/<server|client>/<node>\n", td, plan.Metadata.Region)
			}
			fmt.Fprintf(out, "Master Nodes (%d nodes):\n", len(plan.Inventory.Masters))
			for _, m := range plan.Inventory.Masters {
				fmt.Fprintf(out, "  - %s (%s, %s, mode: %s)%s\n", m.Hostname, m.Host, m.Platform, m.Connection.Mode, nodeLabels(&m, plan))
			}
			fmt.Fprintf(out, "Client Nodes (%d nodes):\n", len(plan.Inventory.Clients))
			for _, c := range plan.Inventory.Clients {
				fmt.Fprintf(out, "  - %s (%s, %s, mode: %s)%s\n", c.Hostname, c.Host, c.Platform, c.Connection.Mode, nodeLabels(&c, plan))
			}
			if len(plan.NodePools) > 0 {
				fmt.Fprintf(out, "Node Pools (%d):\n", len(plan.NodePools))
				for _, np := range plan.NodePools {
					fmt.Fprintf(out, "  - %s: %s\n", np.Name, np.Description)
				}
			}
			if plan.Network.DynamicPorts != nil {
				lo, hi := plan.DynamicPortRange()
				fmt.Fprintf(out, "Dynamic Ports:   %d-%d\n", lo, hi)
			}
			if hp := plan.HostPackages; hp != nil {
				fmt.Fprintf(out, "APT Repositories (%d):\n", len(hp.Repositories))
				for _, r := range hp.Repositories {
					fmt.Fprintf(out, "  - %s: %s %s %v %s\n", r.Name, r.URI, r.Suite, r.Components, rolesLabel(r.Roles, nil))
				}
				fmt.Fprintf(out, "Host Packages (%d):\n", len(hp.Install))
				for _, p := range hp.Install {
					version := p.Version
					if version == "" {
						version = "candidate"
					}
					fmt.Fprintf(out, "  - %s=%s hold=%t %s\n", p.Name, version, p.Hold, rolesLabel(p.Roles, p.Hosts))
				}
			}
			if len(plan.Scripts) > 0 {
				fmt.Fprintf(out, "Scripts (%d):\n", len(plan.Scripts))
				for _, s := range plan.Scripts {
					source := s.Path
					if source == "" {
						source = "inline"
					}
					fmt.Fprintf(out, "  - %s [%s] %s always=%t %s\n", s.Name, s.Phase, source, s.Always, rolesLabel(s.Roles, s.Hosts))
				}
			}
			if acl := plan.Security.ACL; acl != nil && len(acl.Policies) > 0 {
				fmt.Fprintf(out, "ACL Policies (%d):\n", len(acl.Policies))
				for _, p := range acl.Policies {
					bound := ""
					if p.Job != nil {
						bound = fmt.Sprintf(" (job %s/%s)", p.Job.Namespace, p.Job.JobID)
					}
					fmt.Fprintf(out, "  - %s%s\n", p.Name, bound)
				}
			}
			if s := plan.Secrets; s != nil && len(s.Bootstrap) > 0 {
				fmt.Fprintf(out, "Bootstrap Secrets (%d, store %s):\n", len(s.Bootstrap), s.Store)
				for _, b := range s.Bootstrap {
					mode := "create once"
					if b.Overwrite {
						mode = "overwrite on change"
					}
					if b.Type == config.SecretTypeSecret && s.Store == config.SecretStoreExternal {
						mode = "skipped: external store"
					}
					fmt.Fprintf(out, "  - %s %s/%s [%s] (%s)\n", b.Type, b.Namespace, b.Path, strings.Join(sortedKeys(b.Items), ", "), mode)
				}
			}
			if pk := plan.Packs; pk != nil && len(pk.Deploy) > 0 {
				fmt.Fprintf(out, "Packs (%d):\n", len(pk.Deploy))
				for _, d := range pk.Deploy {
					reg := d.Registry
					if reg == "" {
						reg = pk.AllRegistries()[0].Name
					}
					fmt.Fprintf(out, "  - %s: %s/%s enabled=%t\n", d.Name, reg, d.Pack, d.Enabled)
				}
			}
			if bindings := plan.AllGitOpsBindings(); len(bindings) > 0 {
				fmt.Fprintf(out, "GitOps Bindings (%d):\n", len(bindings))
				for _, b := range bindings {
					repo := b.EffectiveRepo()
					fmt.Fprintf(out, "  - %s: release=%s provider=%s url=%s branch=%s atomic=%t\n",
						b.Name, b.ReleaseName, b.EffectiveProvider(), repo.URL, repo.Branch, b.Atomic)
				}
			}

			// Plan compared with the nodes installed by earlier applies.
			report, err := engine.InventoryReport(plan, vaultIfAvailable())
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Node Status (%d):\n", len(report))
			for _, n := range report {
				fmt.Fprintf(out, "  - %-20s %-15s %-6s %s\n", n.Key, n.Host, n.Role, n.Status)
			}
			return nil
		},
	}

	// 4. Apply command
	applyCmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply configuration and provision cluster nodes idempotently",
		RunE: func(cmd *cobra.Command, args []string) error {
			plan, err := config.LoadFromFile(configFile)
			if err != nil {
				return fmt.Errorf("failed to load configuration: %w", err)
			}

			runner := engine.NewRunner(plan, vaultIfAvailable())
			runner.ForceRestart = forceRestart
			if err := runner.Apply(); err != nil {
				return fmt.Errorf("deployment failed: %w", err)
			}
			return nil
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
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			hexKey, _, err := crypto.GenerateMasterKey()
			if err != nil {
				return fmt.Errorf("key generation error: %w", err)
			}
			if _, err := os.Stat(".norca-deployer.key"); err == nil {
				return errors.New(".norca-deployer.key already exists; it decrypts your existing secrets")
			}
			err = os.WriteFile(".norca-deployer.key", []byte(hexKey), 0600)
			if err != nil {
				return fmt.Errorf("failed to save key file: %w", err)
			}
			fmt.Fprintf(out, "[OK] New Master Key saved to .norca-deployer.key\n")
			return nil
		},
	})

	secretCmd.AddCommand(&cobra.Command{
		Use:   "encrypt <plaintext>",
		Short: "Encrypt text into inline secret format (!enc:v1:aes-gcm:...)",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			key, err := crypto.ResolveMasterKey(secretKey, secretKeyFile)
			if err != nil {
				return err
			}
			v, err := crypto.NewVault(key)
			if err != nil {
				return err
			}
			encrypted, err := v.Encrypt(args[0])
			if err != nil {
				return fmt.Errorf("encryption failed: %w", err)
			}
			fmt.Fprintf(out, "Encrypted value:\n%s\n", encrypted)
			return nil
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
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			plan, err := config.LoadFromFile(configFile)
			if err != nil {
				return err
			}
			inter.Force = force
			files, err := engine.IssueIntermediate(plan, inter)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "[OK] Intermediate CA %q issued:\n", inter.Name)
			for _, f := range files {
				fmt.Fprintf(out, "  %s\n", f)
			}
			return nil
		},
	}
	interCmd.Flags().StringVar(&inter.Name, "name", "", "Name of the intermediate, e.g. ziti (required)")
	interCmd.Flags().IntVar(&inter.Years, "years", 5, "Validity in years")
	interCmd.Flags().IntVar(&inter.MaxPathLen, "max-path-len", 1, "CA levels allowed below the intermediate")
	interCmd.Flags().BoolVar(&force, "force", false, "Replace an existing intermediate of the same name")
	_ = interCmd.MarkFlagRequired("name")
	pkiCmd.AddCommand(interCmd)

	nodeCA := engine.NodeCAOptions{}
	nodeCACmd := &cobra.Command{
		Use:   "issue-node-ca",
		Short: "Issue the name-constrained intermediate CA of a region for nodes that join by themselves",
		Long: "Issues an intermediate CA signed by the cluster CA that may only sign client certificates of\n" +
			"one region (client.<region>.norca, localhost, 127.0.0.1 and URIs of the SPIFFE trust domain),\n" +
			"into certs/node-ca-<region>.pem, node-ca-<region>-key.pem and node-ca-<region>-chain.pem.\n" +
			"An operator that creates nodes (autoscaling) signs their certificates with it, so the key of\n" +
			"the cluster CA stays with the deployer. It cannot sign a server certificate.\n" +
			"There is no revocation: the validity of the node CA (180 days by default, --days) is the bound\n" +
			"on what a stolen key is worth, and it cannot reach beyond the cluster CA.",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			plan, err := config.LoadFromFile(configFile)
			if err != nil {
				return err
			}
			nodeCA.Force = force
			files, err := engine.IssueNodeCA(plan, nodeCA)
			if err != nil {
				return err
			}
			fmt.Fprintln(out, "[OK] Node CA issued:")
			for _, f := range files {
				fmt.Fprintf(out, "  %s\n", f)
			}
			return nil
		},
	}
	nodeCACmd.Flags().StringVar(&nodeCA.Region, "region", "", "Region of the nodes (default: metadata.region of the plan)")
	nodeCACmd.Flags().IntVar(&nodeCA.Days, "days", 0, fmt.Sprintf("Validity in days (default %d if neither --days nor --years is given)", engine.DefaultNodeCADays))
	nodeCACmd.Flags().IntVar(&nodeCA.Years, "years", 0, "Validity in years (instead of --days)")
	nodeCACmd.Flags().BoolVar(&force, "force", false, "Replace an existing node CA of the region")
	pkiCmd.AddCommand(nodeCACmd)

	// 7. Version command
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print Norca Deployer version",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Norca Deployer %s (Nomad v1.6.5 compatible orchestrator deployment)\n", version)
			return nil
		},
	}

	initCmd.Flags().BoolVar(&force, "force", false, "Overwrite an existing configuration file")
	applyCmd.Flags().BoolVar(&forceRestart, "force-restart", false, "Restart every agent (rolling), even when nothing changed")

	rootCmd.AddCommand(initCmd, validateCmd, planCmd, applyCmd, secretCmd, pkiCmd, newGitOpsCmd(), versionCmd)
	return rootCmd
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
func printFirewall(out io.Writer, plan *config.DeploymentPlan) {
	if !plan.Tuning.ApplyFirewallRules {
		fmt.Fprintf(out, "Firewall:        not managed\n")
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
	fmt.Fprintf(out, "Firewall:        Linux nodes, default deny: %t\n", fw.DefaultDeny)
	fmt.Fprintf(out, "  HTTP API:      %s\n", from(fw.APICIDRs))
	fmt.Fprintf(out, "  RPC, Serf:     %s\n", from(fw.ClusterCIDRs))
	if plan.Tuning.OpenEphemeralPorts {
		fmt.Fprintf(out, "  Allocations:   %s\n", from(eph))
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
