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
3. `rigger stack apply <dir> [-f values]... [-var name=value]... [-prune]` runs with `NORCA_ADDR` set to the local port and `NORCA_REGION` to the region of the plan. Its output is shown as it runs. Other `NORCA_*` and `NOMAD_*` variables of the environment are not passed on.

Rigger decides itself what is unchanged: it compares the digest of each release with the one in the cluster, so the deployer keeps no marker for the stack. A non-zero exit code of rigger stops the apply.

With ACLs enabled and no management token, the stack is skipped with a warning.

## Notes

- Install `rigger` on the operator machine; the nodes do not need it.
- A stack in a directory (`stack`) uses its own `rigger.lock.hcl`; run `rigger stack sync` there and commit the lock before `apply`.
- Modules from Git or OCI are fetched by rigger on the operator machine; `builtin/<name>` modules ship inside rigger.
- `rigger stack destroy` is not run by the deployer; removing the `rigger` section leaves the releases in place.
