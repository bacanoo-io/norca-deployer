// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/bacanoo-io/norca-deployer/internal/values"
)

// defaultValuesFile is the values file of a written stack the deployer
// sends (design §4.1, Values of a written stack).
const defaultValuesFile = "values/default.vars.hcl"

// localModulePrefix starts the source of a module of the stack's own
// modules/ directory, where a shared module of the tree is put too.
const localModulePrefix = "./modules/"

// defaultStackRegion is the region of a stack whose rigger block names none.
const defaultStackRegion = "global"

// writtenName is the rule of the name of a directory of stacks/ or
// modules/: the name of a stack, of a module.
var writtenName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// WrittenStack is a directory of stacks/: a Rigger stack written by hand.
type WrittenStack struct {
	// Name is the name of the directory, which is the name of the stack.
	Name string
	// Files are its files by their paths below its directory.
	Files map[string][]byte
}

// addWritten sorts a file of stacks/<name>/ or modules/<name>/.
func (t *Tree) addWritten(p string, parts []string, data []byte) error {
	if len(parts) < 3 {
		return fmt.Errorf("%s: a file of %s/ is in the directory of its %s", p, parts[0], strings.TrimSuffix(parts[0], "s"))
	}

	name := parts[1]
	if !writtenName.MatchString(name) {
		return fmt.Errorf("%s: %q: lower case letters, digits and -, at most 63", p, name)
	}

	rest := strings.Join(parts[2:], "/")

	if parts[0] == "modules" {
		if t.Modules == nil {
			t.Modules = map[string]map[string][]byte{}
		}

		if t.Modules[name] == nil {
			t.Modules[name] = map[string][]byte{}
		}

		t.Modules[name][rest] = data

		return nil
	}

	i := slices.IndexFunc(t.Stacks, func(s *WrittenStack) bool { return s.Name == name })
	if i < 0 {
		t.Stacks = append(t.Stacks, &WrittenStack{Name: name, Files: map[string][]byte{}})
		i = len(t.Stacks) - 1
	}

	t.Stacks[i].Files[rest] = data

	return nil
}

// stackShape is what the deployer reads of a written stack: its rigger
// block, its releases and the sources of their modules, its variables.
type stackShape struct {
	name, namespace, region string
	// sources are the module blocks by their labels, the release names.
	sources map[string]string
	// parts are the labels of the stack blocks (published stacks taken as
	// parts), whose releases are <part>-<release>.
	parts     []string
	variables map[string]bool
}

// readShape parses the .hcl files at the root of a written stack.
func readShape(ws *WrittenStack) (*stackShape, error) {
	sh := &stackShape{sources: map[string]string{}, variables: map[string]bool{}}
	rigger := 0

	var errs []error

	for _, name := range slices.Sorted(maps.Keys(ws.Files)) {
		if strings.Contains(name, "/") || !strings.HasSuffix(name, ".hcl") || name == "rigger.lock.hcl" {
			continue
		}

		where := "stacks/" + ws.Name + "/" + name

		f, diags := hclsyntax.ParseConfig(ws.Files[name], where, hcl.InitialPos)
		if diags.HasErrors() {
			errs = append(errs, diags)

			continue
		}

		body, ok := f.Body.(*hclsyntax.Body)
		if !ok {
			continue
		}

		for _, b := range body.Blocks {
			switch b.Type {
			case "rigger":
				rigger++

				if err := sh.readRigger(b, where); err != nil {
					errs = append(errs, err)
				}
			case "module":
				if len(b.Labels) == 1 {
					sh.sources[b.Labels[0]] = literal(b.Body.Attributes["source"])
				}
			case "stack":
				if len(b.Labels) == 1 {
					sh.parts = append(sh.parts, b.Labels[0])
				}
			case "variable":
				if len(b.Labels) == 1 {
					sh.variables[b.Labels[0]] = true
				}
			}
		}
	}

	switch {
	case rigger == 0:
		errs = append(errs, fmt.Errorf("stacks/%s: no rigger block (stack.hcl)", ws.Name))
	case rigger > 1:
		errs = append(errs, fmt.Errorf("stacks/%s: %d rigger blocks, one is allowed", ws.Name, rigger))
	}

	return sh, errors.Join(errs...)
}

