package plan

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestGitOpsConfigYAML(t *testing.T) {
	yamlData := `
metadata:
  name: "test-cluster"
rigger:
  gitops:
    enabled: true
    debounce_duration: "5s"
    bindings:
      - name: "platform"
        namespace: "platform"
        region: "eu-1"
        forgejo:
          url: "https://forgejo.internal/org/repo.git"
          branch: "main"
          subpath: "stacks/platform"
        webhook:
          secret_key: "my-secret"
          events: ["push"]
        values:
          replicas: 3
        policy:
          mode: plan-only
          prune: true
`
	var plan DeploymentPlan
	if err := yaml.Unmarshal([]byte(yamlData), &plan); err != nil {
		t.Fatalf("failed to decode YAML: %v", err)
	}

	bindings := plan.AllGitOpsBindings()
	if len(bindings) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(bindings))
	}
	b := bindings[0]
	if b.Name != "platform" || b.Namespace != "platform" || b.Region != "eu-1" {
		t.Errorf("got %+v", b)
	}
	if b.EffectiveProvider() != "forgejo" {
		t.Errorf("expected provider forgejo, got %s", b.EffectiveProvider())
	}
	if b.EffectiveRepo().URL != "https://forgejo.internal/org/repo.git" || b.EffectiveRepo().Subpath != "stacks/platform" {
		t.Errorf("unexpected repo: %+v", b.EffectiveRepo())
	}
	if b.Values["replicas"] != 3 {
		t.Errorf("expected replicas 3, got %v", b.Values["replicas"])
	}
	if !b.PlanOnly() || !b.Prune() {
		t.Errorf("policy %+v", b.Policy)
	}
}

func TestGitOpsConfigHCL(t *testing.T) {
	hclData := `
metadata {
  name = "test-cluster"
}
rigger {
  gitops {
    enabled = true
    binding {
      name      = "github-frontend"
      namespace = "web"
      github {
        url     = "https://github.com/org/frontend.git"
        branch  = "release"
        subpath = "stack"
      }
      webhook {
        secret_key = "gh-secret"
        events     = ["push", "release"]
      }
      values {
        env = "production"
      }
      policy {
        mode  = "plan-only"
        prune = true
      }
    }
  }
}
`
	var plan DeploymentPlan
	if err := decodeHCL([]byte(hclData), &plan); err != nil {
		t.Fatalf("failed to decode HCL: %v", err)
	}

	bindings := plan.AllGitOpsBindings()
	if len(bindings) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(bindings))
	}
	b := bindings[0]
	if b.Name != "github-frontend" || b.Namespace != "web" {
		t.Errorf("got %+v", b)
	}
	if b.EffectiveProvider() != "github" {
		t.Errorf("expected provider github, got %s", b.EffectiveProvider())
	}
	if b.EffectiveRepo().URL != "https://github.com/org/frontend.git" {
		t.Errorf("unexpected repo URL: %s", b.EffectiveRepo().URL)
	}
	if b.Values["env"] != "production" {
		t.Errorf("values %v", b.Values)
	}
	if !b.PlanOnly() || !b.Prune() {
		t.Errorf("policy %+v", b.Policy)
	}
}

