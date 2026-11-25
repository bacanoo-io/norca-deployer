// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// planHeadHCL is the smallest HCL plan that validates, without a rigger
// section.
const planHeadHCL = `metadata {
  name   = "dev"
  region = "eu"
}

inventory {
  masters {
    host     = "10.0.0.1"
    hostname = "s1"
  }
}
`

// escapingPlan has a stack block with what an HCL v1 string would change
// or cannot hold: interpolations, template directives, an escaped $${,
// expressions of HCL2, comments, a <<- heredoc with a blank line that has
// spaces, and a heredoc whose lines are kept as they are.
var escapingPlan = planHeadHCL + `
rigger {
  stack {
    # A comment that stays.
    rigger {
      name = "base" # a trailing comment
    }

    /* A block comment */
    module "web" {
      source = "./modules/web"
      values = {
        greeting = "hello ${var.name}"
        mode     = "%{ if var.prod }prod%{ else }dev%{ endif }"
        url      = module.db.outputs.url
        hosts    = [for h in var.hosts : "${h}.example"]
        literal  = "$${node.datacenter}"
      }
    }

    flush = <<-EOT
      echo "$${HOME}"
        indented
` + "      " + `
      done
      EOT
    plain = <<EOT
  kept as is
EOT
  }
}
`

// escapingStack is the text of the stack block of escapingPlan.
const escapingStack = `# A comment that stays.
rigger {
  name = "base" # a trailing comment
}

/* A block comment */
module "web" {
  source = "./modules/web"
  values = {
    greeting = "hello ${var.name}"
    mode     = "%{ if var.prod }prod%{ else }dev%{ endif }"
    url      = module.db.outputs.url
    hosts    = [for h in var.hosts : "${h}.example"]
    literal  = "$${node.datacenter}"
  }
}

flush = <<-EOT
  echo "$${HOME}"
    indented
` + "      " + `
  done
  EOT
plain = <<EOT
  kept as is
EOT
`

