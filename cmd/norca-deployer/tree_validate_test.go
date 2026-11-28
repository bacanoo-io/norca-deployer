// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/tree"
)

// tree init writes a skeleton that tree validate accepts, refuses to run
// twice, and keeps an existing startup configuration; tree validate
// reports a broken file in text and in JSON with exit code 1.
func TestTreeInitAndValidate(t *testing.T) {
	t.Setenv(experimentalEnv, "tree")
	t.Setenv("NORCA_DEPLOYER_PLATFORM_CONFIG", "")

	dir := t.TempDir()
	treeDir := filepath.Join(dir, "acme")
	config := filepath.Join(dir, "norca-tree.hcl")

	code, stdout, stderr := execute(t, "tree", "init", "-d", treeDir, "--config", config)
	if code != exitOK || !strings.Contains(stdout, "tree.hcl") || !strings.Contains(stdout, "norca-tree.hcl") {
		t.Fatalf("init = %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	for _, name := range []string{"tree.hcl", "README.md", ".gitattributes", ".gitignore"} {
		if _, err := os.Stat(filepath.Join(treeDir, name)); err != nil {
			t.Errorf("init wrote no %s: %v", name, err)
		}
	}

	if code, _, stderr := execute(t, "tree", "init", "-d", treeDir, "--config", config); code != exitError || !strings.Contains(stderr, "has a tree.hcl already") {
		t.Fatalf("a second init = %d, stderr %q", code, stderr)
	}

	// The template is a startup configuration the tree commands read.
	code, stdout, stderr = execute(t, "tree", "validate", "-d", treeDir, "--config", config)
	if code != exitOK || !strings.Contains(stdout, "Tree acme is valid: 0 stacks") {
		t.Fatalf("validate of the skeleton = %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	write := func(name, data string) {
		p := filepath.Join(treeDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("global/namespaces/apps.namespace.hcl", "namespace \"apps\" {}\n")
	write("namespaces/apps/jobs/api.norca.hcl", "job \"api\" {}\n")
	write("namespaces/web/jobs/site.norca.hcl", "job \"site\" {}\n")

	code, stdout, _ = execute(t, "tree", "validate", "-d", treeDir, "--config", config)
	if code != exitOK || !strings.Contains(stdout, "3 stacks (tree-global, tree-ns-apps, tree-ns-web)") {
		t.Fatalf("validate = %d, stdout %q", code, stdout)
	}

	// --stack and --namespace narrow the stacks; an unknown one is a usage
	// error.
	out := filepath.Join(dir, "built")
	code, stdout, stderr = execute(t, "tree", "build", "-d", treeDir, "--config", config, "--out", out)
	if code != exitOK || !strings.Contains(stdout, "tree-ns-web") {
		t.Fatalf("build = %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	write("namespaces/apps/jobs/broken.norca.hcl", "job \"broken\" {\n")

	code, _, stderr = execute(t, "tree", "validate", "-d", treeDir, "--config", config)
	if code != exitError || !strings.Contains(stderr, "broken.norca.hcl:") {
		t.Fatalf("validate of a broken file = %d, stderr %q", code, stderr)
	}

	code, stdout, stderr = execute(t, "tree", "validate", "-d", treeDir, "--config", config, "--format", "json")
	if code != exitError || stderr != "" {
		t.Fatalf("validate --format json of a broken file = %d, stderr %q", code, stderr)
	}

	var doc struct {
		Schema string
		Valid  bool
		Errors []string
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil || doc.Schema != validateSchema || doc.Valid || len(doc.Errors) == 0 {
		t.Fatalf("validate JSON %+v, %v\n%s", doc, err, stdout)
	}
}

func TestTreeFlags_Narrow(t *testing.T) {
	t.Parallel()

	f := &treeFlags{onlyStacks: []string{"data"}, onlyNamespaces: []string{"apps"}}
	stacks := []*tree.Stack{{Name: "tree-global"}, {Name: "tree-ns-apps"}, {Name: "tree-ns-web"}, {Name: "data"}}

	got, err := f.narrow(stacks)
	if err != nil || len(got) != 2 || got[0].Name != "tree-ns-apps" || got[1].Name != "data" {
		t.Fatalf("narrow = %v, %v", got, err)
	}

	f.onlyStacks = []string{"nope"}
	if _, err := f.narrow(stacks); err == nil || !isUsageError(err) || !strings.Contains(err.Error(), "no stack of the tree: nope") {
		t.Fatalf("an unknown stack: %v", err)
	}
}
