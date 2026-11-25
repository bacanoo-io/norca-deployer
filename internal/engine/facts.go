package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/host"
	"github.com/bacanoo-io/norca-deployer/internal/platform/packages"
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
	t.conn, t.facts = conn, facts
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
		if err := r.checkNodeFacts(t); err != nil {
			problems = append(problems, fmt.Sprintf("node [%s]: %v", t.node.Host, err))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the plan does not fit the nodes (nothing was changed):\n - %s", strings.Join(problems, "\n - "))
	}
	return nil
}

// checkNodeFacts checks the plan against the facts of one node.
func (r *Runner) checkNodeFacts(t *target) error {
	f := t.facts
	if f.Platform == "windows" {
		return nil
	}
	if f.PackageManager != config.PackageManagerApt {
		if sel := packages.Select(r.plan.HostPackages, t.node, t.isMaster); !sel.Empty() {
			return fmt.Errorf("the node uses the package manager %s, but host_packages has packages for its roles only for apt",
				f.PackageManager)
		}
		if r.plan.Release.Source == config.ReleaseHostPackage {
			return fmt.Errorf("release.source host-package: the node uses the package manager %s, but host_packages installs %q only with apt",
				f.PackageManager, r.plan.Release.Package)
		}
	}
	return nil
}