// readRigger reads name, namespace and region of the rigger block; they
// must be plain strings, the deployer routes the stack by them.
func (sh *stackShape) readRigger(b *hclsyntax.Block, where string) error {
	var errs []error

	for key, dst := range map[string]*string{"name": &sh.name, "namespace": &sh.namespace, "region": &sh.region} {
		a := b.Body.Attributes[key]
		if a == nil {
			continue
		}

		if *dst = literal(a); *dst == "" {
			errs = append(errs, fmt.Errorf("%s: rigger.%s is a plain string", where, key))
		}
	}

	return errors.Join(errs...)
}

// literal is the value of an attribute that is a plain string; "" for any
// other.
func literal(a *hclsyntax.Attribute) string {
	if a == nil {
		return ""
	}

	v, diags := a.Expr.Value(nil)
	if diags.HasErrors() || v.IsNull() || !v.IsKnown() || v.Type() != cty.String {
		return ""
	}

	return v.AsString()
}

// buildWritten builds a written stack for a region: its files as written,
// the shared modules it uses, and its values (design §4.1).
func buildWritten(t *Tree, ws *WrittenStack, region string, runValues map[string]any, r *values.Resolver, used map[string]bool) (*Stack, error) {
	sh, err := readShape(ws)
	if err != nil {
		return nil, err
	}

	var errs []error

	where := "stacks/" + ws.Name

	switch {
	case sh.name != ws.Name:
		errs = append(errs, fmt.Errorf("%s: rigger.name is %q; a written stack is named as its directory", where, sh.name))
	case sh.name == globalStack || strings.HasPrefix(sh.name, namespacePrefix):
		errs = append(errs, fmt.Errorf("%s: the name %s is reserved for the stacks the deployer generates", where, sh.name))
	}

	// The servers take a stack without a region as one of global, and
	// refuse it in the request of another region.
	stackRegion := sh.region
	if stackRegion == "" {
		stackRegion = defaultStackRegion
	}

	if stackRegion != region {
		errs = append(errs, fmt.Errorf("%s: the stack is for the region %s, the run goes to %s", where, stackRegion, region))
	}

	for _, release := range slices.Sorted(maps.Keys(sh.sources)) {
		if strings.HasPrefix(release, releasePrefix) {
			errs = append(errs, fmt.Errorf("%s: the release name %s is reserved: releases tree-* are the deployer's", where, release))
		}
	}

	for _, part := range sh.parts {
		if part == "tree" || strings.HasPrefix(part, releasePrefix) {
			errs = append(errs, fmt.Errorf("%s: the stack part %s makes releases %s-*, a reserved name", where, part, part))
		}
	}

	namespace := sh.namespace
	if namespace == "" {
		namespace = "default"
	}

	s := &Stack{Name: ws.Name, Namespace: namespace, Region: region, Files: maps.Clone(ws.Files), Values: map[string]any{}}

	if err := addModules(t, s, sh, where); err != nil {
		errs = append(errs, err)
	}

	if err := s.writtenValues(ws, sh, runValues, r, used); err != nil {
		errs = append(errs, err)
	}

	return s, errors.Join(errs...)
}

