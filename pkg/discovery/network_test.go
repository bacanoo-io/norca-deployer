package discovery

import (
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
)

func TestResolveAdvertiseAddress(t *testing.T) {
	cases := map[string]struct {
		cfg  config.NetworkConfig
		want string
	}{
		"prefix": {config.NetworkConfig{InterfacePrefix: "wg"},
			`{{ GetAllInterfaces | include "name" "^wg" | include "type" "IPv4" | exclude "flags" "loopback" | limit 1 | attr "address" }}:4647`},
		"suffix": {config.NetworkConfig{InterfaceSuffix: ".vpn"},
			`{{ GetAllInterfaces | include "name" "\\.vpn$" | include "type" "IPv4" | exclude "flags" "loopback" | limit 1 | attr "address" }}:4647`},
		"exact": {config.NetworkConfig{VPN: &config.VPNConfig{Enabled: true, InterfaceName: "wg0"}},
			`{{ GetInterfaceIP "wg0" }}:4647`},
		"fallback": {config.NetworkConfig{}, "10.0.0.1:4647"},
	}
	for name, tc := range cases {
		got := ResolveAdvertiseAddress(&tc.cfg, "10.0.0.1", 4647)
		if got != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", name, got, tc.want)
		}
		// GetInterfaceIP takes an exact name; a glob never matches.
		if strings.Contains(got, `GetInterfaceIP "wg*"`) {
			t.Errorf("%s: glob passed to GetInterfaceIP", name)
		}
	}
}
