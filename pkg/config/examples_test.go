package config

import (
	"path/filepath"
	"reflect"
	"testing"
)

// The examples are documentation: each must be valid, and the HCL version of
// a plan must load to the same plan as its YAML version.
func TestExamples(t *testing.T) {
	for _, name := range []string{"single-node", "cluster"} {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join("..", "..", "examples")
			fromYAML, err := LoadFromFile(filepath.Join(dir, name+".yaml"))
			if err != nil {
				t.Fatalf("yaml: %v", err)
			}
			fromHCL, err := LoadFromFile(filepath.Join(dir, name+".hcl"))
			if err != nil {
				t.Fatalf("hcl: %v", err)
			}
			if !reflect.DeepEqual(fromYAML, fromHCL) {
				for _, f := range diffFields(fromYAML, fromHCL) {
					t.Errorf("%s differs between %s.yaml and %s.hcl", f, name, name)
				}
			}
		})
	}
}

// Examples that exist in one format only.
func TestExamplesSingleFormat(t *testing.T) {
	for _, name := range []string{"local-dev.yaml", "add-node.yaml", "decommission-node.yaml", "multi-datacenter.yaml"} {
		if _, err := LoadFromFile(filepath.Join("..", "..", "examples", name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// diffFields names the top-level plan fields that differ.
func diffFields(a, b *DeploymentPlan) []string {
	va, vb := reflect.ValueOf(a).Elem(), reflect.ValueOf(b).Elem()
	var res []string
	for i := 0; i < va.NumField(); i++ {
		if !va.Type().Field(i).IsExported() {
			continue
		}
		if !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			res = append(res, va.Type().Field(i).Name)
		}
	}
	return res
}
