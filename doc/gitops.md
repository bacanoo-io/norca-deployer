# GitOps

`norca-deployer` deploys packs straight from Git repositories: once with `gitops run`, on every `apply`,
and on every push with the `gitops daemon` webhook listener. Repositories are read into memory and never
stored on disk.

## How a pack reaches the cluster

The servers render packs from cluster registries, so a fetched pack is:

1. fetched into memory, through the archive API of the provider or, failing that, with an in-memory clone,
2. uploaded with `PUT /v1/pack-upload` into a registry of the type `uploads` that belongs to the release
   (`gitops-<hash of namespace and release>`), which the upload creates,
3. deployed with `POST /v1/release/<name>/deploy-pack` from that registry.

The servers keep the archive in Raft, so a new leader can render the pack again. Only the latest upload of a
release stays.

- `metadata.hcl` must be at the root of the `subpath`, and its `pack` block needs a `name` and a `version`.
- A pack archive can have 8 MiB at most, and the pack 64 MiB and 4096 files in memory.
- The ACL token needs the rights of a pack registry write and of a release deployment in the namespace.
- An uploaded archive has no signature, so the servers don't verify it; [commit and tag
  signatures](#signed-commits-and-tags) are checked by the deployer before the upload.

The source of a release is in the description of its revision: provider, URL, branch or tag, and commit.

## Configuration

Bindings are under `packs.gitops` or a top-level `gitops`, in YAML or HCL. Nothing runs without
`enabled = true`.

```hcl
packs {
  gitops {
    enabled           = true
    log_buffer_size   = 500
    debounce_duration = "5s"

    binding "payment-api" {
      release_name = "payment-api"
      namespace    = "production"
      atomic       = true
      max_history  = 10

      forgejo {
        url         = "https://forgejo.internal/bacanoo/payment-pack.git"
        branch      = "main"
        subpath     = "packs/payment"
        auth_secret = "!enc:v1:aes-gcm:..."
        ca_cert_pem = "!file:certs/internal-ca.pem"
      }

      webhook {
        secret_key      = "!enc:v1:aes-gcm:..."
        events          = ["push"]
        filter_branches = ["main"]
      }

      values = {
        replicas = 5
      }
    }
  }
}
```

A binding has exactly one of `forgejo`, `gitea`, `github` and `gitlab`, with:

| Setting | |
| --- | --- |
| `url` | Repository URL, http(s) or SSH. Required. |
| `branch`, `ref` | What to fetch: a branch, or any revision. |
| `subpath` | The directory of the pack. |
| `auth_secret` | Token of the provider. |
| `ssh_private_key`, `known_hosts` | Deploy key of an SSH URL. Without `known_hosts`, those of the user apply. |
| `ca_cert_pem`, `insecure_skip_verify` | TLS of the provider. |
| `api_url` (`github`) | API of a GitHub Enterprise Server; by default `https://<host>/api/v3`. |
| `project_path` (`gitlab`) | Project, if it differs from the path of the URL. |

`!enc:` values are decrypted with the master key, and a value that can't be is an error. `!file:` reads a file
relative to the plan, for `ca_cert_pem`, `ssh_private_key`, `known_hosts`, `auth_secret` and `secret_key`.

## Signed commits and tags

A `verify` block deploys a pack only from a commit or tag signed with one of its keys:

```hcl
binding "payment-api" {
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
- Only the deployed commit is checked, not its history: an unsigned ancestor is accepted.
- An archive has no signatures, so a verified binding always clones, which takes longer and more memory.
- A failure stops the deployment, on `apply`, `gitops run` and in the daemon, where it shows in the logs; a success
  is the `VERIFY` phase of the logs.
- The check runs in the deployer: the servers don't know about it, and a token with upload rights can bypass it.

`gitops run` takes `--verify-key-file` (repeatable), and `--no-verify` to skip the check of a binding once.

## On apply

`apply` fetches every binding after the packs of the plan, and deploys when the pack or the settings changed
since the last deployment. It runs through the CLI of the first server, which must run Linux.

## gitops run

```shell
norca-deployer gitops run \
  --provider forgejo --url https://forgejo.internal/bacanoo/payment-pack.git \
  --branch main --subpath packs/payment --token-env GIT_TOKEN \
  --name payment-api --namespace production --var replicas=5
```

Without `--url` it runs a binding of the plan (`--binding`, by default the first), and the flags override it.
`--tag`, `--ref` and `--commit` select other revisions. `--ssh-key-file`, `--known-hosts-file`, `--ca-cert`,
`--insecure-skip-tls-verify` and `--api-url` match the settings above.

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
  `/`. The repository of the event must have the path of the binding, and pings and deleted refs deploy nothing.
- **Debounce.** Pushes within `debounce_duration` (5s) become one run of the latest. A push during a run waits for
  it. Runs of a release are serialized.
- **What deploys.** The pushed commit, or the tag.
- **Logs and status.** `GET /v1/gitops/logs` and `/v1/gitops/status` need `Authorization: Bearer <token>` with the
  token of `NORCA_GITOPS_API_TOKEN` (`--api-token-env`). Without one they answer on the loopback interface only.
  Behind a reverse proxy on the same host set a token: every client looks local.

```shell
norca-deployer gitops logs payment-api --tail 50
norca-deployer gitops logs payment-api -f
```

The logs are in memory (`log_buffer_size` entries, shared by the bindings) and are lost when the daemon stops, as
are pending events. The daemon runs where you start it; it isn't installed into the cluster by `apply`.
