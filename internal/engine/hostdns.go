package engine

import (
	"fmt"

	"github.com/bacanoo-io/norca-deployer/internal/platform/dns"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// configureHostDNS makes a Linux node resolve the Consul domain through the
// local Consul agent (consul.dns_forwarding), or removes what an earlier
// apply wrote for it. It marks nothing for a restart: the agent does not
// depend on it. With docker the docker0 subnet is kept for the firewall.
func (r *Runner) configureHostDNS(t *target) error {
	var fwd *config.ConsulDNSForwarding
	if r.plan.Consul != nil {
		fwd = r.plan.Consul.DNSForwarding
	}
	enabled := r.plan.Consul.DNSForwardingEnabled()
	if t.isWindows() {
		if enabled {
			fmt.Printf("    [WARN] [%s] consul.dns_forwarding skipped on a Windows node\n", t.node.Host)
		}
		return nil
	}
	// The steps print the warnings.
	res, err := dns.Apply(r.abortCtx(), t.conn, t.node.Host, fwd)
	if err != nil {
		return err
	}
	if res.Bridge.IsValid() {
		t.hostDNSSources = []string{res.Bridge.Masked().String()}
	}
	if enabled || len(res.Removed) > 0 {
		fmt.Printf("    [OK] [%s] Consul DNS forwarding: %s\n", t.node.Host, res.Summary())
	}
	return nil
}
