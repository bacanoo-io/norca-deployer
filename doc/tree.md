# Platform tree (experimental)

`norca-deployer tree` manages the objects of a running Norca platform from a directory tree, usually a Git
repository: namespaces, quotas, ACL policies, admission policies, object kinds, jobs, Variables, secrets,
external services, objects, and Rigger stacks written by hand. The tree is applied as [Rigger](rigger.md)
stacks: the deployer turns each part of the tree into a stack and sends it to the servers, and the release
engine of the servers applies it, owns what it created, and finds drift. The deployer keeps no state.

The commands are experimental and hidden. They run only when the environment variable
`NORCA_DEPLOYER_EXPERIMENTAL` names them:

```sh
export NORCA_DEPLOYER_EXPERIMENTAL=tree
norca-deployer tree init -d cluster-config
norca-deployer tree validate -d cluster-config
norca-deployer tree plan -d cluster-config
norca-deployer tree apply -d cluster-config
```

## The layout

```text
cluster-config/
├── tree.hcl                      # the manifest: name, format version, stack order
├── global/                       # cluster-wide objects: the stack tree-global
│   ├── namespaces/apps.namespace.hcl
│   ├── quotas/apps.quota.hcl
│   ├── acl/policies/developer.acl.hcl
│   ├── admission-policies/no-latest.admission.hcl
│   └── object-kinds/database.kind.hcl
├── namespaces/                   # one directory per namespace: the stack tree-ns-<ns>
│   └── apps/
│       ├── jobs/api.norca.hcl
│       ├── variables/api-config.var.hcl
│       ├── services/billing.external.hcl
│       ├── objects/orders-db.object.hcl
│       └── secrets/api/db.hcl    # a secret of the stack: items, never in plain text
├── stacks/                       # Rigger stacks written by hand
│   └── data/
│       ├── stack.hcl
│       ├── variables.hcl
│       └── values/default.vars.hcl
└── modules/                      # local Rigger modules the written stacks share
    └── postgres/
```

- **The extension decides the kind**, the directory only sorts: `.norca.hcl` job, `.namespace.hcl` namespace,
  `.quota.hcl` quota, `.acl.hcl` ACL policy, `.var.hcl` Variable, `.kind.hcl` object kind, `.admission.hcl`
  admission policy, `.external.hcl` external service, `.object.hcl` object. `global/` takes namespaces,
  quotas, ACL policies, admission policies and object kinds; `namespaces/<ns>/` takes jobs, Variables, external
  services and objects. A file of another kind is refused.
- One object per file. Files and directories whose names start with `_` or `.` are left out (drafts).
- A namespace directory name is lower case letters, digits and `-`, at most 58 characters.
- The paths are checked before any file is read: no symbolic link, no two paths that differ only by case, no
  character or name Windows refuses (`<>:"|?*`, a trailing dot or space, `CON`, `NUL`, ...), at most 240
  characters.
- Not supported yet, and refused rather than ignored: ACL roles, auth methods and binding rules, node pools,
  network policies, service intentions, CSI volumes, Consul KV (`kv/`), the `layers` of the manifest and the
  file `_ignore.hcl`. Each comes when Rigger has a resource kind for it.

## The manifest

```hcl
tree {
  name        = "acme-prod"
  version     = "norca.io/tree/v1alpha1"
  environment = "prod" # optional: shown in the plan and its JSON, changes nothing
}

# Stacks applied right after tree-global, in this order; the others follow by name.
stack_order = ["tree-ns-platform", "data"]
```

`tree.hcl` (or `tree.yaml`) holds only what the files mean. How the program reaches the cluster and what it
may do is in the startup configuration, outside the tree, so that a commit can never widen it.
`tree.namespace` is the namespace of the stack `tree-global` and can only be `default` for now.

## The startup configuration

The tree commands read `--config FILE`, else the file `NORCA_DEPLOYER_PLATFORM_CONFIG` names, else
`./norca-tree.hcl` when it exists. Flags win over the file. `tree init` writes a commented template with mode
`0600`. A configuration inside the tree, or one that others may read, is warned about.

```hcl
region "eu-1" {
  address   = "https://10.0.0.11:4646"
  token_env = "NORCA_TOKEN" # or token_file, or token (which may be encrypted, !enc:...)
  ca_cert   = "certs/ca.pem"

  # Values of the written stacks, over their values/default.vars.hcl.
  values = {
    replicas = 3
  }

  # A token used only for the stacks it names (names or patterns of path.Match).
  scoped_token "apps" {
    token_env = "NORCA_TOKEN_APPS"
    stacks    = ["tree-ns-app-*"]
  }

  # A token for the reads of plans and statuses.
  scoped_token "readonly" {
    token_env = "NORCA_TOKEN_READ"
    for       = ["plan"]
  }
}

# Registries the servers sync before the plans.
sync_registries = ["global"]

values {
  key_file = "tree.key" # the tree's own key for !enc: values
  dir      = "values"   # where @path values are read from
  env      = ["APP_*"]  # the environment variables !env: values may read
}

limits {
  kinds       = ["foundation", "job"] # the kinds a run may change
  prune       = false                 # refuse --prune
  max_changes = 50                    # a larger run needs --yes-really
}
```

- **Several regions** mean one `region` block each, and one run per region (`--region`). With one block,
  `--region` can be left out.
- **Tokens.** A stack is planned and applied with the most specific scoped token that names it (a name wins
  over a pattern), else with the token of its region. A plan uses the `for = ["plan"]` token when there is
  one. `--norca-token-env` overrides them all. Without any, `NORCA_TOKEN` is used.
