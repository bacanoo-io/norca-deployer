// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
)

// sampleTree is a tree with global objects, two namespaces and a secret.
func sampleTree() fstest.MapFS {
	return fstest.MapFS{
		"tree.hcl":                                     {Data: []byte(goodManifest)},
		"README.md":                                    {Data: []byte("the tree")},
		"global/namespaces/apps.namespace.hcl":         {Data: []byte(`namespace "apps" {}`)},
		"global/acl/policies/developer.acl.hcl":        {Data: []byte(`acl_policy "developer" {}`)},
		"namespaces/apps/jobs/api.norca.hcl":           {Data: []byte(`job "api" {}`)},
		"namespaces/apps/variables/api-config.var.hcl": {Data: []byte(`variable "api-config" {}`)},
		"namespaces/apps/secrets/api/db.hcl":           {Data: []byte("items = {\n  user = \"api\"\n}\n")},
		"namespaces/apps/_draft.norca.hcl":             {Data: []byte("not read")},
		"namespaces/web/jobs/site.norca.hcl":           {Data: []byte(`job "site" {}`)},
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()

	tr, err := Load(sampleTree())
	if err != nil {
		t.Fatal(err)
	}

	if len(tr.Global) != 2 || len(tr.Namespaces) != 2 || len(tr.Warnings) != 0 {
		t.Fatalf("tree %+v", tr)
	}

	apps := tr.Namespaces[0]
	if apps.Name != "apps" || len(apps.Files) != 2 || len(apps.Secrets) != 1 || apps.Secrets[0].Name != "api/db" {
		t.Fatalf("namespace apps %+v", apps)
	}

	kinds := map[string]string{}
	for _, f := range append(tr.Global, apps.Files...) {
		kinds[f.Name] = f.Kind
	}

	if kinds["apps"] != KindNamespace || kinds["developer"] != KindACLPolicy || kinds["api"] != KindJob || kinds["api-config"] != KindVariable {
		t.Fatalf("kinds %v", kinds)
	}
}

func TestLoad_Warnings(t *testing.T) {
	t.Parallel()

	fsys := sampleTree()
	fsys["global/quotas/developer.acl.hcl"] = &fstest.MapFile{Data: []byte(`acl_policy "x" {}`)}

	tr, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}

	if len(tr.Warnings) != 1 || !strings.Contains(tr.Warnings[0], "a acl-policy in quotas/") {
		t.Fatalf("warnings %v", tr.Warnings)
	}
}

func TestLoad_Refused(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "a kind without a Rigger kind yet", path: "global/acl/roles/dev.hcl", want: "global/acl/roles/ is not supported yet"},
		{name: "Consul KV", path: "kv/app/config", want: "kv/ is not supported yet"},
		{name: "written stacks, later", path: "stacks/data/stack.hcl", want: "stacks/ is not supported yet"},
		{name: "a namespaced kind without a Rigger kind", path: "namespaces/apps/intentions/a.hcl", want: "intentions/ is not supported yet"},
		{name: "an extension of no kind", path: "namespaces/apps/jobs/api.hcl", want: "the extension names no kind"},
		{name: "a job in global/", path: "global/jobs/api.norca.hcl", want: "a job does not belong in global/"},
		{name: "a namespace in a namespace", path: "namespaces/apps/ns/x.namespace.hcl", want: "a namespace does not belong in namespaces/"},
		{name: "an unknown directory", path: "regions/eu-1/scheduler.json", want: "regions/ is not a directory of a tree"},
		{name: "a bad namespace name", path: "namespaces/Apps_1/jobs/a.norca.hcl", want: `namespace "Apps_1"`},
		{name: "a file right below namespaces/", path: "namespaces/a.norca.hcl", want: "in the directory of its namespace"},
		{name: "a character Windows refuses", path: "namespaces/apps/jobs/a:b.norca.hcl", want: `the character ':'`},
		{name: "a name Windows reserves", path: "namespaces/apps/jobs/con.norca.hcl", want: "con is a reserved name on Windows"},
		{name: "a trailing dot", path: "namespaces/apps/jobs./x.norca.hcl", want: "does not end with a dot"},
		{name: "a reserved file", path: "global/_ignore.hcl", want: "_ignore.hcl is reserved"},
		{name: "a path too long", path: "namespaces/apps/jobs/" + strings.Repeat("a", 230) + ".norca.hcl", want: "characters long, at most 240"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fsys := sampleTree()
			fsys[tt.path] = &fstest.MapFile{Data: []byte("x")}

			_, err := Load(fsys)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error %v, want one with %q", err, tt.want)
			}
		})
	}
}

// Two paths that differ only by case break a checkout on a file system that
// ignores case, so they are refused everywhere.
func TestLoad_CaseCollision(t *testing.T) {
	t.Parallel()

	fsys := sampleTree()
	fsys["namespaces/apps/jobs/API.norca.hcl"] = &fstest.MapFile{Data: []byte("x")}

	_, err := Load(fsys)
	if err == nil || !strings.Contains(err.Error(), "differ only by case") {
		t.Fatalf("Load error %v", err)
	}
}

// A symbolic link in the tree is refused, never followed.
func TestLoad_SymbolicLink(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}

	dir := t.TempDir()
	for name, f := range sampleTree() {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.Data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "namespaces/apps/jobs/passwd.norca.hcl")); err != nil {
		t.Fatal(err)
	}

	_, err := Load(os.DirFS(dir))
	if err == nil || !strings.Contains(err.Error(), "a symbolic link") {
		t.Fatalf("Load error %v", err)
	}
}