func TestDecodeHCLStackBlock(t *testing.T) {
	p, err := Parse([]byte(escapingPlan), FormatHCL, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if p.Rigger.Stack != escapingStack {
		t.Fatalf("stack =\n%s\nwant\n%s", p.Rigger.Stack, escapingStack)
	}

	// The text is a stack.hcl of HCL2, and its heredocs have the values
	// they had in the plan.
	stack, diags := hclsyntax.ParseConfig([]byte(p.Rigger.Stack), "stack.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatalf("the stack does not parse: %v", diags)
	}

	plan, diags := hclsyntax.ParseConfig([]byte(escapingPlan), "plan.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	inPlan := blockBody(t, blockBody(t, plan.Body.(*hclsyntax.Body), "rigger"), "stack")
	for _, name := range []string{"flush", "plain"} {
		want := staticString(t, inPlan.Attributes[name])
		if got := staticString(t, stack.Body.(*hclsyntax.Body).Attributes[name]); got != want {
			t.Errorf("%s = %q, in the plan %q", name, got, want)
		}
	}
}

// planFile writes a plan into a directory of its own.
func planFile(t *testing.T, name, src string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func blockBody(t *testing.T, body *hclsyntax.Body, name string) *hclsyntax.Body {
	t.Helper()

	for _, b := range body.Blocks {
		if b.Type == name {
			return b.Body
		}
	}

	t.Fatalf("no block %s", name)

	return nil
}

func staticString(t *testing.T, attr *hclsyntax.Attribute) string {
	t.Helper()

	if attr == nil {
		t.Fatal("no attribute")
	}

	v, diags := attr.Expr.Value(nil)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	return v.AsString()
}

// The stack of the example is a block with comments; its text is the
// stack.hcl in testdata, which the YAML example has too (TestExamples).
func TestExampleClusterStackBlock(t *testing.T) {
	p, err := LoadFromFile(filepath.Join("..", "..", "examples", "nodes", "cluster.hcl"))
	if err != nil {
		t.Fatal(err)
	}

	want, err := os.ReadFile(filepath.Join("testdata", "cluster-stack.hcl"))
	if err != nil {
		t.Fatal(err)
	}

	if got := p.Rigger.Stack; got != string(want) {
		t.Fatalf("stack =\n%s\nwant\n%s", got, want)
	}
}

// Every example plan in HCL is HCL2 syntax too: its blocks of text are read
// the same way as those of a new plan.
func TestExamplePlansAreHCL2(t *testing.T) {
	for _, name := range []string{"cluster.hcl", "single-node.hcl"} {
		path := filepath.Join("..", "..", "examples", "nodes", name)

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		cut, err := cutContentBlocks(data, name)
		if err != nil || cut.notHCL2 != nil {
			t.Errorf("%s: %v, %v", name, err, cut.notHCL2)
		}

		if _, err := LoadFromFile(path); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestDecodeHCLExtraBlocks(t *testing.T) {
	src := `metadata {
  name = "dev"
}

client_defaults {
  extra {
    max_kill_timeout = "60s"
  }
}

node_scaling {
  enabled = true
  client {
    extra = "cpu_total_compute = 4000"
  }
}

inventory {
  masters {
    host     = "10.0.0.1"
    hostname = "s1"
    server_settings {
      bootstrap_expect = 1
      extra {
        heartbeat_grace = "60s" # the RPC timeout is 30s
      }
    }
    client_settings {
      extra {
        options = {
          "docker.cleanup.image" = "false"
        }
      }
    }
  }
}
`

	var p DeploymentPlan
	if err := decodeHCL([]byte(src), "p.hcl", &p); err != nil {
		t.Fatal(err)
	}

	m := p.Inventory.Masters[0]
	checks := []struct{ name, got, want string }{
		{"client_defaults.extra", p.ClientDefaults.Extra, "max_kill_timeout = \"60s\"\n"},
		{"node_scaling.client.extra", p.NodeScaling.Client.Extra, "cpu_total_compute = 4000"},
		{"server_settings.extra", m.ServerSettings.Extra, "heartbeat_grace = \"60s\" # the RPC timeout is 30s\n"},
		{"client_settings.extra", m.ClientSettings.Extra, "options = {\n  \"docker.cleanup.image\" = \"false\"\n}\n"},
	}

	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}

	if m.ServerSettings.BootstrapExpect != 1 || m.Hostname != "s1" {
		t.Errorf("the rest of the node is lost: %+v", m)
	}
}

func TestDecodeHCLRulesBlock(t *testing.T) {
	src := planHeadHCL + `
security {
  acl {
    enabled = true
    policies {
      name = "events-reader"
      rules {
        namespace "default" {
          policy = "read"
        }
      }
    }
    policies {
      name       = "other"
      rules_file = "other.hcl"
    }
  }
}
`

	var p DeploymentPlan
	if err := decodeHCL([]byte(src), "p.hcl", &p); err != nil {
		t.Fatal(err)
	}

	pols := p.Security.ACL.Policies
	if want := "namespace \"default\" {\n  policy = \"read\"\n}\n"; len(pols) != 2 || pols[0].Rules != want || pols[1].RulesFile != "other.hcl" {
		t.Fatalf("policies = %+v, want the rules %q", pols, want)
	}
}

// A text may still be a string: a heredoc keeps working, and a one-line
// block is one line of text.
func TestDecodeHCLContentStrings(t *testing.T) {
	src := planHeadHCL + `
rigger {
  stack = <<EOT
rigger {
  name = "base"
}
EOT
}

client_defaults {
  extra { max_kill_timeout = "60s" }
}
`

	p, err := Parse([]byte(src), FormatHCL, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if want := "rigger {\n  name = \"base\"\n}\n"; p.Rigger.Stack != want {
		t.Errorf("stack = %q, want %q", p.Rigger.Stack, want)
	}

	if want := "max_kill_timeout = \"60s\"\n"; p.ClientDefaults.Extra != want {
		t.Errorf("extra = %q, want %q", p.ClientDefaults.Extra, want)
	}
}

// The errors about a block of text name the file and the line of the block.
func TestDecodeHCLContentBlockErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "block given twice",
			body: "rigger {\n  stack {\n    a = 1\n  }\n  stack {\n    b = 2\n  }\n}\n",
			want: "p.hcl:16: rigger.stack: block given twice (first on line 13)",
		},
		{
			name: "block and attribute",
			body: "rigger {\n  stack = \"a = 1\"\n  stack {\n    b = 2\n  }\n}\n",
			want: "p.hcl:14: rigger.stack: given as a block and as stack = (line 13); write it once",
		},
		{
			name: "labels",
			body: "rigger {\n  stack \"x\" {\n    b = 2\n  }\n}\n",
			want: "p.hcl:13: rigger.stack: a block of text has no labels",
		},
		{
			name: "empty block",
			body: "rigger {\n  stack {\n  }\n}\n",
			want: "p.hcl:13: rigger.stack: the block is empty",
		},
		{
			name: "extra twice in a node",
			body: "client_defaults {\n  extra {\n    a = 1\n  }\n  extra {\n    b = 2\n  }\n}\n",
			want: "p.hcl:16: client_defaults.extra: block given twice",
		},
		{
			name: "rules of a policy",
			body: "security {\n  acl {\n    policies {\n      name = \"x\"\n      rules = \"a\"\n      rules {\n        b = 2\n      }\n    }\n  }\n}\n",
			want: "p.hcl:17: security.acl.policies[0].rules: given as a block and as rules = (line 16)",
		},
		{
			name: "an object is no text",
			body: "rigger {\n  stack = {\n    a = 1\n  }\n}\n",
			want: "rigger.stack: text: a string, or a block whose body is the text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadFromFile(planFile(t, "p.hcl", planHeadHCL+tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v, want %q", err, tt.want)
			}
		})
	}
}

