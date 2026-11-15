package plan

import "fmt"

// BusConfig is the bus block written into the server block of every server:
// settings of the system bus of the servers (the embedded NATS).
type BusConfig struct {
	// ConsulKVMaxBytes bounds the bucket of the Consul KV of the
	// Consul-compatible API on the bus ("256MiB", "1GiB"). A write beyond
	// it fails; no key is dropped. Empty keeps the default of Norca
	// (256MiB, or an eighth of the storage of the bus when that is less).
	ConsulKVMaxBytes string `yaml:"consul_kv_max_bytes,omitempty" json:"consul_kv_max_bytes,omitempty"`

	// LeafNodes turns on the leaf node listener of the servers
	// (network.ports.bus_leafnode, 4652): NATS servers outside the cluster
	// attach with an ACL token whose cdc rule allows a leaf and read the
	// changes of the region (change data capture). The firewall of the
	// servers opens the port to tuning.firewall.leafnode_cidrs.
	LeafNodes bool `yaml:"leafnodes,omitempty" json:"leafnodes,omitempty"`
}

// Empty reports whether the block sets nothing: no block is written then.
func (b *BusConfig) Empty() bool {
	return b == nil || b.ConsulKVMaxBytes == "" && !b.LeafNodes
}

// validateBus checks the bus block as far as the plan can: the size is one.
// Whether it fits into the storage of the bus with the other streams is
// checked by the Norca server, which names what each stream takes.
func (p *DeploymentPlan) validateBus() error {
	b := p.Bus
	if b.Empty() || b.ConsulKVMaxBytes == "" {
		return nil
	}
	n, err := ParseByteSize(b.ConsulKVMaxBytes)
	if err != nil {
		return fmt.Errorf("bus.consul_kv_max_bytes: %q is not a size (\"256MiB\", \"1GiB\")", b.ConsulKVMaxBytes)
	}
	if n <= 0 {
		return fmt.Errorf("bus.consul_kv_max_bytes must be positive, not %s", b.ConsulKVMaxBytes)
	}
	return nil
}
