package config

import (
	"strings"
	"testing"
)

func TestDecodeHCLErrors(t *testing.T) {
	cases := map[string]struct {
		src  string
		want string
	}{
		"unknown key":    {`metadata { nme = "x" }`, `metadata: unknown key "nme"`},
		"repeated block": {"metadata { name = \"a\" }\nmetadata { name = \"b\" }", "metadata: block given 2 times"},
		"syntax":         {`metadata {`, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := decodeHCL([]byte(tc.src), &DeploymentPlan{})
			if err == nil {
				t.Fatal("expected an error")
			}
			if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestDecodeHCLPackValues(t *testing.T) {
	src := `
packs {
  registry {
    name = "builtin"
    url  = "builtin"
  }
  deploy {
    name    = "web"
    pack    = "test-app"
    enabled = true
    values {
      count = 2
      extra_env {
        LOG = "debug"
      }
      datacenters = ["dc1"]
    }
  }
}`
	var p DeploymentPlan
	if err := decodeHCL([]byte(src), &p); err != nil {
		t.Fatal(err)
	}
	v := p.Packs.Deploy[0].Values
	env, ok := v["extra_env"].(map[string]interface{})
	if v["count"] != 2 || !ok || env["LOG"] != "debug" {
		t.Fatalf("values = %#v", v)
	}
}

func TestNetworkValidation(t *testing.T) {
	plan := func(mod func(*DeploymentPlan)) error {
		p := &DeploymentPlan{Inventory: InventoryConfig{Masters: []NodeConfig{{Host: "server1.example.com", Hostname: "s1"}}}}
		mod(p)
		p.SetDefaults()
		return p.validateNetwork()
	}
	if err := plan(func(*DeploymentPlan) {}); err == nil || !strings.Contains(err.Error(), "must be an IP address") {
		t.Fatalf("a DNS name cannot be bound to: %v", err)
	}
	for name, mod := range map[string]func(*DeploymentPlan){
		"cluster_address":  func(p *DeploymentPlan) { p.Inventory.Masters[0].ClusterAddress = "10.0.0.1" },
		"interface_prefix": func(p *DeploymentPlan) { p.Network.InterfacePrefix = "wg" },
		"bind_address":     func(p *DeploymentPlan) { p.Network.BindAddress = "0.0.0.0" },
	} {
		if err := plan(mod); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := plan(func(p *DeploymentPlan) {
		p.Network.BindAddress = "0.0.0.0"
		p.Tuning.Firewall = &FirewallConfig{ClusterCIDRs: []string{"10.0.0.0/33"}}
	}); err == nil || !strings.Contains(err.Error(), "cluster_cidrs") {
		t.Fatalf("invalid CIDR accepted: %v", err)
	}
	if err := plan(func(p *DeploymentPlan) {
		p.Network.BindAddress = "0.0.0.0"
		p.Tuning.Firewall = &FirewallConfig{DefaultDeny: true}
	}); err == nil || !strings.Contains(err.Error(), "apply_firewall_rules") {
		t.Fatalf("default_deny without firewall rules accepted: %v", err)
	}
}

func TestSPIFFEConfig(t *testing.T) {
	plan := func(mod func(*DeploymentPlan)) (*DeploymentPlan, error) {
		p := &DeploymentPlan{
			Metadata:  MetadataConfig{Name: "Prod", Region: "eu"},
			Security:  SecurityConfig{TLSEnabled: true, PKI: &PKIConfig{SPIFFE: &SPIFFEConfig{Enabled: true}}},
			Network:   NetworkConfig{BindAddress: "0.0.0.0"},
			Inventory: InventoryConfig{Masters: []NodeConfig{{Host: "10.0.0.1", Hostname: "s1"}}},
		}
		mod(p)
		p.SetDefaults()
		return p, p.validateSPIFFE()
	}
	p, err := plan(func(*DeploymentPlan) {})
	if err != nil || p.Security.PKI.SPIFFE.TrustDomain != "prod.norca" {
		t.Fatalf("default trust domain: %v %v", p.Security.PKI.SPIFFE.TrustDomain, err)
	}
	if _, err := plan(func(p *DeploymentPlan) { p.Security.PKI.SPIFFE.TrustDomain = "Bad Domain" }); err == nil {
		t.Fatal("invalid trust domain accepted")
	}
	if _, err := plan(func(p *DeploymentPlan) { p.Security.TLSEnabled = false }); err == nil || !strings.Contains(err.Error(), "tls_enabled") {
		t.Fatalf("SPIFFE without TLS accepted: %v", err)
	}
	if _, err := plan(func(p *DeploymentPlan) { p.Inventory.Masters[0].Hostname = "s 1" }); err == nil {
		t.Fatal("a node name that is no SPIFFE path segment was accepted")
	}
}

func TestBootstrapValidation(t *testing.T) {
	base := func() *DeploymentPlan {
		return &DeploymentPlan{
			Network:   NetworkConfig{BindAddress: "0.0.0.0"},
			Security:  SecurityConfig{ACL: &ACLConfig{Enabled: true, AutoBootstrap: true}},
			Inventory: InventoryConfig{Masters: []NodeConfig{{Host: "10.0.0.1", Hostname: "s1"}}},
		}
	}
	for name, tc := range map[string]struct {
		mod  func(*DeploymentPlan)
		want string
	}{
		"bad secret path": {func(p *DeploymentPlan) {
			p.Secrets = &SecretsConfig{Bootstrap: []BootstrapSecret{{Path: "../x", Items: map[string]string{"a": "b"}}}}
		}, "invalid path"},
		"no items": {func(p *DeploymentPlan) {
			p.Secrets = &SecretsConfig{Bootstrap: []BootstrapSecret{{Path: "a/b"}}}
		}, "items are required"},
		"bad generate": {func(p *DeploymentPlan) {
			p.Secrets = &SecretsConfig{Bootstrap: []BootstrapSecret{{Path: "a/b", Items: map[string]string{"p": "!generate:Bad Name"}}}}
		}, "lower case"},
		"missing file": {func(p *DeploymentPlan) {
			p.Secrets = &SecretsConfig{Bootstrap: []BootstrapSecret{{Path: "a/b", Items: map[string]string{"ca": "@missing.pem"}}}}
		}, "missing.pem"},
		"bad store": {func(p *DeploymentPlan) {
			p.Secrets = &SecretsConfig{Store: "vault", Bootstrap: []BootstrapSecret{{Path: "a/b", Items: map[string]string{"a": "b"}}}}
		}, "builtin or external"},
		"policy without rules": {func(p *DeploymentPlan) {
			p.Security.ACL.Policies = []ACLPolicyConfig{{Name: "x"}}
		}, "rules"},
		"pack registry ambiguous": {func(p *DeploymentPlan) {
			p.Packs = &PacksConfig{
				Registries: []PackRegistryConfig{{Name: "a", URL: "a", Type: "dir"}, {Name: "b", URL: "b", Type: "dir"}},
				Deploy:     []PackDeployItem{{Name: "x", Pack: "redis", Enabled: true}},
			}
		}, "registry is required"},
		"pack registry unknown": {func(p *DeploymentPlan) {
			p.Packs = &PacksConfig{Registries: []PackRegistryConfig{{Name: "a", URL: "a", Type: "dir"}},
				Deploy: []PackDeployItem{{Name: "x", Registry: "b", Pack: "redis", Enabled: true}}}
		}, "unknown registry"},
	} {
		p := base()
		tc.mod(p)
		p.SetDefaults()
		if err := p.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", name, err, tc.want)
		}
	}
}
