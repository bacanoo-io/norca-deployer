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
	// RunOn is where the program runs: server (the default), norca rigger
	// on the first server; local, the rigger program on the machine of the
	// deployer.
	RunOn string `yaml:"run_on,omitempty" json:"run_on,omitempty"`
	// Mode is who applies the stack: embedded (the default), the servers,
	// to which the program sends it (-server); standalone, the program.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// Binary is the rigger program of run_on local; rigger on the PATH by
	// default.
	Binary string `yaml:"binary,omitempty" json:"binary,omitempty"`
}

// The values of rigger.run_on.
const (
	RiggerOnServer = "server"
	RiggerOnLocal  = "local"
)

// The values of rigger.mode.
const (
	RiggerEmbedded   = "embedded"
	RiggerStandalone = "standalone"
)

// RunsOnServer reports whether norca rigger on the first server applies the
// stack, which is the default.
func (r *RiggerConfig) RunsOnServer() bool { return r.RunOn != RiggerOnLocal }

// Embedded reports whether the servers apply the stack, which is the
// default.
func (r *RiggerConfig) Embedded() bool { return r.Mode != RiggerStandalone }

var riggerVarName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// validateRigger checks the rigger section: one form of the stack, names of
// variables, where the program runs and who applies. An empty run_on or
// mode is its default.
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

	switch r.RunOn {
	case "", RiggerOnServer, RiggerOnLocal:
	default:
		return fmt.Errorf("rigger: run_on %q is not %s or %s", r.RunOn, RiggerOnServer, RiggerOnLocal)
	}

	switch r.Mode {
	case "", RiggerEmbedded, RiggerStandalone:
	default:
		return fmt.Errorf("rigger: mode %q is not %s or %s", r.Mode, RiggerEmbedded, RiggerStandalone)
	}

	if r.Binary != "" && r.RunsOnServer() {
		return errors.New("rigger: binary is the program of run_on: local; on the first server norca rigger runs")
	}

	for name := range r.Vars {
		if !riggerVarName.MatchString(name) {
			return fmt.Errorf("rigger: %q is not the name of a variable", name)
		}
	}

	return nil
}
