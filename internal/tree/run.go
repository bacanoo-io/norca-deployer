// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/gitops"
	"github.com/bacanoo-io/norca-deployer/internal/values"
)

// The actions of a plan of the servers that the guards look at.
const (
	actionDelete = "delete"
	actionRemove = "remove"
)

// The resource kinds of the release API whose deletion needs more than
// --prune (design §7.4).
const (
	resourceJob      = "job"
	resourceVariable = "variable"
)

// protected are the objects a plan of a tree never deletes, whatever the
// flags (design §7.4): kind and name as the release API names them.
var protected = map[string]bool{
	"namespace/default":    true,
	"acl-policy/anonymous": true,
}

// ErrBlocked marks a stack that a guard stopped: blocked, not failed
// (design §10, exit code 3).
var ErrBlocked = errors.New("blocked")

// Guards are the rules a plan must pass before its stack is applied
// (design §7.4, §7.6).
type Guards struct {
	// Prune allows deletes: a resource a release no longer has, and a
	// release the stack no longer has.
	Prune bool
	// PruneJobs allows the delete of a job, which purges it.
	PruneJobs bool
	// PruneData allows the delete of a Variable.
	PruneData bool
	// Kinds are the kinds a stack may change (limits.kinds and --kind);
	// nil allows all.
	Kinds map[string]bool
}

// Check returns why a plan may not be applied, wrapped in ErrBlocked; nil
// when it may.
func (g Guards) Check(p *gitops.StackPlan) error {
	var reasons []string

	for _, rp := range p.Releases {
		release := rp.Release.Namespace + "/" + rp.Release.Name

		if rp.Action == actionRemove && !g.Prune {
			// The servers keep such a release without prune: no block.
			continue
		}

		for _, c := range rp.Changes {
			if kind := resourceKind(c.Kind); g.Kinds != nil && !g.Kinds[kind] {
				reasons = append(reasons, fmt.Sprintf("the release %s changes the %s %s, a kind this run may not change", release, kind, c.Name))
			}

			if c.Action != actionDelete {
				continue
			}

			what := fmt.Sprintf("%s %s of the release %s", c.Kind, c.Name, release)

			switch {
			case protected[c.Kind+"/"+c.Name]:
				reasons = append(reasons, "the "+what+" is never deleted by a tree")
			case !g.Prune:
				reasons = append(reasons, "the delete of the "+what+" needs --prune")
			case c.Kind == resourceJob && !g.PruneJobs:
				reasons = append(reasons, "the delete of the "+what+" purges the job: it needs --prune-jobs")
			case c.Kind == resourceVariable && !g.PruneData:
				reasons = append(reasons, "the delete of the "+what+" needs --prune-data")
			}
		}
	}

	for _, sp := range p.Secrets {
		if sp.Action != secretKeep && g.Kinds != nil && !g.Kinds[KindSecret] {
			reasons = append(reasons, fmt.Sprintf("the stack writes the secret %s, a kind this run may not change", sp.Path))
		}
	}

	if len(reasons) == 0 {
		return nil
	}

	return fmt.Errorf("%w: %s", ErrBlocked, strings.Join(reasons, "; "))
}

// secretKeep is the action of a stack secret that is left as it is.
const secretKeep = "keep"

// Changes counts what a plan changes: each resource of a release, each
// release removed, each secret written.
func Changes(p *gitops.StackPlan) int {
	n := 0

	for _, rp := range p.Releases {
		n += len(rp.Changes)
		if rp.Action == actionRemove {
			n++
		}
	}

	for _, sp := range p.Secrets {
		if sp.Action != secretKeep {
			n++
		}
	}

	return n
}

// HasChanges reports whether an apply of a plan would do anything: a
// release to install, upgrade or remove, a secret to write, a value to
// generate.
func HasChanges(p *gitops.StackPlan) bool {
	if len(p.Generate) > 0 {
		return true
	}

	for _, sp := range p.Secrets {
		if sp.Action != secretKeep {
			return true
		}
	}

	for _, rp := range p.Releases {
		if rp.Action != "unchanged" && rp.Action != "disabled" {
			return true
		}
	}

	return false
}

