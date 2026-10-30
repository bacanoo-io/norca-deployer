package engine

import (
	"fmt"
	"strings"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/platform/firewall"
)

// configureFirewall opens the ports the node needs, from the sources of
// tuning.firewall. Only a failed default_deny fails the node; other problems
// are warnings.
func (r *Runner) configureFirewall(t *target) error {
	fw := r.plan.Tuning.Firewall
	if fw == nil {
		fw = &config.FirewallConfig{}
	}
	busPort, busClusterPort := r.plan.Network.Ports.BusPorts()
	spec := firewall.Spec{
		Ports: firewall.Ports{
			HTTP: r.plan.Network.Ports.HTTP, RPC: r.plan.Network.Ports.RPC, Serf: r.plan.Network.Ports.Serf,
			Bus: busPort, BusCluster: busClusterPort,
		},
		Server:         t.isMaster,
		Client:         t.needsClient(),
		OpenEphemeral:  r.plan.Tuning.OpenEphemeralPorts,
		ClusterCIDRs:   fw.ClusterCIDRs,
		APICIDRs:       fw.APICIDRs,
		EphemeralCIDRs: fw.EphemeralCIDRs,
		DefaultDeny:    fw.DefaultDeny,
	}
	spec.DynamicMin, spec.DynamicMax = r.plan.DynamicPortRange()
	role := config.RoleClient
	if t.isMaster {
		role = config.RoleMaster
	}
	for _, ep := range fw.ExtraPorts {
		roles := ep.Roles
		if len(roles) == 0 {
			roles = []string{config.RoleClient}
		}
		if !containsString(roles, role) && !(role == config.RoleMaster && t.needsClient() && containsString(roles, config.RoleClient)) {
			continue
		}
		sources := ep.CIDRs
		if len(sources) == 0 {
			sources = fw.ClusterCIDRs
		}
		protos := []string{"tcp"}
		switch ep.Proto {
		case "udp":
			protos = []string{"udp"}
		case "both":
			protos = []string{"tcp", "udp"}
		}
		for _, proto := range protos {
			spec.Extra = append(spec.Extra, firewall.Extra{Port: strings.Replace(ep.Port, "-", ":", 1), Proto: proto, Sources: sources})
		}
	}
	// The port the deployer reached the node on stays open; the ports sshd
	// listens on are detected on the node as well.
	if t.node.Connection.Mode != "local" {
		spec.SSHPorts = []int{t.node.Connection.SSHPort}
	}
	res, err := firewall.Configure(t.conn, spec)
	if err != nil {
		return fmt.Errorf("firewall: %w", err)
	}
	for _, w := range res.Warnings {
		fmt.Printf("    [WARN] [%s] firewall (%s): %s\n", t.node.Host, res.Backend, w)
	}
	return nil
}
