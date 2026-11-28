// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"

	"github.com/bacanoo-io/norca-deployer/internal/crypto"
	"github.com/bacanoo-io/norca-deployer/internal/values"
)

// encryptedPassword is an encrypted value of the tests' resolver.
const encryptedPassword = crypto.EncryptedPrefix + "cGFzc3dvcmQ="

// testResolver decrypts encryptedPassword.
func testResolver() *values.Resolver {
	return &values.Resolver{Decrypt: func(v string) (string, error) {
		if v != encryptedPassword {
			return "", errors.New("unknown value")
		}

		return "s3cret", nil
	}}
}

// mustParse fails the test when data is no HCL.
func mustParse(t *testing.T, name string, data []byte) {
	t.Helper()

	if _, diags := hclsyntax.ParseConfig(data, name, hcl.InitialPos); diags.HasErrors() {
		t.Fatalf("%s is no HCL: %s\n%s", name, diags.Error(), data)
	}
}

func TestBuild(t *testing.T) {
	t.Parallel()

	fsys := sampleTree()
	fsys["namespaces/apps/secrets/api/db.hcl"] = &fstest.MapFile{Data: []byte(
		"items = {\n  user = \"api\"\n  password = \"" + encryptedPassword + "\"\n  token = \"!generate:api-token\"\n  odd = \"${not a template}\"\n}\n\noverwrite = true\n")}

	tr, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}

	stacks, err := Build(tr, "eu-1", nil, testResolver())
	if err != nil {
		t.Fatal(err)
	}

	// stack_order names tree-ns-apps; tree-global is always first.
	names := []string{}
	for _, s := range stacks {
		names = append(names, s.Name)
	}

	if strings.Join(names, " ") != "tree-global tree-ns-apps tree-ns-web" {
		t.Fatalf("stacks %v", names)
	}

	global, apps := stacks[0], stacks[1]

	if global.Namespace != defaultNamespace || apps.Namespace != "apps" || apps.Region != "eu-1" {
		t.Fatalf("stacks %+v %+v", global, apps)
	}

	wantStack := "rigger {\n  name      = \"tree-ns-apps\"\n  namespace = \"apps\"\n  region    = \"eu-1\"\n}\n\nmodule \"tree-apps\" {\n  source = \"./modules/tree-apps\"\n}\n"
	if got := string(apps.Files["stack.hcl"]); got != wantStack {
		t.Fatalf("stack.hcl =\n%s\nwant\n%s", got, wantStack)
	}

	if got := string(apps.Files["modules/tree-apps/metadata.hcl"]); got != "module {\n  name = \"tree-apps\"\n}\n" {
		t.Fatalf("metadata.hcl =\n%s", got)
	}

	if string(apps.Files["modules/tree-apps/api.norca.hcl"]) != `job "api" {}` || apps.Files["modules/tree-apps/api-config.var.hcl"] == nil {
		t.Fatalf("files %v", keys(apps.Files))
	}

	secrets := string(apps.Files["secrets.hcl"])
	for _, want := range []string{`secret "api/db"`, `"user"     = "api"`, `"password" = var.tree_secret_1`, `"token"    = "!generate:api-token"`, `"odd"      = "$${not a template}"`, "overwrite = true"} {
		if !strings.Contains(secrets, want) {
			t.Errorf("secrets.hcl has no %q:\n%s", want, secrets)
		}
	}

	if strings.Contains(secrets, "s3cret") || strings.Contains(string(apps.Files["variables.hcl"]), "s3cret") {
		t.Fatal("a resolved secret value is in the files of the stack")
	}

	if !strings.Contains(string(apps.Files["variables.hcl"]), "variable \"tree_secret_1\" {\n  sensitive = true\n}") {
		t.Fatalf("variables.hcl =\n%s", apps.Files["variables.hcl"])
	}

	if apps.Values["tree_secret_1"] != "s3cret" || len(apps.Sensitive) != 1 {
		t.Fatalf("values %v, sensitive %v", apps.Values, apps.Sensitive)
	}

	for _, s := range stacks {
		for name, data := range s.Files {
			if strings.HasSuffix(name, ".hcl") && !strings.HasPrefix(name, "modules/") {
				mustParse(t, s.Name+"/"+name, data)
			}
		}
	}
}

func TestBuild_Refused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		edit func(fstest.MapFS)
		want string
	}{
		{
			name: "two files of one name in a stack",
			edit: func(f fstest.MapFS) { f["namespaces/apps/other/api.norca.hcl"] = &fstest.MapFile{Data: []byte("x")} },
			want: `are both the job "api" of the stack tree-ns-apps`,
		},
		{
			name: "a secret value that does not resolve",
			edit: func(f fstest.MapFS) {
				f["namespaces/apps/secrets/api/db.hcl"] = &fstest.MapFile{Data: []byte("items = {\n  token = \"!env:NORCA_TOKEN\"\n}\n")}
			},
			want: "item token",
		},
		{
			name: "a secret without items",
			edit: func(f fstest.MapFS) {
				f["namespaces/apps/secrets/api/db.hcl"] = &fstest.MapFile{Data: []byte("overwrite = true\n")}
			},
			want: "a secret has items",
		},
		{
			name: "stack_order names no stack",
			edit: func(f fstest.MapFS) {
				f["tree.hcl"] = &fstest.MapFile{Data: []byte(strings.Replace(goodManifest, "tree-ns-apps", "tree-ns-none", 1))}
			},
			want: "stack_order: tree-ns-none is not a stack of the tree",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fsys := sampleTree()
			tt.edit(fsys)

			tr, err := Load(fsys)
			if err != nil {
				t.Fatal(err)
			}

			_, err = Build(tr, "eu-1", nil, testResolver())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Build error %v, want one with %q", err, tt.want)
			}
		})
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	return out
}