// CheckChanges blocks a run whose plans change more than max resources,
// unless allowed (design §7.6, the blast-radius guard).
func CheckChanges(plans []*Planned, limit int, allowed bool) error {
	total := 0
	for _, p := range plans {
		total += Changes(p.Plan)
	}

	if allowed || limit <= 0 || total <= limit {
		return nil
	}

	return fmt.Errorf("%w: the run changes %d resources, more than limits.max_changes (%d); give --yes-really to apply it", ErrBlocked, total, limit)
}

// Planned is the plan of one stack of a tree.
type Planned struct {
	Stack    *Stack
	Plan     *gitops.StackPlan
	Warnings []string
	// Blocked is why a guard would stop its apply; nil when it would not.
	Blocked error
}

// Runner plans and applies the stacks of a tree through the stacks API of
// the servers.
type Runner struct {
	// Client sends the requests; Report gets the events of the runs.
	Client gitops.APIClient
	// ClientFor, when set, gives the client of a stack: its scoped token
	// (design §5.3), the read token for a plan.
	ClientFor func(stack string, plan bool) (gitops.APIClient, error)
	Report    func(gitops.Event)
	// Tree names the tree in the errors.
	Tree   string
	Guards Guards
}

// stacks is the stack client of a stack.
func (r *Runner) stacks(stack string, plan bool) (*gitops.Runner, error) {
	client := r.Client

	if r.ClientFor != nil {
		var err error
		if client, err = r.ClientFor(stack, plan); err != nil {
			return nil, fmt.Errorf("stack %s: %w", stack, err)
		}
	}

	return &gitops.Runner{Client: client, Report: r.Report}, nil
}

// SyncRegistries has the servers sync the registries in order before the
// plans (gitops-rigger-design.md 5.10); a sync that fails stops the run.
func (r *Runner) SyncRegistries(ctx context.Context, names []string) error {
	for _, name := range names {
		status, body, err := r.Client.API(ctx, "PUT", "/v1/rigger/registry/"+url.PathEscape(name)+"/sync", nil)
		if err != nil {
			return fmt.Errorf("syncing the registry %s: %w", name, err)
		}

		if status/100 != 2 {
			return fmt.Errorf("syncing the registry %s: HTTP %d: %s", name, status, strings.TrimSpace(body))
		}
	}

	return nil
}

// run is the request of a stack.
func (r *Runner) run(s *Stack, planOnly bool) *gitops.StackRun {
	return &gitops.StackRun{
		Label:     fmt.Sprintf("tree %s: stack %s", r.Tree, s.Name),
		Namespace: s.Namespace,
		Region:    s.Region,
		Files:     s.Files,
		Values:    s.Values,
		Prune:     r.Guards.Prune,
		PlanOnly:  planOnly,
		Check:     r.Guards.Check,
	}
}

// Plan has the servers plan every stack, in order, applying nothing, and
// says for each whether a guard would block its apply. An error of a plan
// stops there.
func (r *Runner) Plan(ctx context.Context, stacks []*Stack) ([]*Planned, error) {
	out := make([]*Planned, 0, len(stacks))

	for _, s := range stacks {
		runner, err := r.stacks(s.Name, true)
		if err != nil {
			return out, err
		}

		plan, warnings, err := runner.Plan(ctx, r.run(s, true))
		if err != nil {
			return out, redact(err, s)
		}

		out = append(out, &Planned{Stack: s, Plan: plan, Warnings: warnings, Blocked: r.Guards.Check(plan)})
	}

	return out, nil
}

// Apply applies the stacks in order, each after the guards passed on the
// plan its apply is made of; the first stack that fails or is blocked stops
// the run (design §7.1).
func (r *Runner) Apply(ctx context.Context, stacks []*Stack) ([]*gitops.Result, error) {
	out := make([]*gitops.Result, 0, len(stacks))

	for _, s := range stacks {
		runner, err := r.stacks(s.Name, false)
		if err != nil {
			return out, err
		}

		res, err := runner.Sync(ctx, r.run(s, false))
		if err != nil {
			return out, redact(err, s)
		}

		out = append(out, res)
	}

	return out, nil
}

// redact removes the resolved secret values of a stack from an error: the
// servers may quote what they were sent.
func redact(err error, s *Stack) error {
	if len(s.Sensitive) == 0 {
		return err
	}

	text := values.Redact(err.Error(), s.Sensitive)
	if text == err.Error() {
		return err
	}

	if errors.Is(err, ErrBlocked) {
		return fmt.Errorf("%w: %s", ErrBlocked, text)
	}

	return errors.New(text)
}
