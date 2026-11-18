// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package plan

import (
	"errors"
	"fmt"
	"regexp"
)

// RiggerConfig is the rigger section of a plan: a stack of Rigger (design of
// Rigger, section 13.1), applied by the rigger program after the packs. The
// stack is a directory next to the plan (stack), or its stack.hcl written in
// the plan (stack_hcl).
type RiggerConfig struct {
	// Stack is the directory of the stack, relative to the plan: its
	// stack.hcl, own modules, values and rigger.lock.hcl.
	Stack string `yaml:"stack,omitempty" json:"stack,omitempty"`
	// StackHCL is the content of a stack.hcl, for a stack of modules from
	// registries only, written in the plan.
	StackHCL string `yaml:"stack_hcl,omitempty" json:"stack_hcl,omitempty"`
	// Values are value files of the stack, relative to the plan.
	Values []string `yaml:"values,omitempty" json:"values,omitempty"`
	// Vars are values of variables of the stack, as rigger's -var.
	Vars map[string]string `yaml:"vars,omitempty" json:"vars,omitempty"`
	// Prune uninstalls the releases the stack owns and no longer has.
	Prune bool `yaml:"prune,omitempty" json:"prune,omitempty"`
	// Binary is the rigger program; rigger on the PATH by default.
	Binary string `yaml:"binary,omitempty" json:"binary,omitempty"`
}

var riggerVarName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// validateRigger checks the rigger section: one form of the stack, and names
// of variables.
func (p *DeploymentPlan) validateRigger() error {
	r := p.Rigger
	if r == nil {
		return nil
	}

	switch {
	case r.Stack == "" && r.StackHCL == "":
		return errors.New("rigger: stack or stack_hcl is required")
	case r.Stack != "" && r.StackHCL != "":
		return errors.New("rigger: stack and stack_hcl exclude each other")
	case r.StackHCL != "" && len(r.Values) > 0:
		return errors.New("rigger: values files belong to a stack directory; with stack_hcl give vars")
	}

	for name := range r.Vars {
		if !riggerVarName.MatchString(name) {
			return fmt.Errorf("rigger: %q is not the name of a variable", name)
		}
	}

	return nil
}
