// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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

// planSchema names the shape of the JSON of a plan (design §10).
const planSchema = "norca.io/tree/plan/v1"

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

	cmd.AddCommand(newTreeInitCmd(), newTreeValidateCmd(), newTreeBuildCmd(), newTreePlanCmd(), newTreeApplyCmd(), newTreeStatusCmd())
	return cmd
}

// treeFlags are the flags of the tree commands. They add to the startup
// configuration (design §5.2) and win over it.
type treeFlags struct {
	config    string
	dir       string
	region    string
	keyFile   string
	valuesDir string
	valuesEnv []string
	server    string
	tokenEnv  string
	caCert    string
	insecure  bool
	kinds     []string
	guards    tree.Guards
	format    string
	// onlyStacks and onlyNamespaces narrow a run to whole stacks.
	onlyStacks     []string
	onlyNamespaces []string
}

func (f *treeFlags) register(cmd *cobra.Command, cluster bool) {
	fl := cmd.Flags()
	fl.StringVar(&f.config, "config", "", "The startup configuration (default: "+tree.ConfigEnv+", then ./"+tree.DefaultConfigFile+" if there)")
	fl.StringVarP(&f.dir, "dir", "d", "cluster-config", "The root of the tree")
	fl.StringVar(&f.region, "region", "", "The region the stacks go to (default: the only region of the startup configuration)")
	fl.StringVar(&f.keyFile, "key-file", "", "The key of the tree's encrypted values; never the master key of the node plan")
	fl.StringVar(&f.valuesDir, "values-dir", "", "The directory @path values are read from")
	fl.StringSliceVar(&f.valuesEnv, "values-env", nil, "The environment variables !env: values may read (names or patterns)")

	if !cluster {
		return
	}

	fl.StringVar(&f.server, "server", "", "Norca HTTP API URL (default: the region's address, NORCA_ADDR, then http://127.0.0.1:4646)")
	fl.StringVar(&f.tokenEnv, "norca-token-env", "", "Environment variable with the Norca ACL token (default: the region's token, then NORCA_TOKEN)")
	fl.StringVar(&f.caCert, "norca-ca-cert", "", "CA certificate file of the Norca API (default: the region's, then NORCA_CACERT)")
	fl.BoolVar(&f.insecure, "norca-insecure-skip-tls-verify", false, "Do not verify the certificate of the Norca API")
	fl.StringSliceVar(&f.kinds, "kind", nil, "Only stacks whose plans change these kinds (kind identifiers and groups); others are blocked")
	fl.StringSliceVar(&f.onlyStacks, "stack", nil, "Only these stacks (whole stacks, in the order of the tree)")
	fl.StringSliceVar(&f.onlyNamespaces, "namespace", nil, "Only the stacks of these namespace directories (tree-ns-<ns>)")
	fl.BoolVar(&f.guards.Prune, "prune", false, "Allow deletes: a resource a release no longer has, a release the stack no longer has")
	fl.BoolVar(&f.guards.PruneJobs, "prune-jobs", false, "Allow the delete of a job (it is purged), with --prune")
	fl.BoolVar(&f.guards.PruneData, "prune-data", false, "Allow the delete of a Variable, with --prune")
	fl.StringVar(&f.format, "format", "text", "The output: text or json")
}

// treeSetup is what a tree command works with: the configuration, the
// region, the tree and its stacks.
type treeSetup struct {
	cfg      *tree.Config
	region   *tree.RegionConfig
	tree     *tree.Tree
	stacks   []*tree.Stack
	warnings []string
	done     func()
}

// offlineRegion is the region of an offline command (validate, build) when
// neither --region nor the startup configuration names one.
const offlineRegion = "global"

