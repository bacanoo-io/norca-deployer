package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/host"
	"github.com/bacanoo-io/norca-deployer/internal/platform"
	"github.com/bacanoo-io/norca-deployer/internal/platform/packages"
	"github.com/bacanoo-io/norca-deployer/internal/platform/scripts"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// openNode connects to a node and finds out what it is, before anything is
// changed there: the probe of its facts (as the SSH user), the overrides of
// host_facts, the escalation of the commands that follow, and the check
// that the deployer can drive the node.
func (r *Runner) openNode(node *config.NodeConfig) (connector.Connector, *host.Facts, error) {
	conn, err := r.connect(node)
	if err != nil {
		return nil, nil, fmt.Errorf("connection error: %w", err)
	}
	facts, overrides, err := host.Probe(conn, node)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	become := node.Connection.Become && !strings.EqualFold(node.Connection.Mode, "local")
	if err := facts.ChooseEscalation(become, node.Connection.BecomeMethod); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if err := connector.SetEscalation(conn, connector.Escalation(facts.Escalation)); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	fmt.Printf("    -> [%s] Host: %s\n", node.Host, facts)
	for _, l := range overrides {
		fmt.Printf("    -> [%s] %s\n", node.Host, l)
	}
	for _, w := range facts.Warnings() {
		fmt.Printf("    [WARN] [%s] %s\n", node.Host, w)
	}
	if facts.Arch != "" && node.Arch != "" && facts.Arch != node.Arch {
		fmt.Printf("    [WARN] [%s] the node is %s, the plan says arch %s (the plan is used)\n", node.Host, facts.Arch, node.Arch)
	}
	if err := facts.Check(); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, facts, nil
}

// openTarget connects a target of the apply, unless it is connected.
func (r *Runner) openTarget(t *target) error {
	if t.conn != nil {
		return nil
	}
	fmt.Printf("    -> [%s] Connecting (%s mode)...\n", t.node.Host, t.node.Connection.Mode)
	conn, facts, err := r.openNode(t.node)
	if err != nil {
		return err
	}
	drivers, err := platform.For(facts)
	if err != nil {
		_ = conn.Close()
		return err
	}
	t.conn, t.facts, t.drivers = conn, facts, drivers
	return nil
}

// probeAll connects every node of the apply and collects its facts, in
// parallel, before the first change on any node. A node that cannot be
// reached or driven gets its error; the plan checks that need the facts of
// every node follow (checkFacts).
func (r *Runner) probeAll(ctx context.Context, ts []*target) {
	var wg sync.WaitGroup
	for _, t := range ts {
		wg.Go(func() {
			if ctx.Err() != nil {
				t.err = interrupted(ctx, "the connection to the node")
				return
			}
			t.err = r.openTarget(t)
		})
	}
	wg.Wait()
}

// checkFacts runs the checks of the plan that need the facts of the nodes,
// after the probes and before the first change: an error is an error of the
// plan, and nothing is changed on any node.
func (r *Runner) checkFacts(ts []*target) error {
	var problems []string
	for _, t := range ts {
		if t.facts == nil {
			continue
		}
		err := r.checkNodeFacts(t)
		if err == nil {
			continue
		}
		errs := []error{err}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			errs = joined.Unwrap()
		}
		for _, e := range errs {
			problems = append(problems, fmt.Sprintf("node [%s]: %v", t.node.Host, e))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the plan does not fit the nodes (nothing was changed):\n - %s", strings.Join(problems, "\n - "))
	}
	return nil
}

// checkNodeFacts checks the plan against the facts of one node: the block
// of host_packages of its package manager, the package of release.source
// host-package, and the interpreters of its scripts.
func (r *Runner) checkNodeFacts(t *target) error {
	f := t.facts
	if f.Platform == "windows" {
		return nil
	}
	var errs []error
	// The node takes only the block of its package manager. A block of
	// another manager that selects packages for it, while its own manager
	// has no block, is a block the plan forgot.
	own, hasOwn := packages.Block(r.plan.HostPackages, f.PackageManager, t.node, t.isMaster)
	if !hasOwn {
		var others []string
		for _, m := range packages.BlockManagers {
			if sel, _ := packages.Block(r.plan.HostPackages, m, t.node, t.isMaster); m != f.PackageManager && !sel.Empty() {
				others = append(others, m)
			}
		}
		if len(others) > 0 {
			errs = append(errs, fmt.Errorf("the node uses %s, but host_packages has packages for its roles only in %s",
				f.PackageManager, strings.Join(others, ", ")))
		}
	}
	if r.plan.Release.Source == config.ReleaseHostPackage && !own.Installs(r.plan.Release.Package) {
		errs = append(errs, fmt.Errorf("release.source host-package: the block of host_packages of the node's package manager (%s) does not install %q on it",
			f.PackageManager, r.plan.Release.Package))
	}
	if !f.HasBash {
		for _, phase := range []string{config.PhasePreInstall, config.PhasePostPackages, config.PhasePostInstall} {
			for _, s := range scripts.Select(r.plan.Scripts, phase, t.scriptTarget()) {
				// bash installed by host_packages is there for the
				// scripts after the packages.
				installed := phase != config.PhasePreInstall && own.Installs("bash")
				if config.ScriptInterpreter(s) == config.InterpreterBash && !installed {
					errs = append(errs, fmt.Errorf("script %q runs with bash, which the node does not have: install bash with host_packages (for a script after pre_install) or set interpreter: sh",
						s.Name))
				}
			}
		}
	}
	return errors.Join(errs...)
}
