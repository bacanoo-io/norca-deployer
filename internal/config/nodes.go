package config

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Default range of the dynamic allocation ports.
const (
	DefaultDynamicPortMin = 20000
	DefaultDynamicPortMax = 32000
)

var (
	metaKeyRe   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	nodeLabelRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	portListRe  = regexp.MustCompile(`^[0-9]{1,5}(-[0-9]{1,5})?(,[0-9]{1,5}(-[0-9]{1,5})?)*$`)
)

// DynamicPortRange returns the range of the dynamic allocation ports.
func (p *DeploymentPlan) DynamicPortRange() (int, int) {
	if r := p.Network.DynamicPorts; r != nil {
		return r.Min, r.Max
	}
	return DefaultDynamicPortMin, DefaultDynamicPortMax
}

// applyClientDefaults merges client_defaults into the client settings of the
// clients and of the masters that run a client. The node's own values win:
// strings and numbers when set, map entries by key, lists and blocks when
// given. Booleans are true when either sets them.
func (p *DeploymentPlan) applyClientDefaults() {
	d := p.ClientDefaults
	if d == nil {
		return
	}
	merge := func(n *NodeConfig, isMaster bool) {
		if n.ClientSettings == nil {
			if isMaster {
				return // a master without client_settings runs no client
			}
			n.ClientSettings = &ClientSettings{}
		}
		n.ClientSettings = mergeClientSettings(d, n.ClientSettings)
	}
	for i := range p.Inventory.Masters {
		merge(&p.Inventory.Masters[i], true)
	}
	for i := range p.Inventory.Clients {
		merge(&p.Inventory.Clients[i], false)
	}
}

func mergeClientSettings(d, n *ClientSettings) *ClientSettings {
	r := *n
	str := func(dst *string, def string) {
		if *dst == "" {
			*dst = def
		}
	}
	str(&r.NetworkInterface, d.NetworkInterface)
	str(&r.CNISubnet, d.CNISubnet)
	str(&r.NodeClass, d.NodeClass)
	str(&r.NodePool, d.NodePool)
	str(&r.ExtraHCL, d.ExtraHCL)
	if r.GCDiskUsageThreshold == 0 {
		r.GCDiskUsageThreshold = d.GCDiskUsageThreshold
	}
	r.EnableRawExec = r.EnableRawExec || d.EnableRawExec
	if len(r.Drivers) == 0 {
		r.Drivers = append([]string{}, d.Drivers...)
	}
	if len(r.CNIPlugins) == 0 {
		r.CNIPlugins = append([]string{}, d.CNIPlugins...)
	}
	if len(r.HostNetworks) == 0 {
		r.HostNetworks = append([]HostNetworkConfig{}, d.HostNetworks...)
	}
	if len(r.CustomHostVolumes) == 0 {
		r.CustomHostVolumes = append([]HostVolumeConfig{}, d.CustomHostVolumes...)
	}
	if r.NativeAppVolumes == nil {
		r.NativeAppVolumes = d.NativeAppVolumes
	}
	if r.Reserved == nil {
		r.Reserved = d.Reserved
	}
	if r.DrainOnShutdown == nil {
		r.DrainOnShutdown = d.DrainOnShutdown
	}
	if len(d.Meta) > 0 {
		meta := map[string]string{}
		for k, v := range d.Meta {
			meta[k] = v
		}
		for k, v := range n.Meta {
			meta[k] = v
		}
		r.Meta = meta
	}
	return &r
}

