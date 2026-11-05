package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca-deployer/internal/config"
	"github.com/bacanoo-io/norca-deployer/internal/engine"
)

// newPlanCmd builds the `plan` command.
func newPlanCmd() *cobra.Command {
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

	return planCmd
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
