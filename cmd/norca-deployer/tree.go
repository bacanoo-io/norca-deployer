// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca-deployer/internal/crypto"
	"github.com/bacanoo-io/norca-deployer/internal/gitops"
	"github.com/bacanoo-io/norca-deployer/internal/tree"
	"github.com/bacanoo-io/norca-deployer/internal/values"
)

// experimentalEnv names the experimental command groups that are turned on,
// separated by commas ("tree").
const experimentalEnv = "NORCA_DEPLOYER_EXPERIMENTAL"

// experimental reports whether the experimental feature name is turned on.
func experimental(name string) bool {
	return slices.ContainsFunc(strings.Split(os.Getenv(experimentalEnv), ","), func(f string) bool {
		return strings.TrimSpace(f) == name
	})
}

// newTreeCmd is the command group tree: the objects of a running platform
// from a directory tree, applied as Rigger stacks
// (design/platform-management-design.md). Until its first release it is
// hidden, and refuses to run unless NORCA_DEPLOYER_EXPERIMENTAL names it.
func newTreeCmd() *cobra.Command {
	on := experimental("tree")

	cmd := &cobra.Command{
		Use:    "tree",
		Short:  "Manage the objects of a running platform from a directory tree (experimental)",
		Hidden: !on,
		Long: `Manage the objects of a running Norca platform (namespaces, ACL policies,
jobs, Variables, secrets, objects, ...) from a directory tree, usually a Git
repository. The tree is applied as Rigger stacks. Experimental: turned on
with ` + experimentalEnv + `=tree.`,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			if !on {
				return fmt.Errorf("the tree commands are experimental: set %s=tree to use them", experimentalEnv)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}

	cmd.AddCommand(newTreeBuildCmd(), newTreePlanCmd(), newTreeApplyCmd())
	return cmd
}

// treeFlags are the flags of the tree commands. They stand for the startup
// configuration (design §5.2) until it is read from a file.
type treeFlags struct {
	dir       string
	region    string
	keyFile   string
	valuesDir string
	valuesEnv []string
}

func (f *treeFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVarP(&f.dir, "dir", "d", "cluster-config", "The root of the tree")
	cmd.Flags().StringVar(&f.region, "region", "", "The region the stacks go to (required)")
	cmd.Flags().StringVar(&f.keyFile, "key-file", "", "The key of the tree's encrypted values; never the master key of the node plan")
	cmd.Flags().StringVar(&f.valuesDir, "values-dir", "", "The directory @path values are read from")
	cmd.Flags().StringSliceVar(&f.valuesEnv, "values-env", nil, "The environment variables !env: values may read (names or patterns)")
	_ = cmd.MarkFlagRequired("region")
}

// build reads the tree and builds its stacks. The returned function
// releases the values directory.
func (f *treeFlags) build() (*tree.Tree, []*tree.Stack, func(), error) {
	t, err := tree.Load(os.DirFS(f.dir))
	if err != nil {
		return nil, nil, nil, err
	}

	r := &values.Resolver{Env: f.valuesEnv}
	done := func() {}

	if f.keyFile != "" {
		key, err := crypto.ReadKeyFile(f.keyFile)
		if err != nil {
			return nil, nil, nil, err
		}

		vault, err := crypto.NewVault(key)
		if err != nil {
			return nil, nil, nil, err
		}

		r.Decrypt = vault.Decrypt
	}

	if f.valuesDir != "" {
		root, err := os.OpenRoot(f.valuesDir)
		if err != nil {
			return nil, nil, nil, err
		}

		r.Files = root
		done = func() { _ = root.Close() }
	}

	stacks, err := tree.Build(t, f.region, r)
	if err != nil {
		done()
		return nil, nil, nil, err
	}

	return t, stacks, done, nil
}

// newTreeBuildCmd is tree build: the stacks of a tree written to a
// directory, to read them or to run rigger stack validate on them.
func newTreeBuildCmd() *cobra.Command {
	var (
		f   treeFlags
		out string
	)

	cmd := &cobra.Command{
		Use:   "build",
		Short: "Write the stacks a tree is applied as to a directory",
		Long: `Write the stacks a tree is applied as to a directory, one subdirectory per
stack. The resolved values of secret items are not written: they go only with
the request to the servers.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, stacks, done, err := f.build()
			if err != nil {
				return err
			}
			defer done()

			return writeStacks(cmd.OutOrStdout(), out, stacks)
		},
	}

	f.register(cmd)
	cmd.Flags().StringVar(&out, "out", "", "The directory the stacks are written to (required)")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

// writeStacks writes each stack below out/<stack>/.
func writeStacks(w io.Writer, out string, stacks []*tree.Stack) error {
	for _, s := range stacks {
		for name, data := range s.Files {
			p := filepath.Join(out, s.Name, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(p, data, 0o600); err != nil {
				return err
			}
		}
		fmt.Fprintf(w, "%s: %d files\n", filepath.Join(out, s.Name), len(s.Files))
	}
	return nil
}

// newTreePlanCmd is tree plan: the servers' plan of every stack of a tree.
func newTreePlanCmd() *cobra.Command {
	var (
		f       treeFlags
		cluster clusterFlags
		guards  tree.Guards
	)

	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show what an apply of the tree would change",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t, plans, err := treePlan(cmd, &f, &cluster, guards)
			if err != nil {
				return err
			}

			tree.RenderPlan(cmd.OutOrStdout(), t, f.region, plans)
			return nil
		},
	}

	f.register(cmd)
	cluster.register(cmd)
	registerGuards(cmd, &guards)
	return cmd
}

// newTreeApplyCmd is tree apply: plan every stack, refuse when a guard
// blocks one, then apply them in order.
func newTreeApplyCmd() *cobra.Command {
	var (
		f       treeFlags
		cluster clusterFlags
		guards  tree.Guards
		yes     bool
	)

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Plan the stacks of the tree and apply them in order",
		Long: `Plan every stack of the tree, show the plans, and apply the stacks in order.
Nothing is applied when a guard blocks a stack (a delete without its --prune
flag). The first stack that fails stops the run.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t, plans, err := treePlan(cmd, &f, &cluster, guards)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			changes := tree.RenderPlan(out, t, f.region, plans)

			for _, p := range plans {
				if p.Blocked != nil {
					return fmt.Errorf("stack %s: %w", p.Stack.Name, p.Blocked)
				}
			}
			if !changes {
				return nil
			}
			if !yes && !confirm(cmd.InOrStdin(), out) {
				return errors.New("not applied")
			}

			stacks := make([]*tree.Stack, 0, len(plans))
			for _, p := range plans {
				stacks = append(stacks, p.Stack)
			}

			client, err := cluster.client()
			if err != nil {
				return err
			}

			runner := &tree.Runner{Client: client, Report: reportTo(cmd.ErrOrStderr()), Tree: t.Manifest.Tree.Name, Guards: guards}
			_, err = runner.Apply(cmd.Context(), stacks)
			return err
		},
	}

	f.register(cmd)
	cluster.register(cmd)
	registerGuards(cmd, &guards)
	cmd.Flags().BoolVar(&yes, "yes", false, "Apply without asking")
	return cmd
}