// setup reads the startup configuration and the tree and builds its stacks,
// narrowed by --stack and --namespace. offline commands need no region.
func (f *treeFlags) setup(offline bool) (*treeSetup, error) {
	cfg, warnings, err := tree.LoadConfig(f.config, f.dir)
	if err != nil {
		return nil, err
	}

	name := f.region
	if offline && name == "" && len(cfg.Regions) == 0 {
		name = offlineRegion
	}

	region, err := cfg.Region(name)
	if err != nil {
		return nil, err
	}

	t, err := tree.Load(os.DirFS(f.dir))
	if err != nil {
		return nil, err
	}

	r, done, err := f.resolver(cfg)
	if err != nil {
		return nil, err
	}

	stacks, err := tree.Build(t, region.Name, region.Values, r)
	if err != nil {
		done()
		return nil, err
	}

	if stacks, err = f.narrow(stacks); err != nil {
		done()
		return nil, err
	}

	return &treeSetup{cfg: cfg, region: region, tree: t, stacks: stacks, warnings: warnings, done: done}, nil
}

// narrowed reports whether --stack or --namespace narrows the run.
func (f *treeFlags) narrowed() bool {
	return len(f.onlyStacks) > 0 || len(f.onlyNamespaces) > 0
}

// narrow keeps the stacks --stack and --namespace name, in their order; a
// name that is no stack of the tree is a usage error.
func (f *treeFlags) narrow(stacks []*tree.Stack) ([]*tree.Stack, error) {
	if !f.narrowed() {
		return stacks, nil
	}

	want := map[string]bool{}
	for _, s := range f.onlyStacks {
		want[s] = true
	}
	for _, ns := range f.onlyNamespaces {
		want[tree.NamespaceStack(ns)] = true
	}

	var out []*tree.Stack
	for _, s := range stacks {
		if want[s.Name] {
			out = append(out, s)
			delete(want, s.Name)
		}
	}

	if len(want) > 0 {
		missing := slices.Sorted(maps.Keys(want))
		return nil, usageError{fmt.Errorf("no stack of the tree: %s", strings.Join(missing, ", "))}
	}

	return out, nil
}

// resolver is the resolver of the value forms: the flags, else the values
// block of the startup configuration. The returned function releases the
// values directory.
func (f *treeFlags) resolver(cfg *tree.Config) (*values.Resolver, func(), error) {
	r := &values.Resolver{Env: cfg.Values.Env}
	if len(f.valuesEnv) > 0 {
		r.Env = f.valuesEnv
	}

	keyFile := firstOf(f.keyFile, cfg.Path(cfg.Values.KeyFile))
	if keyFile != "" {
		key, err := crypto.ReadKeyFile(keyFile)
		if err != nil {
			return nil, nil, err
		}

		vault, err := crypto.NewVault(key)
		if err != nil {
			return nil, nil, err
		}

		r.Decrypt = vault.Decrypt
	}

	dir := firstOf(f.valuesDir, cfg.Path(cfg.Values.Dir))
	if dir == "" {
		return r, func() {}, nil
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, nil, err
	}

	r.Files = root
	return r, func() { _ = root.Close() }, nil
}

// runner is the runner of the stacks: a client per stack, from the scoped
// tokens of the region, and the guards of the flags within the limits.
func (f *treeFlags) runner(s *treeSetup, w io.Writer) (*tree.Runner, error) {
	if f.guards.Prune && !s.cfg.PruneAllowed() {
		return nil, errors.New("--prune: limits.prune of the startup configuration does not allow it")
	}

	flagKinds, err := tree.KindSelector(f.kinds)
	if err != nil {
		return nil, usageError{fmt.Errorf("--kind: %w", err)}
	}

	limitKinds, _ := tree.KindSelector(s.cfg.Limits.Kinds) // checked when the configuration was read

	guards := f.guards
	guards.Kinds = tree.IntersectKinds(limitKinds, flagKinds)

	clients := map[string]gitops.APIClient{}
	clientFor := func(stack string, plan bool) (gitops.APIClient, error) {
		source, name := s.region.TokenFor(stack, plan)
		if c, ok := clients[name]; ok {
			return c, nil
		}

		c, err := f.client(s, source)
		if err != nil {
			return nil, err
		}

		clients[name] = c
		return c, nil
	}

	def, err := clientFor("", false)
	if err != nil {
		return nil, err
	}

	return &tree.Runner{
		Client: def, ClientFor: clientFor, Report: reportTo(w),
		Tree: s.tree.Manifest.Tree.Name, Guards: guards,
	}, nil
}

