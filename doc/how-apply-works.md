# How `apply` Works

`apply` brings every node to the state the plan describes. It is idempotent: running it again on an unchanged plan changes nothing and restarts nothing.

## Stages

```text
==> [1/5] Preparing Security, PKI and artifacts...
==> [2/5] Provisioning Master (Server) nodes (3 nodes)...
==> [3/5] Rolling restart of Master nodes...
==> [4/5] Provisioning Client (Worker) nodes (3 nodes)...
==> [5/5] Post-Cluster phase (ACL, decommission, secrets & packs)...
```

### 1. Preparation (operator machine)

- Loads the list of installed nodes and compares it with the plan. A node that was installed but is neither in the inventory nor under `decommission`, or that changed its role, stops the run before anything changes (see [Adding and removing nodes](adding-and-removing-nodes.md)).
- Loads or creates the CA (see [Security](security.md)).
- Loads or generates the gossip key.
- For a replica region, reads the replication token of the authoritative region.
- Resolves the artifacts: the Norca binary and the CNI archive are downloaded once, verified with SHA-256 and cached in `.norca-deployer/cache/`.

### 2. Server preparation (in parallel, no restarts)

On every server, in this order:

1. `pre_install` scripts
2. APT repositories and packages (`host_packages`)
3. `post_packages` scripts
4. Kernel tuning (sysctl, modules) or Windows tuning
5. Directories (bin, config, data, log, tls; CNI and app volumes on client nodes)
6. Norca binary (only copied when its SHA-256 differs)
7. CNI plugins (nodes that run a client)
8. TLS certificates (kept while still valid for the node)
9. `norca.hcl`
10. Firewall rules
11. Service unit (systemd or Windows service)

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
- **Node pools** of `node_pools` (description and meta), the **ACL policies** of `security.acl.policies`, optionally bound to jobs, and the **secrets and Variables** of `secrets.bootstrap` (see [Bootstrap secrets and ACL policies](secrets-and-policies.md)).
- **Packs:** the pack registries are created and synced, and changed pack releases are deployed (see [Packs](packs.md)).

These calls use the `norca` CLI on the first server (`norca operator api`), so the API does not have to be reachable from the operator machine.

## What triggers a restart

| Change | Restart |
| --- | --- |
| Norca binary (new `release.version` or a different local file) | Yes |
| A certificate re-issued (new SANs, SPIFFE ID change, CA change, expiry within 30 days) | Yes |
| `norca.hcl` content (any setting that ends up in the agent config) | Yes |
| systemd unit or Windows service definition | Yes |
| Host packages, scripts, CNI plugins, firewall, sysctl | No |
| Pack values | No (the pack release is redeployed instead) |
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
| `.norca-deployer/packs/*.sha256` | Settings digest of each deployed pack |
| `.norca-deployer/generated/`, `nkeys/` | Generated values and NATS NKey seeds (encrypted when a master key exists) |
| `.norca-deployer/secrets/`, `acl-policies/` | Digests of the written secrets and policies |
| `.norca-deployer/cache/` | Downloaded Norca and CNI artifacts |

On the nodes the deployer uses `/var/lib/norca-deployer/` (script markers, copied `.deb` files and temporary CLI files).

## Failure handling

- An error on a node is reported with the node address. Servers: the run stops before restarts. Clients: the other clients of the batch finish, then the rollout stops.
- Fix the cause and run `apply` again. Everything that was already done is skipped.
