// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"strings"
	"testing"
	"testing/fstest"
)

const goodManifest = `tree {
  name        = "acme-prod"
  version     = "norca.io/tree/v1alpha1"
  environment = "prod"
}

requires {
  norca = ">= 0.3.0"
}

stack_order = ["tree-ns-apps"]
`

func TestLoadManifest(t *testing.T) {
	t.Parallel()

	m, err := LoadManifest(fstest.MapFS{"tree.hcl": {Data: []byte(goodManifest)}})
	if err != nil {
		t.Fatal(err)
	}

	if m.Tree.Name != "acme-prod" || m.Tree.Namespace != defaultNamespace || m.Tree.Environment != "prod" ||
		m.Requires == nil || m.Requires.Norca != ">= 0.3.0" || len(m.StackOrder) != 1 {
		t.Fatalf("manifest %+v", m)
	}

	// The YAML form has the same keys.
	y, err := LoadManifest(fstest.MapFS{"tree.yaml": {Data: []byte("tree:\n  name: acme-prod\n  version: norca.io/tree/v1alpha1\n  environment: prod\nrequires:\n  norca: \">= 0.3.0\"\nstack_order: [tree-ns-apps]\n")}})
	if err != nil {
		t.Fatal(err)
	}

	if y.Tree != m.Tree || *y.Requires != *m.Requires || y.StackOrder[0] != m.StackOrder[0] {
		t.Fatalf("YAML %+v, HCL %+v", y, m)
	}
}

func TestLoadManifest_Refused(t *testing.T) {
	t.Parallel()

	tree := func(body string) fstest.MapFS { return fstest.MapFS{"tree.hcl": {Data: []byte(body)}} }
	v := `version = "norca.io/tree/v1alpha1"`

	tests := []struct {
		name string
		fsys fstest.MapFS
		want string
	}{
		{name: "no manifest", fsys: fstest.MapFS{"README.md": {}}, want: "no tree.hcl"},
		{name: "both forms", fsys: fstest.MapFS{"tree.hcl": {}, "tree.yaml": {}}, want: "are both there"},
		{name: "no version", fsys: tree(`tree { name = "a" }`), want: "tree.version is missing"},
		{name: "unknown version", fsys: tree(`tree {` + "\n" + `name = "a"` + "\n" + `version = "norca.io/tree/v2"` + "\n}"), want: "is not supported by this program"},
		{name: "bad name", fsys: tree(`tree {` + "\n" + `name = "Acme_Prod"` + "\n" + v + "\n}"), want: "tree.name"},
		{name: "another namespace", fsys: tree(`tree {` + "\n" + `name = "a"` + "\n" + v + "\n" + `namespace = "platform"` + "\n}"), want: `only "default"`},
		{name: "an unknown key", fsys: tree(`tree {` + "\n" + `name = "a"` + "\n" + v + "\n}\n" + `region "eu" {}`), want: `unknown key "region"`},
		{name: "tree-global in the order", fsys: tree(`tree {` + "\n" + `name = "a"` + "\n" + v + "\n}\n" + `stack_order = ["tree-global"]`), want: "always first"},
		{name: "a stack twice", fsys: tree(`tree {` + "\n" + `name = "a"` + "\n" + v + "\n}\n" + `stack_order = ["x", "x"]`), want: "named twice"},
		{name: "layers, later", fsys: tree(`tree {` + "\n" + `name = "a"` + "\n" + v + "\n}\n" + `layers = ["base"]`), want: "layers are not supported yet"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := LoadManifest(tt.fsys)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadManifest error %v, want one with %q", err, tt.want)
			}
		})
	}
}
