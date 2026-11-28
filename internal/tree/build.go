// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca-deployer/internal/values"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// The reserved names of the generated stacks (design §4.1): tree-global for
// global/, tree-ns-<ns> for namespaces/<ns>/, whose releases are
// tree-global and tree-<ns>.
const (
	globalStack     = "tree-global"
	namespacePrefix = "tree-ns-"
	releasePrefix   = "tree-"
)

// secretVarPrefix starts the names of the stack variables that carry the
// resolved values of secret items; they are sensitive and travel only in
// the values of the request, never in the files of the stack.
const secretVarPrefix = "tree_secret_"

// Stack is a Rigger stack built from a tree, ready to be sent to the
// servers as files (design §4.1).
type Stack struct {
	// Name is the name of the stack; Namespace and Region those of its
	// rigger block, where the request goes.
	Name      string
	Namespace string
	Region    string
	// Files are the files of the stack by their paths below its directory.
	Files map[string][]byte
	// Values are the values of its variables: the resolved secret items.
	Values map[string]any
	// Sensitive are the resolved secret values, to be redacted from what
	// the servers answer.
	Sensitive []string
}

// secretFile is a secret manifest of namespaces/<ns>/secrets/ (design §8).
type secretFile struct {
	Items     map[string]string `yaml:"items"`
	Overwrite bool              `yaml:"overwrite"`
}

