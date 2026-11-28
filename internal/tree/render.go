// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"fmt"
	"io"
	"strings"
)

// The signs of the actions of a plan (design §7.2).
var actionSigns = map[string]string{
	"create": "+", "update": "~", "delete": "-", "hook": "*",
	"install": "+", "upgrade": "~", "remove": "-", "unchanged": "=", "disabled": "=",
}

// sign is the sign of an action; " " for one without.
func sign(action string) string {
	if s, ok := actionSigns[action]; ok {
		return s
	}

	return " "
}

// RenderPlan writes the plans of the stacks of a tree for a person
// (design §7.2): per stack its releases, the changes of each with what the
// servers would refuse and the lines of a job's diff, the secrets and the
// generated values, and what a guard blocks. It reports whether there is
// anything to apply.
func RenderPlan(w io.Writer, t *Tree, region string, plans []*Planned) bool {
	head := "Tree " + t.Manifest.Tree.Name
	if env := t.Manifest.Tree.Environment; env != "" {
		head += " (environment " + env + ")"
	}

	fmt.Fprintf(w, "%s, region %s\n", head, region)

	for _, warning := range t.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warning)
	}

	changes, blocked := 0, 0

	for _, p := range plans {
		fmt.Fprintf(w, "Stack %s (namespace %s)\n", p.Stack.Name, p.Stack.Namespace)

		for _, rp := range p.Plan.Releases {
			line := fmt.Sprintf("  %s release %s  %s", sign(rp.Action), rp.Release.Name, rp.Action)
			if rp.Action == actionRemove {
				line += " (with --prune)"
			}

			fmt.Fprintln(w, line)

			if rp.Action != "unchanged" && rp.Action != "disabled" {
				changes++
			}

			for _, c := range rp.Changes {
				fmt.Fprintf(w, "    %s %-16s %s\n", sign(c.Action), c.Kind, c.Name)

				if c.Error != "" {
					fmt.Fprintf(w, "        refused: %s\n", c.Error)
				}

				for _, d := range c.Diff {
					fmt.Fprintf(w, "        %s\n", d)
				}
			}
		}

		for _, sp := range p.Plan.Secrets {
			fmt.Fprintf(w, "  secret %s  %s\n", sp.Path, sp.Action)

			if sp.Action != "keep" {
				changes++
			}
		}

		if len(p.Plan.Generate) > 0 {
			fmt.Fprintf(w, "  generates %s\n", strings.Join(p.Plan.Generate, ", "))
			changes++
		}

		for _, warning := range p.Warnings {
			fmt.Fprintf(w, "  warning: %s\n", warning)
		}

		if p.Blocked != nil {
			fmt.Fprintf(w, "  BLOCKED: %v\n", p.Blocked)
			blocked++
		}
	}

	switch {
	case blocked > 0:
		fmt.Fprintf(w, "Summary: %d stacks, %d blocked; nothing is applied until every stack passes\n", len(plans), blocked)
	case changes == 0:
		fmt.Fprintf(w, "Summary: %d stacks, nothing to do\n", len(plans))
	default:
		fmt.Fprintf(w, "Summary: %d stacks, %d changes\n", len(plans), changes)
	}

	return changes > 0
}
