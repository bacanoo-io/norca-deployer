# Multi-Region

A Norca cluster can span several regions. (Nearby sites with low latency can also be datacenters of one region: see [one region over several datacenters](multi-datacenter.md).) Each region has its own servers and Raft quorum; the regions are joined over Serf (federation) and forward API requests to each other. ACLs are bootstrapped in one **authoritative region** and replicated to the others.

## One plan per region

Write one plan per region and keep the plans **in the same directory**. Then they share:

- the CA (`certs/`), so the regions trust each other;
- the gossip key (`.norca-deployer/gossip.key`);
- the management token of the authoritative region (`.norca-deployer/management-token`).

```text
regions/
  eu-central.yaml     # authoritative region
  us-east.yaml        # replica
  ap-south.yaml       # replica
  certs/              # shared CA
  .norca-deployer/    # shared gossip key and ACL token
```

## Authoritative region

```yaml
metadata:
  region: eu-central
federation:
  authoritative_region: eu-central
```

It bootstraps ACLs as usual. Its servers get `authoritative_region` in the server block.

With ACLs, its servers also get its own management token as `acl.replication_token`. The leader needs it to delete the namespace of a [tenant](https://bacanoo-io.github.io/norca-doc/docs/features/tenants/): it asks every other region whether the namespace still has jobs there, and it asks with this token, because its own credentials never leave the region. Without the token a federation cannot delete tenants.

- **First apply:** the token does not exist before the bootstrap, so the deployer chooses it: it generates a secret, stores it as `<management_token_output>.pending` (encrypted with the master key), writes it into `norca.hcl` and gives it to `norca acl bootstrap`, which makes it the management token. The servers start with it; no second restart. An apply that stops in between finds the same secret again.
- **Running cluster:** the stored management token is written into `norca.hcl`, so adding the `federation` block restarts the servers one at a time (see [Order](#order)).
- **`auto_bootstrap: false` and no stored token:** the servers get no replication token, and `apply` warns. Put the management token into `management_token_output` and apply again.

`replication_token_file` is refused in the authoritative region: its servers always get its own management token.

## Replica regions

```yaml
metadata:
  region: us-east
federation:
  authoritative_region: eu-central
  join:                                    # servers of the authoritative region
    - 10.8.0.10                            # host or host:serf_port (default: network.ports.serf)
    - 10.8.0.11
    - 10.8.0.12
  replication_token_file: .norca-deployer/management-token
```

- The `join` addresses are added to the servers' `retry_join`, which federates the regions.
- `replication_token_file` (required with ACLs) is the authoritative region's management token. It is written to the servers as `acl.replication_token`.
- A replica region does not bootstrap ACLs: `apply` prints `Region us-east replicates ACLs from eu-central (no bootstrap)`, and what `apply` writes there (secrets, the Rigger stack, GitOps bindings) uses the authoritative token.

## Order

1. Apply the authoritative region. If it already runs, adding the `federation` block changes `norca.hcl` (`authoritative_region`, and with ACLs `acl.replication_token`), so its servers restart one at a time.
2. Apply the replica regions. They start as new clusters and join the authoritative region.

Check the result with `norca operator api /v1/regions` on any server: it lists every region.
