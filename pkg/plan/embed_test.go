package plan_test

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/pkg/plan"
)

const embeddedPlan = `
metadata: {name: shop, region: eu, datacenter: dc-a}
release: {version: v0.2.0}
security: {tls_enabled: true}
inventory:
  masters:
    - host: 10.0.0.1
      hostname: s1
`

// A program that embeds the model reads a plan it holds in memory.
func ExampleParse() {
	p, err := plan.Parse([]byte(embeddedPlan), plan.FormatYAML, "")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(p.Metadata.Name, p.Metadata.Region, len(p.Inventory.Masters), p.Kind)
	// Output: shop eu 1 DeploymentPlan
}

func TestParse(t *testing.T) {
	// The same plan in JSON and HCL.
	jsonPlan := `{"metadata": {"name": "shop", "region": "eu"}, "release": {"version": "v0.2.0"},
		"inventory": {"masters": [{"host": "10.0.0.1", "hostname": "s1"}]}}`
	hclPlan := `
metadata {
  name   = "shop"
  region = "eu"
}
release {
  version = "v0.2.0"
}
inventory {
  masters {
    host     = "10.0.0.1"
    hostname = "s1"
  }
}
`
	for format, data := range map[plan.Format]string{plan.FormatYAML: embeddedPlan, plan.FormatJSON: jsonPlan, plan.FormatHCL: hclPlan} {
		p, err := plan.Parse([]byte(data), format, "/srv/plans")
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if p.Metadata.Region != "eu" || len(p.Inventory.Masters) != 1 || p.BaseDir() != "/srv/plans" {
			t.Fatalf("%s: %+v base %s", format, p.Metadata, p.BaseDir())
		}
	}

	// The defaults are applied and the rules checked.
	if _, err := plan.Parse([]byte("metadata: {name: x}\n"), plan.FormatYAML, ""); err == nil ||
		!strings.Contains(err.Error(), "master") {
		t.Fatalf("a plan without servers: %v", err)
	}
	if _, err := plan.Parse([]byte("metadata: ["), plan.FormatYAML, ""); err == nil || !strings.Contains(err.Error(), "YAML") {
		t.Fatalf("a syntax error: %v", err)
	}
	if _, err := plan.Parse([]byte(embeddedPlan), plan.Format("toml"), ""); err == nil {
		t.Fatal("an unknown format is accepted")
	}

	for path, want := range map[string]plan.Format{
		"a.yaml": plan.FormatYAML, "b.YML": plan.FormatYAML, "c.hcl": plan.FormatHCL, "d.json": plan.FormatJSON,
		filepath.Join("x", "plan"): plan.FormatYAML,
	} {
		if got := plan.FormatOf(path); got != want {
			t.Errorf("FormatOf(%q) = %s, want %s", path, got, want)
		}
	}
}
