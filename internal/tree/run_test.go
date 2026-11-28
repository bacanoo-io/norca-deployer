// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/gitops"
)

// plan is a plan of one release with these changes.
func planWith(action string, changes ...gitops.ResourceChange) *gitops.StackPlan {
	return &gitops.StackPlan{Stack: "tree-ns-apps", Releases: []gitops.ReleasePlan{{
		Release: gitops.ReleaseRef{Namespace: "apps", Name: "tree-apps"}, Action: action, Changes: changes,
	}}}
}

func TestGuards_Check(t *testing.T) {
	t.Parallel()

	del := func(kind, name string) gitops.ResourceChange {
		return gitops.ResourceChange{Kind: kind, Name: name, Action: actionDelete}
	}

	tests := []struct {
		name   string
		guards Guards
		plan   *gitops.StackPlan
		want   string
	}{
		{name: "nothing deleted", plan: planWith("upgrade", gitops.ResourceChange{Kind: "job", Name: "api", Action: "update"})},
		{name: "a delete without --prune", plan: planWith("upgrade", del("acl-policy", "old")), want: "the delete of the acl-policy old of the release apps/tree-apps needs --prune"},
		{name: "a delete with --prune", guards: Guards{Prune: true}, plan: planWith("upgrade", del("acl-policy", "old"))},
		{name: "a job needs --prune-jobs", guards: Guards{Prune: true}, plan: planWith("upgrade", del("job", "batch")), want: "purges the job: it needs --prune-jobs"},
		{name: "a job with both", guards: Guards{Prune: true, PruneJobs: true}, plan: planWith("upgrade", del("job", "batch"))},
		{name: "a Variable needs --prune-data", guards: Guards{Prune: true, PruneJobs: true}, plan: planWith("upgrade", del("variable", "config/x")), want: "needs --prune-data"},
		{name: "the default namespace never", guards: Guards{Prune: true, PruneJobs: true, PruneData: true}, plan: planWith("upgrade", del("namespace", "default")), want: "never deleted by a tree"},
		{name: "a release removed without --prune stays", plan: planWith("remove", del("job", "x"))},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.guards.Check(tt.plan)

			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("Check = %v, want none", err)
			case tt.want != "" && (err == nil || !errors.Is(err, ErrBlocked) || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("Check = %v, want a block with %q", err, tt.want)
			}
		})
	}
}

// fakeAPI answers the stacks API: every plan with planBody; it records the
// calls.
type fakeAPI struct {
	mu       sync.Mutex
	planBody string
	calls    []string
}

func (f *fakeAPI) API(_ context.Context, method, path string, body []byte) (int, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, method+" "+path)

	if strings.HasPrefix(path, "/v1/rigger/stack/plan") {
		return http.StatusOK, f.planBody, nil
	}

	return http.StatusInternalServerError, "not in the fake", nil
}

func testStack() *Stack {
	return &Stack{
		Name: "tree-ns-apps", Namespace: "apps", Region: "eu-1",
		Files:     map[string][]byte{"stack.hcl": []byte(`rigger {}`)},
		Values:    map[string]any{"tree_secret_1": "s3cret"},
		Sensitive: []string{"s3cret"},
	}
}

const deletePlan = `{"Plan":{"stack":"tree-ns-apps","region":"eu-1","releases":[{"release":{"namespace":"apps","name":"tree-apps"},"action":"upgrade","module":{"name":"tree-apps"},"changes":[{"kind":"job","name":"batch","action":"delete"},{"kind":"job","name":"api","action":"update","diff":["Priority: \"50\" -> \"60\""],"error":"Permission denied"}]}]}}`

// Plan plans each stack and marks the one a guard would block; Apply stops
// a blocked stack before its apply, and the error shows no secret value.
func TestRunner(t *testing.T) {
	t.Parallel()

	f := &fakeAPI{planBody: deletePlan}
	r := &Runner{Client: f, Tree: "acme"}

	plans, err := r.Plan(t.Context(), []*Stack{testStack()})
	if err != nil {
		t.Fatal(err)
	}

	if len(plans) != 1 || !errors.Is(plans[0].Blocked, ErrBlocked) || len(plans[0].Plan.Releases[0].Changes) != 2 {
		t.Fatalf("plans %+v", plans)
	}

	if c := plans[0].Plan.Releases[0].Changes[1]; c.Error != "Permission denied" || len(c.Diff) != 1 {
		t.Fatalf("the change keeps its error and diff: %+v", c)
	}

	_, err = r.Apply(t.Context(), []*Stack{testStack()})
	if !errors.Is(err, ErrBlocked) || !strings.Contains(err.Error(), "tree acme: stack tree-ns-apps") {
		t.Fatalf("Apply error %v", err)
	}

	for _, c := range f.calls {
		if strings.Contains(c, "/apply") {
			t.Fatalf("a blocked stack was applied: %v", f.calls)
		}
	}
}

// The resolved secret values of a stack never appear in an error.
func TestRedact(t *testing.T) {
	t.Parallel()

	s := testStack()

	err := redact(errors.New("the servers refused s3cret"), s)
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("redact = %v", err)
	}

	blocked := redact(errors.Join(ErrBlocked, errors.New("s3cret")), s)
	if !errors.Is(blocked, ErrBlocked) || strings.Contains(blocked.Error(), "s3cret") {
		t.Fatalf("redact = %v", blocked)
	}
}

func TestRenderPlan(t *testing.T) {
	t.Parallel()

	tr := &Tree{Manifest: &Manifest{Tree: TreeBlock{Name: "acme-prod", Environment: "prod"}}}

	blocked := planWith("upgrade",
		gitops.ResourceChange{Kind: "job", Name: "api", Action: "update", Diff: []string{`Priority: "50" -> "60"`}},
		gitops.ResourceChange{Kind: "job", Name: "batch", Action: "delete"})
	blocked.Secrets = []gitops.SecretPlan{{Path: "api/db", Action: "keep"}}

	var buf bytes.Buffer

	changes := RenderPlan(&buf, tr, "eu-1", []*Planned{{Stack: testStack(), Plan: blocked, Blocked: Guards{}.Check(blocked)}})

	out := buf.String()
	for _, want := range []string{
		"Tree acme-prod (environment prod), region eu-1",
		"Stack tree-ns-apps (namespace apps)",
		"~ release tree-apps  upgrade",
		"~ job              api",
		`Priority: "50" -> "60"`,
		"- job              batch",
		"secret api/db  keep",
		"BLOCKED: blocked: the delete of the job batch",
		"1 blocked; nothing is applied",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the plan has no %q:\n%s", want, out)
		}
	}

	if !changes {
		t.Error("RenderPlan reports no changes")
	}

	buf.Reset()

	if RenderPlan(&buf, tr, "eu-1", []*Planned{{Stack: testStack(), Plan: planWith("unchanged")}}) || !strings.Contains(buf.String(), "nothing to do") {
		t.Fatalf("an unchanged plan: %s", buf.String())
	}
}
