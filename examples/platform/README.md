# Platform example

A small, complete tree for `norca-deployer platform` (design: `design/platform-management-design.md`).
It has one namespace (`apps`) with an API job that depends on a Postgres pack release, a periodic
worker job, a Variable, a secret, a service intention and a few KV entries.

```text
examples/platform/
├── norca-platform.hcl          startup configuration: cluster, tokens, limits  (NOT part of the tree, never committed)
└── platform/                   THE TREE (this is what lives in Git)
    ├── platform.hcl            tree manifest: format version, requires, graph, defaults
    ├── .gitattributes          Git must not convert line endings of values
    │
    ├── global/                 objects Norca replicates; applied once to the authoritative region
    │   ├── acl/policies/       developer.hcl            native Norca ACL policy
    │   ├── acl/roles/          developer.hcl            role = name + policies
    │   ├── quotas/             apps.hcl                 applied before the namespace that names it
    │   ├── namespaces/         apps.hcl
    │   └── node-pools/         general.hcl
    │
    ├── namespaces/apps/        everything one application team owns (CODEOWNERS per namespace)
    │   ├── references.hcl      read-only dependencies: the postgres pack release (packs stay with `packs`)
    │   ├── jobs/api/           api.norca.hcl (the job), unit.hcl (depends_on, tier), vars/default + vars/eu-1
    │   ├── jobs/worker/        a periodic batch job, no unit.hcl needed
    │   ├── variables/api/      config.hcl               a Variable (path api/config)
    │   ├── secrets/api/        db.hcl                   built-in secret store, `!generate:` password
    │   └── intentions/         api-to-db.hcl            service intention
    │
    ├── kv/                     Consul KV: file path = key, file content = value
    │   ├── _kv.hcl             managed prefixes and one inline entry
    │   └── app/config/db.json
    │
    └── regions/eu-1/           settings that exist only in one region
        └── scheduler.json
```

## What happens on `apply` (fixed order, then the graph)

1. `global/`: ACL policy and role, quota, namespace `apps`, node pool `general`
2. Variable, secret, KV entries (before the jobs that read them)
3. service intention
4. jobs, ordered by the unit graph: `api` waits until the reference `pack/postgres` is healthy
   (`pending`, not a failure, if it is not installed yet); `worker` has no dependency

## Things to try

```text
norca-deployer platform validate -d ./platform                  # offline checks, no cluster needed
norca-deployer platform graph    -d ./platform --format mermaid # dependency graph incl. the reference
norca-deployer platform plan     -d ./platform --config ./norca-platform.hcl
norca-deployer platform apply    -d ./platform --config ./norca-platform.hcl --dry-run
```

Notes: the commands are the design's target CLI (the feature is not implemented yet). Image names,
addresses and the `eu-1` region are placeholders. The tree contains no token and no address;
those are in `norca-platform.hcl`, which a commit can never widen.
