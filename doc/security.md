# Security

## TLS and the internal CA

With `security.tls_enabled: true`, HTTP and RPC use mutual TLS.

**CA selection, in this order:**
1. `security.pki.ca_cert_path` and `ca_key_path` (your own CA). If they cannot be loaded, `apply` fails rather than create a second CA.
2. `certs/norca-ca.pem` from an earlier `apply`.
3. A new CA, valid for 10 years, written to `certs/` (key mode 0600).

**The CA key at rest.** The key of a CA the deployer generated is stored **encrypted with the master key** (see [Secrets in the plan](#secrets-in-the-plan)) when one exists, as the gossip key and the management token are. Without a master key it is stored in plain text and `apply` warns; a master key that appears later encrypts the existing key in place on the next `apply`, and the CA stays the same. A key file you name in `security.pki.ca_key_path` is yours and is never rewritten (an encrypted one is decrypted with the master key). An encrypted key without the master key is an error; no second CA is generated.

**Node certificates:**

| Role | Common name | DNS SANs | IP SANs |
| --- | --- | --- | --- |
| Server | `server.<region>.norca` | `server.<region>.norca`, `*.server.<region>.norca`, `*.<region>.norca`, `localhost`, `*.norca.internal`, `pki.server_additional_dns_sans` | `host`, `cluster_address`, `127.0.0.1`, `pki.server_additional_ip_sans` |
| Client | `client.<region>.norca` | `client.<region>.norca`, `*.client.<region>.norca`, `localhost` | `127.0.0.1` |

Node certificates are valid for 3 years.

Norca checks the role names, not the peer IP addresses (`verify_server_hostname: true`). A client certificate is therefore independent of the node address. With `pki.generate_worker_pool_cert: true`, `certs/client-pool.pem` is issued once for clients that join outside the deployer.

An existing certificate on a node is kept while it is signed by the CA, carries the expected names (and exactly the expected SPIFFE ID) and is valid for more than 30 days. Otherwise it is re-issued and the agent restarts.

**HTTP client certificates:** `security.verify_https_client: true` makes the HTTP API require a client certificate. The deployer then issues `certs/cli.pem` for operators.

## SPIFFE IDs

With `security.pki.spiffe.enabled: true` every certificate the deployer issues carries a SPIFFE ID as URI SAN. SPIFFE consumers, OpenZiti in particular, identify the nodes by it and take the trust domain from the certificate chain.

```yaml
security:
  pki:
    spiffe:
      enabled: true
      trust_domain: prod.example.com   # default "<metadata.name>.norca"
```

| Certificate | SPIFFE ID |
| --- | --- |
| Server | `spiffe://<trust_domain>/norca/<region>/server/<hostname>` |
| Client | `spiffe://<trust_domain>/norca/<region>/client/<hostname>` |
| `certs/client-pool.pem` | `spiffe://<trust_domain>/norca/<region>/client-pool` |
| `certs/cli.pem` | `spiffe://<trust_domain>/norca/<region>/operator` |
| A CA generated with SPIFFE on, and intermediates | `spiffe://<trust_domain>` |

- Every certificate carries at most one SPIFFE ID, as SPIFFE and OpenZiti require. The `/norca/...` paths do not collide with the IDs of Norca workloads (`/ns/<namespace>/job/...`) or of OpenZiti components (`/controller/<id>`, `/router/<id>`).
- Turning SPIFFE on, or changing the trust domain, re-issues the node certificates; the agents restart one at a time as for any certificate change.
- An existing CA keeps its subject: a CA from before SPIFFE was enabled has no SPIFFE ID. That is fine for OpenZiti, which searches the chain from the leaf up and finds the trust domain in the leaf or intermediate certificate. A CA with a different trust domain is reported as a warning.
- The trust domain must consist of lower case letters, digits, `.`, `-` and `_`; node names and the region must be valid SPIFFE path segments (letters, digits, `.`, `-`, `_`). `validate` checks both.
- Use the same trust domain in all plans of a multi-region cluster.

### Intermediate CA for OpenZiti

OpenZiti runs its own PKI: its controllers and routers get certificates with `spiffe://<trust domain>/controller/<id>` and similar IDs from a signing CA, and in HA mode it requires the trust domain in that chain. To make that PKI chain up to the cluster CA, issue an intermediate:

```sh
norca-deployer pki issue-intermediate -c cluster.yaml --name ziti
```

It writes `certs/ziti-ca.pem`, `certs/ziti-ca-key.pem` (0600) and `certs/ziti-ca-chain.pem` (intermediate and root). The intermediate carries `spiffe://<trust_domain>`, is valid for 5 years (`--years`) and allows one more CA level below it (`--max-path-len`, for OpenZiti's own signer). An existing intermediate is only replaced with `--force`.

### Node CA for nodes that come up by themselves

An operator that creates nodes (autoscaling) has to give every new node a client certificate, and the key of the cluster CA must stay with the deployer. Issue the operator a limited intermediate CA for one region:

```sh
norca-deployer pki issue-node-ca -c cluster.yaml --region eu
```

It writes `certs/node-ca-eu.pem`, `certs/node-ca-eu-key.pem` (0600) and `certs/node-ca-eu-chain.pem` (node CA and root). The region defaults to `metadata.region`, and an existing node CA is only replaced with `--force`.

The node CA is valid for **180 days** by default (`--days`, or `--years` for whole years, not both) and never longer than the cluster CA. A short validity is the point: there is no revocation, and a new node CA does not end the validity of an older one (the servers trust the cluster CA, and the old node CA still chains up to it), so the certificates that a stolen key signs stay valid until the node CA itself expires. Rotate it well before it expires and give the operator the new one.

The node CA signs leaf certificates only (path length 0), for server and client authentication, and **name constraints** limit what it can sign to what a node certificate carries: the DNS names `client.<region>.norca` (and subdomains) and `localhost`, the address `127.0.0.1`, and with SPIFFE the URIs of the trust domain (without SPIFFE, no URI at all). A certificate for a server (`server.<region>.norca`), for another region or with a foreign name, address or trust domain fails verification, so the key of a node CA cannot be used to join the Raft cluster or to impersonate a server. It can still make client certificates of its region, so keep the key where only the operator can read it.

An agent presents the chain it loads from its certificate file: the file of a node signed by a node CA holds the **leaf followed by the node CA**, and without the node CA the servers cannot verify it. The agents keep the cluster CA as their only trusted CA; nothing changes on the servers. After a renewal, send the agent a SIGHUP: it reloads the certificate without a restart and keeps its connections.

### Shutting out a node CA

When the key of a node CA is lost, list its certificate on the servers: a client whose chain has a
certificate with its public key is refused, also a node CA issued again with the same key.

```yaml
security:
  pki:
    distrusted_ca_files: [certs/node-ca-eu.pem]
```

`apply` joins the files into `distrusted-ca.pem` in the TLS directory of every server and sets
`distrusted_ca_file` in their `tls` block; the servers restart, which closes the connections of the
nodes of that CA. Each file must hold at least one certificate. Then issue a new node CA (`--force`)
for the operator and replace the certificates of the nodes that stay. Remove the entry once the
listed CA has expired.

## Gossip encryption

`security.gossip_encryption_key` accepts:

- `AUTO_GENERATE`: a 32-byte key is generated once and stored in `.norca-deployer/gossip.key` (encrypted when a master key exists). Every plan in the same directory uses the same key, which matters for [multi-region](multi-region.md).
- A base64 key of 16, 24 or 32 bytes, or an `!enc:v1:...` secret.
- Empty: no gossip encryption.

The key is written to the server block (`encrypt`).

## ACLs

With `security.acl.enabled: true` the agents run with ACLs. With `auto_bootstrap: true`, the first `apply` runs `norca acl bootstrap` on the first server and stores the management token in `security.acl.management_token_output` (default `.norca-deployer/management-token`). Later runs read the stored token; packs are deployed with it.

If the cluster was bootstrapped earlier but the token file is missing, `apply` warns and skips the packs; it cannot bootstrap a second time.

## Secrets in the plan

The master key is a 256-bit AES key. It is looked up in this order:

1. `--secret-key`
2. `NORCA_DEPLOYER_SECRET_KEY`
3. `--secret-key-file`
4. `.norca-deployer.key` in the current directory (created by `secret create-key`)

`norca-deployer secret encrypt '<value>'` prints an `!enc:v1:aes-gcm:...` value (AES-256-GCM). These fields accept encrypted values:

- `connection.ssh_password`, `ssh_key_passphrase`, `ssh_private_key`, `become_password`
- `host_packages.repositories[].auth.password`
- `scripts[].env` values
- `security.gossip_encryption_key`

An encrypted value that cannot be decrypted is an error; it is never sent as is. With a master key, the gossip key, the management token and the key of the generated CA are also stored encrypted.

## SSH host keys

`connection.host_key_checking`:

| Mode | Behavior |
| --- | --- |
| `tofu` (default) | The key seen on the first connection is stored in `.norca-deployer/known_hosts`; a changed key is an error |
| `strict` | The key must already be in `known_hosts_file` |
| `off` | No check (lab use only) |

## Files

- `init` writes the plan with mode 0600 and overwrites an existing file only with `--force`.
- `secret create-key` never overwrites an existing key: the key decrypts your existing secrets. Do not lose it: with it gone, the encrypted CA key cannot be read.
- The key files of `pki issue-intermediate` and `pki issue-node-ca` (`certs/*-ca-key.pem`, mode 0600) are plain text: the operator or the PKI that uses them needs the key itself.
- On the nodes, the TLS directory is 0700, keys are 0600, and script environment files are 0600 and deleted after use.