// The plan the HCL v1 decoder reads keeps the lines of the file: its errors
// after a block of text name the line of the file.
func TestDecodeHCLLinesAfterABlock(t *testing.T) {
	src := "rigger {\n  stack {\n    rigger {\n      name = \"x\"\n    }\n  }\n}\n\nmetadata {\n  name = upper(\"x\")\n}\n"

	err := decodeHCL([]byte(src), "p.hcl", &DeploymentPlan{})
	if err == nil || !strings.Contains(err.Error(), "At 10:") {
		t.Fatalf("error %v, want one at line 10", err)
	}
}

// A plan that is HCL v1 but no HCL2 syntax (a shell ${X:-y} in a script) is
// read by HCL v1; a block of text then needs HCL2, and the error says why
// the plan is not.
func TestDecodeHCLNotHCL2(t *testing.T) {
	const script = `
scripts {
  name   = "env"
  phase  = "post_install"
  inline = <<EOT
echo "${NORCA_DC:-dc1}"
EOT
}
`

	t.Run("no block of text", func(t *testing.T) {
		p, err := Parse([]byte(planHeadHCL+script), FormatHCL, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}

		if want := "echo \"${NORCA_DC:-dc1}\"\n"; p.Scripts[0].Inline != want {
			t.Fatalf("inline = %q, want %q", p.Scripts[0].Inline, want)
		}
	})

	t.Run("a block HCL v1 reads", func(t *testing.T) {
		src := planHeadHCL + script + "rigger {\n  stack {\n    rigger {\n      name = \"x\"\n    }\n  }\n}\n"

		_, err := LoadFromFile(planFile(t, "p.hcl", src))
		for _, want := range []string{"rigger.stack: text", "a block of text needs a plan of HCL2 syntax", "p.hcl:17,"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error %v, want %q", err, want)
			}
		}
	})

	t.Run("a block of HCL2 expressions", func(t *testing.T) {
		src := planHeadHCL + script + "rigger {\n  stack {\n    module \"x\" {\n      url = module.db.outputs.url\n    }\n  }\n}\n"

		_, err := LoadFromFile(planFile(t, "p.hcl", src))
		for _, want := range []string{"as HCL2 syntax, which the blocks of text (stack, extra, rules) need", "p.hcl:17,"} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error %v, want %q", err, want)
			}
		}
	})

	t.Run("neither", func(t *testing.T) {
		err := decodeHCL([]byte("metadata {\n"), "p.hcl", &DeploymentPlan{})
		if err == nil || !strings.Contains(err.Error(), "as HCL2 syntax") {
			t.Fatalf("error %v, want both reasons", err)
		}
	})
}

