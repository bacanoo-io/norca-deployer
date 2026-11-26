package plan

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// DNSForwardingEnabled reports whether the nodes forward the Consul domain
// to the local Consul agent.
func (c *ConsulConfig) DNSForwardingEnabled() bool {
	return c != nil && c.DNSForwarding != nil && c.DNSForwarding.Enabled
}

// DNSForwardingNeedsResolved reports whether the forwarding is on and can
// only be done with systemd-resolved: method systemd-resolved, or docker
// (DNSStubListenerExtra=). A node without systemd (OpenRC) cannot have it.
func (c *ConsulConfig) DNSForwardingNeedsResolved() bool {
	if !c.DNSForwardingEnabled() {
		return false
	}
	f := c.DNSForwarding
	return f.Method == DNSMethodResolved || f.Docker
}

// setConsulDefaults fills the defaults of consul.dns_forwarding.
func (p *DeploymentPlan) setConsulDefaults() {
	if p.Consul == nil || p.Consul.DNSForwarding == nil {
		return
	}
	f := p.Consul.DNSForwarding
	if f.Address == "" {
		f.Address = DefaultConsulDNSAddress
	}
	if f.Domain == "" {
		f.Domain = DefaultConsulDNSDomain
	}
	if f.Method == "" {
		f.Method = DNSMethodAuto
	}
}

// validateConsul checks consul.dns_forwarding, also while it is disabled.
func (p *DeploymentPlan) validateConsul() error {
	if p.Consul == nil || p.Consul.DNSForwarding == nil {
		return nil
	}
	f := p.Consul.DNSForwarding
	if _, _, err := SplitDNSAddress(f.Address); err != nil {
		return fmt.Errorf("consul.dns_forwarding.address: %w", err)
	}
	if err := validDomain(f.Domain); err != nil {
		return fmt.Errorf("consul.dns_forwarding.domain %q: %w", f.Domain, err)
	}
	switch f.Method {
	case DNSMethodAuto, DNSMethodResolved, DNSMethodDnsmasq:
	default:
		return fmt.Errorf("consul.dns_forwarding.method %q must be %s, %s or %s",
			f.Method, DNSMethodAuto, DNSMethodResolved, DNSMethodDnsmasq)
	}
	if f.Docker && f.Method == DNSMethodDnsmasq {
		return fmt.Errorf("consul.dns_forwarding.docker needs systemd-resolved (DNSStubListenerExtra), not method %s", DNSMethodDnsmasq)
	}
	return nil
}

// SplitDNSAddress splits the address of a DNS server, host:port with an IP
// host ("127.0.0.1:8600", "[::1]:8600").
func SplitDNSAddress(s string) (netip.Addr, int, error) {
	host, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return netip.Addr{}, 0, fmt.Errorf("%q is not host:port: %w", s, err)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || addr.Zone() != "" {
		return netip.Addr{}, 0, fmt.Errorf("the host of %q must be an IP address", s)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return netip.Addr{}, 0, fmt.Errorf("the port of %q must be 1-65535", s)
	}
	return addr.Unmap(), port, nil
}

// validDomain accepts a sequence of DNS labels: letters, digits and hyphens,
// 1-63 characters each, no hyphen at either end, no trailing dot.
func validDomain(d string) error {
	if d == "" || len(d) > 253 {
		return fmt.Errorf("must be 1-253 characters")
	}
	for label := range strings.SplitSeq(d, ".") {
		if label == "" || len(label) > 63 {
			return fmt.Errorf("each label must be 1-63 characters (no empty label, no trailing dot)")
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("label %q starts or ends with a hyphen", label)
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-') {
				return fmt.Errorf("label %q may have letters, digits and hyphens only", label)
			}
		}
	}
	return nil
}
