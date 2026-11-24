# Rigger stacks

[Rigger](https://github.com/bacanoo-io/norca/tree/main/rigger) is the package manager of Norca: modules written in HCL, deployed as a stack with a lock file of versions and digests. `apply` can apply one Rigger stack after the packs, so a new cluster comes with its base services from modules.

```yaml
rigger:
  stack: stacks/platform                       # directory of stack.hcl (and rigger.lock.hcl)
  values: [stacks/platform/values/prod.vars.hcl]
  vars: {count: "3"}                           # -var name=value
  prune: true                                  # destroy releases removed from the stack
  binary: /usr/local/bin/rigger                # default: rigger on the PATH
```

The same in HCL:

```hcl
rigger {
  stack  = "stacks/platform"
  values = ["stacks/platform/values/prod.vars.hcl"]
  vars   = { count = "3" }
  prune  = true
}
```

A short stack can be written in the plan with `stack_hcl` instead of `stack`:

```yaml
rigger:
  stack_hcl: |
    rigger {
      name      = "platform"
      namespace = "platform"
    }

    module "cache" {
      source  = "builtin/redis"
      version = "~> 0.3"
    }
```

## What happens

In the post-cluster stage, after the packs and before GitOps:

1. The deployer serves the HTTP API of the cluster on a local port (`127.0.0.1`, a random port). Each request goes to the first server through the `norca` CLI (`norca operator api`), with the management token, one at a time; the API does not have to be reachable from the operator machine.
2. With `stack_hcl`, the stack is written to `.norca-deployer/rigger-stack/stack.hcl` and `rigger stack sync` runs first, so its lock file lives in the local state and keeps its versions from one run to the next.
3. `rigger stack apply <dir> [-f values]... [-var name=value]... [-prune] -server=false` runs with `NORCA_ADDR` set to the local port. Its output is shown as it runs. The `NORCA_*` and `NOMAD_*` variables of the environment are not passed on, and `-server=false` keeps `RIGGER_SERVER` of the environment from sending the stack to the servers; the other `RIGGER_*` variables reach the program (credentials of sources, the cache directory, `RIGGER_VAR_<name>`).

Rigger decides itself what is unchanged: it compares the digest of each release with the one in the cluster, so the deployer keeps no marker for the stack. A non-zero exit code of rigger stops the apply.

With ACLs enabled and no management token, the stack is skipped with a warning.

## The region of the stack

The stack is applied in the region its `rigger` block names, and a stack that names none is of the region `global`: the deployer gives the program no region, and the program reads none from its environment. So unless the region of the plan (`metadata.region`) is `global`, name it in the stack:

```hcl
rigger {
  name   = "platform"
  region = "eu"
}
```

Without it the apply of a cluster of the region `eu` stops with `Unexpected response code: 500 (No path to region)`.

## The directory of the stack

`stack` is a directory with a `stack.hcl`, checked before the program runs, and the program gets it as an absolute path. The program takes a name that is no directory for a published stack (`stacks/platform` would be the stack `platform` of the namespace `stacks` in the global registry), so a directory that is missing stops the apply here.

## Notes

- Install `rigger` on the operator machine; the nodes do not need it.
- A stack in a directory (`stack`) uses its own `rigger.lock.hcl`; run `rigger stack sync` there and commit the lock before `apply`.
- Modules from Git or OCI are fetched by rigger on the operator machine; `builtin/<name>` modules ship inside rigger.
- `rigger stack destroy` is not run by the deployer; removing the `rigger` section leaves the releases in place.
