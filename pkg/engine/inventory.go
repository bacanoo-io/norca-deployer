package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/crypto"
	"github.com/bacanoo-io/norca-deployer/pkg/state"
)

// The list of installed nodes (.norca-deployer/nodes-<region>.json) is what
// lets apply notice a node that was deleted from the plan: comparing the
// nodes with the plan only covers the nodes the plan still names.

// loadInventory loads the installed nodes, resolves the decommission entries
// and checks the plan against the list before anything changes:
//   - a node that was installed but is neither in the plan nor in
//     decommission stops the apply (deleting it from the plan alone would
//     leave Norca running there);
//   - a node cannot change its role in place.
func (r *Runner) loadInventory() error {
	inst, err := r.store.LoadInstalled(r.plan.Metadata.Region)
	if err != nil {
		return fmt.Errorf("installed nodes: %w", err)
	}
	r.installed = inst
	r.decom, r.decomDone, err = resolveDecommission(r.plan, inst)
	if err != nil {
		return err
	}
	return checkInventory(r.plan, inst, r.decom)
}

// resolveDecommission completes the decommission entries from the installed
// list: an entry may name only the host or hostname, and the node then gets
// the role and connection settings it was installed with. An entry with only
// a name that is not in the list was decommissioned by an earlier apply; it
// is skipped, so the entry can stay in the plan.
func resolveDecommission(plan *config.DeploymentPlan, inst *state.Installed) (res []*config.DecommissionNode, done []string, err error) {
	for i := range plan.Inventory.Decommission {
		d := plan.Inventory.Decommission[i]
		name := config.NodeKey(&d.NodeConfig)
		key, e := inst.Lookup(name)
		if e == nil && d.Host != "" {
			key, e = inst.Lookup(d.Host)
		}
		if e != nil {
			if !d.Connection.HasConnection() {
				d.NodeConfig = e.Node
			}
			if d.Role == "" {
				d.Role = e.Role
			} else if d.Role != e.Role {
				return nil, nil, fmt.Errorf("decommission %s: role %s, but it was installed as %s", key, d.Role, e.Role)
			}
		}
		if e == nil && !d.Connection.HasConnection() {
			done = append(done, name)
			continue
		}
		switch {
		case d.Role == "":
			return nil, nil, fmt.Errorf("decommission %s: not in the installed nodes; set role (master or client)", name)
		case d.Hostname == "":
			return nil, nil, fmt.Errorf("decommission %s: hostname is required (the cluster knows nodes by name)", name)
		}
		res = append(res, &d)
	}
	return res, done, nil
}

func checkInventory(plan *config.DeploymentPlan, inst *state.Installed, decom []*config.DecommissionNode) error {
	inPlan := map[string]string{}
	for i := range plan.Inventory.Masters {
		inPlan[config.NodeKey(&plan.Inventory.Masters[i])] = config.RoleMaster
	}
	for i := range plan.Inventory.Clients {
		inPlan[config.NodeKey(&plan.Inventory.Clients[i])] = config.RoleClient
	}
	leaving := map[string]bool{}
	for _, d := range decom {
		if key, _ := inst.Lookup(config.NodeKey(&d.NodeConfig)); key != "" {
			leaving[key] = true
		}
	}

	var orphans, roleChanges []string
	for _, key := range inst.Keys() {
		e := inst.Nodes[key]
		role, ok := inPlan[key]
		switch {
		case leaving[key]:
		case !ok:
			orphans = append(orphans, fmt.Sprintf("%s (%s, %s)", key, e.Node.Host, e.Role))
		case role != e.Role:
			roleChanges = append(roleChanges, fmt.Sprintf("%s (%s -> %s)", key, e.Role, role))
		}
	}
	if len(orphans) > 0 {
		return fmt.Errorf("installed nodes missing from the plan: %s\n"+
			"  Put them back into the inventory, or list them under inventory.decommission (the hostname is enough)",
			strings.Join(orphans, ", "))
	}
	if len(roleChanges) > 0 {
		return fmt.Errorf("nodes cannot change their role in place: %s\n"+
			"  Decommission the node first, then add it again with the new role", strings.Join(roleChanges, ", "))
	}
	return nil
}

// recordInstalled adds the successfully provisioned nodes to the list.
func (r *Runner) recordInstalled(ts []*target, role string) error {
	changed := false
	for _, t := range ts {
		if t.err == nil && t.conn != nil {
			r.installed.Record(t.node, role, r.plan.Release.Version)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	if err := r.store.SaveInstalled(r.installed); err != nil {
		return fmt.Errorf("failed to save the installed nodes: %w", err)
	}
	return nil
}

// forgetInstalled removes a decommissioned node from the list.
func (r *Runner) forgetInstalled(d *config.DecommissionNode) error {
	key, e := r.installed.Lookup(config.NodeKey(&d.NodeConfig))
	if e == nil {
		return nil
	}
	delete(r.installed.Nodes, key)
	return r.store.SaveInstalled(r.installed)
}

// NodeStatus is one line of the inventory report of `plan`.
type NodeStatus struct {
	Key, Host, Role, Status string
}

// InventoryReport compares the plan with the installed nodes for `plan`:
// new, installed, decommission, or missing from the plan.
func InventoryReport(plan *config.DeploymentPlan, vault *crypto.Vault) ([]NodeStatus, error) {
	inst, err := state.OpenReadOnly(plan, vault).LoadInstalled(plan.Metadata.Region)
	if err != nil {
		return nil, err
	}
	decom, done, err := resolveDecommission(plan, inst)
	if err != nil {
		return nil, err
	}
	var res []NodeStatus
	seen := map[string]bool{}
	add := func(n *config.NodeConfig, role string) {
		key := config.NodeKey(n)
		seen[key] = true
		status := "new"
		if e := inst.Nodes[key]; e != nil {
			status = "installed " + e.Release
			if e.Role != role {
				status = "role change from " + e.Role + " (not allowed)"
			}
		}
		res = append(res, NodeStatus{key, n.Host, role, status})
	}
	for i := range plan.Inventory.Masters {
		add(&plan.Inventory.Masters[i], config.RoleMaster)
	}
	for i := range plan.Inventory.Clients {
		add(&plan.Inventory.Clients[i], config.RoleClient)
	}
	for _, d := range decom {
		key, e := inst.Lookup(config.NodeKey(&d.NodeConfig))
		if key == "" {
			key = config.NodeKey(&d.NodeConfig)
		}
		seen[key] = true
		status := "decommission"
		if e == nil {
			status = "decommission (not in the installed nodes)"
		}
		res = append(res, NodeStatus{key, d.Host, d.Role, status})
	}
	for _, key := range done {
		seen[key] = true
		res = append(res, NodeStatus{key, "", "", "decommissioned earlier (entry can be removed)"})
	}
	var missing []string
	for _, key := range inst.Keys() {
		if !seen[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	for _, key := range missing {
		e := inst.Nodes[key]
		res = append(res, NodeStatus{key, e.Node.Host, e.Role, "MISSING from the plan (apply refuses; decommission it or add it back)"})
	}
	return res, nil
}
