// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const testConfig = `region "eu-1" {
  address   = "https://10.0.0.11:4646"
  token_env = "NORCA_TOKEN_EU1"
  ca_cert   = "certs/ca.pem"

  values = {
    replicas = 3
  }

  scoped_token "foundation" {
    token_env = "TOKEN_FOUNDATION"
    stacks    = ["tree-global"]
  }

  scoped_token "apps" {
    token_file = "apps.token"
    stacks     = ["tree-ns-app-*"]
  }

  scoped_token "shop" {
    token_env = "TOKEN_SHOP"
    stacks    = ["tree-ns-app-shop"]
  }

  scoped_token "readonly" {
    token_env = "TOKEN_RO"
    for       = ["plan"]
  }
}

region "eu-2" {
  address = "https://10.0.1.11:4646"
}

sync_registries = ["global"]

values {
  key_file = "tree.key"
  dir      = "values"
  env      = ["APP_*"]
}

limits {
  kinds       = ["foundation", "job"]
  prune       = false
  max_changes = 20
}
`

// writeConfig writes a startup configuration with mode 0600.
func writeConfig(t *testing.T, dir, body string) string {
	t.Helper()

	p := filepath.Join(dir, DefaultConfigFile)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return p
}

func TestLoadConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	p := writeConfig(t, dir, testConfig)

	c, warnings, err := LoadConfig(p, filepath.Join(dir, "tree"))
	if err != nil {
		t.Fatal(err)
	}

	if len(warnings) != 0 {
		t.Errorf("warnings %v", warnings)
	}

	if len(c.Regions) != 2 || c.SyncRegistries[0] != "global" || c.Values.Env[0] != "APP_*" ||
		c.PruneAllowed() || c.MaxChanges() != 20 || len(c.Limits.Kinds) != 2 {
		t.Fatalf("config %+v", c)
	}

	if got := c.Path(c.Values.KeyFile); got != filepath.Join(dir, "tree.key") {
		t.Errorf("Path = %q", got)
	}

	eu1, err := c.Region("eu-1")
	if err != nil || eu1.Address != "https://10.0.0.11:4646" || eu1.Values["replicas"] != 3 || len(eu1.ScopedTokens) != 4 {
		t.Fatalf("region eu-1 %+v, %v", eu1, err)
	}

	tokens := []struct {
		stack string
		plan  bool
		want  string
	}{
		{stack: "tree-global", want: "foundation"},
		{stack: "tree-ns-app-web", want: "apps"},
		{stack: "tree-ns-app-shop", want: "shop"},
		{stack: "tree-ns-other", want: ""},
		{stack: "tree-ns-app-web", plan: true, want: "readonly"},
	}

	for _, tt := range tokens {
		if _, name := eu1.TokenFor(tt.stack, tt.plan); name != tt.want {
			t.Errorf("TokenFor(%s, %v) = %q, want %q", tt.stack, tt.plan, name, tt.want)
		}
	}

	if src, _ := eu1.TokenFor("tree-ns-other", false); src.Env != "NORCA_TOKEN_EU1" {
		t.Errorf("the region's own token: %+v", src)
	}

	if _, err := c.Region(""); err == nil || !strings.Contains(err.Error(), "several regions") {
		t.Errorf("Region(\"\") = %v", err)
	}

	if _, err := c.Region("us-1"); err == nil {
		t.Error("an unknown region is not refused")
	}
}

func TestLoadConfig_NoFile(t *testing.T) {
	t.Setenv(ConfigEnv, "")
	t.Chdir(t.TempDir())

	c, _, err := LoadConfig("", "")
	if err != nil || len(c.Regions) != 0 || !c.PruneAllowed() || c.MaxChanges() != defaultMaxChanges {
		t.Fatalf("an absent default file: %+v, %v", c, err)
	}

	r, err := c.Region("global")
	if err != nil || r.Name != "global" {
		t.Fatalf("Region without blocks = %+v, %v", r, err)
	}

	if _, _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.hcl"), ""); err == nil {
		t.Fatal("a named file that is missing is not refused")
	}
}

func TestLoadConfig_Refused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "a region twice", body: "region \"a\" {}\nregion \"a\" {}\n", want: `region "a" is given twice`},
		{name: "a scoped token without stacks", body: "region \"a\" {\n  scoped_token \"x\" {\n    token_env = \"T\"\n  }\n}\n", want: "names stacks"},
		{name: "a scoped token for something else", body: "region \"a\" {\n  scoped_token \"x\" {\n    for = [\"apply\"]\n  }\n}\n", want: `for takes only "plan"`},
		{name: "a kind that does not exist", body: "limits {\n  kinds = [\"namespaces\"]\n}\n", want: `"namespaces" is no kind`},
		{name: "a negative limit", body: "limits {\n  max_changes = -1\n}\n", want: "can't be negative"},
		{name: "an unknown key", body: "watch {\n  mode = \"apply\"\n}\n", want: `unknown key "watch"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := LoadConfig(writeConfig(t, t.TempDir(), tt.body), "")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("LoadConfig error %v, want one with %q", err, tt.want)
			}
		})
	}
}

// A startup configuration inside the tree, or one others may read, is
// warned about.
func TestLoadConfig_Warnings(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	treeDir := filepath.Join(dir, "tree")

	if err := os.MkdirAll(treeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	p := writeConfig(t, treeDir, "")
	if runtime.GOOS != "windows" {
		if err := os.Chmod(p, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	_, warnings, err := LoadConfig(p, treeDir)
	if err != nil {
		t.Fatal(err)
	}

	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "inside the tree") {
		t.Errorf("warnings %v", warnings)
	}

	if runtime.GOOS != "windows" && !strings.Contains(joined, "can be read by others") {
		t.Errorf("warnings %v", warnings)
	}
}