// client is a client of the region's endpoint with a token.
func (f *treeFlags) client(s *treeSetup, source tree.TokenSource) (*gitops.HTTPClient, error) {
	server := firstOf(f.server, s.region.Address, os.Getenv("NORCA_ADDR"), "http://127.0.0.1:4646")

	token, err := f.token(s, source)
	if err != nil {
		return nil, err
	}

	caFile := firstOf(f.caCert, s.cfg.Path(s.region.CACert), os.Getenv("NORCA_CACERT"))
	caPEM := ""
	if caFile != "" {
		data, err := os.ReadFile(caFile) //nolint:gosec // G703: the CA file is named by the operator
		if err != nil {
			return nil, err
		}
		caPEM = string(data)
	}

	return gitops.NewHTTPClient(server, token, caPEM, f.insecure || s.region.Insecure)
}

// token resolves a token: --norca-token-env first, then the source of the
// configuration (environment variable, file, value), then NORCA_TOKEN. An
// encrypted value is decrypted with the deployer's master key: it is the
// operator's credential, not a value of the tree.
func (f *treeFlags) token(s *treeSetup, source tree.TokenSource) (string, error) {
	switch {
	case f.tokenEnv != "":
		return os.Getenv(f.tokenEnv), nil
	case source.Env != "":
		return os.Getenv(source.Env), nil
	case source.File != "":
		data, err := os.ReadFile(s.cfg.Path(source.File))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	case source.Value != "":
		if !strings.HasPrefix(strings.TrimSpace(source.Value), crypto.EncryptedPrefix) {
			return source.Value, nil
		}
		key, err := crypto.ResolveMasterKey(secretKey, secretKeyFile)
		if err != nil {
			return "", fmt.Errorf("an encrypted token of the startup configuration: %w", err)
		}
		vault, err := crypto.NewVault(key)
		if err != nil {
			return "", err
		}
		return vault.Decrypt(source.Value)
	}
	return os.Getenv("NORCA_TOKEN"), nil
}

// firstOf returns the first value that is not empty.
func firstOf(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
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
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := f.setup(true)
			if err != nil {
				return err
			}
			defer s.done()

			if err := tree.WriteStacks(out, s.stacks); err != nil {
				return err
			}
			for _, st := range s.stacks {
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %d files\n", filepath.Join(out, st.Name), len(st.Files))
			}
			return nil
		},
	}

	f.register(cmd, false)
	cmd.Flags().StringVar(&out, "out", "", "The directory the stacks are written to (required)")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}

