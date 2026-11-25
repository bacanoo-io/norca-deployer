// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package plan

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// RiggerConfig is the rigger section of a plan: a stack of Rigger (design of
// Rigger, section 13.1), applied by the rigger program. The stack is a
// directory next to the plan (stack_dir), or its stack.hcl written in the
// plan (stack). The section also holds the GitOps bindings, which apply
// stacks of repositories (gitops); it may hold them alone.
type RiggerConfig struct {
	// StackDir is the directory of the stack, relative to the plan: its
	// stack.hcl, own modules, values and rigger.lock.hcl.
	StackDir string `yaml:"stack_dir,omitempty" json:"stack_dir,omitempty"`
	// Stack is the text of a stack.hcl, for a stack of modules from
	// registries only, written in the plan.
	Stack string `yaml:"stack,omitempty" json:"stack,omitempty" plan:"content"`
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
	// SyncRegistries are the Rigger registries of the cluster that the
	// servers sync before the stack is applied (rigger registry sync), in
	// this order. The servers fetch the modules of a stack only from a
	// registry they have synced, and a new cluster has synced none.
	SyncRegistries []string `yaml:"sync_registries,omitempty" json:"sync_registries,omitempty"`
	// GitOps are the GitOps bindings: stacks in repositories, apart from
	// the stack of the plan and its settings above.
	GitOps *GitOpsConfig `yaml:"gitops,omitempty" json:"gitops,omitempty"`
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

// HasStack reports whether the section has a stack of the plan; one with
// GitOps bindings only has none.
func (r *RiggerConfig) HasStack() bool { return r.StackDir != "" || r.Stack != "" }

// UnmarshalYAML reads the section and refuses a key it does not have: a
// key that is ignored would leave out a stack or a setting of it.
func (r *RiggerConfig) UnmarshalYAML(node *yaml.Node) error {
	if err := knownKeys(node, reflect.TypeFor[RiggerConfig](), "rigger"); err != nil {
		return err
	}

	type plain RiggerConfig

	return node.Decode((*plain)(r))
}

// onlyGitOps reports a section of GitOps bindings without a stack of the
// plan and without a setting of one. run_on and mode count when they are
// not their defaults, which a loaded plan has written out.
func (r *RiggerConfig) onlyGitOps() bool {
	return r.GitOps != nil && !r.HasStack() &&
		len(r.Values) == 0 && len(r.Vars) == 0 && !r.Prune && r.Binary == "" && len(r.SyncRegistries) == 0 &&
		r.RunsOnServer() && r.Embedded()
}

// RunsOnServer reports whether norca rigger on the first server applies the
// stack, which is the default.
func (r *RiggerConfig) RunsOnServer() bool { return r.RunOn != RiggerOnLocal }

// Embedded reports whether the servers apply the stack, which is the
// default.
func (r *RiggerConfig) Embedded() bool { return r.Mode != RiggerStandalone }

// looksLikeDirectory reports a stack that is a name rather than the text of
// a stack.hcl: one line, without a block or an attribute.
func looksLikeDirectory(stack string) bool {
	s := strings.TrimSpace(stack)

	return s != "" && !strings.ContainsAny(s, "\n{=")
}

var riggerVarName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// riggerRegistryName is the name of a registry as an argument of the
// program: it does not read as a flag.
var riggerRegistryName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// validateRigger checks the rigger section: one form of the stack, names of
// variables, where the program runs and who applies. An empty run_on or
// mode is its default.
func (p *DeploymentPlan) validateRigger() error {
	r := p.Rigger
	if r == nil {
		return nil
	}

	switch {
	case r.onlyGitOps():
		return nil
	case r.StackDir == "" && r.Stack == "":
		return errors.New("rigger: stack or stack_dir is required")
	case r.StackDir != "" && r.Stack != "":
		return errors.New("rigger: stack and stack_dir exclude each other")
	case looksLikeDirectory(r.Stack):
		return fmt.Errorf("rigger: stack is the text of a stack.hcl, and %q looks like a directory: a directory is stack_dir", r.Stack)
	case r.Stack != "" && len(r.Values) > 0:
		return errors.New("rigger: values files belong to stack_dir; with stack give vars")
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

	if len(r.SyncRegistries) > 0 && !r.Embedded() {
		return errors.New("rigger: sync_registries is of mode: embedded, where the servers fetch the modules; with standalone the program fetches them")
	}

	for i, name := range r.SyncRegistries {
		if !riggerRegistryName.MatchString(name) {
			return fmt.Errorf("rigger: sync_registries: %q is not the name of a registry", name)
		}

		if slices.Contains(r.SyncRegistries[:i], name) {
			return fmt.Errorf("rigger: sync_registries: %q is named twice", name)
		}
	}

	return nil
}