// validateNodeSettings checks the client settings that end up in norca.hcl
// and the node pools.
func (p *DeploymentPlan) validateNodeSettings() error {
	if r := p.Network.DynamicPorts; r != nil {
		if r.Min < 1024 || r.Max > 65535 || r.Min >= r.Max {
			return fmt.Errorf("network.dynamic_ports: min %d and max %d must be 1024 <= min < max <= 65535", r.Min, r.Max)
		}
	}
	check := func(what string, cs *ClientSettings) error {
		if cs == nil {
			return nil
		}
		for k := range cs.Meta {
			if !metaKeyRe.MatchString(k) {
				return fmt.Errorf("%s: meta key %q must be letters, digits, '_', '.' or '-'", what, k)
			}
		}
		if cs.NodeClass != "" && !nodeLabelRe.MatchString(cs.NodeClass) {
			return fmt.Errorf("%s: node_class %q must be letters, digits, '_' or '-'", what, cs.NodeClass)
		}
		if cs.NodePool != "" && (!nodeLabelRe.MatchString(cs.NodePool) || cs.NodePool == "all") {
			return fmt.Errorf("%s: node_pool %q must be letters, digits, '_' or '-', and not \"all\"", what, cs.NodePool)
		}
		names := map[string]bool{}
		for _, hn := range cs.HostNetworks {
			if !nodeLabelRe.MatchString(hn.Name) || names[hn.Name] {
				return fmt.Errorf("%s: host network name %q is invalid or used twice", what, hn.Name)
			}
			names[hn.Name] = true
			if (hn.Interface == "") == (hn.CIDR == "") {
				return fmt.Errorf("%s: host network %s needs interface or cidr (one of them)", what, hn.Name)
			}
			if hn.CIDR != "" {
				if _, _, err := net.ParseCIDR(hn.CIDR); err != nil {
					return fmt.Errorf("%s: host network %s: %w", what, hn.Name, err)
				}
			}
			if hn.ReservedPorts != "" && !portListRe.MatchString(hn.ReservedPorts) {
				return fmt.Errorf("%s: host network %s: reserved_ports %q must look like 22,80,8000-8100", what, hn.Name, hn.ReservedPorts)
			}
		}
		if r := cs.Reserved; r != nil {
			if r.CPU < 0 || r.Memory < 0 || r.Disk < 0 {
				return fmt.Errorf("%s: reserved values must not be negative", what)
			}
			if r.ReservedPorts != "" && !portListRe.MatchString(r.ReservedPorts) {
				return fmt.Errorf("%s: reserved.reserved_ports %q must look like 22,80,8000-8100", what, r.ReservedPorts)
			}
		}
		if d := cs.DrainOnShutdown; d != nil && d.Deadline != "" {
			if _, err := time.ParseDuration(d.Deadline); err != nil {
				return fmt.Errorf("%s: drain_on_shutdown.deadline: %w", what, err)
			}
		}
		if strings.Contains(cs.ExtraHCL, "\x00") {
			return fmt.Errorf("%s: extra_hcl contains a NUL byte", what)
		}
		return nil
	}
	if err := check("client_defaults", p.ClientDefaults); err != nil {
		return err
	}
	for i := range p.Inventory.Masters {
		if err := check("master "+NodeKey(&p.Inventory.Masters[i]), p.Inventory.Masters[i].ClientSettings); err != nil {
			return err
		}
	}
	for i := range p.Inventory.Clients {
		if err := check("client "+NodeKey(&p.Inventory.Clients[i]), p.Inventory.Clients[i].ClientSettings); err != nil {
			return err
		}
	}

	seen := map[string]bool{}
	for i, np := range p.NodePools {
		what := fmt.Sprintf("node_pools[%d] (%s)", i, np.Name)
		if !nodeLabelRe.MatchString(np.Name) || np.Name == "all" {
			return fmt.Errorf("%s: name must be letters, digits, '_' or '-', and not \"all\"", what)
		}
		if seen[np.Name] {
			return fmt.Errorf("%s: listed twice", what)
		}
		seen[np.Name] = true
		for k := range np.Meta {
			if !metaKeyRe.MatchString(k) {
				return fmt.Errorf("%s: meta key %q is invalid", what, k)
			}
		}
	}
	return nil
}

// FormatPortRange formats a range for the firewall ("20000:32000").
func FormatPortRange(min, max int) string {
	return strconv.Itoa(min) + ":" + strconv.Itoa(max)
}
