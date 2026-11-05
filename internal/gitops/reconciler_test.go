package gitops

import (
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/config"
)

func TestReconcilerRegisterAndLock(t *testing.T) {
	rec := NewReconciler(NewRingLogger(10), &fakeNorca{}, 0)
	defer rec.Close()

	rec.RegisterBinding(config.GitOpsBinding{
		Name:        "test-app",
		ReleaseName: "test-app-rel",
		Atomic:      true,
		Forgejo: &config.RepoConfig{
			URL:    "https://forgejo.internal/org/test-pack.git",
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