// newTreeValidateCmd is tree validate: the offline checks of a tree, and
// with --rigger the installed rigger program on the built stacks.
func newTreeValidateCmd() *cobra.Command {
	var (
		f      treeFlags
		rigger bool
		bin    string
		format string
	)

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check a tree offline",
		Long: `Check a tree without a cluster: the paths and names, the manifest, the kinds,
the reserved names, the written stacks and their modules, the value forms
(with the key, directory and variables of the startup configuration), and the
syntax of every HCL file of the stacks it builds. With --rigger the installed
rigger program runs rigger stack validate on each built stack. The servers'
plan (tree plan) checks the rest.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if format != "text" && format != "json" {
				return usageError{fmt.Errorf("--format %q: text or json", format)}
			}

			out := cmd.OutOrStdout()
			s, err := f.setup(true)
			if err == nil {
				defer s.done()
				err = tree.CheckSyntax(s.stacks)
			}
			if err == nil && rigger {
				err = tree.RiggerValidate(cmd.Context(), bin, s.stacks)
			}

			if format == "json" {
				return writeValidateJSON(out, s, err)
			}
			if err != nil {
				return err
			}

			for _, w := range append(s.warnings, s.tree.Warnings...) {
				fmt.Fprintf(cmd.ErrOrStderr(), "[WARN] %s\n", w)
			}
			names := make([]string, 0, len(s.stacks))
			for _, st := range s.stacks {
				names = append(names, st.Name)
			}
			fmt.Fprintf(out, "Tree %s is valid: %d stacks (%s)\n", s.tree.Manifest.Tree.Name, len(names), strings.Join(names, ", "))
			return nil
		},
	}

	f.register(cmd, false)
	cmd.Flags().BoolVar(&rigger, "rigger", false, "Also run rigger stack validate on each built stack")
	cmd.Flags().StringVar(&bin, "rigger-bin", "rigger", "The rigger program --rigger runs")
	cmd.Flags().StringVar(&format, "format", "text", "The output: text or json")
	return cmd
}

// validateSchema names the shape of the JSON of tree validate.
const validateSchema = "norca.io/tree/validate/v1"

// writeValidateJSON writes the result of tree validate as JSON; an invalid
// tree is still exit code 1, its errors written in the JSON only.
func writeValidateJSON(w io.Writer, s *treeSetup, err error) error {
	doc := struct {
		Schema   string   `json:"schema"`
		Tree     string   `json:"tree,omitempty"`
		Valid    bool     `json:"valid"`
		Stacks   []string `json:"stacks"`
		Errors   []string `json:"errors,omitempty"`
		Warnings []string `json:"warnings,omitempty"`
	}{Schema: validateSchema, Valid: err == nil, Stacks: []string{}}

	if s != nil {
		doc.Tree = s.tree.Manifest.Tree.Name
		doc.Warnings = append(append(doc.Warnings, s.warnings...), s.tree.Warnings...)
		for _, st := range s.stacks {
			doc.Stacks = append(doc.Stacks, st.Name)
		}
	}
	if err != nil {
		doc.Errors = strings.Split(err.Error(), "\n")
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if encErr := enc.Encode(doc); encErr != nil {
		return encErr
	}
	if err != nil {
		return silentError{err}
	}
	return nil
}

// silentError fails a command whose output already says why: exit code 1,
// nothing printed.
type silentError struct{ error }

func (e silentError) Unwrap() error { return e.error }

// newTreeInitCmd is tree init: the skeleton of a new tree and a commented
// startup configuration.
func newTreeInitCmd() *cobra.Command {
	var (
		dir, name, config string
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write the skeleton of a new tree and a startup configuration template",
		Long: `Write the skeleton of a new tree: tree.hcl, README.md, a .gitattributes that
keeps Git from converting the line endings of values, and a .gitignore for
credentials. A commented startup configuration is written to --config (default
./` + tree.DefaultConfigFile + `) with mode 0600 unless it exists; it holds
credentials and stays out of Git. A directory that has a tree is refused.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				abs, err := filepath.Abs(dir)
				if err != nil {
					return err
				}
				name = filepath.Base(abs)
			}

			out := cmd.OutOrStdout()
			written, err := tree.Init(dir, name)
			for _, p := range written {
				fmt.Fprintf(out, "wrote %s\n", p)
			}
			if err != nil {
				return err
			}

			ok, err := tree.WriteConfigTemplate(config)
			if err != nil {
				return err
			}
			if ok {
				fmt.Fprintf(out, "wrote %s (the startup configuration: credentials, keep it out of Git)\n", config)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&dir, "dir", "d", "cluster-config", "The root of the new tree")
	cmd.Flags().StringVar(&name, "name", "", "The name of the tree (default: the name of its directory)")
	cmd.Flags().StringVar(&config, "config", tree.DefaultConfigFile, "Where the startup configuration template is written")
	return cmd
}