// Build builds the stacks of a tree for a region, in the order they are
// applied: tree-global from global/, tree-ns-<ns> from each
// namespaces/<ns>/, and each written stack of stacks/ with the shared
// modules it uses and its values, runValues (the values of the region)
// over its values/default.vars.hcl. The value forms are resolved with r;
// a value the servers generate ("!generate:name") is written as it is. A
// value of the run that no written stack declares is warned about in
// t.Warnings.
func Build(t *Tree, region string, runValues map[string]any, r *values.Resolver) ([]*Stack, error) {
	var (
		stacks []*Stack
		errs   []error
	)

	if len(t.Global) > 0 {
		s, err := buildStack(globalStack, globalStack, t.Manifest.Tree.Namespace, region, t.Global, nil, r)
		if err != nil {
			errs = append(errs, err)
		} else {
			stacks = append(stacks, s)
		}
	}

	for _, ns := range t.Namespaces {
		if len(ns.Files) == 0 && len(ns.Secrets) == 0 {
			continue
		}

		s, err := buildStack(namespacePrefix+ns.Name, releasePrefix+ns.Name, ns.Name, region, ns.Files, ns.Secrets, r)
		if err != nil {
			errs = append(errs, err)
		} else {
			stacks = append(stacks, s)
		}
	}

	used := map[string]bool{}

	for _, ws := range t.Stacks {
		s, err := buildWritten(t, ws, region, runValues, r, used)
		if err != nil {
			errs = append(errs, err)
		} else {
			stacks = append(stacks, s)
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	t.Warnings = append(t.Warnings, unusedValues(runValues, used)...)

	return order(stacks, t.Manifest.StackOrder)
}

// order puts the stacks in the order of design §4.1: tree-global first, then
// those named in stack_order, in that order, then the other generated
// namespace stacks in name order, then the other written stacks.
func order(stacks []*Stack, stackOrder []string) ([]*Stack, error) {
	byName := map[string]*Stack{}
	for _, s := range stacks {
		byName[s.Name] = s
	}

	var out []*Stack

	if s := byName[globalStack]; s != nil {
		out = append(out, s)
		delete(byName, globalStack)
	}

	var errs []error

	for _, name := range stackOrder {
		s := byName[name]
		if s == nil {
			errs = append(errs, fmt.Errorf("stack_order: %s is not a stack of the tree", name))

			continue
		}

		out = append(out, s)
		delete(byName, name)
	}

	rest := make([]string, 0, len(byName))
	for name := range byName {
		rest = append(rest, name)
	}

	slices.SortFunc(rest, func(a, b string) int {
		// Generated namespace stacks before written ones, then by name.
		ga, gb := strings.HasPrefix(a, namespacePrefix), strings.HasPrefix(b, namespacePrefix)
		switch {
		case ga && !gb:
			return -1
		case gb && !ga:
			return 1
		}

		return strings.Compare(a, b)
	})

	for _, name := range rest {
		out = append(out, byName[name])
	}

	return out, errors.Join(errs...)
}

// buildStack builds one generated stack: stack.hcl with one module block,
// the implicit module of the plain files below modules/<release>/, and the
// secret blocks and their sensitive variables.
func buildStack(name, release, namespace, region string, files, secrets []File, r *values.Resolver) (*Stack, error) {
	s := &Stack{Name: name, Namespace: namespace, Region: region, Files: map[string][]byte{}, Values: map[string]any{}}
	moduleDir := "modules/" + release

	byName := map[string]string{}

	var errs []error

	for _, f := range files {
		target := moduleDir + "/" + path.Base(f.Path)

		if other, ok := byName[target]; ok {
			errs = append(errs, fmt.Errorf("%s and %s are both the %s %q of the stack %s", other, f.Path, f.Kind, f.Name, name))

			continue
		}

		byName[target] = f.Path
		s.Files[target] = f.Data
	}

	s.Files[moduleDir+"/metadata.hcl"] = metadataHCL(release)
	s.Files["stack.hcl"] = stackHCL(name, release, namespace, region)

	if len(secrets) > 0 {
		secretsHCL, variablesHCL, err := s.secretBlocks(secrets, r)
		if err != nil {
			errs = append(errs, err)
		} else {
			s.Files["secrets.hcl"] = secretsHCL
			if variablesHCL != nil {
				s.Files["variables.hcl"] = variablesHCL
			}
		}
	}

	return s, errors.Join(errs...)
}

// stackHCL is the stack.hcl of a generated stack.
func stackHCL(name, release, namespace, region string) []byte {
	f := hclwrite.NewEmptyFile()
	body := f.Body()

	rig := body.AppendNewBlock("rigger", nil).Body()
	rig.SetAttributeValue("name", cty.StringVal(name))
	rig.SetAttributeValue("namespace", cty.StringVal(namespace))
	rig.SetAttributeValue("region", cty.StringVal(region))

	body.AppendNewline()
	body.AppendNewBlock("module", []string{release}).Body().SetAttributeValue("source", cty.StringVal("./modules/"+release))

	return hclwrite.Format(f.Bytes())
}

// metadataHCL is the metadata.hcl of the implicit module of a generated
// stack: a module of the stack's own needs no version (design §13.4).
func metadataHCL(release string) []byte {
	f := hclwrite.NewEmptyFile()
	f.Body().AppendNewBlock("module", nil).Body().SetAttributeValue("name", cty.StringVal(release))

	return hclwrite.Format(f.Bytes())
}

// secretBlocks are secrets.hcl and variables.hcl of the secrets of a
// namespace: a secret block per manifest, and a sensitive variable for each
// item whose value is resolved here, so that the value travels only in the
// values of the request (design §13.4, row 2).
func (s *Stack) secretBlocks(secrets []File, r *values.Resolver) (secretsHCL, variablesHCL []byte, err error) {
	sf := hclwrite.NewEmptyFile()
	vf := hclwrite.NewEmptyFile()

	n := 0

	var errs []error

	for _, f := range secrets {
		var m secretFile
		if err := config.DecodeInto(f.Data, config.FormatHCL, &m); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.Path, err))

			continue
		}

		if len(m.Items) == 0 {
			errs = append(errs, fmt.Errorf("%s: a secret has items", f.Path))

			continue
		}

		var items []hclwrite.ObjectAttrTokens

		add := func(key string, value hclwrite.Tokens) {
			items = append(items, hclwrite.ObjectAttrTokens{Name: hclwrite.TokensForValue(cty.StringVal(key)), Value: value})
		}

		for _, key := range slices.Sorted(maps.Keys(m.Items)) {
			raw := m.Items[key]

			if values.IsGenerated(raw) {
				add(key, hclwrite.TokensForValue(cty.StringVal(raw)))

				continue
			}

			v, err := r.Resolve(raw)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: item %s: %w", f.Path, key, err))

				continue
			}

			if v == raw {
				// Plain text: it is in the tree already.
				add(key, hclwrite.TokensForValue(cty.StringVal(v)))

				continue
			}

			n++
			varName := fmt.Sprintf("%s%d", secretVarPrefix, n)
			vb := vf.Body().AppendNewBlock("variable", []string{varName}).Body()
			vb.SetAttributeValue("sensitive", cty.True)
			vf.Body().AppendNewline()

			s.Values[varName] = v
			s.Sensitive = append(s.Sensitive, v)
			add(key, hclwrite.TokensForTraversal(hcl.Traversal{hcl.TraverseRoot{Name: "var"}, hcl.TraverseAttr{Name: varName}}))
		}

		block := sf.Body().AppendNewBlock("secret", []string{f.Name}).Body()
		block.SetAttributeRaw("items", hclwrite.TokensForObject(items))

		if m.Overwrite {
			block.SetAttributeValue("overwrite", cty.True)
		}

		sf.Body().AppendNewline()
	}

	if err := errors.Join(errs...); err != nil {
		return nil, nil, err
	}

	secretsHCL = hclwrite.Format(sf.Bytes())
	if n > 0 {
		variablesHCL = hclwrite.Format(vf.Bytes())
	}

	return secretsHCL, variablesHCL, nil
}
