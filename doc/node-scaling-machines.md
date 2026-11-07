# Machines of the node scaling operator

The node scaling operator (`norca-nodescaler` of `norca-platform`) asks a provider for new
machines and hands each one a bootstrap: a cloud-init document with the configuration and the
identity of the node. The bootstrap does not install anything. It expects the machine to already
have the agent and what the agent needs. This page describes what that is, how one install
script generated from the plan puts it there, and where the keys of a scaled node are. The node
groups themselves are described in
[Node groups](https://bacanoo-io.github.io/norca-doc/docs/features/node-scaling/nodegroup/).

## What a scaled machine needs

A scaled node is a Linux client of the plan. Before its bootstrap runs, the machine needs:

| What | Where (plan defaults) | From |
| --- | --- | --- |
| The `norca` binary of the release | `paths.linux.bin_dir` (`/usr/local/bin/norca`) | `release` |
| The CNI plugins | `paths.linux.cni_bin_dir` (`/opt/cni/bin`) | `cni`, when `cni.install` is true |
| `norca-join`, for nodes that make their own key | `node_scaling.machine.join_binary` (`/usr/local/bin/norca-join`) | `node_scaling.machine.join_download` |
| The directories: bin `0755`, config `0750`, data `0750`, log `0750`, tls `0700`, CNI bin and config `0755` | `paths.linux` | the same modes as `apply` |
| The systemd unit of the agent, enabled but not started | `/etc/systemd/system/norca.service` | the unit `apply` writes, with `tuning.file_descriptor_limit` |
| The host packages of a client (Docker, for example) | APT | `host_packages` with the role `client` (or no role) |
| The sysctl tuning and the kernel modules | `/etc/sysctl.d/99-norca-production.conf`, `/etc/modules-load.d/norca-bridge.conf` | `tuning.apply_sysctl` |

The configuration (`norca.hcl`), the cluster CA and the certificate are not on this list: they
belong to one node, and the bootstrap writes them (see [What the bootstrap writes](#what-the-bootstrap-writes)).
The firewall is not on it either. A scaled machine gets its network rules from the security
group of the cloud or from its image. The ranges of the region are in the Variable
`nodescaler/network/<region>` (`cluster_cidrs`, `api_cidrs` of `tuning.firewall`), and the
operator checks the network of a node group against them.

## One script, two modes

`norca-deployer node-scaling install-script` prints a bash script that installs all of the
above. The plan decides when it runs (`node_scaling.machine.install`):

- **`image`** (the default): run the script when the image of the provider is built, with Packer
  or by hand. Every machine of the image then starts with everything in place, and needs no
  download at boot.
- **`boot`**: `apply` writes the script into the bootstrap Variable
  `nodescaler/bootstrap/<region>` (item `install_script`). The bootstrap writes it to
  `/usr/local/sbin/norca-node-install.sh` (mode `0700`) and runs it first, before it starts
  `norca-join` or the agent. The machine then downloads the release, the CNI plugins and
  `norca-join` at its first boot, so it needs to reach the download URLs. A plain image of the
  distribution is enough.

```yaml
node_scaling:
  enabled: true
  machine:
    install: image            # image (default) | boot
    arch: amd64               # the architecture of the boot script
    join_binary: /usr/local/bin/norca-join
    join_download:
      url: https://downloads.example.com/norca-join/v0.3.0/norca-join_linux_amd64
      checksum_sha256: 9f2c...  # 64 hex characters
```

The keys are in the [configuration reference](configuration.md#node_scaling).

`boot` has limits that `image` does not have:

- The release and the CNI plugins must be downloadable: `release.source` `official`,
  `custom-url` or `host-package`, and `cni.source` not `local-path`. The plan check refuses
  `local-path` with `boot`.
- An APT repository with `auth` is refused: its password would travel in the user data of every
  machine.
- One region has one bootstrap Variable, so the script is for one architecture
  (`node_scaling.machine.arch`). Machines of another architecture need an image built with
  `install-script --arch`.
- The script travels in the user data, base64 encoded. A plan with APT repositories and their
  keys makes it larger: a cloud with a small user data limit (16 KB on some) may need `image`.

### Generate the script

```shell
norca-deployer node-scaling install-script -c cluster.yaml > norca-node-install.sh
norca-deployer node-scaling install-script -c cluster.yaml --arch arm64 > norca-node-install-arm64.sh
```

The script is the whole output on stdout. The deployer only resolves the URLs and the SHA-256
digests of the files; the machine downloads them itself. The URLs are the same as for `apply`
(`release.download_base_url`, `cni.download_base_url`). The digests are `release.checksum_sha256`
and `cni.checksum_sha256`; without them the deployer downloads the checksum file of the release
(`norca_<version>_SHA256SUMS`, `cni-plugins-linux-<arch>-<version>.tgz.sha256`) and writes the
digest into the script, so the machine checks the archive against a digest the deployer chose.
The same plan always gives the same script, so `apply` rewrites the bootstrap Variable only when
the plan changes.

With Packer, for example:

```hcl
provisioner "file" {
  source      = "norca-node-install.sh"
  destination = "/tmp/norca-node-install.sh"
}
provisioner "shell" {
  inline = ["sudo bash /tmp/norca-node-install.sh"]
}
```

### What the script does

The script runs as root under `set -euo pipefail`, and is idempotent: it writes a file only when
its content or mode differs, and a second run changes nothing (it ends with
`done: nothing to change`). In order:

1. **Host packages** of a client: the keyring, the deb822 source, the pin and the credentials of
   each repository, `apt-get update` and `apt-get install` only when a package is missing or not
   at its version, `apt-mark hold`/`unhold`, and a check of the installed versions. It writes the
   same files as `apply`. A package from a local `.deb` file cannot be downloaded: the script
   checks that it is installed and says to install it into the image first.
2. **sysctl tuning**: the modules file and the sysctl file, `modprobe` and `sysctl --system`. A
   failure to apply them now (in a container, for example) is a warning: systemd applies the
   files at the next boot.
3. **Directories** with the modes above, and `/var/lib/norca-deployer` (`0700`).
4. **The norca binary**: download the release archive, check its SHA-256, extract `norca` and
   install it. A marker with the digests of the archive and of the binary skips the download on
   the next run, but a binary that was changed since is replaced.
   - `release.source: local-path`: the image build names the file in `NORCA_BINARY` (the binary,
     or a `.tar.gz` release archive); with `release.checksum_sha256` it is checked.
   - `release.source: host-package`: the package installs it; the script only links it into the
     bin directory when it is elsewhere, and checks that it is executable.
5. **The CNI plugins**: download, check, unpack into `cni_bin_dir`, with the same marker file as
   `apply` (`.norca-deployer-cni.sha256`). With `cni.source: local-path` the image build names
   the archive in `NORCA_CNI_ARCHIVE`.
6. **norca-join** from `join_download`, checked against its digest. Without `join_download` the
   script warns when `join_binary` is missing and says how to provide it. A node whose key the
   operator makes (no join endpoint) does not use `norca-join`.
7. **The unit** `norca.service`, then `systemctl daemon-reload` (when systemd runs and the unit
   changed) and `systemctl enable`. It does not start the agent: the bootstrap, or `norca-join`,
   starts it once the configuration and the certificate exist. Without `systemctl` (an image
   built in a container) the script enables the unit with the link
   `/etc/systemd/system/multi-user.target.wants/norca.service`, which is what `systemctl enable`
   creates, and says so.

The downloads use `curl`, or `wget` when there is no `curl`. The script needs `sha256sum`, `tar`
and the other coreutils of a Debian or Ubuntu machine, and `apt-get` only with host packages.

An excerpt of a generated script (defaults of the plan, `install: image`):

```bash
#!/usr/bin/env bash
# The install script of a scaled Norca node, generated by norca-deployer
# (node-scaling install-script) from the plan of cluster prod, region eu.
# Do not edit: generate it again when the plan changes.
...
set -euo pipefail
umask 022
...
# Directories
ensure_dir '/usr/local/bin' 0755
ensure_dir '/etc/norca.d' 0750
ensure_dir '/var/lib/norca' 0750
ensure_dir '/var/log/norca' 0750
ensure_dir '/etc/norca.d/tls' 0700
ensure_dir '/opt/cni/bin' 0755
ensure_dir '/opt/cni/config' 0755
ensure_dir "$state_dir" 0700

# The norca binary (v0.3.0)
norca_bin='/usr/local/bin/norca'
release_marker="$state_dir/norca-release.sha256"
if [ -f "$release_marker" ] && [ -f "$norca_bin" ] &&
  [ "$(cat "$release_marker")" = "4b0c...e1d2 $(sha256_of "$norca_bin")" ]; then
  log "$norca_bin is the binary of the release (unchanged)"
else
  fetch 'https://github.com/bacanoo-io/norca/releases/download/v0.3.0/norca_0.3.0_linux_amd64.tar.gz' "$work/norca.tar.gz"
  verify "$work/norca.tar.gz" '4b0c...e1d2'
  src=$(extract_binary "$work/norca.tar.gz" norca)
  put_file "$norca_bin" 0755 <"$src"
  ...
fi
...
# The systemd unit of the agent: enabled, not started
put_file '/etc/systemd/system/norca.service' 0644 <<'NORCA_EOF'
[Unit]
Description=Norca Orchestration Agent
...
ExecStart=/usr/local/bin/norca agent -config=/etc/norca.d/norca.hcl
...
NORCA_EOF
```

## What the bootstrap writes

The operator renders one cloud-init document per machine. The paths come from the bootstrap
Variable (`tls_dir`, `config_file`, `service`), which `apply` fills from `paths.linux` of the
plan: with the defaults, `/etc/norca.d/tls`, `/etc/norca.d/norca.hcl` and `norca`.

**Basic mode** (the operator makes the key of the node):

| File | Mode | Content |
| --- | --- | --- |
| `<tls_dir>/ca.pem` | `0644` | The cluster CA |
| `<tls_dir>/client.pem` | `0644` | The certificate of the node and the node CA |
| `<tls_dir>/client-key.pem` | `0600` | The key of the node |
| `<config_file>` | `0640` | The agent configuration of the node |
| `/usr/local/sbin/norca-node-install.sh` | `0700` | The install script, with `install: boot` only |

Then `runcmd` runs the install script (with `install: boot`) and `systemctl enable --now norca`.

**Join mode** (the operator serves its join endpoint; the key stays on the node):

| File | Mode | Content |
| --- | --- | --- |
| `<tls_dir>/ca.pem` | `0644` | The cluster CA |
| `<config_file>` | `0640` | The agent configuration of the node, with the meta `norca.identity = "join"` |
| `<config dir>/norca-join.json` | `0600` | The configuration of `norca-join`, with the one-time token |
| `/etc/systemd/system/norca-join.service` | `0644` | The unit of `norca-join`: `ExecStart=<join_binary> run -config <config dir>/norca-join.json` |
| `/usr/local/sbin/norca-node-install.sh` | `0700` | The install script, with `install: boot` only |

Then `runcmd` runs the install script (with `install: boot`), `systemctl daemon-reload` and
`systemctl enable --now norca-join`. `norca-join` makes the key, gets the certificate from the
endpoint, writes `client.pem` and `client-key.pem` into `tls_dir`, and starts the agent unit.

An example `norca-join.json`:

```json
{
  "instance": "ng-scaled-3-1",
  "region": "ns",
  "urls": ["https://10.77.1.11:8490", "https://10.77.1.13:8490"],
  "endpoint": "spiffe://lab.norca/region/ns/nodescaler/join",
  "token": "<one-time token>",
  "ca_file": "/etc/norca.d/tls/ca.pem",
  "cert_file": "/etc/norca.d/tls/client.pem",
  "key_file": "/etc/norca.d/tls/client-key.pem",
  "state_file": "/etc/norca.d/tls/norca-join.state",
  "service": "norca"
}
```

`urls` are the join endpoints, tried in order. `endpoint` is the only name the certificate of an
endpoint may carry (`spiffe://no-spiffe.invalid/...` without SPIFFE). `token` is removed once it
is used. `state_file` keeps the endpoints the operator last named and when the certificate was
issued, and `service` is the unit `norca-join` starts and sends `SIGHUP` after a renewal.

## Where the keys are

| Key | Where | Who can read it |
| --- | --- | --- |
| The key of a node, basic mode | Made by the operator; it travels in the user data to the provider and stays in the user data of the machine in the cloud. On the node: `<tls_dir>/client-key.pem`, `0600`, root | The node's root; whoever can read the user data of the machine (the cloud account, and any process on the machine that can reach the metadata service, unless the cloud blocks it) |
| The key of a node, join mode | Made on the machine by `norca-join`; it never leaves the machine | The node's root |
| The key of the node CA of the region | The bootstrap Variable `nodescaler/bootstrap/<region>` (item `node_ca_key`), encrypted at rest in Raft like every Variable | The job of the operator: give the namespace of the Variable (`node_scaling.namespace`) to the operator only |
| The key of the cluster root CA | With the deployer only (`certs/` next to the plan), encrypted with the master key when there is one | Whoever runs the deployer with the master key |

The node CA can only sign client certificates of its region, so a stolen key of the node CA
cannot make a server. There is no revocation: the validity of a node certificate (and of the node
CA) bounds what a stolen key is worth. A join-mode certificate lives 7 days by default and is
renewed on the node.

## A container node is not a machine

The docker provider of `norca-platform` runs each node as a container on one host. It mounts
`norca` and `norca-join` from the host and does not run the install script or `runcmd`. That is
enough for a lab, but it is not the isolation of a machine:

- The root user and the members of the `docker` group of the host can read the key of every
  container node: from the files the provider writes on the host in basic mode, and from inside
  the container (`docker exec`, `docker cp`) in join mode.
- The containers run `--privileged`, as the agent of a client needs. A workload that breaks out
  of its container reaches the host.
- A container that is cloned (`docker commit` and run again, for example) carries the identity of
  its node. The operator sees a second ready node with the name of an instance, reports it as
  **foreign** and makes it ineligible for new work, but the certificate it holds stays valid until
  it expires.

Use the docker provider for labs and hosts you trust. On a cloud, each node is a machine of its
own, built from an image with the install script, or installed by the script at boot.