// addModules adds the shared modules of the tree that the stack uses as
// ./modules/<name> and does not have itself. A module in both places is an
// error, so it is always clear which one runs.
func addModules(t *Tree, s *Stack, sh *stackShape, where string) error {
	var errs []error

	own := map[string]bool{}

	for p := range s.Files {
		if rest, ok := strings.CutPrefix(p, "modules/"); ok {
			name, _, _ := strings.Cut(rest, "/")
			own[name] = true
		}
	}

	for _, name := range slices.Sorted(maps.Keys(own)) {
		if t.Modules[name] != nil {
			errs = append(errs, fmt.Errorf("%s: the module %s is both in the stack's modules/ and in the tree's modules/", where, name))
		}
	}

	added := map[string]bool{}

	for _, release := range slices.Sorted(maps.Keys(sh.sources)) {
		rest, ok := strings.CutPrefix(sh.sources[release], localModulePrefix)
		if !ok {
			continue
		}

		name, _, _ := strings.Cut(rest, "/")

		switch {
		case own[name] || added[name]:
			continue
		case t.Modules[name] == nil:
			errs = append(errs, fmt.Errorf("%s: module %q: %s%s is neither in the stack nor in the tree's modules/", where, release, localModulePrefix, name))

			continue
		}

		for p, data := range t.Modules[name] {
			s.Files["modules/"+name+"/"+p] = data
		}

		added[name] = true
	}

	return errors.Join(errs...)
}

// writtenValues are the values of a written stack: values/default.vars.hcl
// of the stack, then the values of the run for the variables the stack
// declares; a later source wins per variable. The value forms are resolved
// here; a value a form gave is sensitive.
func (s *Stack) writtenValues(ws *WrittenStack, sh *stackShape, runValues map[string]any, r *values.Resolver, used map[string]bool) error {
	merged := map[string]any{}

	if data, ok := ws.Files[defaultValuesFile]; ok {
		vals, err := parseVars(data, "stacks/"+ws.Name+"/"+defaultValuesFile)
		if err != nil {
			return err
		}

		maps.Copy(merged, vals)
	}

	for name, v := range runValues {
		if sh.variables[name] {
			merged[name] = v
			used[name] = true
		}
	}

	for _, name := range slices.Sorted(maps.Keys(merged)) {
		v, err := s.resolveForms(merged[name], r)
		if err != nil {
			return fmt.Errorf("stacks/%s: value %s: %w", ws.Name, name, err)
		}

		s.Values[name] = v
	}

	return nil
}

// resolveForms resolves the value forms in a tree of values and records
// what they gave as sensitive. A generated value is left to the servers.
func (s *Stack) resolveForms(v any, r *values.Resolver) (any, error) {
	switch t := v.(type) {
	case string:
		if values.IsGenerated(t) {
			return t, nil
		}

		out, err := r.Resolve(t)
		if err != nil {
			return nil, err
		}

		if out != t && out != "" {
			s.Sensitive = append(s.Sensitive, out)
		}

		return out, nil
	case map[string]any:
		out := make(map[string]any, len(t))

		for k, e := range t {
			x, err := s.resolveForms(e, r)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}

			out[k] = x
		}

		return out, nil
	case []any:
		out := make([]any, len(t))

		for i, e := range t {
			x, err := s.resolveForms(e, r)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}

			out[i] = x
		}

		return out, nil
	}

	return v, nil
}

// parseVars reads a values file (*.vars.hcl): attributes of plain values,
// as JSON gives them.
func parseVars(data []byte, where string) (map[string]any, error) {
	f, diags := hclsyntax.ParseConfig(data, where, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diags
	}

	attrs, diags := f.Body.JustAttributes()
	if diags.HasErrors() {
		return nil, diags
	}

	out := make(map[string]any, len(attrs))

	for name, a := range attrs {
		v, diags := a.Expr.Value(nil)
		if diags.HasErrors() {
			return nil, fmt.Errorf("%s: %s is not a plain value: %w", where, name, diags)
		}

		raw, err := ctyjson.Marshal(v, v.Type())
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", where, name, err)
		}

		var x any
		if err := json.Unmarshal(raw, &x); err != nil {
			return nil, fmt.Errorf("%s: %s: %w", where, name, err)
		}

		out[name] = x
	}

	return out, nil
}

// unusedValues warns about values of the run that no written stack
// declares: a typo there would otherwise go unnoticed.
func unusedValues(runValues map[string]any, used map[string]bool) []string {
	var out []string

	for _, name := range slices.Sorted(maps.Keys(runValues)) {
		if !used[name] {
			out = append(out, fmt.Sprintf("the value %s of the region is a variable of no written stack", name))
		}
	}

	return out
}
