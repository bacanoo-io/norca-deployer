// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"errors"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/gitops"
)

func TestKindSelector(t *testing.T) {
	t.Parallel()

	if set, err := KindSelector(nil); err != nil || set != nil {
		t.Fatalf("no words = %v, %v", set, err)
	}

	set, err := KindSelector([]string{"data", "job"})
	if err != nil || !set[KindVariable] || !set[KindSecret] || !set[KindJob] || set[KindNamespace] {
		t.Fatalf("data, job = %v, %v", set, err)
	}

	if _, err := KindSelector([]string{"jobs"}); err == nil || !strings.Contains(err.Error(), `"jobs" is no kind`) {
		t.Fatalf("a plural is not refused: %v", err)
	}

	both := IntersectKinds(set, map[string]bool{KindJob: true, KindNamespace: true})
	if len(both) != 1 || !both[KindJob] {
		t.Fatalf("IntersectKinds = %v", both)
	}

	if IntersectKinds(nil, set)[KindJob] != true || IntersectKinds(set, nil)[KindJob] != true {
		t.Fatal("nil allows all")
	}
}

// A stack that changes a kind the run may not change is blocked; an object
// counts as custom-object, a written secret as secret.
func TestGuards_Kinds(t *testing.T) {
	t.Parallel()

	g := Guards{Kinds: map[string]bool{KindJob: true}}

	if err := g.Check(planWith("upgrade", gitops.ResourceChange{Kind: "job", Name: "api", Action: "update"})); err != nil {
		t.Fatalf("a job change: %v", err)
	}

	err := g.Check(planWith("upgrade", gitops.ResourceChange{Kind: "object", Name: "db", Action: "create"}))
	if !errors.Is(err, ErrBlocked) || !strings.Contains(err.Error(), "changes the custom-object db") {
		t.Fatalf("an object change: %v", err)
	}

	p := planWith("unchanged")
	p.Secrets = []gitops.SecretPlan{{Path: "api/db", Action: "create"}}

	if err := g.Check(p); !errors.Is(err, ErrBlocked) || !strings.Contains(err.Error(), "writes the secret api/db") {
		t.Fatalf("a secret written: %v", err)
	}
}

func TestCheckChanges(t *testing.T) {
	t.Parallel()

	p := planWith("upgrade",
		gitops.ResourceChange{Kind: "job", Name: "a", Action: "update"},
		gitops.ResourceChange{Kind: "job", Name: "b", Action: "create"})
	p.Secrets = []gitops.SecretPlan{{Path: "x", Action: "keep"}, {Path: "y", Action: "create"}}
	plans := []*Planned{{Plan: p}, {Plan: planWith("remove")}}

	if n := Changes(p); n != 3 {
		t.Fatalf("Changes = %d, want 3", n)
	}

	if err := CheckChanges(plans, 4, false); err != nil {
		t.Fatalf("4 changes within 4: %v", err)
	}

	if err := CheckChanges(plans, 3, false); !errors.Is(err, ErrBlocked) || !strings.Contains(err.Error(), "4 resources, more than limits.max_changes (3)") {
		t.Fatalf("over the limit: %v", err)
	}

	if err := CheckChanges(plans, 3, true); err != nil {
		t.Fatalf("--yes-really: %v", err)
	}

	if !HasChanges(p) || HasChanges(planWith("unchanged")) {
		t.Fatal("HasChanges answers wrong")
	}
}