// newTreeStatusCmd is tree status: the releases of each stack of a tree,
// their drift and last operation, and the orphan releases.
func newTreeStatusCmd() *cobra.Command {
	var f treeFlags

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the releases of the tree's stacks, their drift and last operation",
		Long: `Show what the servers hold for each stack of the tree: the releases it owns,
their revision, status and drift, the stack's last operation, and the releases
of generated stacks the tree no longer has (an apply with --prune uninstalls
them). Nothing is changed.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.format != "text" && f.format != "json" {
				return usageError{fmt.Errorf("--format %q: text or json", f.format)}
			}

			s, err := f.setup(false)
			if err != nil {
				return err
			}
			s.done()

			runner, err := f.runner(s, cmd.ErrOrStderr())
			if err != nil {
				return err
			}

			states, err := runner.Status(cmd.Context(), s.stacks)
			if err != nil {
				return err
			}

			var orphans []tree.Orphan
			if !f.narrowed() {
				if orphans, err = runner.Orphans(cmd.Context(), s.stacks, s.region.Name); err != nil {
					return err
				}
			}

			out := cmd.OutOrStdout()
			if f.format == "json" {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(struct {
					Schema  string             `json:"schema"`
					Tree    string             `json:"tree"`
					Region  string             `json:"region"`
					Stacks  []*tree.StackState `json:"stacks"`
					Orphans []tree.Orphan      `json:"orphans,omitempty"`
				}{statusSchema, s.tree.Manifest.Tree.Name, s.region.Name, states, orphans})
			}

			tree.RenderStatus(out, s.tree, s.region.Name, states, orphans)
			return nil
		},
	}

	f.register(cmd, true)
	return cmd
}

// statusSchema names the shape of the JSON of tree status.
const statusSchema = "norca.io/tree/status/v1"

// newTreePlanCmd is tree plan: the servers' plan of every stack of a tree.
func newTreePlanCmd() *cobra.Command {
	var (
		f        treeFlags
		detailed bool
	)

	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show what an apply of the tree would change",
		Long: `Show what an apply of the tree would change: the servers' plan of every stack,
and what a guard would block. Exit codes: 0 the plan was made; with
--detailed-exitcode 0 nothing to change, 2 changes pending, 3 the run is
blocked; 1 an error.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, plans, _, err := treePlan(cmd, &f)
			if err != nil {
				return err
			}

			changes, err := writePlan(cmd.OutOrStdout(), &f, s, plans, false)
			if err != nil || !detailed {
				return err
			}
			if blocked := firstBlocked(plans, s, false); blocked != nil {
				return silentBlocked{blocked}
			}
			if changes {
				return errChangesPending
			}
			return nil
		},
	}

	f.register(cmd, true)
	cmd.Flags().BoolVar(&detailed, "detailed-exitcode", false, "Exit with 2 when there are changes and 3 when the run is blocked")
	return cmd
}

// newTreeApplyCmd is tree apply: plan every stack, refuse when a guard
// blocks one, then apply them in order.
func newTreeApplyCmd() *cobra.Command {
	var (
		f         treeFlags
		yes       bool
		yesReally bool
	)

	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Plan the stacks of the tree and apply them in order",
		Long: `Plan every stack of the tree, show the plans, and apply the stacks in order.
Nothing is applied when the run is blocked (a delete without its --prune flag,
a kind outside limits.kinds or --kind, more changes than limits.max_changes
without --yes-really): exit code 3. The first stack that fails stops the run.`,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, plans, runner, err := treePlan(cmd, &f)
			if err != nil {
				return err
			}

			changes, err := writePlan(cmd.OutOrStdout(), &f, s, plans, yesReally)
			if err != nil {
				return err
			}
			if blocked := firstBlocked(plans, s, yesReally); blocked != nil {
				return blocked
			}
			if !changes {
				return nil
			}
			if !yes && !confirm(cmd.InOrStdin(), cmd.ErrOrStderr()) {
				return errors.New("not applied")
			}

			stacks := make([]*tree.Stack, 0, len(plans))
			for _, p := range plans {
				// A stack with nothing to do is not applied: an orphan
				// release without --prune, a stack in sync.
				if tree.HasChanges(p) {
					stacks = append(stacks, p.Stack)
				}
			}

			_, err = runner.Apply(cmd.Context(), stacks)
			return err
		},
	}

	f.register(cmd, true)
	cmd.Flags().BoolVar(&yes, "yes", false, "Apply without asking")
	cmd.Flags().BoolVar(&yesReally, "yes-really", false, "Apply a run of more changes than limits.max_changes")
	return cmd
}

// silentBlocked is a blocked plan: exit code 3, its reason already shown by
// the plan.
type silentBlocked struct{ error }

func (e silentBlocked) Unwrap() error { return e.error }

// firstBlocked is the first reason the run may not be applied: a blocked
// stack, or more changes than limits.max_changes.
func firstBlocked(plans []*tree.Planned, s *treeSetup, yesReally bool) error {
	for _, p := range plans {
		if p.Blocked != nil {
			return fmt.Errorf("stack %s: %w", p.Stack.Name, p.Blocked)
		}
	}
	return tree.CheckChanges(plans, s.cfg.MaxChanges(), yesReally)
}

