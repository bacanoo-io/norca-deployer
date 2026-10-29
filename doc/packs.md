# Packs

Packs are parameterized application bundles of Norca. `apply` can register a pack registry in the cluster and install pack releases after the cluster is up, so a new cluster comes with its base services.

With one registry, `packs.registry` is enough; with several, list them under `packs.registries` and name the registry of each release:

```yaml
packs:
  registries:
    - {name: builtin, type: dir, url: builtin}
    - {name: norca, type: git, url: https://github.com/bacanoo-io/norca-registry, ref: v0.1.0}
  deploy:
    - {name: cache, registry: builtin, pack: redis, enabled: true}
    - {name: nats, registry: norca, pack: nats, enabled: true}
```

String values may be generated values (`!generate:<name>`, `!nkey-public:<name>`, see [Bootstrap secrets](secrets-and-policies.md#item-values)) or `!enc:` secrets; they are resolved before the deploy. Secrets a pack reads from the secret store or from Variables are written before the packs (`secrets.bootstrap`).

A single registry:

```yaml
packs:
  registry:
    name: builtin           # registry name in the cluster (lower case, digits, '-')
    type: dir               # git (default) | oci | dir
    url: builtin            # git URL, OCI reference, directory on the servers, or "builtin"
    ref: latest             # tag or branch (default "latest")
    sync_interval: 1h       # optional periodic sync
  deploy:
    - name: cache           # release name
      pack: redis           # pack in the registry
      version: ""           # optional pack version
      enabled: true         # false: keep the entry without deploying it
      values:               # pack variables
        datacenters: [dc1]
        count: 1
```

## What happens

In the post-cluster stage, through the first server:

1. `POST /v1/pack-registry` creates or updates each registry, and `POST /v1/pack-registry/<name>/sync` syncs it.
2. For each enabled entry, `POST /v1/release/<name>/deploy-pack` renders and deploys the pack on the server side and waits until the deployment is healthy (10 minutes at most).
3. The digest of the request (registry, pack, version, values) is stored in `.norca-deployer/packs/<name>.sha256`. On later runs an unchanged entry is skipped:

   ```text
       Pack registry "builtin" synced
       Pack cache unchanged (skipped)
   ```

With ACLs enabled the calls use the management token. Without a token (bootstrap disabled and no token file) the packs are skipped with a warning.

## The built-in registry

`type: dir` with `url: builtin` uses the packs that ship with Norca, so no network access to a registry is needed: `redis`, `postgres`, `test-app` and `loadbalancer-operator`. Most of them use the Docker driver, so install Docker on the clients (see [host packages](host-packages-and-scripts.md)).

## Notes

- Removing an entry from `deploy` does not uninstall the release; use the Norca CLI or UI for that.
- Secrets of a pack (e.g. a database password) should not be put in `values` as plain text. Write them with `secrets.bootstrap` (generated or encrypted), or set them in the Norca UI or CLI.
