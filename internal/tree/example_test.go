// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/values"
)

// The example tree builds offline with its startup configuration: its
// stacks, the shared module copied in, the value of the region over the
// stack's own, and the secret resolved from the environment.
func TestExampleTree(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..", "examples", "tree")

	cfg, _, err := LoadConfig(filepath.Join(root, "norca-tree.hcl"), filepath.Join(root, "cluster-config"))
	if err != nil {
		t.Fatal(err)
	}

	region, err := cfg.Region("")
	if err != nil {
		t.Fatal(err)
	}

	tr, err := Load(os.DirFS(filepath.Join(root, "cluster-config")))
	if err != nil {
		t.Fatal(err)
	}

	r := &values.Resolver{Env: cfg.Values.Env, LookupEnv: func(name string) (string, bool) {
		return "example-password", name == "APP_DB_PASSWORD"
	}}

	stacks, err := Build(tr, region.Name, region.Values, r)
	if err != nil {
		t.Fatal(err)
	}

	if err := CheckSyntax(stacks); err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(stacks))
	for _, s := range stacks {
		names = append(names, s.Name)
	}

	if !slices.Equal(names, []string{"tree-global", "tree-ns-apps", "web-settings"}) {
		t.Fatalf("stacks %v", names)
	}

	web := stacks[2]
	if web.Files["modules/settings/settings.var.hcl"] == nil || web.Values["replicas"] != 3 || web.Values["color"] != "green" {
		t.Fatalf("web-settings: files %v, values %v", keys(web.Files), web.Values)
	}

	if !slices.Contains(stacks[1].Sensitive, "example-password") {
		t.Fatal("the secret of the example is not resolved")
	}

	if len(tr.Warnings) != 0 {
		t.Errorf("warnings %v", tr.Warnings)
	}
}