// treePlan reads the configuration and the tree, syncs the registries, and
// has the servers plan the stacks.
func treePlan(cmd *cobra.Command, f *treeFlags) (*treeSetup, []*tree.Planned, *tree.Runner, error) {
	if f.format != "text" && f.format != "json" {
		return nil, nil, nil, usageError{fmt.Errorf("--format %q: text or json", f.format)}
	}

	s, err := f.setup(false)
	if err != nil {
		return nil, nil, nil, err
	}
	// The stacks are built: the values directory is read no more.
	s.done()

	if err := tree.CheckSyntax(s.stacks); err != nil {
		return nil, nil, nil, err
	}

	stderr := cmd.ErrOrStderr()
	for _, w := range s.warnings {
		fmt.Fprintf(stderr, "[WARN] %s\n", w)
	}

	runner, err := f.runner(s, stderr)
	if err != nil {
		return nil, nil, nil, err
	}

	if err := runner.SyncRegistries(cmd.Context(), s.cfg.SyncRegistries); err != nil {
		return nil, nil, nil, err
	}

	// The releases of generated stacks whose directories are gone: their
	// stacks are planned empty, so that the plan shows their removal.
	if !f.narrowed() {
		orphans, err := runner.Orphans(cmd.Context(), s.stacks, s.region.Name)
		if err != nil {
			fmt.Fprintf(stderr, "[WARN] the releases of removed namespace directories were not looked for: %v\n", err)
		}
		s.stacks = append(s.stacks, tree.OrphanStacks(orphans, s.region.Name)...)
	}

	plans, err := runner.Plan(cmd.Context(), s.stacks)
	return s, plans, runner, err
}

// planJSON is the JSON of a plan (design §10, §7.13).
type planJSON struct {
	Schema      string          `json:"schema"`
	Tree        string          `json:"tree"`
	Environment string          `json:"environment,omitempty"`
	Region      string          `json:"region"`
	Warnings    []string        `json:"warnings,omitempty"`
	Stacks      []stackPlanJSON `json:"stacks"`
	Changes     int             `json:"changes"`
	Blocked     string          `json:"blocked,omitempty"`
}

type stackPlanJSON struct {
	Stack     string            `json:"stack"`
	Namespace string            `json:"namespace"`
	Plan      *gitops.StackPlan `json:"plan"`
	Warnings  []string          `json:"warnings,omitempty"`
	Blocked   string            `json:"blocked,omitempty"`
}

// writePlan writes the plans as text or JSON and reports whether there is
// anything to apply.
func writePlan(w io.Writer, f *treeFlags, s *treeSetup, plans []*tree.Planned, yesReally bool) (bool, error) {
	if f.format != "json" {
		changes := tree.RenderPlan(w, s.tree, s.region.Name, plans)
		if err := tree.CheckChanges(plans, s.cfg.MaxChanges(), yesReally); err != nil {
			fmt.Fprintf(w, "BLOCKED: %v\n", err)
		}
		return changes, nil
	}

	doc := planJSON{
		Schema: planSchema, Tree: s.tree.Manifest.Tree.Name, Environment: s.tree.Manifest.Tree.Environment,
		Region: s.region.Name, Warnings: s.tree.Warnings, Stacks: []stackPlanJSON{},
	}
	pending := false
	for _, p := range plans {
		sp := stackPlanJSON{Stack: p.Stack.Name, Namespace: p.Stack.Namespace, Plan: p.Plan, Warnings: p.Warnings}
		if p.Blocked != nil {
			sp.Blocked = p.Blocked.Error()
		}
		doc.Stacks = append(doc.Stacks, sp)
		doc.Changes += tree.Changes(p)
		pending = pending || tree.HasChanges(p)
	}
	if err := firstBlocked(plans, s, yesReally); err != nil {
		doc.Blocked = err.Error()
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return pending, enc.Encode(doc)
}

// reportTo writes the events of the runs of the stacks of a tree, the plans
// left out (the plan output shows them).
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
