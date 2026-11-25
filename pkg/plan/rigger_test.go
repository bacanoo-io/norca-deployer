// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package plan

import (
	"strings"
	"testing"
)

func TestValidateRigger(t *testing.T) {
	tests := []struct {
		name   string
		rigger *RiggerConfig
		want   string
	}{
		{name: "a directory", rigger: &RiggerConfig{StackDir: "stacks/platform", Vars: map[string]string{"count": "3"}}},
		{name: "a stack in the plan", rigger: &RiggerConfig{Stack: "rigger {}"}},
		{name: "a stack of one attribute", rigger: &RiggerConfig{Stack: `x = 1`}},
		{name: "no stack", rigger: &RiggerConfig{}, want: "stack or stack_dir is required"},
		{name: "bindings without a stack of the plan", rigger: &RiggerConfig{GitOps: &GitOpsConfig{}, RunOn: RiggerOnServer, Mode: RiggerEmbedded}},
		{name: "bindings beside a stack of the plan", rigger: &RiggerConfig{StackDir: "a", GitOps: &GitOpsConfig{}}},
		{name: "bindings and a setting of a stack", rigger: &RiggerConfig{GitOps: &GitOpsConfig{}, Vars: map[string]string{"count": "3"}}, want: "stack or stack_dir is required"},
		{name: "bindings and where a stack runs", rigger: &RiggerConfig{GitOps: &GitOpsConfig{}, RunOn: RiggerOnLocal}, want: "stack or stack_dir is required"},
		{name: "both forms", rigger: &RiggerConfig{StackDir: "a", Stack: "rigger {}"}, want: "stack and stack_dir exclude each other"},
		{name: "values with stack", rigger: &RiggerConfig{Stack: "rigger {}", Values: []string{"v.hcl"}}, want: "values files belong to stack_dir; with stack give vars"},
		{name: "values with stack_dir", rigger: &RiggerConfig{StackDir: "a", Values: []string{"v.hcl"}}},
		{name: "a directory as stack", rigger: &RiggerConfig{Stack: "stacks/platform"}, want: `"stacks/platform" looks like a directory: a directory is stack_dir`},
		{name: "the plan directory as stack", rigger: &RiggerConfig{Stack: " ./stack "}, want: "a directory is stack_dir"},
		{name: "name of a variable", rigger: &RiggerConfig{StackDir: "a", Vars: map[string]string{"Count": "3"}}, want: `"Count" is not the name of a variable`},
		{name: "the program of this machine", rigger: &RiggerConfig{StackDir: "a", RunOn: RiggerOnLocal, Mode: RiggerStandalone, Binary: "/opt/rigger"}},
		{name: "the defaults written out", rigger: &RiggerConfig{StackDir: "a", RunOn: RiggerOnServer, Mode: RiggerEmbedded}},
		{name: "unknown run_on", rigger: &RiggerConfig{StackDir: "a", RunOn: "host"}, want: `run_on "host"`},
		{name: "unknown mode", rigger: &RiggerConfig{StackDir: "a", Mode: "remote"}, want: `mode "remote"`},
		{name: "binary on the server", rigger: &RiggerConfig{StackDir: "a", Binary: "/opt/rigger"}, want: "binary is the program of run_on: local"},
		{name: "registries the servers sync", rigger: &RiggerConfig{StackDir: "a", SyncRegistries: []string{"global", "team-modules"}}},
		{name: "a registry without a name", rigger: &RiggerConfig{StackDir: "a", SyncRegistries: []string{""}}, want: `sync_registries: "" is not the name of a registry`},
		{name: "a registry that reads as a flag", rigger: &RiggerConfig{StackDir: "a", SyncRegistries: []string{"-region"}}, want: `sync_registries: "-region" is not the name of a registry`},
		{name: "a registry named twice", rigger: &RiggerConfig{StackDir: "a", SyncRegistries: []string{"global", "global"}}, want: `sync_registries: "global" is named twice`},
		{name: "registries where the program applies", rigger: &RiggerConfig{StackDir: "a", Mode: RiggerStandalone, SyncRegistries: []string{"global"}}, want: "sync_registries is of mode: embedded"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := (&DeploymentPlan{Rigger: tt.rigger}).validateRigger()
			if (err == nil) != (tt.want == "") || (err != nil && !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("validateRigger(%+v) = %v, want %q", tt.rigger, err, tt.want)
			}
		})
	}
}

