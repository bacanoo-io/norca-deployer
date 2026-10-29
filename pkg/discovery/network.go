package discovery

import (
	"fmt"
	"regexp"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
)

// ResolveAdvertiseAddress produces the appropriate Nomad/Norca advertise expression based on configuration.
func ResolveAdvertiseAddress(netCfg *config.NetworkConfig, nodeHost string, port int) string {
	return fmt.Sprintf("%s:%d", ResolveNodeAddress(netCfg, nodeHost), port)
}

// ResolveNodeAddress is the address of a node in the cluster network: a
// go-sockaddr template when an interface is matched, otherwise the node's
// cluster address. It is used for advertising and for binding.
//
// go-sockaddr's GetInterfaceIP takes an exact interface name, so prefix and
// suffix matches are expressed with GetAllInterfaces and a name regexp.
func ResolveNodeAddress(netCfg *config.NetworkConfig, nodeHost string) string {
	if netCfg.InterfacePrefix != "" {
		return interfaceRegexpTemplate("^" + regexp.QuoteMeta(netCfg.InterfacePrefix))
	}
	if netCfg.InterfaceSuffix != "" {
		return interfaceRegexpTemplate(regexp.QuoteMeta(netCfg.InterfaceSuffix) + "$")
	}
	if netCfg.VPN != nil && netCfg.VPN.Enabled {
		if netCfg.VPN.InterfacePrefix != "" {
			return interfaceRegexpTemplate("^" + regexp.QuoteMeta(netCfg.VPN.InterfacePrefix))
		}
		if netCfg.VPN.InterfaceSuffix != "" {
			return interfaceRegexpTemplate(regexp.QuoteMeta(netCfg.VPN.InterfaceSuffix) + "$")
		}
		if netCfg.VPN.InterfaceName != "" {
			return fmt.Sprintf("{{ GetInterfaceIP %q }}", netCfg.VPN.InterfaceName)
		}
	}
	return nodeHost
}

// interfaceRegexpTemplate selects the first IPv4 address of the first
// interface whose name matches the regexp.
func interfaceRegexpTemplate(nameRegexp string) string {
	return fmt.Sprintf(`{{ GetAllInterfaces | include "name" %q | include "type" "IPv4" | exclude "flags" "loopback" | limit 1 | attr "address" }}`,
		nameRegexp)
}
