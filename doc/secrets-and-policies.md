# Bootstrap Secrets and ACL Policies

After the cluster is up, `apply` can create the ACL policies, the secrets and the Variables that the jobs of the cluster need, before it deploys the packs that read them. The order in the post-cluster stage is: ACL bootstrap, decommission, ACL policies, secrets, packs.

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
| `bootstrap[].namespace` | `default` | Namespace |
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

Generated values are created on first use and kept in `.norca-deployer/generated/<name>` and `.norca-deployer/nkeys/<name>`, encrypted when a master key exists. Every apply and every entry that names the same `<name>` gets the same value, so one value can be used in several places (a server token and the client that presents it), and you can read it from the state directory to hand it to a client. The generated and NKey forms can also be used in pack values; `@path` only in secret items.

## ACL policies

```yaml
security:
  acl:
    enabled: true
    auto_bootstrap: true
    policies:
      - name: natsbridge
        description: Event stream to NATS bridge
        rules_file: policies/natsbridge.hcl     # or rules: | inline
        job:                                    # optional: bind to a job
          namespace: default
          job_id: natsbridge
          # group: bridge
          # task: bridge
```

A policy with `job` is bound to the workload identity of that job (as `norca acl policy apply -job`), so the job needs no static token. The deployer writes a policy again only when its name, description, rules or binding changed. In a replica region the policies are not written: they replicate from the authoritative region.

## Example: NATS with the event bridge

The cluster example ([cluster.yaml](../examples/cluster.yaml)) deploys the `nats` pack of the Norca pack registry with user/password authentication and the event stream bridge:

1. `release.natsbridge: true` installs `norca-natsbridge` from the release archive on the clients.
2. The ACL policy `natsbridge` ([policies/natsbridge.hcl](../examples/policies/natsbridge.hcl)) is bound to the `natsbridge` job.
3. The secrets `nats/users/app` and `nats/users/ops` get generated passwords; `nats/users/natsbridge` gets the generated NKey seed of the bridge's user.
4. The pack values name the users and pass the public key of the bridge's NKey (`bridge_nkey: "!nkey-public:natsbridge"`).

The passwords of `app` and `ops` are in `.norca-deployer/generated/nats-app` and `nats-ops` for the clients that connect.
