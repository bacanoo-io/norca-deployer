# How `apply` Works

`apply` brings every node to the state the plan describes. It is idempotent: running it again on an unchanged plan changes nothing and restarts nothing.

## Stages

```text
==> [1/5] Preparing Security, PKI and artifacts...
==> [2/5] Provisioning Master (Server) nodes (3 nodes)...
==> [3/5] Rolling restart of Master nodes...
==> [4/5] Provisioning Client (Worker) nodes (3 nodes)...
==> [5/5] Post-Cluster phase (ACL, decommission, secrets, Rigger stack & GitOps)...
```

### 1. Preparation (operator machine)

- Loads the list of installed nodes and compares it with the plan. A node that was installed but is neither in the inventory nor under `decommission`, or that changed its role, stops the run before anything changes (see [Adding and removing nodes](adding-and-removing-nodes.md)).
- Loads or creates the CA (see [Security](security.md)).
- Loads or generates the gossip key.
- For a replica region, reads the replication token of the authoritative region.
- Resolves the artifacts: the Norca binary and the CNI archive are downloaded once, verified with SHA-256 and cached in `.norca-deployer/cache/`.
- Connects to every node and finds out what it is: the family of its operating system, its package manager, its init system and how commands get root (`sudo` or `doas`). Each node prints a `Host:` line. Then the plan is checked against the nodes: the block of `host_packages` of each node's package manager, the package of `release.source: host-package`, the interpreter of the scripts. A node that cannot be reached or driven, or a plan that does not fit a node, stops the run **before anything changes** (a client that cannot be reached is reported and skipped, as before).

### 2. Server preparation (in parallel, no restarts)

On every server, in this order:

1. `pre_install` scripts
2. Repositories and packages of the node's package manager (`host_packages.apt`, `dnf` or `apk`)
3. On OpenRC (Alpine): the dependencies of the services read again and the cgroups mounted, so that a `post_packages` script can start a service of the packages (see [Host packages and scripts](host-packages-and-scripts.md))
4. `post_packages` scripts
5. Kernel tuning (sysctl, modules) or Windows tuning
6. Directories (bin, config, data, log, tls; CNI and app volumes on client nodes)
7. Norca binary (only copied when its SHA-256 differs)
8. CNI plugins (nodes that run a client)
9. TLS certificates (kept while still valid for the node)
10. `norca.hcl`
11. Firewall rules
12. Service unit (systemd, OpenRC or Windows service)

Each item is a step: its files are written only when their content differs, and its change runs only when its check finds something to do, so a second `apply` changes nothing. The commands run in `sh` on the node (dash on Debian and Ubuntu), each starting with a comment that names its step (`# apply: apt packages`). The install script of a scaled machine is made of the same steps (see [Node scaling machines](node-scaling-machines.md)).

The deployer records for each node whether the binary, a certificate, `norca.hcl` or the service unit changed. If any server fails here, the run stops **before any agent is restarted**.

### 3. Server restarts

- **New cluster** (no server agent running yet): all servers start at once. A single server cannot elect a leader before `bootstrap_expect` servers have joined.
- **Existing cluster:** only changed servers restart, **one at a time**. After each restart the deployer waits until:
  - `/v1/agent/health?type=server` is OK,
  - a leader is elected (`/v1/status/leader`),
  - all servers of the plan are Raft peers (`/v1/status/peers`).

  If a server does not become healthy within `upgrade.health_timeout` seconds, the rollout stops and the remaining servers are left untouched, so the quorum is kept.
- `post_install` scripts run on each server afterwards.

### 4. Clients

Clients are prepared in parallel like the servers, then restarted in batches of `upgrade.max_parallel_clients`. Each client reports one of:

```text
    [10.0.0.21] restarting...
    [OK] [10.0.0.21] healthy
    [10.0.0.22] unchanged, not restarted
```

Allocations keep running while a client agent restarts. A failed batch stops the rollout. `post_install` scripts run after each client is healthy.

### 5. Post-cluster tasks (through the first server)

