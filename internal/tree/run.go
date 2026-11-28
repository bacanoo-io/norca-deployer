// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"context"
	"errors"
	"fmt"
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

	if len(reasons) == 0 {
		return nil
	}

	return fmt.Errorf("%w: %s", ErrBlocked, strings.Join(reasons, "; "))
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
	Report func(gitops.Event)
	// Tree names the tree in the errors.
	Tree   string
	Guards Guards
}

func (r *Runner) stacks() *gitops.Runner {
	return &gitops.Runner{Client: r.Client, Report: r.Report}
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
		plan, warnings, err := r.stacks().Plan(ctx, r.run(s, true))
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
		res, err := r.stacks().Sync(ctx, r.run(s, false))
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
