# Norca Deployer

A single-binary installer and lifecycle tool for [Norca](https://github.com/bacanoo-io/norca) clusters. It reads a declarative deployment plan (YAML or HCL), connects to the nodes over SSH (or runs locally) and installs, configures and upgrades the Norca agents idempotently. Nothing has to be preinstalled on the nodes.

## Features

- **Servers and clients** on Linux and Windows nodes, per-node datacenter, multi-region federation (`authoritative_region`, ACL replication).
- **Rolling restarts:** only changed nodes restart. Servers restart one at a time and the deployer waits until each one is healthy, has a leader and sees all Raft peers. Clients restart in batches. A new cluster starts all servers at once.
- **Security:** an internal CA with mTLS certificates (`server.<region>.norca`, `client.<region>.norca`), gossip encryption (`AUTO_GENERATE`), ACL bootstrap with the management token stored locally, encrypted with AES-256-GCM when a master key exists. Optional SPIFFE IDs in all certificates and intermediate CAs (for OpenZiti). Secrets in the plan can be inline `!enc:v1:...` values.
- **Artifacts:** the Norca binary and the CNI plugins are downloaded on the deployer machine (SHA-256 verified and cached) or taken from local files, then copied to the nodes. The nodes need no internet access.
- **Debian packages:** APT repositories (deb822 with `Signed-By` keyring, pin priority, repository auth) and local `.deb` files copied from the deployer machine, with version pinning and `apt-mark hold`.
- **Bash scripts** in the phases `pre_install`, `post_packages` and `post_install`. A script runs again only when the script or its environment changed.
- **Network exposure:** by default the agent listens only on each node's cluster address (and on loopback for the local CLI). Firewall rules on Linux nodes (ufw, firewalld or iptables) limit the API, the cluster ports and the allocation ports to given CIDRs; `default_deny` turns ufw on without locking out SSH.
- **Tuning:** sysctl, kernel modules and log rotation.
- **Node labels and resources:** meta labels and node class (with cluster-wide `client_defaults`), node pools, host networks, reserved resources, drain on shutdown, the dynamic port range (kept in step with the firewall), and `extra_hcl` for other agent settings.
- **Node lifecycle:** new nodes are installed by adding them to the plan; `inventory.decommission` drains or removes nodes from Raft and uninstalls them. A list of installed nodes stops an apply when a node was only deleted from the plan.
- **Bootstrap secrets and policies:** after the cluster is up, secrets of the built-in secret store and Variables (plain, encrypted, from files, or generated once: passwords, NATS NKeys) and ACL policies bound to jobs.
- **Packs:** creates pack registries and installs packs with the server-side `deploy-pack`. A pack is deployed again only when its settings changed.
- **GitOps:** deploys packs straight from Forgejo, Gitea, GitHub and GitLab repositories, read into memory: once with `gitops run`, on every `apply`, and on every push with the `gitops daemon` webhook listener (HMAC-verified, debounced). Bindings can require a signed commit or tag.
- **SSH host keys:** trust on first use (`tofu`), `strict` or `off`.

## Build

Requires Go 1.26 or newer.

```sh
go build -o bin/norca-deployer ./cmd/norca-deployer
```

The deployer runs on Linux, macOS and Windows (amd64 and arm64) and needs no cgo. For a build for another system, turn cgo off:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/norca-deployer-linux-amd64 ./cmd/norca-deployer
```

`connection.mode: local` (installing on the machine the deployer runs on) needs a Linux host with systemd.

## Quick start

```sh
# 1. Create a sample plan (or start from one of the examples)
norca-deployer init -c cluster-deploy.yaml

# 2. Optional: create a master key to encrypt secrets and the ACL token
norca-deployer secret create-key
norca-deployer secret encrypt 'my-ssh-password'   # prints !enc:v1:aes-gcm:...

# 3. Check the plan and see what will happen
norca-deployer validate -c cluster-deploy.yaml
norca-deployer plan -c cluster-deploy.yaml

# 4. Install or update the cluster (safe to run again)
norca-deployer apply -c cluster-deploy.yaml
```

## Commands

| Command | Description |
| --- | --- |
| `init [--force]` | Write a sample plan; `--force` overwrites an existing file |
| `validate [-o json]` | Check the syntax and the logical constraints of the plan |
| `plan [-o json]` | Show what `apply` would do, without changing anything (dry run) |
| `apply [--force-restart]` | Provision the nodes and roll the changes out; `--force-restart` restarts every agent (rolling) even when nothing changed |
| `secret create-key` | Create a 256-bit master key (`.norca-deployer.key`); never overwrites an existing key |
| `secret encrypt <text>` | Encrypt a value for the plan (`!enc:v1:aes-gcm:...`) |
| `pki issue-intermediate --name <name>` | Issue an intermediate CA signed by the cluster CA, e.g. for OpenZiti |
| `pki issue-node-ca [--region <region>]` | Issue the name-constrained intermediate CA of a region, with which an operator signs the certificates of nodes that join by themselves |
| `version` | Print the version |

`-o json` prints a JSON document on stdout for scripts. Errors go to stderr as `[ERROR] ...`, never to stdout. Exit codes: `0` success, `1` a failure while the command ran, `2` a wrong command line (unknown command or flag, missing argument), `130` the command was interrupted (Ctrl-C) and stopped between two steps.

Global flags: `-c, --config` (plan file, `.yaml`/`.yml` or `.hcl`; default `cluster-deploy.yaml`), `--secret-key`, `--secret-key-file`. The master key can also be given in `NORCA_DEPLOYER_SECRET_KEY`.

## Examples

Complete plans with a comment on every setting are in [examples/](examples):

| File | Content |
| --- | --- |
| [single-node.yaml](examples/nodes/single-node.yaml) / [single-node.hcl](examples/nodes/single-node.hcl) | One machine running server and client: TLS, gossip encryption, ACLs, host packages, a script |
| [multi-datacenter.yaml](examples/nodes/multi-datacenter.yaml) | One region over three datacenters: a server per datacenter, the event stream on the system bus with 3 replicas, NATS with one server per datacenter for the applications |
| [add-node.yaml](examples/nodes/add-node.yaml) | Adding a client to a running cluster (only the new node is installed) |
| [decommission-node.yaml](examples/nodes/decommission-node.yaml) | Removing clients and servers: drain, Raft removal, uninstall |
| [local-dev.yaml](examples/nodes/local-dev.yaml) | Development node on the machine running the deployer (`connection.mode: local`, run with `sudo`): loopback only, no TLS or ACLs, docker/exec/raw_exec, optional local Norca build |
| [cluster.yaml](examples/nodes/cluster.yaml) / [cluster.hcl](examples/nodes/cluster.hcl) | 3 servers, 2 Linux clients with Docker from the Docker APT repository, 1 Windows client; custom paths, firewall and sysctl tuning, PKI and SPIFFE, backups, scripts, host volumes; NATS for the applications (generated secrets, an ACL policy for event readers, two pack registries); Norca from a .deb and federation as comments |

Plans can be written in YAML or HCL; both formats use the same keys, and the YAML and HCL examples load to the same plan. Paths in a plan (keys, `.deb` files, scripts) are relative to the plan file.

## Local state

`apply` keeps its state next to the plan. Keep these files private and do not commit them:

| Path | Content |
| --- | --- |
| `certs/` | The cluster CA, the client pool and operator certificates |
| `.norca-deployer/gossip.key` | Gossip encryption key |
| `.norca-deployer/management-token` | ACL management token (encrypted when a master key exists) |
| `.norca-deployer/nodes-<region>.json` | Installed nodes (role, node definition, release) |
| `.norca-deployer/generated/`, `nkeys/` | Generated passwords and NATS NKeys |
| `.norca-deployer/known_hosts` | Trusted SSH host keys |
| `.norca-deployer/cache/` | Downloaded Norca and CNI artifacts |
| `.norca-deployer.key` | Master encryption key (created by `secret create-key` in the current directory) |

On the nodes the deployer keeps its markers under `/var/lib/norca-deployer/`.

## The plan as a library

The model of a deployment plan is the public package
[`pkg/plan`](pkg/plan): its types, loading from a file or from memory (YAML,
HCL, JSON), the defaults and the validation. Other programs (an operator, a
user interface, another tool) embed it to read, check and write plans
without the deployer; it makes no connection and has no state.

```go
import "github.com/bacanoo-io/norca-deployer/pkg/plan"

p, err := plan.Parse(data, plan.FormatYAML, "")
```

The rest of the deployer (the installation over SSH, the state, the
commands) stays internal.

## Testing

```sh
go test ./...
```

The end-to-end test runs against a real Debian node over SSH (needs `openssh-server`, `sudo` and internet access for the APT part):

```sh
NORCA_E2E_SSH=127.0.0.1:2222 NORCA_E2E_USER=deploy NORCA_E2E_PASSWORD=... \
  go test -tags e2e ./internal/e2e/
```

## Documentation

The user documentation is in [doc/](doc/README.md):

- [Getting started](doc/getting-started.md)
- [How apply works](doc/how-apply-works.md)
- [Configuration reference](doc/configuration.md)
- [Security](doc/security.md)
- [Host packages and scripts](doc/host-packages-and-scripts.md)
- [Networking and tuning](doc/networking-and-tuning.md)
- [Node labels, pools and resources](doc/node-labels-and-pools.md)
- [Adding and removing nodes](doc/adding-and-removing-nodes.md)
- [Bootstrap secrets and ACL policies](doc/secrets-and-policies.md)
- [Packs](doc/packs.md)
- [GitOps](doc/gitops.md)
- [Several datacenters with NATS](doc/multi-datacenter.md)
- [Multi-region](doc/multi-region.md)
- [Windows nodes](doc/windows.md)