// registerGuards registers the flags that allow deletes.
func registerGuards(cmd *cobra.Command, g *tree.Guards) {
	cmd.Flags().BoolVar(&g.Prune, "prune", false, "Allow deletes: a resource a release no longer has, a release the stack no longer has")
	cmd.Flags().BoolVar(&g.PruneJobs, "prune-jobs", false, "Allow the delete of a job (it is purged), with --prune")
	cmd.Flags().BoolVar(&g.PruneData, "prune-data", false, "Allow the delete of a Variable, with --prune")
}

// treePlan reads and builds the tree and has the servers plan its stacks.
func treePlan(cmd *cobra.Command, f *treeFlags, cluster *clusterFlags, guards tree.Guards) (*tree.Tree, []*tree.Planned, error) {
	t, stacks, done, err := f.build()
	if err != nil {
		return nil, nil, err
	}
	defer done()

	client, err := cluster.client()
	if err != nil {
		return nil, nil, err
	}

	runner := &tree.Runner{Client: client, Report: reportTo(cmd.ErrOrStderr()), Tree: t.Manifest.Tree.Name, Guards: guards}
	plans, err := runner.Plan(cmd.Context(), stacks)
	return t, plans, err
}

// reportTo writes the events of the runs of the stacks of a tree, the plans
// left out (RenderPlan shows them).
func reportTo(w io.Writer) func(gitops.Event) {
	return func(e gitops.Event) {
		if e.Phase == gitops.PhasePlan {
			return
		}
		fmt.Fprintf(w, "[%s] %s\n", e.Phase, e.Details)
	}
}

// confirm asks on in whether to apply.
func confirm(in io.Reader, out io.Writer) bool {
	fmt.Fprint(out, "Apply these changes? [y/N] ")
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}
