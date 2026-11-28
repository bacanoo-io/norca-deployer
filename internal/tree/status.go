// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/gitops"
)

// StackState is the state of a stack of a tree on the servers.
type StackState struct {
	Stack     string              `json:"stack"`
	Namespace string              `json:"namespace"`
	Status    *gitops.StackStatus `json:"status"`
	// LastOperation is the newest operation of the stack; nil when it has
	// none.
	LastOperation *OperationSummary `json:"last_operation,omitempty"`
}

// OperationSummary is an operation of a stack, as far as status shows it.
type OperationSummary struct {
	Name       string `json:"name"`
	Action     string `json:"action"`
	Phase      string `json:"phase"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	Error      string `json:"error,omitempty"`
}

// Orphan is a release a generated stack owns whose stack the tree no longer
// has: its namespace directory was removed. An apply with --prune
// uninstalls it.
type Orphan struct {
	Namespace string `json:"namespace"`
	Release   string `json:"release"`
	Owner     string `json:"owner"`
}

// Status reads the state of every stack: the releases it owns, their
// revision and drift, and its last operation.
func (r *Runner) Status(ctx context.Context, stacks []*Stack) ([]*StackState, error) {
	out := make([]*StackState, 0, len(stacks))

	for _, s := range stacks {
		runner, err := r.stacks(s.Name, true)
		if err != nil {
			return out, err
		}

		st, err := runner.Status(ctx, r.run(ctx, runner.Client, s, true))
		if err != nil {
			return out, redact(err, s)
		}

		op, err := r.lastOperation(ctx, runner, s)
		if err != nil {
			return out, err
		}

		out = append(out, &StackState{Stack: s.Name, Namespace: s.Namespace, Status: st, LastOperation: op})
	}

	return out, nil
}

// operationObject is an object rigger.norca.io/Operation as the list of
// objects answers it.
type operationObject struct {
	Name string
	Spec struct {
		Stack  string `json:"stack"`
		Action string `json:"action"`
	}
	Status struct {
		Phase      string `json:"phase"`
		StartedAt  string `json:"startedAt"`
		FinishedAt string `json:"finishedAt"`
		Error      string `json:"error"`
	}
}

// lastOperation is the newest operation of a stack in its namespace.
func (r *Runner) lastOperation(ctx context.Context, client *gitops.Runner, s *Stack) (*OperationSummary, error) {
	q := url.Values{"namespace": {s.Namespace}}
	if s.Region != "" {
		q.Set("region", s.Region)
	}

	var objects []operationObject
	if err := getJSON(ctx, client.Client, "/v1/objects/rigger.norca.io/Operation?"+q.Encode(), &objects); err != nil {
		return nil, fmt.Errorf("stack %s: the operations: %w", s.Name, err)
	}

	var last *operationObject

	for i := range objects {
		o := &objects[i]
		if o.Spec.Stack == s.Name && (last == nil || o.Status.StartedAt > last.Status.StartedAt) {
			last = o
		}
	}

	if last == nil {
		return nil, nil
	}

	return &OperationSummary{
		Name: last.Name, Action: last.Spec.Action, Phase: last.Status.Phase,
		StartedAt: last.Status.StartedAt, FinishedAt: last.Status.FinishedAt, Error: last.Status.Error,
	}, nil
}

// releaseObject is a release as the list of releases answers it.
type releaseObject struct {
	Namespace string
	Name      string
	Spec      struct {
		Owner *struct {
			Kind      string `json:"kind"`
			Namespace string `json:"namespace"`
			Name      string `json:"name"`
		} `json:"owner"`
	}
}

// Orphans lists the releases owned by a generated stack (tree-global,
// tree-ns-*) that is not among stacks.
func (r *Runner) Orphans(ctx context.Context, stacks []*Stack, region string) ([]Orphan, error) {
	q := url.Values{"namespace": {"*"}}
	if region != "" {
		q.Set("region", region)
	}

	var releases []releaseObject
	if err := getJSON(ctx, r.Client, "/v1/releases?"+q.Encode(), &releases); err != nil {
		return nil, fmt.Errorf("the releases: %w", err)
	}

	known := map[string]bool{}
	for _, s := range stacks {
		known[s.Name] = true
	}

	var out []Orphan

	for _, rel := range releases {
		o := rel.Spec.Owner
		if o == nil || o.Kind != "stack" || known[o.Name] {
			continue
		}

		if o.Name == globalStack || strings.HasPrefix(o.Name, namespacePrefix) {
			out = append(out, Orphan{Namespace: rel.Namespace, Release: rel.Name, Owner: o.Namespace + "/" + o.Name})
		}
	}

	slices.SortFunc(out, func(a, b Orphan) int {
		return strings.Compare(a.Namespace+"/"+a.Release, b.Namespace+"/"+b.Release)
	})

	return out, nil
}

// OrphanStacks are the generated stacks of orphan releases, sent with no
// module: their plan removes the releases, which an apply does only with
// --prune (design §4.1, a namespace directory removed).
func OrphanStacks(orphans []Orphan, region string) []*Stack {
	var out []*Stack

	seen := map[string]bool{}

	for _, o := range orphans {
		if seen[o.Owner] {
			continue
		}

		seen[o.Owner] = true
		namespace, name, _ := strings.Cut(o.Owner, "/")
		out = append(out, &Stack{
			Name: name, Namespace: namespace, Region: region,
			Files:  map[string][]byte{"stack.hcl": emptyStackHCL(name, namespace, region)},
			Values: map[string]any{},
		})
	}

	return out
}

// getJSON reads the answer of a GET into out.
func getJSON(ctx context.Context, c gitops.APIClient, path string, out any) error {
	status, body, err := c.API(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}

	if status != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", status, strings.TrimSpace(body))
	}

	return json.Unmarshal([]byte(body), out)
}

// RenderStatus writes the states and the orphan releases as text.
func RenderStatus(w io.Writer, t *Tree, region string, states []*StackState, orphans []Orphan) {
	fmt.Fprintf(w, "Tree %s, region %s\n", t.Manifest.Tree.Name, region)

	for _, st := range states {
		fmt.Fprintf(w, "\nstack %s (namespace %s)\n", st.Stack, st.Namespace)

		if op := st.LastOperation; op != nil {
			line := fmt.Sprintf("  last operation: %s %s %s", op.Name, op.Action, op.Phase)
			if op.FinishedAt != "" {
				line += ", finished " + op.FinishedAt
			}

			if op.Error != "" {
				line += ": " + op.Error
			}

			fmt.Fprintln(w, line)
		}

		if st.Status == nil || len(st.Status.Releases) == 0 {
			fmt.Fprintln(w, "  no releases installed")

			continue
		}

		for _, rs := range st.Status.Releases {
			rev := rs.Revision
			drift := "in sync"

			if len(rs.Drift) > 0 {
				drift = fmt.Sprintf("%d drifted", len(rs.Drift))
			}

			fmt.Fprintf(w, "  release %s/%s: revision %d, %s, %s\n", rev.Release.Namespace, rev.Release.Name, rev.Number, rev.Status, drift)

			for _, d := range rs.Drift {
				fmt.Fprintf(w, "    ~ %s %s: %s\n", d.Kind, d.Name, d.Detail)
			}
		}
	}

	if len(orphans) > 0 {
		fmt.Fprintln(w, "\nreleases of stacks the tree no longer has (an apply with --prune uninstalls them):")

		for _, o := range orphans {
			fmt.Fprintf(w, "  %s/%s (stack %s)\n", o.Namespace, o.Release, o.Owner)
		}
	}
}
