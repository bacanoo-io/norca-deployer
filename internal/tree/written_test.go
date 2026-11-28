// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// withWrittenStack adds the written stack data, its own module own, and the
// shared modules postgres (used) and unused of the tree.
func withWrittenStack(fsys fstest.MapFS) fstest.MapFS {
	add := func(p, data string) { fsys[p] = &fstest.MapFile{Data: []byte(data)} }

	add("stacks/data/stack.hcl", "rigger {\n  name      = \"data\"\n  namespace = \"platform\"\n  region    = \"eu-1\"\n}\n\n"+
		"module \"db\" {\n  source = \"./modules/postgres\"\n  values = { replicas = var.replicas }\n}\n\n"+
		"module \"cache\" {\n  source = \"./modules/own\"\n}\n\n"+
		"module \"web\" {\n  source  = \"acme/web\"\n  version = \"~> 1.0\"\n}\n")
	add("stacks/data/variables.hcl", "variable \"replicas\" {}\n\nvariable \"password\" {\n  sensitive = true\n}\n\nvariable \"token\" {}\n")
	add("stacks/data/values/default.vars.hcl",
		"replicas = 1\npassword = \""+encryptedPassword+"\"\ntoken = \"!generate:data-token\"\nlabels = { team = \"data\" }\n")
	add("stacks/data/modules/own/metadata.hcl", "module {\n  name = \"own\"\n}\n")
	add("modules/postgres/metadata.hcl", "module {\n  name = \"postgres\"\n}\n")
	add("modules/postgres/db.norca.hcl", "job \"db\" {}\n")
	add("modules/unused/metadata.hcl", "module {\n  name = \"unused\"\n}\n")

	return fsys
}

// A written stack is sent as written, with the shared modules it uses and
// its values: the run's over values/default.vars.hcl, only for the
// variables the stack declares, the forms resolved.
func TestBuild_WrittenStack(t *testing.T) {
	t.Parallel()

	tr, err := Load(withWrittenStack(sampleTree()))
	if err != nil {
		t.Fatal(err)
	}

	stacks, err := Build(tr, "eu-1", map[string]any{"replicas": 3, "zone": "b"}, testResolver())
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(stacks))
	for _, s := range stacks {
		names = append(names, s.Name)
	}

	// The generated stacks first, the written ones after them.
	if !slices.Equal(names, []string{"tree-global", "tree-ns-apps", "tree-ns-web", "data"}) {
		t.Fatalf("stacks %v", names)
	}

	data := stacks[3]
	if data.Namespace != "platform" || data.Region != "eu-1" {
		t.Fatalf("data: namespace %s, region %s", data.Namespace, data.Region)
	}

	for _, p := range []string{"stack.hcl", "variables.hcl", "modules/own/metadata.hcl", "modules/postgres/metadata.hcl", "modules/postgres/db.norca.hcl"} {
		if data.Files[p] == nil {
			t.Errorf("data has no %s: %v", p, keys(data.Files))
		}
	}

	if data.Files["modules/unused/metadata.hcl"] != nil {
		t.Error("a shared module the stack does not use is sent")
	}

	want := map[string]any{"replicas": 3, "password": "s3cret", "token": "!generate:data-token", "labels": map[string]any{"team": "data"}}
	if len(data.Values) != len(want) || data.Values["replicas"] != 3 || data.Values["password"] != "s3cret" ||
		data.Values["token"] != "!generate:data-token" || data.Values["labels"].(map[string]any)["team"] != "data" {
		t.Fatalf("values %v, want %v", data.Values, want)
	}

	if !slices.Equal(data.Sensitive, []string{"s3cret"}) {
		t.Errorf("sensitive %v", data.Sensitive)
	}

	if !slices.ContainsFunc(tr.Warnings, func(w string) bool { return strings.Contains(w, "the value zone of the region") }) {
		t.Errorf("warnings %v", tr.Warnings)
	}

	// The generated stacks get no values of the run.
	if stacks[2].Values["replicas"] != nil {
		t.Error("a generated stack got a value of the run")
	}
}

func TestBuild_WrittenStackRefused(t *testing.T) {
	t.Parallel()

	stackHCL := func(name, extra string) string {
		return "rigger {\n  name = \"" + name + "\"\n" + extra + "}\n\nmodule \"db\" {\n  source = \"./modules/postgres\"\n}\n"
	}

	tests := []struct {
		name string
		edit func(fstest.MapFS)
		want string
	}{
		{
			name: "a name that is not the directory's",
			edit: func(f fstest.MapFS) { f["stacks/data/stack.hcl"].Data = []byte(stackHCL("other", "")) },
			want: `rigger.name is "other"`,
		},
		{
			name: "a reserved stack name",
			edit: func(f fstest.MapFS) {
				f["stacks/tree-ns-x/stack.hcl"] = &fstest.MapFile{Data: []byte(stackHCL("tree-ns-x", ""))}
			},
			want: "the name tree-ns-x is reserved",
		},
		{
			name: "a reserved release name",
			edit: func(f fstest.MapFS) {
				f["stacks/data/stack.hcl"].Data = []byte(stackHCL("data", "") + "\nmodule \"tree-db\" {\n  source = \"./modules/postgres\"\n}\n")
			},
			want: "the release name tree-db is reserved",
		},
		{
			name: "a stack of another region",
			edit: func(f fstest.MapFS) {
				f["stacks/data/stack.hcl"].Data = []byte(stackHCL("data", "  region = \"us-1\"\n"))
			},
			want: "the stack is for the region us-1, the run goes to eu-1",
		},
		{
			name: "a stack without a region is of global",
			edit: func(f fstest.MapFS) {
				f["stacks/data/stack.hcl"].Data = []byte(stackHCL("data", ""))
			},
			want: "the stack is for the region global, the run goes to eu-1",
		},
		{
			name: "a module that is nowhere",
			edit: func(f fstest.MapFS) {
				delete(f, "modules/postgres/metadata.hcl")
				delete(f, "modules/postgres/db.norca.hcl")
			},
			want: "./modules/postgres is neither in the stack nor in the tree's modules/",
		},
		{
			name: "a module in both places",
			edit: func(f fstest.MapFS) {
				f["stacks/data/modules/postgres/metadata.hcl"] = &fstest.MapFile{Data: []byte("module {\n  name = \"postgres\"\n}\n")}
			},
			want: "the module postgres is both in the stack's modules/ and in the tree's modules/",
		},
		{
			name: "no rigger block",
			edit: func(f fstest.MapFS) {
				f["stacks/data/stack.hcl"].Data = []byte("module \"db\" {\n  source = \"./modules/postgres\"\n}\n")
			},
			want: "stacks/data: no rigger block",
		},
		{
			name: "a namespace that is no plain string",
			edit: func(f fstest.MapFS) {
				f["stacks/data/stack.hcl"].Data = []byte(stackHCL("data", "  namespace = var.ns\n"))
			},
			want: "rigger.namespace is a plain string",
		},
		{
			name: "a value that is no plain value",
			edit: func(f fstest.MapFS) {
				f["stacks/data/values/default.vars.hcl"].Data = []byte("replicas = var.other\n")
			},
			want: "replicas is not a plain value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fsys := withWrittenStack(sampleTree())
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
