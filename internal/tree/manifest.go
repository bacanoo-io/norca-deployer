// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"

	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// FormatVersion is the format of a tree this program reads
// (design §5.1, review item G9).
const FormatVersion = "norca.io/tree/v1alpha1"

// The names of the manifest of a tree; one of them, never both.
const (
	manifestHCL  = "tree.hcl"
	manifestYAML = "tree.yaml"
	manifestYML  = "tree.yml"
)

// defaultNamespace is the namespace of the stack tree-global, the only one
// allowed until a tree needs another (design §13a, G19).
const defaultNamespace = "default"

// Manifest is the manifest of a tree, tree.hcl (design §5.1): what the
// files mean. It holds nothing about the cluster or the credentials.
type Manifest struct {
	Tree       TreeBlock     `yaml:"tree"`
	Requires   *RequireBlock `yaml:"requires"`
	StackOrder []string      `yaml:"stack_order"`
	Layers     []string      `yaml:"layers"`
}

// TreeBlock is the tree block of the manifest.
type TreeBlock struct {
	// Name names the tree in plans and statuses.
	Name string `yaml:"name"`
	// Version is the format of the tree (FormatVersion).
	Version string `yaml:"version"`
	// Namespace is the namespace of the stack tree-global.
	Namespace string `yaml:"namespace"`
	// Environment names the environment, for information only.
	Environment string `yaml:"environment"`
}

// RequireBlock is the requires block: the versions the files were written
// for. The version preflight checks them (implementation plan T2-4).
type RequireBlock struct {
	Norca    string `yaml:"norca"`
	Deployer string `yaml:"deployer"`
}

// treeName is the rule of the name of a tree: it goes into the names of
// stacks and releases.
var treeName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// LoadManifest reads and checks the manifest at the root of a tree.
func LoadManifest(fsys fs.FS) (*Manifest, error) {
	name, format, err := manifestFile(fsys)
	if err != nil {
		return nil, err
	}

	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}

	m := &Manifest{}
	if err := config.DecodeInto(data, format, m); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	if err := m.check(); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	return m, nil
}

// manifestFile finds the manifest: tree.hcl, or tree.yaml (tree.yml).
func manifestFile(fsys fs.FS) (string, config.Format, error) {
	var found []string

	for _, name := range []string{manifestHCL, manifestYAML, manifestYML} {
		if _, err := fs.Stat(fsys, name); err == nil {
			found = append(found, name)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", "", err
		}
	}

	switch len(found) {
	case 0:
		return "", "", fmt.Errorf("no %s at the root of the tree", manifestHCL)
	case 1:
		if found[0] == manifestHCL {
			return found[0], config.FormatHCL, nil
		}

		return found[0], config.FormatYAML, nil
	}

	return "", "", fmt.Errorf("%s and %s are both there: a tree has one manifest", found[0], found[1])
}

// check checks the manifest and fills in its defaults.
func (m *Manifest) check() error {
	var errs []error

	switch m.Tree.Version {
	case FormatVersion:
	case "":
		errs = append(errs, fmt.Errorf("tree.version is missing: add version = %q", FormatVersion))
	default:
		errs = append(errs, fmt.Errorf("tree format %s is not supported by this program (it supports %s)", m.Tree.Version, FormatVersion))
	}

	if !treeName.MatchString(m.Tree.Name) {
		errs = append(errs, fmt.Errorf("tree.name %q: lower case letters, digits and -, at most 63", m.Tree.Name))
	}

	switch m.Tree.Namespace {
	case "":
		m.Tree.Namespace = defaultNamespace
	case defaultNamespace:
	default:
		errs = append(errs, fmt.Errorf("tree.namespace %q: only %q for now", m.Tree.Namespace, defaultNamespace))
	}

	seen := map[string]bool{}

	for _, s := range m.StackOrder {
		if s == globalStack {
			errs = append(errs, fmt.Errorf("stack_order: %s is always first and is not named", globalStack))
		}

		if seen[s] {
			errs = append(errs, fmt.Errorf("stack_order: %s is named twice", s))
		}

		seen[s] = true
	}

	return errors.Join(errs...)
}