// A directory in rigger.stack is refused with the key it belongs to.
func TestStackDirectoryInStack(t *testing.T) {
	for name, src := range map[string]string{
		"yaml": "inventory:\n  masters:\n    - {host: 10.0.0.1, hostname: s1}\nrigger:\n  stack: stacks/platform\n",
		"hcl":  planHeadHCL + "rigger {\n  stack = \"stacks/platform\"\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadFromFile(planFile(t, "p."+name, src))
			if err == nil || !strings.Contains(err.Error(), "a directory is stack_dir") {
				t.Fatalf("error %v, want the directory refused", err)
			}
		})
	}
}

// The keys of the stack and of the settings in YAML: the old keys are
// unknown keys.
func TestContentKeysYAML(t *testing.T) {
	const head = "inventory:\n  masters:\n    - host: 10.0.0.1\n      hostname: s1\n"

	p, err := Parse([]byte(head+`      server_settings:
        bootstrap_expect: 1
        extra: |
          heartbeat_grace = "60s"
      client_settings:
        extra: |
          max_kill_timeout = "60s"
rigger:
  stack: |
    rigger {
      name = "base"
    }
`), FormatYAML, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	m := p.Inventory.Masters[0]
	if m.ServerSettings.Extra != "heartbeat_grace = \"60s\"\n" || m.ClientSettings.Extra != "max_kill_timeout = \"60s\"\n" ||
		p.Rigger.Stack != "rigger {\n  name = \"base\"\n}\n" {
		t.Fatalf("server %+v, client %+v, rigger %+v", m.ServerSettings, m.ClientSettings, p.Rigger)
	}

	for _, tt := range []struct{ name, src, want string }{
		{"stack_hcl", head + "rigger:\n  stack_hcl: |\n    rigger {}\n", `rigger (line 6): unknown key "stack_hcl"`},
		{"server extra_hcl", head + "      server_settings:\n        extra_hcl: x = 1\n", `server_settings (line 6): unknown key "extra_hcl"`},
		{"client extra_hcl", head + "      client_settings:\n        extra_hcl: x = 1\n", `client settings (line 6): unknown key "extra_hcl"`},
		{"client_defaults extra_hcl", head + "client_defaults:\n  extra_hcl: x = 1\n", `client settings (line 6): unknown key "extra_hcl"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.src), FormatYAML, t.TempDir())
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %v, want %q", err, tt.want)
			}
		})
	}
}

func TestBlockTextIndentation(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "one line", body: "stack { a = 1 }", want: "a = 1\n"},
		{name: "a comment after the brace", body: "stack { # the stack\n    a = 1\n  }", want: "# the stack\na = 1\n"},
		{name: "tabs", body: "stack {\n\t\ta = 1\n\t\tb {\n\t\t\tc = 2\n\t\t}\n\t}", want: "a = 1\nb {\n\tc = 2\n}\n"},
		{name: "blank lines inside and around", body: "stack {\n\n    a = 1\n   \n    b = 2\n\n  }", want: "a = 1\n\nb = 2\n"},
		{name: "crlf", body: "stack {\r\n    a = 1\r\n  }", want: "a = 1\n"},
		{
			name: "a <<- heredoc less indented than the block keeps its lines",
			body: "stack {\n    s = <<-EOT\n  x\n    EOT\n  }",
			want: "s = <<-EOT\n  x\n    EOT\n",
		},
		{
			name: "a heredoc in a heredoc keeps its lines",
			body: "stack {\n    s = <<-EOT\n      ${<<-IN\n      y\n      IN\n      }\n      EOT\n  }",
			want: "s = <<-EOT\n      ${<<-IN\n      y\n      IN\n      }\n      EOT\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := []byte("rigger {\n  " + tt.body + "\n}\n")

			cut, err := cutContentBlocks(src, "p.hcl")
			if err != nil || cut.notHCL2 != nil {
				t.Fatalf("%v, %v", err, cut.notHCL2)
			}

			if len(cut.texts) != 1 {
				t.Fatalf("texts = %q", cut.texts)
			}

			for _, got := range cut.texts {
				if got != tt.want {
					t.Errorf("text = %q, want %q", got, tt.want)
				}
			}
		})
	}
}
