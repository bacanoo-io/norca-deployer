package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"

	"github.com/bacanoo-io/norca-deployer/pkg/pki"
)

var portRangeRe = regexp.MustCompile(`^[1-9][0-9]{0,4}(-[1-9][0-9]{0,4})?$`)

// MatchesInterface reports whether the cluster address of the nodes comes
// from an interface match (network.interface_prefix/suffix or network.vpn)
// rather than from their configured addresses.
func (n *NetworkConfig) MatchesInterface() bool {
	if n.InterfacePrefix != "" || n.InterfaceSuffix != "" {
		return true
	}
	v := n.VPN
	return v != nil && v.Enabled && (v.InterfacePrefix != "" || v.InterfaceSuffix != "" || v.InterfaceName != "")
}

// SPIFFETrustDomain returns the trust domain URI, or nil when SPIFFE IDs are
// off.
func (p *DeploymentPlan) SPIFFETrustDomain() (*url.URL, error) {
	s := p.Security.PKI
	if s == nil || s.SPIFFE == nil || !s.SPIFFE.Enabled {
		return nil, nil
	}
	return pki.TrustDomain(s.SPIFFE.TrustDomain)
}

// validateSPIFFE checks the trust domain and that the region and the node
// names can be SPIFFE path segments.
func (p *DeploymentPlan) validateSPIFFE() error {
	td, err := p.SPIFFETrustDomain()
	if err != nil || td == nil {
		if err != nil {
			return fmt.Errorf("security.pki.spiffe.trust_domain: %w", err)
		}
		return nil
	}
	if !p.Security.TLSEnabled {
		return fmt.Errorf("security.pki.spiffe needs security.tls_enabled")
	}
	nodes := append(append([]NodeConfig{}, p.Inventory.Masters...), p.Inventory.Clients...)
	for i := range nodes {
		if _, err := pki.SPIFFEID(td, "norca", p.Metadata.Region, RoleClient, NodeKey(&nodes[i])); err != nil {
			return fmt.Errorf("node %s: %w", NodeKey(&nodes[i]), err)
		}
	}
	return nil
}

// validateNetwork checks the bind address and the firewall sources.
func (p *DeploymentPlan) validateNetwork() error {
	// Without bind_address each node listens on its cluster address, which
	// must then be an IP address (the agent cannot bind to a DNS name).
	if p.Network.BindAddress == "" && !p.Network.MatchesInterface() {
		nodes := append(append([]NodeConfig{}, p.Inventory.Masters...), p.Inventory.Clients...)
		for i := range nodes {
			n := &nodes[i]
			addr := n.ClusterAddress
			if addr == "" {
				addr = n.Host
			}
			if net.ParseIP(addr) == nil {
				return fmt.Errorf("node %s: the agent listens on its cluster address, which must be an IP address (%q); "+
					"set cluster_address, network.interface_prefix, or network.bind_address", NodeKey(n), addr)
			}
		}
	}

	fw := p.Tuning.Firewall
	if fw == nil {
		return nil
	}
	for key, list := range map[string][]string{
		"cluster_cidrs":   fw.ClusterCIDRs,
		"api_cidrs":       fw.APICIDRs,
		"ephemeral_cidrs": fw.EphemeralCIDRs,
	} {
		for _, c := range list {
			if _, _, err := net.ParseCIDR(c); err != nil && net.ParseIP(c) == nil {
				return fmt.Errorf("tuning.firewall.%s: %q is not a CIDR or an IP address", key, c)
			}
		}
	}
	for i, ep := range fw.ExtraPorts {
		what := fmt.Sprintf("tuning.firewall.extra_ports[%d]", i)
		if !portRangeRe.MatchString(ep.Port) {
			return fmt.Errorf("%s: port %q must be a port or a range like 4222-4223", what, ep.Port)
		}
		switch ep.Proto {
		case "", "tcp", "udp", "both":
		default:
			return fmt.Errorf("%s: proto %q must be tcp, udp or both", what, ep.Proto)
		}
		for _, c := range ep.CIDRs {
			if _, _, err := net.ParseCIDR(c); err != nil && net.ParseIP(c) == nil {
				return fmt.Errorf("%s: %q is not a CIDR or an IP address", what, c)
			}
		}
		if err := validateRoles(what, ep.Roles); err != nil {
			return err
		}
	}
	if fw.DefaultDeny && !p.Tuning.ApplyFirewallRules {
		return fmt.Errorf("tuning.firewall.default_deny needs tuning.apply_firewall_rules")
	}
	return nil
}
