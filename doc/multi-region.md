# Multi-Region

A Norca cluster can span several regions. Each region has its own servers and Raft quorum; the regions are joined over Serf (federation) and forward API requests to each other. ACLs are bootstrapped in one **authoritative region** and replicated to the others.

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
- A replica region does not bootstrap ACLs: `apply` prints `Region us-east replicates ACLs from eu-central (no bootstrap)`, and packs use the authoritative token.

## Order

1. Apply the authoritative region. If it already runs, adding the `federation` block changes `norca.hcl`, so its servers restart one at a time.
2. Apply the replica regions. They start as new clusters and join the authoritative region.

Check the result with `norca operator api /v1/regions` on any server: it lists every region.
