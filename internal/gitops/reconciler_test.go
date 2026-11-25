package gitops

import (
	"testing"

	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

func TestReconcilerRegisterAndLock(t *testing.T) {
	rec := NewReconciler(NewRingLogger(10), &fakeNorca{}, 0)
	defer rec.Close()

	rec.RegisterBinding(config.GitOpsBinding{
		Name:      "test-app",
		Namespace: "apps",
		Forgejo: &config.RepoConfig{
			URL:    "https://forgejo.internal/org/test-env.git",
			Branch: "main",
		},
	})

	if len(rec.bindings) != 1 {
		t.Fatalf("expected 1 binding, got %d", len(rec.bindings))
	}
	if rec.debounce != DefaultDebounce {
		t.Fatalf("debounce %s", rec.debounce)
	}
}

// Two bindings of one stack directory in one namespace and region run one
// after the other; another directory, namespace or region is another stack.
func TestStackKey(t *testing.T) {
	binding := func(url, subpath, namespace, region string) *config.GitOpsBinding {
		return &config.GitOpsBinding{Namespace: namespace, Region: region, Forgejo: &config.RepoConfig{URL: url, Subpath: subpath}}
	}
	base := stackKey(binding("https://git.example/o/r.git", "stacks/a", "ns", "eu"))
	for name, tc := range map[string]struct {
		b    *config.GitOpsBinding
		same bool
	}{
		"the same":           {binding("https://git.example/o/r.git", "stacks/a", "ns", "eu"), true},
		"other credentials":  {binding("https://token@git.example/O/R", "/stacks/a/", "ns", "eu"), true},
		"another directory":  {binding("https://git.example/o/r.git", "stacks/b", "ns", "eu"), false},
		"another namespace":  {binding("https://git.example/o/r.git", "stacks/a", "other", "eu"), false},
		"another region":     {binding("https://git.example/o/r.git", "stacks/a", "ns", "us"), false},
		"another repository": {binding("https://git.example/o/s.git", "stacks/a", "ns", "eu"), false},
		"another host":       {binding("https://other.example/o/r.git", "stacks/a", "ns", "eu"), false},
	} {
		if got := stackKey(tc.b) == base; got != tc.same {
			t.Errorf("%s: the same stack %t, want %t", name, got, tc.same)
		}
	}
}
