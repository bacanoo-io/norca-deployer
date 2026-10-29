# Getting Started

## Requirements

**Operator machine:** the `norca-deployer` binary (build it with Go 1.26 or newer) and SSH access to every node.

**Linux nodes:** systemd, `bash`, `sha256sum`, `tar`; Debian or Ubuntu when you use `host_packages`. SSH as `root`, or as a user with `sudo` (`connection.become: true`).

**Windows nodes:** OpenSSH Server and an administrator account.

**Network between the nodes:** TCP 4646-4647 and TCP/UDP 4648 to the servers; TCP/UDP 20000-32000 to the clients for allocation ports (the deployer can open these, see [Networking and tuning](networking-and-tuning.md)).

## Build

```sh
git clone https://github.com/bacanoo-io/norca-deployer
cd norca-deployer
go build -o bin/norca-deployer ./cmd/norca-deployer
```

## First deployment

1. **Write a plan.** Start from [examples/single-node.yaml](../examples/single-node.yaml) or [examples/cluster.yaml](../examples/cluster.yaml), or let the tool write a sample:

   ```sh
   norca-deployer init -c cluster-deploy.yaml
   ```

2. **Protect secrets (optional but recommended).** A master key encrypts passwords in the plan and the ACL token on disk:

   ```sh
   norca-deployer secret create-key                 # writes .norca-deployer.key (keep it safe)
   norca-deployer secret encrypt 'ssh-password'     # paste the !enc:v1:... output into the plan
   ```

3. **Check the plan.**

   ```sh
   norca-deployer validate -c cluster-deploy.yaml
   norca-deployer plan -c cluster-deploy.yaml       # dry run: nodes, release, rollout settings
   ```

4. **Deploy.**

   ```sh
   norca-deployer apply -c cluster-deploy.yaml
   ```

   The output shows five stages: preparation, servers, server restarts, clients, and post-cluster tasks. See [How apply works](how-apply-works.md).

5. **Use the cluster.** With ACLs enabled, the management token is in `.norca-deployer/management-token` (or encrypted, when a master key exists). The CA is `certs/norca-ca.pem`.

   ```sh
   export NORCA_ADDR=https://<server>:4646
   export NORCA_CACERT=certs/norca-ca.pem
   export NORCA_TOKEN=$(cat .norca-deployer/management-token)
   norca node status
   ```

## A local development node

[examples/local-dev.yaml](../examples/local-dev.yaml) installs Norca as server and client on the machine you run the deployer on, without SSH (`connection.mode: local`). It listens on 127.0.0.1 only, without TLS and ACLs. Run it as root on a Linux host with systemd:

```sh
sudo norca-deployer apply -c examples/local-dev.yaml
export NORCA_ADDR=http://127.0.0.1:4646
norca node status
```

To test your own Norca build, switch `release.source` to `local-path` and point `local_binary_path` at the binary; each `apply` restarts the agent only when the binary changed.

## Updating

Change the plan and run `apply` again. Only nodes whose binary, certificate, `norca.hcl` or service unit changed are restarted: servers one at a time with health checks, clients in batches. Upgrading Norca is a change of `release.version`.

```sh
norca-deployer apply -c cluster-deploy.yaml                   # roll out changes
norca-deployer apply -c cluster-deploy.yaml --force-restart   # restart every agent (rolling)
```

## Keep the local state

Keep the plan directory with `certs/`, `.norca-deployer/` and the master key. Losing the CA means re-issuing every certificate; losing the management token means you cannot bootstrap ACLs again. Do not commit these files (see the repository `.gitignore`).