func writePlan(t *testing.T, name, gitops string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	head := "inventory:\n  masters:\n    - host: 10.0.0.1\n"
	if strings.HasSuffix(name, ".hcl") {
		head = "inventory {\n  masters {\n    host = \"10.0.0.1\"\n  }\n}\n"
	}
	if err := os.WriteFile(path, []byte(head+gitops), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGitOpsLabelledBindingsHCL(t *testing.T) {
	plan, err := LoadFromFile(writePlan(t, "p.hcl", `
rigger {
  gitops {
    enabled = true
    binding "payment" {
      namespace = "shop"
      forgejo {
        url = "https://f.example/o/r.git"
      }
      webhook {
        secret_key = "s"
        events     = ["push"]
      }
      verify {
        public_keys = ["KEY"]
      }
      values = {
        replicas = 5
      }
    }
    binding "orders" {
      gitea {
        url = "https://g.example/o/r.git"
      }
      policy {
        mode = "plan-only"
      }
    }
  }
}
`))
	if err != nil {
		t.Fatal(err)
	}
	b := plan.AllGitOpsBindings()
	if len(b) != 2 || b[0].Name != "payment" || b[1].Name != "orders" {
		t.Fatalf("got %+v", b)
	}
	if b[0].Namespace != "shop" || b[0].Webhook.SecretKey != "s" || b[0].PlanOnly() || b[0].Prune() {
		t.Fatalf("got %+v", b[0])
	}
	if b[1].EffectiveProvider() != "gitea" || b[1].EffectiveRepo().URL != "https://g.example/o/r.git" || !b[1].PlanOnly() {
		t.Fatalf("got %+v", b[1])
	}
}

// A rigger section may hold bindings only: the stack of the plan and the
// stacks of repositories are apart.
func TestGitOpsUnderRiggerNeedsNoStackOfThePlan(t *testing.T) {
	plan, err := LoadFromFile(writePlan(t, "p.yaml", `rigger:
  gitops:
    enabled: true
    bindings:
      - name: a
        forgejo:
          url: https://f.example/o/r.git
        policy:
          mode: plan-only
`))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Rigger.HasStack() || len(plan.AllGitOpsBindings()) != 1 {
		t.Fatalf("got %+v", plan.Rigger)
	}

	// A setting of the stack of the plan without that stack is still an error.
	_, err = LoadFromFile(writePlan(t, "p.yaml", `rigger:
  prune: true
  gitops:
    enabled: true
`))
	if err == nil || !strings.Contains(err.Error(), "stack or stack_hcl is required") {
		t.Fatalf("got %v", err)
	}
}

// The bindings of rigger.gitops come before those of the top-level gitops.
func TestGitOpsBothPlaces(t *testing.T) {
	plan, err := LoadFromFile(writePlan(t, "p.yaml", `rigger:
  gitops:
    enabled: true
    debounce_duration: 2s
    bindings:
      - name: a
        forgejo: {url: "https://f.example/o/a.git"}
        policy: {mode: plan-only}
gitops:
  enabled: true
  debounce_duration: 9s
  bindings:
    - name: b
      forgejo: {url: "https://f.example/o/b.git"}
      policy: {mode: plan-only}
`))
	if err != nil {
		t.Fatal(err)
	}
	b := plan.AllGitOpsBindings()
	if len(b) != 2 || b[0].Name != "a" || b[1].Name != "b" {
		t.Fatalf("got %+v", b)
	}
	if plan.GitOpsDebounce() != 2*time.Second {
		t.Fatalf("debounce %s", plan.GitOpsDebounce())
	}
}

func TestGitOpsDisabledHasNoBindings(t *testing.T) {
	plan, err := LoadFromFile(writePlan(t, "p.yaml", `gitops:
  enabled: false
  bindings:
    - name: a
      forgejo:
        url: https://f.example/o/r.git
`))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(plan.AllGitOpsBindings()); n != 0 {
		t.Fatalf("%d bindings", n)
	}
}

func TestGitOpsValidation(t *testing.T) {
	bind := func(rest string) string {
		return "gitops:\n  enabled: true\n  bindings:\n" + rest
	}
	planOnly := "      policy:\n        mode: plan-only\n"
	good := "    - name: a\n      forgejo:\n        url: https://f/o/r.git\n" + planOnly
	for name, tc := range map[string]struct{ plan, want string }{
		"no name":     {bind("    - forgejo:\n        url: https://f/o/r.git\n" + planOnly), "binding name"},
		"no provider": {bind("    - name: a\n" + planOnly), "exactly one"},
		"two blocks":  {bind("    - name: a\n      forgejo:\n        url: u\n      gitea:\n        url: u\n" + planOnly), "exactly one"},
		"no url":      {bind("    - name: a\n      forgejo:\n        branch: main\n" + planOnly), "url"},
		"duplicate":   {bind(good + good), "duplicate"},
		"subpath":     {bind("    - name: a\n      forgejo:\n        url: u\n        subpath: ../x\n" + planOnly), "leaves"},
		"no secret":   {bind(good + "      webhook:\n        events: [push]\n"), "secret_key"},
		"pattern":     {bind(good + "      webhook:\n        secret_key: s\n        filter_branches: ['[']\n"), "invalid filter"},
		"debounce":    {"gitops:\n  enabled: true\n  debounce_duration: soon\n", "debounce_duration"},
		"mode":        {bind("    - name: a\n      forgejo:\n        url: u\n      policy:\n        mode: dry-run\n"), `mode "dry-run"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadFromFile(writePlan(t, "p.yaml", tc.plan))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// The servers evaluate a stack in their own process, so a binding that
// applies needs signed commits or tags; one that only plans does not.
func TestGitOpsApplyNeedsVerify(t *testing.T) {
	head := "gitops:\n  enabled: true\n  bindings:\n    - name: a\n      forgejo:\n        url: https://f/o/r.git\n"
	verify := "      verify:\n        public_keys: [KEY]\n"
	for name, tc := range map[string]struct {
		rest string
		ok   bool
	}{
		"the default mode without verify":  {"", false},
		"apply without verify":             {"      policy:\n        mode: apply\n", false},
		"apply with verify turned off":     {"      policy:\n        mode: apply\n      verify:\n        enabled: false\n        public_keys: [KEY]\n", false},
		"the default mode with verify":     {verify, true},
		"apply with verify":                {verify + "      policy:\n        mode: apply\n        prune: true\n", true},
		"plan-only without verify":         {"      policy:\n        mode: plan-only\n", true},
		"plan-only with verify turned off": {"      policy:\n        mode: plan-only\n      verify:\n        enabled: false\n", true},
		"plan-only with verify":            {verify + "      policy:\n        mode: plan-only\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadFromFile(writePlan(t, "p.yaml", head+tc.rest))
			if tc.ok {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			// The message says why, and names the mode that needs no keys.
			if err == nil {
				t.Fatal("must be refused")
			}
			for _, want := range []string{"gitops binding a", "verify", "own process", "plan-only"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("%q is not in: %v", want, err)
				}
			}
		})
	}
}

// A binding and its policy take only their own keys, in YAML and JSON too:
// a key that is ignored would leave a stack applied in another way than
// written.
func TestGitOpsUnknownKeysAreRefused(t *testing.T) {
	yamlPlan := func(binding, policy string) string {
		return "gitops:\n  enabled: true\n  bindings:\n    - name: a\n" + binding +
			"      forgejo:\n        url: https://f/o/r.git\n      policy:\n        mode: plan-only\n" + policy
	}
	hclPlan := func(binding, policy string) string {
		return "gitops {\n  enabled = true\n  binding \"a\" {\n" + binding +
			"    forgejo {\n      url = \"https://f/o/r.git\"\n    }\n    policy {\n      mode = \"plan-only\"\n" + policy + "    }\n  }\n}\n"
	}
	jsonPlan := `{"inventory":{"masters":[{"host":"10.0.0.1"}]},"gitops":{"enabled":true,"bindings":[` +
		`{"name":"a","forgejo":{"url":"u"},"policy":{"mode":"plan-only","window":"x"}}]}}`
	for name, tc := range map[string]struct{ file, plan, key string }{
		"yaml: a key of the policy": {"p.yaml", yamlPlan("", "        poll_interval: 60s\n"), "poll_interval"},
		"yaml: drift":               {"p.yaml", yamlPlan("", "        drift: heal\n"), "drift"},
		"yaml: release_name":        {"p.yaml", yamlPlan("      release_name: r\n", ""), "release_name"},
		"yaml: atomic":              {"p.yaml", yamlPlan("      atomic: true\n", ""), "atomic"},
		"yaml: max_history":         {"p.yaml", yamlPlan("      max_history: 5\n", ""), "max_history"},
		"yaml: a published stack":   {"p.yaml", yamlPlan("      source: acme/data\n", ""), "source"},
		"yaml: not enabled":         {"p.yaml", strings.Replace(yamlPlan("      release_name: r\n", ""), "enabled: true", "enabled: false", 1), "release_name"},
		"hcl: a key of the policy":  {"p.hcl", hclPlan("", "      poll_interval = \"60s\"\n"), "poll_interval"},
		"hcl: release_name":         {"p.hcl", hclPlan("    release_name = \"r\"\n", ""), "release_name"},
		"hcl: atomic":               {"p.hcl", hclPlan("    atomic = true\n", ""), "atomic"},
		"hcl: max_history":          {"p.hcl", hclPlan("    max_history = 5\n", ""), "max_history"},
		"json: a key of the policy": {"p.json", jsonPlan, "window"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tc.file)
			if tc.file == "p.json" {
				// A JSON plan is whole: writePlan puts a head before it.
				if err := os.WriteFile(path, []byte(tc.plan), 0600); err != nil {
					t.Fatal(err)
				}
			} else {
				path = writePlan(t, tc.file, tc.plan)
			}
			_, err := LoadFromFile(path)
			if err == nil || !strings.Contains(err.Error(), `unknown key "`+tc.key+`"`) {
				t.Fatalf("got %v", err)
			}
		})
	}

	// The keys a binding has are taken.
	all := yamlPlan("      namespace: n\n      region: r\n      provider: forgejo\n      values: {a: 1}\n", "        prune: true\n")
	if _, err := LoadFromFile(writePlan(t, "p.yaml", all)); err != nil {
		t.Fatal(err)
	}
}

func TestGitOpsResolvedBindings(t *testing.T) {
	path := writePlan(t, "p.yaml", `gitops:
  enabled: true
  debounce_duration: 2s
  log_buffer_size: 9
  bindings:
    - name: a
      github:
        url: https://ghe.example/o/r.git
        api_url: https://api.ghe.example
        auth_secret: "!enc:token"
        ca_cert_pem: "!file:ca.pem"
      webhook:
        secret_key: "!enc:hook"
      policy:
        mode: plan-only
      values:
        password: "!enc:value"
        nested:
          list: ["!enc:item", 5]
`)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "ca.pem"), []byte("PEM"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	resolve := func(v string) (string, error) {
		if after, ok := strings.CutPrefix(v, "!enc:"); ok {
			return "plain-" + after, nil
		}
		return v, nil
	}
	got, err := plan.ResolvedGitOpsBindings(resolve)
	if err != nil {
		t.Fatal(err)
	}
	b := got[0]
	repo := b.EffectiveRepo()
	if repo.AuthSecret != "plain-token" || repo.CACertPEM != "PEM" || b.Webhook.SecretKey != "plain-hook" {
		t.Fatalf("got %+v %+v", repo, b.Webhook)
	}
	if b.EffectiveAPIURL() != "https://api.ghe.example" {
		t.Fatalf("api url %s", b.EffectiveAPIURL())
	}
	if b.Values["password"] != "plain-value" {
		t.Fatalf("values %v", b.Values)
	}
	nested := b.Values["nested"].(map[string]any)["list"].([]any)
	if nested[0] != "plain-item" || nested[1] != 5 {
		t.Fatalf("nested %v", nested)
	}
	if plan.GitOpsDebounce() != 2*time.Second || plan.GitOpsLogBufferSize() != 9 {
		t.Fatalf("settings %s %d", plan.GitOpsDebounce(), plan.GitOpsLogBufferSize())
	}

	// The plan keeps the encrypted values.
	if orig := plan.AllGitOpsBindings()[0]; orig.EffectiveRepo().AuthSecret != "!enc:token" || orig.Webhook.SecretKey != "!enc:hook" {
		t.Fatalf("the plan was changed: %+v", orig.EffectiveRepo())
	}

	// A value that can't be decrypted is an error.
	if _, err := plan.ResolvedGitOpsBindings(func(v string) (string, error) {
		if strings.HasPrefix(v, "!enc:") {
			return "", errors.New("no key")
		}
		return v, nil
	}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestGitOpsVerifyConfig(t *testing.T) {
	head := "gitops:\n  enabled: true\n  bindings:\n    - name: a\n      forgejo:\n        url: https://f/o/r.git\n      policy:\n        mode: plan-only\n"
	if _, err := LoadFromFile(writePlan(t, "p.yaml", head+"      verify:\n        public_keys: []\n")); err == nil || !strings.Contains(err.Error(), "public_keys") {
		t.Fatalf("got %v", err)
	}
	plan, err := LoadFromFile(writePlan(t, "p.yaml", head+"      verify:\n        enabled: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if plan.AllGitOpsBindings()[0].Verify.Active() {
		t.Fatal("enabled: false must turn it off")
	}

	path := writePlan(t, "p.hcl", `
gitops {
  enabled = true
  binding "a" {
    forgejo {
      url = "https://f/o/r.git"
    }
    verify {
      public_keys = ["!file:k.asc"]
    }
  }
}
`)
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "k.asc"), []byte("KEY"), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err = LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := plan.ResolvedGitOpsBindings(func(v string) (string, error) { return v, nil })
	if err != nil {
		t.Fatal(err)
	}
	if !got[0].Verify.Active() || got[0].Verify.PublicKeys[0] != "KEY" {
		t.Fatalf("got %+v", got[0].Verify)
	}
	if plan.AllGitOpsBindings()[0].Verify.PublicKeys[0] != "!file:k.asc" {
		t.Fatal("the plan was changed")
	}
	// The default policy applies, and prunes nothing.
	if got[0].PlanOnly() || got[0].Prune() {
		t.Fatalf("policy %+v", got[0].Policy)
	}
}
