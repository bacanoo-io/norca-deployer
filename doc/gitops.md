# GitOps

`norca-deployer` applies [Rigger](rigger.md) stacks straight from Git repositories: once with `gitops run`, on
every `apply`, and on every push with the `gitops daemon` webhook listener. Repositories are read into memory
and never stored on disk.

## How a stack reaches the cluster

A binding ties one stack directory of a repository to a namespace and a region. A run of a binding:

1. fetches the directory into memory, through the archive API of the provider or, failing that, with an
   in-memory clone,
2. checks the [signature](#signed-commits-and-tags) of the commit or tag, when the binding has a `verify` block,
3. sends the files of the directory to the servers with `PUT /v1/rigger/stack/plan`. The servers evaluate the
   stack and answer what an apply would do to each release,
4. stops when the plan has nothing to do (the stack is **in sync**), or when the binding
   [only plans](#policy),
5. otherwise starts the apply with `PUT /v1/rigger/stack/apply`. The servers run it as an operation (an object
   of the kind `rigger.norca.io/Operation`); the run passes on the log of the operation and waits for its end.

- `stack.hcl` must be at the root of the `subpath`. Only that directory is sent, so the modules of the stack's
  own (`./modules/...`) must be inside it. Modules of a registry are fetched by the servers, from the registries
  they have synced (see [Rigger stacks](rigger.md#modules-of-a-registry)).
- Files and directories whose name starts with a dot (`.env`, `.rigger/`) are not sent.
- A stack directory can have 4096 files and 64 MiB at most, and 16 MiB compressed.
- The `rigger` block of the stack names its namespace and its region. The `namespace` and the `region` of the
  binding are where the request is sent, and must be the same: the servers refuse a stack of another namespace
  or region. A stack without a region is of `global`.
- The servers compare the stack with the releases of the cluster, so "in sync" does not depend on the machine
  that runs the deployer, and the deployer keeps no state of a binding.
- The ACL token needs object `write` on `rigger.norca.io/Release` in the namespace, and object `read` on
  `rigger.norca.io/Operation` to follow the operation. A stack with modules of a registry also needs object
  `read` on `rigger.norca.io/Registry`.

**Where a stack came from** is recorded by the servers: the operation has the repository (without credentials),
the branch or tag, the commit and, when the signature was checked, who signed; the releases of the stack's own
modules have the commit.

**How a run ends:**

| The operation ends | The run |
| --- | --- |
| `Succeeded` | succeeds |
| `Failed`, `Cancelled` | fails, with the error of the operation |
| `Interrupted` (the leader of the servers changed) | applies again at once, three operations at most; the releases already done come back unchanged |

- **One operation of a stack runs at a time.** While another one runs (the servers answer 409 and name it) the
  run waits, plans again every 5 seconds, and fails after 10 minutes.
- A stack with a module of a registry that the servers have never synced is refused, and the run fails at once:
  sync the registry first (`rigger registry sync <name>`). The deployer syncs no registry for a binding.
- A read of the log that fails is tried again after 2 seconds: a change of leader fails some. The tenth read
  in a row that fails ends the run.
- The records of the log below `INFO` are not passed on. The servers keep the full log:
  `rigger operation follow <name>`.
- The wait for the end of an operation has no time limit of its own. Stopping the deployer leaves a running
  operation running; `rigger operation cancel <name>` stops it.
- A release that another stack owns or another tool applied is not taken over: the run fails and names it.

## Configuration

Bindings are under `rigger.gitops` or a top-level `gitops`, in YAML or HCL. Nothing runs without
`enabled = true`. A `rigger` section may hold bindings only, without a stack of the plan.

```hcl
rigger {
  gitops {
    enabled           = true
    log_buffer_size   = 500
    debounce_duration = "5s"

    binding "platform" {
      namespace = "platform"
      region    = "eu-1"

      forgejo {
        url         = "https://forgejo.internal/acme/platform-env.git"
        branch      = "main"
        subpath     = "stacks/platform"
        auth_secret = "!enc:v1:aes-gcm:..."
        ca_cert_pem = "!file:certs/internal-ca.pem"
      }

      verify {
        public_keys = ["!file:keys/ops.asc"]
      }

      webhook {
        secret_key      = "!enc:v1:aes-gcm:..."
        events          = ["push"]
        filter_branches = ["main"]
      }

      policy {
        mode  = "apply"
        prune = false
      }

      values = {
        replicas = 5
      }
    }
  }
}
```

| Setting of a binding | |
| --- | --- |
| `namespace`, `region` | Where the stack is sent: those of the `rigger` block of its `stack.hcl`. Without them the `default` namespace, and the region of the server that is asked. |
| `values` | Values of the variables of the stack. |
| `forgejo`, `gitea`, `github`, `gitlab` | The repository; exactly one of them. |
| `verify` | The keys a commit or tag must be signed with; see [below](#signed-commits-and-tags). |
| `webhook` | The pushes that run the binding in the [daemon](#gitops-daemon). |
| `policy` | What a run does; see [below](#policy). |

A stack names its releases, and sets `atomic` and `max_history` for each module, in its `stack.hcl`: a binding
has no key for them. A key that a binding or its `policy` does not have is refused.

The repository block has:

| Setting | |
| --- | --- |
| `url` | Repository URL, http(s) or SSH. Required. |
| `branch`, `ref` | What to fetch: a branch, or any revision. |
| `subpath` | The directory of the stack. |
| `auth_secret` | Token of the provider. |
| `ssh_private_key`, `known_hosts` | Deploy key of an SSH URL. Without `known_hosts`, those of the user apply. |
| `ca_cert_pem`, `insecure_skip_verify` | TLS of the provider. |
| `api_url` (`github`) | API of a GitHub Enterprise Server; by default `https://<host>/api/v3`. |
| `project_path` (`gitlab`) | Project, if it differs from the path of the URL. |

`!enc:` values are decrypted with the master key, and a value that can't be is an error. `!file:` reads a file
relative to the plan, for `ca_cert_pem`, `ssh_private_key`, `known_hosts`, `auth_secret` and `secret_key`.

## Policy

```hcl
policy {
  mode  = "plan-only"   # apply (default) | plan-only
  prune = true          # default false
}
```

- `mode = "apply"` has the servers apply the stack. `mode = "plan-only"` stops after the plan and logs it: the
  releases to install, upgrade and remove, the secrets and the generated values.
- `prune = true` uninstalls the releases the stack owns and no longer has. Without it they stay, and the plan
  says so; a stack that differs from the cluster only by such releases is in sync.

**A binding with `mode = "apply"` needs a `verify` block.** The servers evaluate a stack in their own process:
a stack that exhausts its memory ends the leader, and applying on every push would put that within reach of
everyone who can push. So the servers are sent to apply only what a signed commit or tag vouches for. A
binding with `plan-only` needs no `verify`.

## Signed commits and tags

A `verify` block runs a stack only from a commit or tag signed with one of its keys:

```hcl
binding "platform" {
  # ...
  verify {
    public_keys = ["!file:keys/release.asc", "!file:keys/ci.asc"]
    # enabled = false turns the check off and keeps the keys.
  }
}
```

- `public_keys` are armored PGP public keys, inline or with `!file:`. PGP signatures only: commits signed with SSH
  keys or X.509 certificates are refused.
- A branch or commit needs a signed commit. A tag needs a signed annotated tag that points at its commit, or a signed
  commit.
- Only the fetched commit is checked, not its history: an unsigned ancestor is accepted.
- An archive has no signatures, so a verified binding always clones, which takes longer and more memory.
- A failure stops the run, on `apply`, `gitops run` and in the daemon, where it shows in the logs; a success
  is the `VERIFY` phase of the logs.
- The check runs in the deployer. The servers record who signed as the deployer says and verify nothing of it:
  a token with the right to write releases can send them any stack.

`gitops run` takes `--verify-key-file` (repeatable), and `--no-verify` to skip the check of a binding once,
which `--mode plan-only` allows.

## On apply

`apply` runs every binding after the stack of the plan: it fetches the stack, the servers plan
it, and a stack that is not in sync is applied (or, with `plan-only`, its plan is shown). It runs through the
CLI of the first server, which must run Linux.

## gitops run

```shell
norca-deployer gitops run \
  --provider forgejo --url https://forgejo.internal/acme/platform-env.git \
  --branch main --subpath stacks/platform --token-env GIT_TOKEN \
  --verify-key-file keys/ops.asc \
  --namespace platform --var replicas=5
```

Without `--url` it runs a binding of the plan (`--binding`, by default the first), and the flags override it.
`--tag`, `--ref` and `--commit` select other revisions. `--ssh-key-file`, `--known-hosts-file`, `--ca-cert`,
`--insecure-skip-tls-verify` and `--api-url` match the settings above.

- `--mode apply` or `--mode plan-only`, and `--prune`, are the [policy](#policy) of the run. A run that applies
  needs `--verify-key-file`, or the `verify` block of its binding.
- `--var name=value` gives a variable of the stack as text; the servers convert it to the type of the variable.

The cluster comes from `--server` or `NORCA_ADDR`, its token from `NORCA_TOKEN` (`--norca-token-env`), its CA from
`--norca-ca-cert` or `NORCA_CACERT`.

## gitops daemon

```shell
NORCA_GITOPS_API_TOKEN=... norca-deployer gitops daemon --addr :9095 \
  --tls-cert daemon.pem --tls-key daemon-key.pem
```

Point the webhook of the provider at `POST /v1/gitops/webhook`. A shared URL finds the bindings by their secret;
`?binding=<name>` names one.

- **Authentication.** Each binding has its `secret_key`, and the provider of the signature must fit the binding.
  Secrets don't leave the memory of the daemon.
- **Filters.** `events` defaults to `push`; `release` counts published releases. A branch push needs
  `filter_branches`, or the `branch` of the binding (by default `main`). A tag needs `filter_tags`. `*` stops at a
  `/`. The repository of the event must have the path of the binding, and pings and deleted refs run nothing.
- **Debounce.** Pushes within `debounce_duration` (5s) become one run of the latest. A push during a run waits for
  it. Runs of a stack directory are serialized.
- **What runs.** The pushed commit, or the tag.
- **Logs and status.** `GET /v1/gitops/logs` and `/v1/gitops/status` need `Authorization: Bearer <token>` with the
  token of `NORCA_GITOPS_API_TOKEN` (`--api-token-env`). Without one they answer on the loopback interface only.
  Behind a reverse proxy on the same host set a token: every client looks local.

```shell
norca-deployer gitops logs platform --tail 50
norca-deployer gitops logs platform -f
```

An entry of the log names the binding, the commit and, once the run knows them, the stack and the operation.
Its phases are `TRIGGER`, `FETCH`, `VERIFY`, `PLAN`, `APPLY`, `OPERATION` (a record of the log of the operation)
and `SUCCESS` or `FAILED`.

The logs are in memory (`log_buffer_size` entries, shared by the bindings) and are lost when the daemon stops, as
are pending events. The daemon runs where you start it; it isn't installed into the cluster by `apply`.
