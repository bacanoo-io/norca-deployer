package config

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
packs:
  gitops:
    enabled: true
    debounce_duration: "5s"
    bindings:
      - name: "payment-api"
        release_name: "payment-api"
        namespace: "prod"
        atomic: true
        forgejo:
          url: "https://forgejo.internal/org/repo.git"
          branch: "main"
          subpath: "packs/payment"
        webhook:
          secret_key: "my-secret"
          events: ["push"]
        values:
          replicas: 3
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
	if b.Name != "payment-api" {
		t.Errorf("expected name payment-api, got %s", b.Name)
	}
	if b.EffectiveProvider() != "forgejo" {
		t.Errorf("expected provider forgejo, got %s", b.EffectiveProvider())
	}
	if b.EffectiveRepo().URL != "https://forgejo.internal/org/repo.git" {
		t.Errorf("unexpected repo URL: %s", b.EffectiveRepo().URL)
	}
	if b.Values["replicas"] != 3 {
		t.Errorf("expected replicas 3, got %v", b.Values["replicas"])
	}
}

func TestGitOpsConfigHCL(t *testing.T) {
	hclData := `
metadata {
  name = "test-cluster"
}
packs {
  gitops {
    enabled = true
    binding {
      name         = "github-frontend"
      release_name = "frontend"
      atomic       = true
      github {
        url     = "https://github.com/org/frontend.git"
        branch  = "release"
        subpath = "pack"
      }
      webhook {
        secret_key = "gh-secret"
        events     = ["push", "release"]
      }
      values {
        env = "production"
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
	if b.Name != "github-frontend" {
		t.Errorf("expected name github-frontend, got %s", b.Name)
	}
	if b.EffectiveProvider() != "github" {
		t.Errorf("expected provider github, got %s", b.EffectiveProvider())
	}
	if b.EffectiveRepo().URL != "https://github.com/org/frontend.git" {
		t.Errorf("unexpected repo URL: %s", b.EffectiveRepo().URL)
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
packs {
  gitops {
    enabled = true
    binding "payment" {
      release_name = "payment-api"
      atomic       = true
      forgejo {
        url = "https://f.example/o/r.git"
      }
      webhook {
        secret_key = "s"
        events     = ["push"]
      }
      values = {
        replicas = 5
      }
    }
    binding "orders" {
      release_name = "orders-api"
      gitea {
        url = "https://g.example/o/r.git"
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
	if b[0].ReleaseName != "payment-api" || !b[0].Atomic || b[0].Webhook.SecretKey != "s" {
		t.Fatalf("got %+v", b[0])
	}
	if b[1].EffectiveProvider() != "gitea" || b[1].EffectiveRepo().URL != "https://g.example/o/r.git" {
		t.Fatalf("got %+v", b[0])
	}
}

func TestGitOpsDisabledHasNoBindings(t *testing.T) {
	plan, err := LoadFromFile(writePlan(t, "p.yaml", `gitops:
  enabled: false
  bindings:
    - name: a
      release_name: r
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
	good := "    - name: a\n      release_name: r\n      forgejo:\n        url: https://f/o/r.git\n"
	for name, tc := range map[string]struct{ plan, want string }{
		"no release":  {bind("    - name: a\n      forgejo:\n        url: https://f/o/r.git\n"), "release_name"},
		"no name":     {bind("    - release_name: r\n      forgejo:\n        url: https://f/o/r.git\n"), "binding name"},
		"no provider": {bind("    - name: a\n      release_name: r\n"), "exactly one"},
		"two blocks":  {bind("    - name: a\n      release_name: r\n      forgejo:\n        url: u\n      gitea:\n        url: u\n"), "exactly one"},
		"no url":      {bind("    - name: a\n      release_name: r\n      forgejo:\n        branch: main\n"), "url"},
		"duplicate":   {bind(good + good), "duplicate"},
		"subpath":     {bind("    - name: a\n      release_name: r\n      forgejo:\n        url: u\n        subpath: ../x\n"), "leaves"},
		"no secret":   {bind(good + "      webhook:\n        events: [push]\n"), "secret_key"},
		"pattern":     {bind(good + "      webhook:\n        secret_key: s\n        filter_branches: ['[']\n"), "invalid filter"},
		"debounce":    {"gitops:\n  enabled: true\n  debounce_duration: soon\n", "debounce_duration"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadFromFile(writePlan(t, "p.yaml", tc.plan))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestGitOpsResolvedBindings(t *testing.T) {
	path := writePlan(t, "p.yaml", `gitops:
  enabled: true
  debounce_duration: 2s
  log_buffer_size: 9
  bindings:
    - name: a
      release_name: r
      github:
        url: https://ghe.example/o/r.git
        api_url: https://api.ghe.example
        auth_secret: "!enc:token"
        ca_cert_pem: "!file:ca.pem"
      webhook:
        secret_key: "!enc:hook"
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
	nested := b.Values["nested"].(map[string]interface{})["list"].([]interface{})
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
	head := "gitops:\n  enabled: true\n  bindings:\n    - name: a\n      release_name: r\n      forgejo:\n        url: https://f/o/r.git\n"
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
    release_name = "r"
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
}
