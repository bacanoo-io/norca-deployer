# Getting Started

## Requirements

**Operator machine:** the `norca-deployer` binary (build it with Go 1.26 or newer) and SSH access to every node.

**Linux nodes:** a supported operating system (below) with systemd (OpenRC on Alpine), `sha256sum` and `tar`. SSH as `root`, or as a user with `sudo` or `doas` (`connection.become: true`). The deployer's commands run in `sh`; `bash` is needed only for scripts of the plan with `interpreter: bash` (the default).

**Windows nodes:** OpenSSH Server and an administrator account.

## Supported operating systems

Before it changes anything, `apply` finds out what every node is (`/etc/os-release`, the package manager, the init system) and prints it (`Host: debian 12 (debian family), amd64, systemd, apt`). A node the deployer cannot drive fails before anything is changed on it.

| Family | Systems | Package manager | Status |
| --- | --- | --- | --- |
| Debian | Debian 11+, Ubuntu 20.04+ (and systems with `ID_LIKE=debian`) | `apt` (`host_packages.apt`) | supported |
| RHEL | RHEL 9+, Rocky Linux 9+, AlmaLinux 9+ (RHEL 10 included; Fedora is detected as this family but not tested) | `dnf` (`host_packages.dnf`) | supported; SELinux enforcing: see below |
| Alpine | Alpine 3.22+ (the branches Alpine supports) with OpenRC | `apk` (`host_packages.apk`) | supported; see below |
| Windows | Windows Server with OpenSSH and Windows PowerShell 5.1 | none | supported (no `host_packages`; scripts in PowerShell, see [Windows nodes](windows.md#scripts)) |

Older systems are refused before anything is changed, with the reason: RHEL 8 and its rebuilds (the Norca binary needs glibc 2.34, which RHEL 8 with glibc 2.28 does not have), Debian 10 and Ubuntu 18.04 (glibc 2.28 and 2.27; the Linux release needs glibc 2.31, and the deployer writes deb822 sources with `Signed-By`), and Alpine before 3.22 (no longer supported by Alpine). The [install script of scaled machines](node-scaling-machines.md#what-the-script-does) refuses them with the same reasons.

**Alpine:** the Linux release of Norca is built against glibc and does not run on Alpine, so an Alpine node gets the musl build of the release (the archive `norca_<version>_linux_<arch>_musl.tar.gz`; with `release.source: local-path`, `{musl}` in `local_binary_path`). The deployer checks every Linux binary against the C library of its node and refuses one of the other. The agent runs as an OpenRC service (`/etc/init.d/norca`, supervised by `supervise-daemon`), its tuning is applied with `sysctl -p`, and the `modules`, `sysctl` and `cgroups` services of OpenRC are put into their runlevels when they are in none. Alpine often has `doas` instead of `sudo`, busybox instead of the GNU tools, and no `bash` (write the scripts of the plan for `sh`, or install `bash` with `host_packages.apk`). `consul.dns_forwarding` uses dnsmasq on Alpine, which the plan installs and sets up (see [Alpine: dnsmasq](networking-and-tuning.md#alpine-dnsmasq)); its `method: systemd-resolved` and `docker: true` are errors for an Alpine node. The agent's task drivers work with cgroup v1 and v2; OpenRC mounts cgroup v2 by default.

**SELinux (RHEL):** the deployer restores the SELinux labels of what it writes (`restorecon`) when SELinux is enforcing or permissive. Norca under SELinux enforcing is not verified yet: `apply` prints one warning per node under enforcing and goes on; set SELinux to permissive if the agent or its drivers are denied.

**Firewall:** `apply_firewall_rules` uses an active ufw, a running firewalld (the default on RHEL), else nftables (a table of the deployer), else iptables; see [Networking and tuning](networking-and-tuning.md#firewall).

When the detection is wrong, set the right values in [`host_facts`](configuration.md#host_facts) of the node.

**Network between the nodes:** TCP 4646-4647 and TCP/UDP 4648 to the servers; TCP/UDP 20000-32000 to the clients for allocation ports (the deployer can open these, see [Networking and tuning](networking-and-tuning.md)).

## Build

```sh
git clone https://github.com/bacanoo-io/norca-deployer
cd norca-deployer
go build -o bin/norca-deployer ./cmd/norca-deployer
```

## First deployment

1. **Write a plan.** Start from [examples/single-node.yaml](../examples/nodes/single-node.yaml) or [examples/cluster.yaml](../examples/nodes/cluster.yaml), or let the tool write a sample:

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

[examples/local-dev.yaml](../examples/nodes/local-dev.yaml) installs Norca as server and client on the machine you run the deployer on, without SSH (`connection.mode: local`). It listens on 127.0.0.1 only, without TLS and ACLs. Run it as root on a Linux host with systemd:

```sh
sudo norca-deployer apply -c examples/nodes/local-dev.yaml
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
