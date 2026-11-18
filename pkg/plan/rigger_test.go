// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package plan

import (
	"strings"
	"testing"
)

func TestValidateRigger(t *testing.T) {
	tests := []struct {
		rigger *RiggerConfig
		want   string
	}{
		{rigger: &RiggerConfig{Stack: "stacks/platform", Vars: map[string]string{"count": "3"}}},
		{rigger: &RiggerConfig{StackHCL: "rigger {}"}},
		{rigger: &RiggerConfig{}, want: "stack or stack_hcl is required"},
		{rigger: &RiggerConfig{Stack: "a", StackHCL: "b"}, want: "exclude each other"},
		{rigger: &RiggerConfig{StackHCL: "b", Values: []string{"v.hcl"}}, want: "with stack_hcl give vars"},
		{rigger: &RiggerConfig{Stack: "a", Vars: map[string]string{"Count": "3"}}, want: `"Count" is not the name of a variable`},
	}

	for _, tt := range tests {
		err := (&DeploymentPlan{Rigger: tt.rigger}).validateRigger()
		if (err == nil) != (tt.want == "") || (err != nil && !strings.Contains(err.Error(), tt.want)) {
			t.Errorf("validateRigger(%+v) = %v, want %q", tt.rigger, err, tt.want)
		}
	}
}

func TestDecodeHCLRigger(t *testing.T) {
	src := `
rigger {
  stack  = "stacks/platform"
  values = ["stacks/platform/values/prod.vars.hcl"]
  vars = {
    count = "3"
  }
  prune = true
}
`

	var p DeploymentPlan
	if err := decodeHCL([]byte(src), &p); err != nil {
		t.Fatal(err)
	}

	if p.Rigger == nil || p.Rigger.Stack != "stacks/platform" || len(p.Rigger.Values) != 1 || p.Rigger.Vars["count"] != "3" || !p.Rigger.Prune {
		t.Fatalf("rigger = %+v", p.Rigger)
	}
}