- **ACL bootstrap** once, when `security.acl.auto_bootstrap` is set; the token is stored locally. A replica region does not bootstrap; it uses the authoritative region's token.
- **Decommission:** nodes under `inventory.decommission` are drained or removed from Raft, stopped and uninstalled (see [Adding and removing nodes](adding-and-removing-nodes.md)).
- The **ACL policies** of `security.acl.policies`, optionally bound to jobs, and the **node pools** of `node_pools` (description and meta).
- **Namespaces:** `norca-system`, the namespace of the operator modules, and those of `namespaces`; a missing one is created, and none is deleted (see [Namespaces](secrets-and-policies.md#namespaces)).
- The **secrets and Variables** of `secrets.bootstrap` (see [Bootstrap secrets and ACL policies](secrets-and-policies.md)), and with `node_scaling.enabled` the Variables of the node scaling operator.
- **Rigger:** the stack of the `rigger` section is applied with `norca rigger stack apply` on the first server, by the servers; the plan may choose the `rigger` program of the operator machine, and the program as the one that applies (see [Rigger stacks](rigger.md)).
- **GitOps:** the stack of each enabled binding is fetched into memory and planned by the servers; one that is not in sync is applied, or with `plan-only` its plan is shown (see [GitOps](gitops.md)).

These calls use the `norca` CLI on the first server (`norca operator api`), so the API does not have to be reachable from the operator machine.

## What triggers a restart

| Change | Restart |
| --- | --- |
| Norca binary (new `release.version` or a different local file) | Yes |
| A certificate re-issued (new SANs, SPIFFE ID change, CA change, expiry within 30 days) | Yes |
| `norca.hcl` content (any setting that ends up in the agent config) | Yes |
| systemd unit or Windows service definition | Yes |
| Host packages, scripts, CNI plugins, firewall, sysctl | No |
| The Rigger stack, its values and `vars` | No (Rigger upgrades the releases that changed) |
| `apply --force-restart` | Every agent, rolling |

A node whose agent is not running is always started. Successfully provisioned nodes are recorded in the list of installed nodes after their restart (or check).

## Local state

`apply` keeps its state next to the plan file:

| Path | Content |
| --- | --- |
| `certs/norca-ca.pem`, `certs/norca-ca-key.pem` | Cluster CA (generated on the first run) |
| `certs/client-pool.pem` | Reusable client certificate (`pki.generate_worker_pool_cert`) |
| `certs/cli.pem` | Operator certificate (`security.verify_https_client`) |
| `.norca-deployer/gossip.key` | Gossip key (`AUTO_GENERATE`) |
| `.norca-deployer/management-token` | ACL management token |
| `.norca-deployer/nodes-<region>.json` | Installed nodes with role, node definition and release (encrypted when a master key exists) |
| `.norca-deployer/known_hosts` | Host keys trusted on first use |
| `.norca-deployer/rigger-stack/` | The stack of `rigger.stack` (the `stack.hcl` written in the plan) and its lock file |
| `.norca-deployer/generated/`, `nkeys/` | Generated values and NATS NKey seeds (encrypted when a master key exists) |
| `.norca-deployer/secrets/`, `acl-policies/`, `namespaces/` | Digests of the written secrets and policies and of the namespace entries |
| `.norca-deployer/cache/` | Downloaded Norca and CNI artifacts |

On the nodes the deployer uses `/var/lib/norca-deployer/` (script markers, copied `.deb` files and temporary CLI files; on the first server also `rigger/`, the copy of the Rigger stack while it is applied).

**Lost state.** Without `certs/` an `apply` makes a new CA (and, with `AUTO_GENERATE`, a new gossip key). Agents restarted with them are not trusted by the rest of the cluster ("x509: certificate signed by unknown authority", "No cluster leader"). So, after connecting to the nodes and before it changes anything, `apply` compares the `ca.pem` in the TLS directory of each node with its own CA. A node set up with another CA stops the run: the error names the nodes and the SHA-256 fingerprints of both CAs, nothing is changed on a node, and the new CA and gossip key are not kept. Then:

- restore `certs/` and `.norca-deployer/` from a backup of the directory that set up the cluster, or
- set `security.pki.ca_cert_path` and `ca_key_path` to the CA of the cluster (and `security.gossip_encryption_key` to its gossip key), or
- decommission the nodes, or remove Norca from them, to set up a new cluster.

The same check stops an `apply` whose `ca_cert_path` names another CA than the cluster has: the deployer does not rotate the CA of a running cluster.

## Failure handling

- An error on a node is reported with the node address. Servers: the run stops before restarts. Clients: the other clients of the batch finish, then the rollout stops.
- Fix the cause and run `apply` again. Everything that was already done is skipped.

## Interrupting an apply

- **Ctrl-C (or SIGTERM) once:** the step in progress is finished and the apply stops. A server or client that is being restarted is restarted and checked healthy; the next server, the next batch of clients and the post-cluster phase are not started. Nodes that were not started yet are skipped. The exit code is `130`. Run `apply` again to continue: what was done is skipped.
- **Ctrl-C a second time:** what is in progress is cut at once. The SSH sessions and local commands are closed, downloads and the wait for a node to become healthy end. A command that had already started on a node may run to its end there.
