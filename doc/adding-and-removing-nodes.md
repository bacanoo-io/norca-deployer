# Adding and Removing Nodes

The inventory of the plan is the desired set of nodes. `apply` installs what is new, checks what exists, and removes what is listed under `inventory.decommission`. Examples: [add-node.yaml](../examples/add-node.yaml) and [decommission-node.yaml](../examples/decommission-node.yaml).

## The list of installed nodes

After a node was provisioned and its agent is healthy, `apply` records it in `.norca-deployer/nodes-<region>.json`: its role, the node definition from the plan (including the connection settings), the release and the time of installation and of the last update. The file is 0600 and encrypted when a master key exists. Each region has its own file, so plans of several regions can share the state directory.

The list complements the comparison with the nodes themselves. Comparing a node with the plan only works for nodes the plan still names; the list also knows the nodes the plan no longer names. Before changing anything, `apply` checks:

| Situation | Result |
| --- | --- |
| Node in the plan, not in the list | New node: installed |
| Node in the plan and in the list | Checked and updated; restarted only when something changed |
| Node in the list, missing from the plan and from `decommission` | `apply` stops: put the node back, or decommission it |
| Node in the list with another role than in the plan | `apply` stops: a node cannot switch between server and client in place |
| Node under `decommission` | Removed from the cluster and uninstalled, then removed from the list |

`norca-deployer plan` shows the result of this comparison:

```text
Node Status (6):
  - norca-server-01      10.8.0.10       master installed v0.2.0
  - norca-worker-01      10.8.0.20       client installed v0.2.0
  - norca-worker-03      10.8.0.22       client new
  - norca-worker-02      10.8.0.21       client decommission
```

Clusters installed before the list existed get it on their next `apply`; nodes removed from those plans earlier are not known to it.

## Adding a node

Add the node to `inventory.masters` or `inventory.clients` and run `apply`.

- **Client:** only the new node is installed. Other nodes do not change and are not restarted.
- **Server:** every server's configuration changes (join list, `bootstrap_expect`), so the existing servers restart one at a time with health, leader and peer checks. Keep an odd number of servers and add them one plan change at a time.

The new node gets a certificate from the existing CA, the existing gossip key and the same release.

## Removing a node

Move the node from `masters`/`clients` to `inventory.decommission`:

```yaml
inventory:
  decommission:
    - hostname: norca-worker-02   # from the installed list: role and connection are known
      drain_deadline: 900         # seconds, default 600
      purge_data: true            # also delete data, log and application directories
```

An entry needs the hostname or host of a node in the installed list. For a node that is not in the list (installed before the list existed, or from another state directory), give `role` (`master` or `client`), `hostname` and the connection settings explicitly, like an inventory entry.

The removal runs in the post-cluster stage, after the remaining nodes were rolled out and after the ACL bootstrap; with ACLs the management token is required. Clients go first, then servers one at a time.

**Client:**
1. The node is drained (`/v1/node/<id>/drain`) with `drain_deadline`; its allocations are rescheduled on the other clients. Whatever still runs at the deadline is stopped.
2. The drained allocations are garbage-collected on the client (`/v1/client/gc`), which also removes their networks (the pause containers of bridge mode).
3. The agent is stopped and Norca is uninstalled; Docker containers of Norca allocations that are still there (for example because the node could not be reached) are removed.
4. The node is purged from the cluster (`/v1/node/<id>/purge`).

**Server:**
1. Before this stage, the remaining servers were already restarted one at a time without the leaving server in their configuration; the quorum was kept throughout.
2. The agent on the leaving server is stopped and Norca is uninstalled.
3. The server is removed from Raft (`/v1/operator/raft/peer`) and from Serf (`force-leave`).
4. `apply` waits until the remaining servers report the new peer count.

**Uninstall** removes the service, `norca.hcl`, the TLS files, the binary and `/var/lib/norca-deployer`; on Windows the service, the binary, the configuration and the TLS files. With `purge_data` the data directory (after unmounting allocation mounts), the log directory and the application directory are deleted too. Host packages, CNI plugins, sysctl and firewall settings are left in place.

A machine that cannot be reached (for example, already deleted) is still removed from the cluster; `apply` prints a warning that Norca could not be uninstalled from it.

The entries can stay in the plan: a later `apply` reports them as decommissioned earlier and does nothing.

## Replacing a node

To replace a machine under the same name, decommission the old node in one `apply`, then add the new machine in the next. To change a node's role, do the same: decommission it, then add it with the new role.
