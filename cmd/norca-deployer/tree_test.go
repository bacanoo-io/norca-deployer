// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tree commands are experimental: without NORCA_DEPLOYER_EXPERIMENTAL
// naming them they refuse to run and are left out of the help; with it they
// show their help.
func TestTreeCommandIsExperimental(t *testing.T) {
	tests := []struct {
		name       string
		env        string
		code       int
		inStdout   string
		inStderr   string
		rootListed bool
	}{
		{name: "off", env: "", code: exitError, inStderr: "the tree commands are experimental: set NORCA_DEPLOYER_EXPERIMENTAL=tree"},
		{name: "another feature", env: "other", code: exitError, inStderr: "experimental"},
		{name: "on", env: "other, tree", code: exitOK, inStdout: "applied as Rigger stacks", rootListed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(experimentalEnv, tt.env)

			code, stdout, stderr := execute(t, "tree")
			if code != tt.code || !strings.Contains(stdout, tt.inStdout) || !strings.Contains(stderr, tt.inStderr) {
				t.Fatalf("tree = %d, stdout %q, stderr %q", code, stdout, stderr)
			}

			_, help, _ := execute(t, "--help")
			if listed := strings.Contains(help, "\n  tree "); listed != tt.rootListed {
				t.Fatalf("tree listed in the help: %v, want %v", listed, tt.rootListed)
			}
		})
	}
}

// tree build writes the stacks of a tree to a directory; the resolved value
// of a secret item is not among the files.
func TestTreeBuildCommand(t *testing.T) {
	t.Setenv(experimentalEnv, "tree")
	t.Setenv("APP_DB_PASSWORD", "s3cret")

	dir := t.TempDir()
	files := map[string]string{
		"tree.hcl":                             "tree {\n  name    = \"acme\"\n  version = \"norca.io/tree/v1alpha1\"\n}\n",
		"global/namespaces/apps.namespace.hcl": "namespace \"apps\" {}\n",
		"namespaces/apps/jobs/api.norca.hcl":   "job \"api\" {}\n",
		"namespaces/apps/secrets/api/db.hcl":   "items = {\n  password = \"!env:APP_DB_PASSWORD\"\n}\n",
	}
	for name, content := range files {
		p := filepath.Join(dir, "tree", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	out := filepath.Join(dir, "out")

	code, stdout, stderr := execute(t, "tree", "build", "-d", filepath.Join(dir, "tree"), "--region", "eu-1", "--out", out, "--values-env", "APP_*")
	if code != exitOK || !strings.Contains(stdout, "tree-ns-apps: 5 files") {
		t.Fatalf("tree build = %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	for _, name := range []string{"tree-global/stack.hcl", "tree-ns-apps/stack.hcl", "tree-ns-apps/modules/tree-apps/api.norca.hcl", "tree-ns-apps/secrets.hcl", "tree-ns-apps/variables.hcl"} {
		data, err := os.ReadFile(filepath.Join(out, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "s3cret") {
			t.Fatalf("%s holds the secret value", name)
		}
	}

	// Without --values-env the variable is not read.
	code, _, stderr = execute(t, "tree", "build", "-d", filepath.Join(dir, "tree"), "--region", "eu-1", "--out", out)
	if code != exitError || !strings.Contains(stderr, "not allowed") {
		t.Fatalf("tree build without --values-env = %d, stderr %q", code, stderr)
	}
}