- **The key** of the tree's encrypted values is never the master key of the node plan: a value encrypted for
  the plan does not decrypt in the tree. An encrypted `token` of the configuration is the operator's
  credential and is decrypted with the master key (`--secret-key`, `--secret-key-file`).

## How the tree is applied

| Part of the tree | Becomes | Namespace of the stack | Releases |
| --- | --- | --- | --- |
| `global/` | the stack `tree-global` | `default` | one release `tree-global` |
| `namespaces/<ns>/` | the stack `tree-ns-<ns>` | `<ns>` | one release `tree-<ns>` |
| `stacks/<name>/` | that stack, as written | its `rigger` block | its module blocks |
| `modules/<name>/` | copied into each written stack that uses it | – | – |

- **Order.** `tree-global` first, then the stacks of `stack_order`, then the other `tree-ns-*` stacks by name,
  then the other written stacks by name. One stack at a time: the first stack that fails stops the run.
- **Generated stacks.** The files of a directory become one module; the deployer writes its `stack.hcl` and
  `metadata.hcl`. The region of the stack is the region of the run.
- **Written stacks** are sent as they are. The directory name is the stack's name and must be the `name` of its
  `rigger` block. Its namespace is the block's `namespace` (default `default`), its region the block's `region`
  (default `global`), which must be the region of the run. A module used as `source = "./modules/<name>"` that
  the stack does not have is copied from the tree's `modules/`; a module in both places is an error.
- **Values of a written stack** are its `values/default.vars.hcl`, then the `values` of the region block for
  the variables the stack declares; a later source wins per variable. A value of the region that no written
  stack declares is warned about.
- **Reserved names.** The stacks `tree-global` and `tree-ns-*` and the releases `tree-*` are the deployer's;
  a written stack that uses one is refused.
- **Prune is the servers'.** Every release names its stack as its owner. A file removed from `namespaces/apps/`
  is a resource removed from the release `tree-apps`, deleted by the next apply with `--prune`. A namespace
  directory removed altogether leaves its release behind: the plan shows it as `remove`, and an apply with
  `--prune` uninstalls it.

## Secrets and value forms

A file below `namespaces/<ns>/secrets/` is a secret of the stack of that namespace, its path below `secrets/`
the path of the secret:

```hcl
# namespaces/apps/secrets/api/db.hcl
items = {
  user     = "api"
  password = "!enc:v1:aes-gcm:..." # decrypted with values.key_file
  ca       = "@certs/ca.pem"       # read from values.dir
  token    = "!env:APP_DB_TOKEN"   # an allowed environment variable
  session  = "!generate:session"   # made once by the servers and kept
}

overwrite = false
```

A resolved value travels only in the values of the request, as a sensitive variable of the stack; it is never
written to the files of the stack, and it is removed from the errors the servers answer. The same forms work in the
values of written stacks.

## Guards and limits

Every stack is planned before any is applied. Nothing is applied when the run is **blocked**:

- the delete of a resource needs `--prune`; the delete of a job (which purges it) also `--prune-jobs`, and of
  a Variable also `--prune-data`. This holds for the resources of a release that is removed as a whole,
- the namespace `default` and the ACL policy `anonymous` are never deleted,
- a stack whose plan changes a kind outside `limits.kinds` or `--kind` (kind identifiers, or the groups
  `acl`, `data`, `foundation`, `all`; an unknown word is an error),
- more changes than `limits.max_changes` (default 50), unless `--yes-really`,
- `--prune` when `limits.prune = false`.

The plan the apply of each stack is made of passes the same guards again.

## Commands

| Command | What it does |
| --- | --- |
| `tree init [-d DIR]` | writes `tree.hcl`, `README.md`, `.gitattributes`, `.gitignore`, and a startup configuration template |
| `tree validate [--rigger] [--format json]` | checks the tree offline: paths, names, kinds, manifest, written stacks, value forms, HCL syntax; `--rigger` runs the installed `rigger stack validate` on each built stack |
| `tree build --out DIR` | writes the stacks the tree is applied as, to read or to validate |
| `tree plan [--detailed-exitcode] [--format json]` | the servers' plan of every stack and what is blocked |
| `tree apply [--yes] [--prune] [--prune-jobs] [--prune-data] [--yes-really]` | plans, shows the plans, asks, and applies the stacks in order |
| `tree status [--format json]` | the releases of each stack, their revision, status and drift, its last operation, and orphan releases |

`--stack` and `--namespace` narrow `plan`, `apply` and `status` to whole stacks; the order of the tree is kept.

**Exit codes**

| Code | Meaning |
| --- | --- |
| 0 | done; with `plan --detailed-exitcode`: nothing to change |
| 1 | an error |
| 2 | a command line the program could not use; with `plan --detailed-exitcode`: changes pending |
| 3 | the run is blocked by a guard |

`--format json` writes the plan as `norca.io/tree/plan/v1`, the status as `norca.io/tree/status/v1` and the
result of `validate` as `norca.io/tree/validate/v1`. In CI: `tree validate` and
`tree plan --detailed-exitcode --format json` on every pull request.

## Tokens on the servers

The servers check the request of each stack in its namespace and write each release with the caller's token.
A token needs object `write` on `rigger.norca.io/Release` and `read` on `rigger.norca.io/Operation` in every
namespace its stacks write to; `tree-global` writes namespaces and ACL policies, which needs a management
token. A plan or a status needs `read` on `rigger.norca.io/Release`. A written stack with modules of a
registry also needs `read` on `rigger.norca.io/Registry`.

## Not yet

A tree binding of the GitOps controller (applied on every push), `tree import` and adoption, `tree watch`,
saved plans, rollback across stacks, the version preflight of `requires`, and the kinds listed above as not
supported.
