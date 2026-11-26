package plan

import (
	"reflect"
	"strings"
	"testing"
)

const consulPlanHeadYAML = "inventory:\n  masters:\n    - host: 10.0.0.1\n"
const consulPlanHeadHCL = "inventory {\n  masters {\n    host = \"10.0.0.1\"\n  }\n}\n"

// The block reads the same from YAML and HCL, with the defaults filled in.
func TestConsulDNSForwardingDecode(t *testing.T) {
	tests := []struct {
		name   string
		format Format
		src    string
		want   ConsulDNSForwarding
	}{
		{
			name:   "yaml defaults",
			format: FormatYAML,
			src:    consulPlanHeadYAML + "consul:\n  dns_forwarding:\n    enabled: true\n",
			want:   ConsulDNSForwarding{Enabled: true, Address: "127.0.0.1:8600", Domain: "consul", Method: DNSMethodAuto},
		},
		{
			name:   "hcl defaults",
			format: FormatHCL,
			src:    consulPlanHeadHCL + "consul {\n  dns_forwarding {\n    enabled = true\n  }\n}\n",
			want:   ConsulDNSForwarding{Enabled: true, Address: "127.0.0.1:8600", Domain: "consul", Method: DNSMethodAuto},
		},
		{
			name:   "yaml every key",
			format: FormatYAML,
			src: consulPlanHeadYAML + `consul:
  address: 127.0.0.1:8500
  dns_forwarding:
    enabled: true
    address: 127.0.0.1:18600
    domain: dc1.example
    method: systemd-resolved
    docker: true
`,
			want: ConsulDNSForwarding{Enabled: true, Address: "127.0.0.1:18600", Domain: "dc1.example", Method: DNSMethodResolved, Docker: true},
		},
		{
			name:   "hcl every key",
			format: FormatHCL,
			src: consulPlanHeadHCL + `consul {
  address = "127.0.0.1:8500"
  dns_forwarding {
    enabled = true
    address = "127.0.0.1:18600"
    domain  = "dc1.example"
    method  = "systemd-resolved"
    docker  = true
  }
}
`,
			want: ConsulDNSForwarding{Enabled: true, Address: "127.0.0.1:18600", Domain: "dc1.example", Method: DNSMethodResolved, Docker: true},
		},
		{
			name:   "disabled",
			format: FormatYAML,
			src:    consulPlanHeadYAML + "consul:\n  dns_forwarding:\n    enabled: false\n    method: dnsmasq\n",
			want:   ConsulDNSForwarding{Address: "127.0.0.1:8600", Domain: "consul", Method: DNSMethodDnsmasq},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Parse([]byte(tt.src), tt.format, t.TempDir())
			if err != nil {
				t.Fatalf("Parse() = %v", err)
			}
			if p.Consul == nil || p.Consul.DNSForwarding == nil {
				t.Fatalf("consul = %+v, want a dns_forwarding block", p.Consul)
			}
			if got := *p.Consul.DNSForwarding; !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("dns_forwarding = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// The same plan in both formats loads to the same block.
func TestConsulDNSForwardingYAMLHCLParity(t *testing.T) {
	fromYAML, err := Parse([]byte(consulPlanHeadYAML+`consul:
  dns_forwarding:
    enabled: true
    address: "[::1]:8600"
    method: dnsmasq
`), FormatYAML, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fromHCL, err := Parse([]byte(consulPlanHeadHCL+`consul {
  dns_forwarding {
    enabled = true
    address = "[::1]:8600"
    method  = "dnsmasq"
  }
}
`), FormatHCL, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromYAML.Consul, fromHCL.Consul) {
		t.Fatalf("yaml %+v != hcl %+v", fromYAML.Consul.DNSForwarding, fromHCL.Consul.DNSForwarding)
	}
}

func TestConsulDNSForwardingEnabled(t *testing.T) {
	tests := []struct {
		name string
		c    *ConsulConfig
		want bool
	}{
		{name: "no consul block", c: nil, want: false},
		{name: "no dns_forwarding", c: &ConsulConfig{}, want: false},
		{name: "disabled", c: &ConsulConfig{DNSForwarding: &ConsulDNSForwarding{}}, want: false},
		{name: "enabled", c: &ConsulConfig{DNSForwarding: &ConsulDNSForwarding{Enabled: true}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.DNSForwardingEnabled(); got != tt.want {
				t.Fatalf("DNSForwardingEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConsulDNSForwardingNeedsResolved(t *testing.T) {
	tests := []struct {
		name string
		c    *ConsulConfig
		want bool
	}{
		{name: "no consul block", c: nil},
		{name: "disabled with systemd-resolved", c: &ConsulConfig{DNSForwarding: &ConsulDNSForwarding{Method: DNSMethodResolved, Docker: true}}},
		{name: "auto", c: &ConsulConfig{DNSForwarding: &ConsulDNSForwarding{Enabled: true, Method: DNSMethodAuto}}},
		{name: "dnsmasq", c: &ConsulConfig{DNSForwarding: &ConsulDNSForwarding{Enabled: true, Method: DNSMethodDnsmasq}}},
		{name: "systemd-resolved", c: &ConsulConfig{DNSForwarding: &ConsulDNSForwarding{Enabled: true, Method: DNSMethodResolved}}, want: true},
		{name: "auto with docker", c: &ConsulConfig{DNSForwarding: &ConsulDNSForwarding{Enabled: true, Method: DNSMethodAuto, Docker: true}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.DNSForwardingNeedsResolved(); got != tt.want {
				t.Fatalf("DNSForwardingNeedsResolved() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConsulDNSForwardingValidation(t *testing.T) {
	tests := []struct {
		name string
		body string // keys under dns_forwarding
		want string // "" = valid
	}{
		{name: "loopback", body: "address: 127.0.0.1:8600"},
		{name: "ipv6 loopback", body: `address: "[::1]:8600"`},
		{name: "another IP", body: "address: 10.0.0.5:53"},
		{name: "subdomain", body: "domain: dc1.consul"},
		{name: "explicit dnsmasq", body: "method: dnsmasq"},
		{name: "docker with resolved", body: "method: systemd-resolved\n    docker: true"},
		{name: "docker with auto", body: "docker: true"},
		{name: "no port", body: "address: 127.0.0.1", want: "consul.dns_forwarding.address"},
		{name: "host name", body: "address: localhost:8600", want: "must be an IP address"},
		{name: "port zero", body: "address: 127.0.0.1:0", want: "port"},
		{name: "port too big", body: "address: 127.0.0.1:70000", want: "port"},
		{name: "domain with space", body: `domain: "my consul"`, want: "consul.dns_forwarding.domain"},
		{name: "domain with trailing dot", body: "domain: consul.", want: "consul.dns_forwarding.domain"},
		{name: "domain label starts with a hyphen", body: "domain: -consul", want: "consul.dns_forwarding.domain"},
		{name: "domain label too long", body: "domain: " + strings.Repeat("a", 64), want: "consul.dns_forwarding.domain"},
		{name: "domain with a slash", body: "domain: con/sul", want: "consul.dns_forwarding.domain"},
		{name: "unknown method", body: "method: unbound", want: "auto, systemd-resolved or dnsmasq"},
		{name: "docker with dnsmasq", body: "method: dnsmasq\n    docker: true", want: "docker needs systemd-resolved"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := consulPlanHeadYAML + "consul:\n  dns_forwarding:\n    enabled: true\n    " + tt.body + "\n"
			_, err := Parse([]byte(src), FormatYAML, t.TempDir())
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Parse() = %v, want the plan loaded", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse() = %v, want an error with %q", err, tt.want)
			}
		})
	}
}

// A disabled block is checked too, so that a mistake shows before it is
// turned on.
func TestConsulDNSForwardingDisabledIsValidated(t *testing.T) {
	src := consulPlanHeadYAML + "consul:\n  dns_forwarding:\n    enabled: false\n    method: unbound\n"
	if _, err := Parse([]byte(src), FormatYAML, t.TempDir()); err == nil {
		t.Fatal("Parse() = nil, want the unknown method refused also while disabled")
	}
}
