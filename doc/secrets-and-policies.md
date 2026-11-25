# Bootstrap Secrets and ACL Policies

After the cluster is up, `apply` can create the ACL policies, the namespaces, the secrets and the Variables that the jobs of the cluster need, before it applies the Rigger stack and the GitOps bindings, whose releases read them. The order in the post-cluster stage is: ACL bootstrap, decommission, ACL policies, node pools, namespaces, secrets, node scaling Variables, the Rigger stack, GitOps bindings.

## Namespaces

```yaml
namespaces:
  - name: apps
    description: Applications of the teams
    meta:
      owner: platform
```

`apply` always creates the namespace **`norca-system`** (description `Norca operators`). The operator modules of the global registry (`loadbalancer-operator`, `nodescaler-operator`, `norca-acme`, `norca-dns`, `norca-provider`) run their jobs there by default and keep their token Variables and secrets there. They expect the namespace to exist and do not create it, so `apply` makes sure it is there before the secrets and the Rigger stack. Give rights in it to the operators of the cluster only: the jobs run with ACL policies bound to them, and the Variables hold provider tokens and the key of the node CA. The `namespaces` block adds more namespaces.

| Key | Default | Description |
| --- | --- | --- |
| `namespaces[].name` | | Letters, digits and `-`, up to 128 characters; each name once |
| `namespaces[].description` | | Up to 256 characters |
| `namespaces[].meta` | | Key/value pairs |

- **Missing:** the namespace is created.
- **Existing:** it is left as it is when its description and meta are the plan's. Otherwise `apply` sets them to the plan's and keeps the other settings of the namespace (quota, task driver capabilities, node pool settings), which you manage with `norca namespace apply`.
- **`norca-system` not in the plan:** it is only created, never changed: a description or meta set in the cluster stays. List it under `namespaces` to manage its description and meta from the plan.
- **Removed from the plan:** nothing happens. The deployer never deletes a namespace.
- **Changed meanwhile:** the write is a check-and-set against the namespace as `apply` read it, so a namespace that somebody else creates or changes between the read and the write is not overwritten: `apply` reads it again and decides again (keeping what the other writer set), and fails the step after three conflicting attempts in a row.

The deployer keeps a digest of each entry in `.norca-deployer/namespaces/` and looks a namespace up only when its entry in the plan changed since the last `apply`. With ACLs the calls use the management token; without a token the namespaces are skipped with a warning. In a replica region the namespaces are not written: they replicate from the authoritative region, so apply the authoritative region first.

## Secrets and Variables

```yaml
secrets:
  store: builtin                 # builtin (default) | external
  bootstrap:
    - path: nats/users/app       # a secret of the built-in secret store
      items:
        password: "!generate:nats-app"
    - type: variable             # a Variable
      path: nomad/jobs/nats
      namespace: default
      items:
        tls_ca: "@certs/nats-ca.pem"
        tls_cert: "@certs/nats.pem"
        tls_key: "@certs/nats-key.pem"
      overwrite: true
```

| Key | Default | Description |
| --- | --- | --- |
| `store` | `builtin` | `builtin`: the secrets go into Norca's built-in secret store. `external`: the jobs read their secrets from an external store (e.g. Vault), which must be set up before; the entries of type `secret` are skipped, Variables are still written |
| `bootstrap[].type` | `secret` | `secret` (built-in secret store, `/v1/secret/<path>`) or `variable` (`/v1/var/<path>`) |
| `bootstrap[].path` | | Path of the secret or Variable |
| `bootstrap[].namespace` | `default` | Namespace; the tokens and secrets the operator modules read belong to `norca-system` |
| `bootstrap[].items` | | Item names and values (see below) |
| `bootstrap[].overwrite` | `false` | See below |

**Create once (default).** The entry is written with check-and-set index 0: it is created when it does not exist, and an existing secret or Variable is never touched. A password rotated in the cluster is not reset by the next `apply`.

**`overwrite: true`.** A new version is written whenever the items in the plan changed since the last `apply` (the deployer keeps a digest of the written items in `.norca-deployer/secrets/`). Use it for values the plan owns, such as certificates.

With ACLs the writes use the management token; without a token the entries are skipped with a warning. Secrets and Variables are regional: each region's plan writes its own.

### Item values

| Form | Value |
| --- | --- |
| `plain text` | As written (avoid for real secrets: the plan is a file) |
| `!enc:v1:aes-gcm:...` | Encrypted with the master key (`norca-deployer secret encrypt`) |
| `@path` | Content of a file on the operator machine, relative to the plan |
| `!generate:<name>` | 32 random bytes, base64url (43 characters) |
| `!generate:<name>:<bytes>` | The same with 16 to 512 random bytes |
| `!nkey-seed:<name>` | Seed of a NATS user NKey pair (`SU...`) |
| `!nkey-public:<name>` | Public key of that pair (`U...`) |

Generated values are created on first use and kept in `.norca-deployer/generated/<name>` and `.norca-deployer/nkeys/<name>`, encrypted when a master key exists. Every apply and every entry that names the same `<name>` gets the same value, so one value can be used in several places (a server token and the client that presents it), and you can read it from the state directory to hand it to a client. The generated and NKey forms can also be used in the `values` of a GitOps binding; `@path` only in secret items.

## ACL policies

```yaml
security:
  acl:
    enabled: true
    auto_bootstrap: true
    policies:
      - name: events-reader
        description: Reads the event stream from the system bus
        rules_file: policies/events-reader.hcl  # or rules: | inline
        job:                                    # optional: bind to a job
          namespace: default
          job_id: audit
          # group: archive
          # task: archive
```

A policy with `job` is bound to the workload identity of that job (as `norca acl policy apply -job`), so the job needs no static token. The operator modules bind their policies to their jobs themselves; a policy for another job in `norca-system` names `namespace: norca-system`. The deployer writes a policy again only when its name, description, rules or binding changed. In a replica region the policies are not written: they replicate from the authoritative region.

## Example: NATS for the applications

The cluster example ([cluster.yaml](../examples/nodes/cluster.yaml)) installs the `nats` module of the global registry with user/password authentication, in the stack of its `rigger` section:

1. The secrets `nats/users/app` and `nats/users/ops` get generated passwords.
2. The values of the module name the users; the module reads their passwords from the secrets.

The passwords of `app` and `ops` are in `.norca-deployer/generated/nats-app` and `nats-ops` for the clients that connect.

The event stream of the region is not on this NATS but on the system bus of the servers; the ACL policy `events-reader` ([policies/events-reader.hcl](../examples/nodes/policies/events-reader.hcl)) lets a token read it.