func TestRiggerConfigDefaults(t *testing.T) {
	tests := []struct {
		name         string
		section      string
		wantOnServer bool
		wantEmbedded bool
	}{
		{name: "nothing said", section: "  stack_dir: stacks/platform\n", wantOnServer: true, wantEmbedded: true},
		{name: "the program of this machine", section: "  stack_dir: stacks/platform\n  run_on: local\n", wantEmbedded: true},
		{name: "the program applies", section: "  stack_dir: stacks/platform\n  mode: standalone\n", wantOnServer: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "metadata: {name: dev}\nrelease: {version: v0.2.0}\nrigger:\n" + tt.section +
				"inventory:\n  masters:\n    - {host: 10.0.0.1, hostname: s1}\n"

			p, err := Parse([]byte(src), FormatYAML, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}

			r := p.Rigger
			if r.RunsOnServer() != tt.wantOnServer || r.Embedded() != tt.wantEmbedded {
				t.Errorf("run_on %q (on the server: %t), mode %q (embedded: %t), want on the server %t, embedded %t",
					r.RunOn, r.RunsOnServer(), r.Mode, r.Embedded(), tt.wantOnServer, tt.wantEmbedded)
			}

			// The defaults are written into the plan: a plan printed back says them.
			if r.RunOn == "" || r.Mode == "" {
				t.Errorf("run_on %q, mode %q: a default is missing in the loaded plan", r.RunOn, r.Mode)
			}
		})
	}
}

func TestDecodeHCLRigger(t *testing.T) {
	src := `
rigger {
  stack_dir = "stacks/platform"
  values    = ["stacks/platform/values/prod.vars.hcl"]
  vars = {
    count = "3"
  }
  prune  = true
  run_on = "local"
  mode   = "standalone"
  binary = "/usr/local/bin/rigger"
  sync_registries = ["global", "team-modules"]
}
`

	var p DeploymentPlan
	if err := decodeHCL([]byte(src), "", &p); err != nil {
		t.Fatal(err)
	}

	if p.Rigger == nil || p.Rigger.StackDir != "stacks/platform" || len(p.Rigger.Values) != 1 || p.Rigger.Vars["count"] != "3" || !p.Rigger.Prune {
		t.Fatalf("rigger = %+v", p.Rigger)
	}

	if p.Rigger.RunsOnServer() || p.Rigger.Embedded() || p.Rigger.Binary != "/usr/local/bin/rigger" {
		t.Fatalf("rigger = %+v, want the program of this machine, standalone", p.Rigger)
	}

	if got := p.Rigger.SyncRegistries; len(got) != 2 || got[0] != "global" || got[1] != "team-modules" {
		t.Fatalf("sync_registries = %q, want global and team-modules", got)
	}
}

// Norca has no pack manager any more: a plan that still has the packs key is
// refused in every format, whatever is under the key, and the message names
// the rigger section and where the GitOps bindings go.
func TestPacksSectionIsRefused(t *testing.T) {
	const (
		yamlHead = "inventory:\n  masters:\n    - host: 10.0.0.1\n"
		hclHead  = "inventory {\n  masters {\n    host = \"10.0.0.1\"\n  }\n}\n"
	)

	tests := []struct {
		name   string
		format Format
		src    string
	}{
		{
			name:   "yaml",
			format: FormatYAML,
			src: yamlHead + `packs:
  registry: {name: builtin, type: dir, url: builtin}
  deploy:
    - {name: cache, pack: redis, enabled: true}
`,
		},
		{
			name:   "yaml, bindings under the key",
			format: FormatYAML,
			src: yamlHead + `packs:
  gitops:
    enabled: true
    bindings:
      - name: a
        forgejo:
          url: https://f/o/r.git
`,
		},
		{
			name:   "yaml, the key without a value",
			format: FormatYAML,
			src:    "packs:\n" + yamlHead,
		},
		{
			name:   "json",
			format: FormatJSON,
			src: `{"inventory": {"masters": [{"host": "10.0.0.1"}]},
				"packs": {"registry": {"name": "builtin", "type": "dir", "url": "builtin"},
					"deploy": [{"name": "cache", "pack": "redis", "enabled": true}]}}`,
		},
		{
			name:   "json, null",
			format: FormatJSON,
			src:    `{"inventory": {"masters": [{"host": "10.0.0.1"}]}, "packs": null}`,
		},
		{
			name:   "hcl",
			format: FormatHCL,
			src: hclHead + `packs {
  registry {
    name = "builtin"
    type = "dir"
    url  = "builtin"
  }
  deploy {
    name    = "cache"
    pack    = "redis"
    enabled = true
    values {
      count = 1
    }
  }
}
`,
		},
		{
			name:   "hcl, bindings under the key",
			format: FormatHCL,
			src: hclHead + `packs {
  gitops {
    enabled = true
    binding "a" {
      forgejo {
        url = "https://f/o/r.git"
      }
    }
  }
}
`,
		},
		{
			name:   "hcl, an empty block",
			format: FormatHCL,
			src:    hclHead + "packs {}\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.src), tt.format, t.TempDir())
			if err == nil {
				t.Fatal("Parse() accepted a plan with the packs key")
			}

			for _, want := range []string{"packs: the pack manager is gone", "a rigger section", "rigger.gitops", "top-level gitops"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Parse() = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// The refusal is of the key: a plan that only has the word in a value loads.
func TestPlanWithoutPacksKeyLoads(t *testing.T) {
	src := "metadata: {name: packs}\ninventory:\n  masters:\n    - host: 10.0.0.1\n"
	if _, err := Parse([]byte(src), FormatYAML, t.TempDir()); err != nil {
		t.Fatalf("Parse() = %v, want the plan loaded", err)
	}
}
