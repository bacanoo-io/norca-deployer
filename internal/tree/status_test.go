// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"testing"
)

// statusAPI answers the status of a stack, the operations and the releases
// by the path of the request.
type statusAPI map[string]string

func (s statusAPI) API(_ context.Context, _, path string, _ []byte) (int, string, error) {
	p, _, _ := strings.Cut(path, "?")
	if body, ok := s[p]; ok {
		return http.StatusOK, body, nil
	}

	return http.StatusNotFound, "not in the fake", nil
}

func TestRunner_Status(t *testing.T) {
	t.Parallel()

	api := statusAPI{
		"/v1/rigger/stack/status": `{"Stack":"tree-ns-apps","Releases":[{"revision":{"release":{"namespace":"apps","name":"tree-apps"},` +
			`"number":3,"status":"deployed","owner":"apps/tree-ns-apps"},"drift":[{"kind":"job","name":"api","detail":"count 2, the release has 1"}]}]}`,
		"/v1/objects/rigger.norca.io/Operation": `[` +
			`{"Name":"op-1","Spec":{"stack":"tree-ns-apps","action":"apply"},"Status":{"phase":"Succeeded","startedAt":"2026-11-28T10:00:00Z"}},` +
			`{"Name":"op-2","Spec":{"stack":"tree-ns-apps","action":"apply"},"Status":{"phase":"Failed","startedAt":"2026-11-28T11:00:00Z","error":"job api failed"}},` +
			`{"Name":"op-3","Spec":{"stack":"other","action":"apply"},"Status":{"phase":"Running","startedAt":"2026-11-28T12:00:00Z"}}]`,
		"/v1/releases": `[` +
			`{"Namespace":"apps","Name":"tree-apps","Spec":{"owner":{"kind":"stack","namespace":"apps","name":"tree-ns-apps"}}},` +
			`{"Namespace":"old","Name":"tree-old","Spec":{"owner":{"kind":"stack","namespace":"old","name":"tree-ns-old"}}},` +
			`{"Namespace":"web","Name":"site","Spec":{"owner":{"kind":"stack","namespace":"web","name":"web-stack"}}},` +
			`{"Namespace":"web","Name":"by-hand","Spec":{}}]`,
	}

	r := &Runner{Client: api, Tree: "acme"}
	stacks := []*Stack{testStack()}

	states, err := r.Status(context.Background(), stacks)
	if err != nil {
		t.Fatal(err)
	}

	st := states[0]
	if st.Status == nil || len(st.Status.Releases) != 1 || st.Status.Releases[0].Revision.Number != 3 || len(st.Status.Releases[0].Drift) != 1 {
		t.Fatalf("status %+v", st.Status)
	}

	if op := st.LastOperation; op == nil || op.Name != "op-2" || op.Phase != "Failed" || op.Error != "job api failed" {
		t.Fatalf("last operation %+v", st.LastOperation)
	}

	// Only a release of a generated stack the tree no longer has is an
	// orphan: not one of another stack, not one without an owner.
	orphans, err := r.Orphans(context.Background(), stacks, "eu-1")
	if err != nil {
		t.Fatal(err)
	}

	if len(orphans) != 1 || orphans[0].Release != "tree-old" || orphans[0].Owner != "old/tree-ns-old" {
		t.Fatalf("orphans %+v", orphans)
	}

	// Its stack is planned empty in its namespace, so the plan removes it.
	empty := OrphanStacks(orphans, "eu-1")
	if len(empty) != 1 || empty[0].Name != "tree-ns-old" || empty[0].Namespace != "old" || len(empty[0].Files) != 1 {
		t.Fatalf("orphan stacks %+v", empty)
	}

	mustParse(t, "stack.hcl", empty[0].Files["stack.hcl"])

	var out bytes.Buffer

	RenderStatus(&out, &Tree{Manifest: &Manifest{Tree: TreeBlock{Name: "acme"}}}, "eu-1", states, orphans)

	for _, want := range []string{
		"release apps/tree-apps: revision 3, deployed, 1 drifted",
		"~ job api: count 2, the release has 1",
		"last operation: op-2 apply Failed",
		"old/tree-old (stack old/tree-ns-old)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status has no %q:\n%s", want, out.String())
		}
	}
}

// A release a plan removes has its resources added as deletes: the job of
// an orphan release needs --prune-jobs like any other job delete.
func TestRunner_PlanOfARemovedRelease(t *testing.T) {
	t.Parallel()

	api := statusAPI{
		"/v1/rigger/stack/plan": `{"Plan":{"stack":"tree-ns-old","region":"eu-1","releases":[` +
			`{"release":{"namespace":"old","name":"tree-old"},"action":"remove","module":{"name":"tree-old"}}]}}`,
		"/v1/release/tree-old": `{"Status":{"resources":[{"type":"job","name":"hello"},` +
			`{"type":"job","name":"migrate","hook":"pre-install"},{"type":"variable","name":"cfg"}]}}`,
	}

	stack := OrphanStacks([]Orphan{{Namespace: "old", Release: "tree-old", Owner: "old/tree-ns-old"}}, "eu-1")

	for _, tt := range []struct {
		name   string
		guards Guards
		want   string
	}{
		{name: "prune alone", guards: Guards{Prune: true}, want: "the delete of the job hello of the release old/tree-old purges the job"},
		{name: "prune and prune-jobs", guards: Guards{Prune: true, PruneJobs: true}, want: "the delete of the variable cfg of the release old/tree-old needs --prune-data"},
		{name: "all three", guards: Guards{Prune: true, PruneJobs: true, PruneData: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			r := &Runner{Client: api, Tree: "acme", Guards: tt.guards}

			plans, err := r.Plan(context.Background(), stack)
			if err != nil {
				t.Fatal(err)
			}

			p := plans[0]
			if got := len(p.Plan.Releases[0].Changes); got != 2 {
				t.Fatalf("%d deletes, want 2 (the hook left out): %+v", got, p.Plan.Releases[0].Changes)
			}

			switch {
			case tt.want == "" && p.Blocked != nil:
				t.Fatalf("blocked: %v", p.Blocked)
			case tt.want != "" && (p.Blocked == nil || !strings.Contains(p.Blocked.Error(), tt.want)):
				t.Fatalf("blocked %v, want %q", p.Blocked, tt.want)
			}

			if n := Changes(p); n != 3 {
				t.Errorf("Changes = %d, want 2 resources and the release", n)
			}
		})
	}
}
