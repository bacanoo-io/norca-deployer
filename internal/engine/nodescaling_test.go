package engine

import (
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"text/template"

	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
	"github.com/hashicorp/hcl"
)

// loadPlanErr loads a plan and returns the error of the checks.
func loadPlanErr(path string) (*config.DeploymentPlan, error) { return config.LoadFromFile(path) }

const nodeScalingPlan = `
metadata: {name: prod, region: eu}
release: {version: v0.2.0}
security:
  tls_enabled: true
  verify_server_hostname: true
  acl: {enabled: true}
  pki:
    spiffe: {enabled: true, trust_domain: prod.example}
tuning:
  firewall:
    cluster_cidrs: [10.0.0.0/16, 10.1.0.0/16]
    api_cidrs: [192.168.1.0/24]
client_defaults:
  drivers: [raw_exec, exec]
  enable_raw_exec: true
  meta: {rack: r1}
node_scaling:
  enabled: true
  validity: 30d
inventory:
  masters:
    - host: 10.0.0.1
      hostname: s1
    - host: 10.0.0.2
      hostname: s2
  clients:
    - host: 10.0.0.9
      hostname: w1
`

// renderTemplate renders the agent configuration template as the operator
// does (norca-platform, package bootstrap): hcl quotes a string.
func renderTemplate(t *testing.T, tmpl string, data map[string]any) string {
	t.Helper()
	tp, err := template.New("agent").Option("missingkey=error").Funcs(template.FuncMap{
		"hcl": strconv.Quote,
		"hclmap": func(m map[string]string) string {
			var b strings.Builder
			b.WriteString("{\n")
			for _, k := range slices.Sorted(maps.Keys(m)) {
				b.WriteString(strconv.Quote(k) + " = " + strconv.Quote(m[k]) + "\n")
			}
			return b.String() + "}"
		},
	}).Parse(tmpl)
	if err != nil {
		t.Fatalf("the template does not parse: %v\n%s", err, tmpl)
	}
	var b strings.Builder
	if err := tp.Execute(&b, data); err != nil {
		t.Fatalf("the template does not render: %v", err)
	}
	return b.String()
}

func nodeData(class string) map[string]any {
	return map[string]any{
		"Name": "workers-dc1-3-1", "Region": "eu", "Datacenter": "dc1", "Pool": "workers", "Class": class,
		"Meta":   map[string]string{"norca.instance": "workers-dc1-3-1", "norca.node_group": "workers-dc1"},
		"CAFile": "/etc/norca/tls/ca.pem", "CertFile": "/etc/norca/tls/client.pem", "KeyFile": "/etc/norca/tls/client-key.pem",
	}
}

func TestGenerateNodeTemplate(t *testing.T) {
	plan := loadPlan(t, nodeScalingPlan)
	tmpl := GenerateNodeTemplate(plan)

	cfg := renderTemplate(t, tmpl, nodeData("gpu"))
	var parsed map[string]any
	if err := hcl.Decode(&parsed, cfg); err != nil {
		t.Fatalf("the rendered configuration is not HCL: %v\n%s", err, cfg)
	}
	for _, want := range []string{
		`name       = "workers-dc1-3-1"`, `region     = "eu"`, `datacenter = "dc1"`,
		`node_pool  = "workers"`, `node_class = "gpu"`,
		`cert_file = "/etc/norca/tls/client.pem"`, `verify_server_hostname = true`,
		`"10.0.0.1:4647"`, `"10.0.0.2:4647"`,
		`"norca.instance" = "workers-dc1-3-1"`, `"norca.node_group" = "workers-dc1"`,
		`"driver.allowlist" = "raw_exec,exec"`, `plugin "raw_exec"`, "acl {",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("the configuration has no %s", want)
		}
	}
	// Nothing of one node of the plan: no address, no meta of the plan, no
	// server.
	for _, unwanted := range []string{"advertise {", "addresses {", "10.0.0.9", "rack", "server {", "bootstrap_expect"} {
		if strings.Contains(cfg, unwanted) {
			t.Errorf("the configuration has %q:\n%s", unwanted, cfg)
		}
	}

	// Without a class there is no node_class line.
	if cfg := renderTemplate(t, tmpl, nodeData("")); strings.Contains(cfg, "node_class") {
		t.Errorf("an empty class is written:\n%s", cfg)
	}
}

func TestNodeScalingBootstrap(t *testing.T) {
	plan := loadPlan(t, nodeScalingPlan)
	if err := NewRunner(plan, nil).initPKI(); err != nil {
		t.Fatal(err)
	}
	// Without a node CA the bootstrap says how to make one.
	if _, err := nodeScalingBootstrap(plan); err == nil || !strings.Contains(err.Error(), "pki issue-node-ca") {
		t.Fatalf("want a missing node CA, got %v", err)
	}
	files, err := IssueNodeCA(plan, NodeCAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	items, err := nodeScalingBootstrap(plan)
	if err != nil {
		t.Fatal(err)
	}
	nodeCA, _ := os.ReadFile(files[0])
	nodeKey, _ := os.ReadFile(files[1])
	caCert, _, _ := caPaths(plan)
	ca, _ := os.ReadFile(caCert)
	for item, want := range map[string]string{
		"ca": string(ca), "node_ca": string(nodeCA), "node_ca_key": string(nodeKey),
		"trust_domain": "prod.example", "validity": "30d", "service": "norca",
	} {
		if items[item] != want {
			t.Errorf("item %s = %q", item, items[item])
		}
	}
	if !strings.Contains(items["agent_config"], "{{hcl .Name}}") || items["tls_dir"] == "" ||
		!strings.HasSuffix(items["config_file"], "/norca.hcl") {
		t.Errorf("items: tls_dir %q config_file %q", items["tls_dir"], items["config_file"])
	}

	network := nodeScalingNetwork(plan)
	if network["cluster_cidrs"] != "10.0.0.0/16,10.1.0.0/16" || network["api_cidrs"] != "192.168.1.0/24" {
		t.Fatalf("network: %v", network)
	}
}

func TestNodeScalingPlanChecks(t *testing.T) {
	for name, bad := range map[string]string{
		"a bad validity": strings.Replace(nodeScalingPlan, "validity: 30d", "validity: soon", 1),
		"no TLS":         strings.Replace(nodeScalingPlan, "tls_enabled: true", "tls_enabled: false", 1),
	} {
		path := t.TempDir() + "/plan.yaml"
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadPlanErr(path); err == nil || !strings.Contains(err.Error(), "node_scaling") {
			t.Errorf("%s: want a node_scaling error, got %v", name, err)
		}
	}
}
