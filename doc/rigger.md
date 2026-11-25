# Rigger stacks

[Rigger](https://github.com/bacanoo-io/norca/tree/main/rigger) is the package manager of Norca: modules written in HCL, deployed as a stack with a lock file of versions and digests. `apply` can apply one Rigger stack after the cluster is up, so a new cluster comes with its base services from modules.

```yaml
rigger:
  stack_dir: stacks/platform                   # directory of stack.hcl (and rigger.lock.hcl)
  values: [stacks/platform/values/prod.vars.hcl]
  vars: {count: "3"}                           # -var name=value
  prune: true                                  # destroy releases removed from the stack
  run_on: server                               # server (default) | local
  mode: embedded                               # embedded (default) | standalone
  sync_registries: [global]                    # registries the servers sync before the apply
```

The same in HCL:

```hcl
rigger {
  stack_dir = "stacks/platform"
  values    = ["stacks/platform/values/prod.vars.hcl"]
  vars      = { count = "3" }
  prune     = true
}
```

A short stack can be written in the plan with `stack` instead of `stack_dir`: its value is the text of the `stack.hcl` (see [content and reference keys](configuration.md#content-and-reference-keys)).

```yaml
rigger:
  stack: |
    rigger {
      name      = "platform"
      namespace = "platform"
    }

    module "cache" {
      source  = "builtin/redis"
      version = "~> 0.3"
    }
```

In an HCL plan the stack is a `stack` block, whose body is the `stack.hcl`:

```hcl
rigger {
  stack {
    rigger {
      name      = "platform"
      namespace = "platform"
    }

    module "cache" {
      source  = "builtin/redis"
      version = "~> 0.3"
    }
  }
}
```

The body is taken as it is written, one level of indentation less: comments, heredocs, `${...}`, `$${...}` and `%{...}` reach Rigger unchanged. A heredoc string (`stack = <<EOT ... EOT`) works too. The block needs the whole plan to be HCL2 syntax; a plan that is not (for example a shell `${VAR:-default}` in a script written in the plan) gets an error that says where, and can give the stack as a heredoc string instead. The deployer's errors about the block name the plan file and its line; Rigger's own errors name the lines of the `stack.hcl` it was given (`.norca-deployer/rigger-stack/stack.hcl`).

`stack_dir` and `stack` exclude each other. Value files (`values`) belong to `stack_dir`; with `stack` give `vars`. A `stack` that looks like a directory (one line, without `{` or `=`) is refused: a directory is `stack_dir`.

The section also holds the GitOps bindings (`rigger.gitops`), which apply stacks of Git repositories; it may hold them alone, without a stack of the plan. See [GitOps](gitops.md).

## Where the program runs, and who applies

The stack is applied with the command `stack apply` of Rigger. Two settings choose how:

| `run_on` | The program | On the operator machine |
| --- | --- | --- |
| `server` (default) | `norca rigger` on the first server, on a copy of the stack | Nothing to install: the `norca` binary of the server has the commands of Rigger |
| `local` | The `rigger` program on the operator machine | `rigger` on the `PATH`, or `binary` names it |

| `mode` | Who applies the stack |
| --- | --- |
| `embedded` (default) | The servers. The program sends them the stack (`stack apply -server`); they fetch the modules, evaluate the stack and apply it as an operation, and the program follows its log to the end |
| `standalone` | The program. It fetches and evaluates the modules where it runs, and sends the resources of each release to the cluster (`stack apply -server=false`) |

The four combinations work. The deployer always gives `-server` or `-server=false`, so `RIGGER_SERVER` of an environment never chooses.

## Modules of a registry

With `embedded` the servers fetch the modules of the stack, and they fetch only from a registry they have synced: its modules are what it offered at the commit of its last sync. A new cluster has synced none, so a stack with a module of the global registry (`source = "norca/nats"`) stops there:

```text
Unexpected response code: 409 (the registry "global" has not been synced: sync it first (rigger registry sync global))
```

`sync_registries` names the registries the servers sync before the stack is applied, in the order of the list:

```yaml
rigger:
  stack_dir: stacks/platform
  sync_registries: [global]
```

- `global` is the global registry, which every cluster has. Another name is a registry of the cluster (an object of the kind `rigger.norca.io/Registry`), which must exist: the deployer does not create it.
- The program runs `registry sync <name>` for each name where it runs (`run_on`), against the region of the first server. A sync that fails stops the apply, before the stack is sent.
- The registries are synced at every apply, so the servers move to what a registry offers now. The versions of the stack do not move with it: its lock file names them.
- A stack of `builtin/` modules and of modules of its own directory needs no registry.
- With `standalone` the program fetches the modules itself, and the key is refused.

## What happens

In the post-cluster stage, after the secrets and the node scaling Variables and before GitOps.

With `run_on: server`:

1. The stack is copied to the first server, into `/var/lib/norca-deployer/rigger/`: every regular file of its directory, without the files and directories whose name starts with a dot (`.rigger/`, `.git/`, `.env`), and the value files. What an interrupted run left there is removed first. The files are private (`0600`); a stack of more than 4096 files or 64 MiB is refused.
2. With `stack`, the stack is the `stack.hcl` written to `.norca-deployer/rigger-stack/` on the operator machine. `norca rigger stack sync` runs on the copy first, and the lock file it writes comes back into `.norca-deployer/rigger-stack/`, so the versions stay from one run to the next.
3. `norca rigger stack apply <copy> [-f values]... [-var name=value]... [-prune] -server` runs on the first server like every `norca` command of the deployer: against the agent of that server, with the TLS files of the node and the management token, which is in a private file and not on the command line. The output is shown when the program has ended.
4. The copy is removed, after a failure too.

With `run_on: local`:

1. The deployer serves the HTTP API of the cluster on a local port (`127.0.0.1`, a random port). Each request goes to the first server through the `norca` CLI (`norca operator api`), with the management token, one at a time; the API does not have to be reachable from the operator machine.
2. With `stack`, the stack is written to `.norca-deployer/rigger-stack/stack.hcl` and `rigger stack sync` runs first, so its lock file lives in the local state and keeps its versions from one run to the next.
3. `rigger stack apply <dir> [-f values]... [-var name=value]... [-prune] -server` runs with `NORCA_ADDR` set to the local port. Its output is shown as it runs. The `NORCA_*` and `NOMAD_*` variables of the environment are not passed on; the `RIGGER_*` variables reach the program (credentials of sources, the cache directory, `RIGGER_VAR_<name>`).

Rigger decides itself what is unchanged: it compares the digest of each release with the one in the cluster, so the deployer keeps no marker for the stack. A failure of the program stops the apply.

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

- `stack_dir` is a directory with a `stack.hcl`, checked before the program runs, and the program gets it as an absolute path. The program takes a name that is no directory for a published stack (`stacks/platform` would be the stack `platform` of the namespace `stacks` in the global registry), so a directory that is missing stops the apply here.
- **The stack has a directory of its own.** All of its files are copied to the first server or sent to the servers, so a `stack_dir` that is the directory of the plan, or one that holds it, is refused: the keys of the cluster (`certs/`) are next to the plan.
- With `run_on: server`, the modules of the stack itself are in its directory (`source = "./modules/api"`). A module from beside the directory (`source = "../modules/api"`) is not copied, and the program stops with `Source outside the root`; apply such a stack with `run_on: local`.

## Notes

- **Secrets come first.** The secrets and Variables a module reads are written before the stack is applied: put them into `secrets.bootstrap` (generated or encrypted, see [Bootstrap secrets](secrets-and-policies.md)), not into the values of the stack as plain text. The namespace `norca-system` of the operator modules exists by then too.
- **Modules are fetched where the program runs**, also with `embedded`: the program loads the stack with the modules its lock file names before it sends it. With `run_on: server` that is the first server, which must reach the Git and OCI sources of the stack; its cache is the cache directory of the user the commands of the deployer run as there (`~/.cache/rigger`). With `embedded` the servers fetch the modules too. `builtin/<name>` modules ship inside Norca and Rigger.
- A stack in a directory (`stack_dir`) uses its own `rigger.lock.hcl`; run `rigger stack sync` (or `norca rigger stack sync`) there and commit the lock before `apply`.
- `vars` are arguments of the program, visible in the process list of the machine it runs on: no secrets in them. A value file is copied to the first server as a private file and removed with the copy.
- A Windows first server takes no copy of a stack: use `run_on: local`.
- With `run_on: server` the environment of the operator machine does not reach the program (`RIGGER_VAR_<name>`, credentials of sources).
- `stack destroy` is not run by the deployer; removing the `rigger` section leaves the releases in place.
