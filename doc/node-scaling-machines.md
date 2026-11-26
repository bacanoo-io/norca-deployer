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
| The `norca` binary of the release (the musl build on Alpine) | `paths.linux.bin_dir` (`/usr/local/bin/norca`) | `release` |
| The CNI plugins | `paths.linux.cni_bin_dir` (`/opt/cni/bin`) | `cni`, when `cni.install` is true |
| `norca-join`, for nodes that make their own key | `node_scaling.machine.join_binary` (`/usr/local/bin/norca-join`) | `node_scaling.machine.join_download`: a release of norca-platform (`source: official`), whose static binary runs on Alpine and the glibc distributions alike, or a file of your own; build that with `CGO_ENABLED=0` (a static binary) so that it runs on Alpine too |
| The directories: bin `0755`, config `0750`, data `0750`, log `0750`, tls `0700`, CNI bin and config `0755` | `paths.linux` | the same modes as `apply` |
| The service of the agent, enabled but not started | `/etc/systemd/system/norca.service`; on Alpine the OpenRC service `/etc/init.d/norca` in the default runlevel | the service `apply` writes, with `tuning.file_descriptor_limit` |
| The host packages of a client (Docker, for example) | APT (Debian, Ubuntu), dnf (RHEL, Rocky Linux, AlmaLinux) or apk (Alpine) | `host_packages.apt`, `host_packages.dnf` and `host_packages.apk` with the role `client` (or no role) |
| The sysctl tuning and the kernel modules | `/etc/sysctl.d/99-norca-production.conf`, `/etc/modules-load.d/norca-bridge.conf` | `tuning.apply_sysctl` |
| A TPM 2.0, only for keys in the TPM (`join_key_store` of the operator `tpm` or `auto`) | `/dev/tpmrm0` | the machine: a virtual TPM on clouds (Google Shielded VM, Azure Trusted Launch, AWS NitroTPM), the firmware TPM on hardware |

The configuration (`norca.hcl`), the cluster CA and the certificate are not on this list: they
belong to one node, and the bootstrap writes them (see [What the bootstrap writes](#what-the-bootstrap-writes)).
The firewall is not on it either. A scaled machine gets its network rules from the security
group of the cloud or from its image. The ranges of the region are in the Variable
`nodescaler/network/<region>` (`cluster_cidrs`, `api_cidrs` of `tuning.firewall`), and the
operator checks the network of a node group against them. `apply` writes its Variables into the
namespace `node_scaling.namespace`, by default `norca-system`, where the `nodescaler-operator`
module runs and reads them.

## One script, two modes

`norca-deployer node-scaling install-script` prints a POSIX `sh` script that installs all of
the above. It is made of the same steps `apply` runs over SSH on the nodes of its inventory (the
packages, the tuning, the DNS forwarding, the directories, the binary, the CNI plugins and the
unit), rendered into shell. The plan decides when it runs (`node_scaling.machine.install`):

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
    join_download:            # a release of norca-platform
      source: official
      version: v0.1.0
      # or a file of your own:
      # url: https://downloads.example.com/norca-join/v0.3.0/norca-join_linux_amd64
      # checksum_sha256: 9f2c...  # 64 hex characters
```

The keys are in the [configuration reference](configuration.md#node_scaling).

`boot` has limits that `image` does not have:

- The release and the CNI plugins must be downloadable: `release.source` `official`,
  `custom-url` or `host-package`, and `cni.source` not `local-path`. The plan check refuses
  `local-path` with `boot`.
- An APT or dnf repository with `auth` is refused (apk repositories take no credentials at all): its password would travel in the user data of
  every machine (`... has credentials: at boot they would travel in the user data of the machine;
  use node_scaling.machine.install: image`).
- One region has one bootstrap Variable, so the script is for one architecture
  (`node_scaling.machine.arch`). Machines of another architecture need an image built with
  `install-script --arch`.
- The script travels in the user data, base64 encoded. A plan with repositories and their keys,
  and with several blocks (`apt`, `dnf`, `apk`), makes it larger: a cloud with a small user data
  limit (16 KB on some) may need `image`.

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
`norca-join` of `join_download.source: official` is checked the same way, against
`norca-platform_<version>_SHA256SUMS`.
The same plan always gives the same script, so `apply` rewrites the bootstrap Variable only when
the plan changes.

With Packer, for example:

```hcl
provisioner "file" {
  source      = "norca-node-install.sh"
  destination = "/tmp/norca-node-install.sh"
}
provisioner "shell" {
  inline = ["sudo sh /tmp/norca-node-install.sh"]
}
```

### What the script does

The script runs as root under `set -eu` in `sh` (dash, bash or busybox ash), and is idempotent:
it writes a file only when its content or mode differs, and a second run changes nothing (it ends
with `done: nothing to change`). It first finds out the family of the machine from
`/etc/os-release` (its `ID`, then `ID_LIKE`, as `apply` does over SSH) and runs the packages and
the service of that family. It has the steps of the families a client of `apply` would be
installed on: the Debian family (Debian, Ubuntu; `host_packages.apt`), the RHEL family (RHEL,
Rocky Linux, AlmaLinux; `host_packages.dnf`) and the Alpine family (`host_packages.apk`, OpenRC).
A family whose block is missing while another block has packages for the clients is left out, as
`apply` refuses such a node, and so is the Alpine family when `consul.dns_forwarding` needs
systemd-resolved (`method: systemd-resolved` or `docker: true`), as `apply` refuses an Alpine
node then; a plan that leaves no family is an error of `install-script`. On a machine of a
family it has no steps of, the script stops before it changes anything: `this script has no
install for rocky (family rhel); it installs on the families: debian`. It also stops on a version
older than `apply` installs on (Debian 11, Ubuntu 20.04, RHEL and its rebuilds 9, Alpine 3.22;
see [Supported operating systems](getting-started.md#supported-operating-systems)), with the
reason `apply` gives: `this machine is not supported: it runs debian 10, but Debian is supported
from version 11: ...`. The tuning and the norca
binary of a family that differ from the others' (on Alpine: the tuning of OpenRC and the musl
build) are functions of that family. In order:

1. **Host packages** of a client, with the manager of the family:
   - `host_packages.apt`: the keyring, the deb822 source, the pin and the credentials of each
     repository, `apt-get update` and `apt-get install` (on every run while `update_cache` is on,
     as `apply` does; otherwise only when a package is missing or not at its version),
     `apt-mark hold`/`unhold`, and a check of the installed versions.
   - `host_packages.dnf`: the key and the repository file of each repository (with `dnf makecache`
     of it when they changed), `dnf install` of the packages that are missing or not at their
     version, the versionlock command (installed when a package is held) and the holds, and a
     check of the installed versions.
   - `host_packages.apk`: the keys and `/etc/apk/repositories.d/norca.list` (with `apk update`
     when they changed), the removal of the keys of dropped repositories, `apk add` of the
     packages that are missing, not at their version or without their world entry (the hold of
     apk), and a check of the installed versions.

   It writes the same files as `apply`. A package from a local `.deb`, `.rpm` or `.apk` file
   cannot be downloaded: the script checks that it is installed and says to install it into the
   image first.
2. **sysctl tuning**: the modules file (`/etc/modules-load.d/norca-bridge.conf`, so the modules
   load at boot) and the sysctl file, `modprobe` and `sysctl --system`. A module that cannot be
   loaded now is a warning; a failed `sysctl --system` is a warning where `/proc/sys` cannot be
   written (an image built in a container: systemd applies the files at the next boot) and an
   error elsewhere. On Alpine the same files (the `modules` and `sysctl` services of OpenRC read
   them at boot), applied with `sysctl -p`; a key the machine does not have, or one that is
   read-only there (a container), is a warning; the `modules`, `sysctl` and `cgroups` services
   are put into their runlevels when they are in none.
   - With `consul.dns_forwarding` enabled, **the DNS forwarding of the consul domain**: the script
     writes the POSIX `sh` script `apply` runs on the nodes of the inventory to
     `/var/lib/norca-deployer/consul-dns.sh` and runs it (see
     [DNS of the hosts](networking-and-tuning.md#dns-of-the-hosts-the-consul-domain)); its lines
     are logged as `consul.dns_forwarding: ...`. The init system it drives is that of the
     family of the machine: systemd on Debian and RHEL, OpenRC on Alpine (dnsmasq, with
     `rc-service`). A machine without systemd-resolved or dnsmasq stops the script with the error
     `apply` gives. When the init system does not run (an image built in a container), the
     resolver is chosen by its enabled unit, on Alpine by dnsmasq in a runlevel;
     `/etc/resolv.conf` is not checked, nothing is restarted, and with `docker: true` the
     containers are left out when `docker0` is missing (a warning). The script does not run the
     `scripts` of the plan, so the image build of an Alpine machine installs dnsmasq
     (`apk add dnsmasq`) and runs the script of
     [Alpine: dnsmasq](networking-and-tuning.md#alpine-dnsmasq) before the install script.
3. **Directories** with the modes above, and `/var/lib/norca-deployer` (`0700`).
4. **The norca binary**: download the release archive (on Alpine the `_musl` archive), check its
   SHA-256, extract `norca` and install it. A marker with the digests of the archive and of the binary skips the download on
   the next run, but a binary that was changed since is replaced.
   - `release.source: local-path`: the image build names the file in `NORCA_BINARY` (the binary,
     or a `.tar.gz` release archive); with `release.checksum_sha256` it is checked.
   - `release.source: host-package`: the package installs it; the script only links it into the
     bin directory when it is elsewhere, and checks that it is executable.
5. **The CNI plugins**: download, check, unpack into `cni_bin_dir`, with the same marker file as
   `apply` (`.norca-deployer-cni.sha256`). With `cni.source: local-path` the image build names
   the archive in `NORCA_CNI_ARCHIVE`.
6. **norca-join** from `join_download`, checked against its digest. With `source: official` the
   file is `norca-join_<version>_linux_<arch>` of the release of norca-platform, and the digest
   is the one of `norca-platform_<version>_SHA256SUMS`, which the deployer downloads when it
   writes the script (once: it is cached in the state directory). Every family gets the same
   static binary. Without `join_download` the script warns when `join_binary` is missing and
   says how to provide it. A node whose key the operator makes (no join endpoint) does not use
   `norca-join`.
7. **The unit** `norca.service`, then `systemctl daemon-reload` (when systemd runs and the unit
   changed) and `systemctl enable`. It does not start the agent: the bootstrap, or `norca-join`,
   starts it once the configuration and the certificate exist. Without `systemctl` (an image
   built in a container) the script enables the unit with the link
   `/etc/systemd/system/multi-user.target.wants/norca.service`, which is what `systemctl enable`
   creates, and says so. On Alpine: the OpenRC service `/etc/init.d/norca` and `rc-update add
   norca default` (without `rc-update`, the link `/etc/runlevels/default/norca`).
8. **On RHEL, the SELinux labels**: when the run changed something and `getenforce` says
   SELinux is not disabled, `restorecon` restores the labels of what the script wrote, as `apply`
   does (see [SELinux](host-packages-and-scripts.md#selinux)). Under SELinux enforcing the script
   warns once at its start: Norca under SELinux enforcing is not verified yet.

The downloads use `curl`, or `wget` when there is no `curl` (the `wget` of busybox on Alpine).
The script needs `sha256sum`, `tar`, `install`, `base64`, `find`, `stat` and `mktemp` (GNU
coreutils or busybox; a minimal RHEL image and Alpine have them all), and `apt-get`, `dnf` or
`apk` only with host packages. It needs no `bash` and no `cmp`.

**The bootstrap and Alpine.** The bootstrap of the operator (below) works on Alpine machines
too: it decides at boot between systemd and OpenRC (see
[systemd or OpenRC](#systemd-or-openrc-decided-at-boot)). An Alpine image needs a cloud-init
of its own: the `cloud-init` package of Alpine, or tiny-cloud, the bootstrapper of the Alpine
cloud images.

An excerpt of a generated script (defaults of the plan, `install: image`):

```sh
#!/bin/sh
# The install script of a scaled Norca node, generated by norca-deployer
# (node-scaling install-script) from the plan of cluster prod, region eu.
# Do not edit: generate it again when the plan changes.
...
set -eu
umask 022
...
# The family of this machine: the script has the steps of these families.
os_id=$(os_release ID)
os_family=$(family_of "$os_id" "$(os_release ID_LIKE)")
case $os_family in
debian | rhel) log "family $os_family (${os_id})" ;;
*) die "this script has no install for ${os_id:-this system} (family $os_family); it installs on the families: debian, rhel" ;;
esac
os_problem=$(os_too_old "$os_id" "$os_family" "$(os_release VERSION_ID)")
[ -z "$os_problem" ] || die "this machine is not supported: $os_problem"
...
# directories
ensure_dir '/usr/local/bin' 0755
ensure_dir '/etc/norca.d' 0750
...
ensure_dir '/var/lib/norca-deployer' 0700

# The norca binary (v0.3.0)
# norca binary
if ! {
[ -f '/var/lib/norca-deployer/norca-release.sha256' ] && [ -f '/usr/local/bin/norca' ] &&
  [ "$(cat '/var/lib/norca-deployer/norca-release.sha256')" = "4b0c...e1d2 $(sha256_of '/usr/local/bin/norca')" ]
}; then
fetch 'https://github.com/bacanoo-io/norca/releases/download/v0.3.0/norca_0.3.0_linux_amd64.tar.gz' "$work/norca.tar.gz"
verify "$work/norca.tar.gz" '4b0c...e1d2'
norca_src=$(extract_binary "$work/norca.tar.gz" norca)
put_file '/usr/local/bin/norca' 0755 <"$norca_src"
...
  changed=1
fi
...
case $os_family in
debian) service_debian ;;
rhel) service_rhel ;;
esac
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

Then `runcmd` runs the install script (with `install: boot`) and enables and starts the service
`norca` (see [systemd or OpenRC](#systemd-or-openrc-decided-at-boot)).

**Join mode** (the operator serves its join endpoint; the key stays on the node):

| File | Mode | Content |
| --- | --- | --- |
| `<tls_dir>/ca.pem` | `0644` | The cluster CA |
| `<config_file>` | `0640` | The agent configuration of the node, with the meta `norca.identity = "join"` |
| `<config dir>/norca-join.json` | `0600` | The configuration of `norca-join`, with the one-time token |
| `/etc/systemd/system/norca-join.service` | `0644` | The unit of `norca-join`: `ExecStart=<join_binary> run -config <config dir>/norca-join.json`, restarted 10 seconds after it ended |
| `/etc/init.d/norca-join` | `0755` | The OpenRC service of `norca-join`: the same command under `supervise-daemon`, started again 10 seconds after it ended, its output in syslog |
| `/usr/local/sbin/norca-node-install.sh` | `0700` | The install script, with `install: boot` only |

Then `runcmd` runs the install script (with `install: boot`) and enables and starts the service
`norca-join`. `norca-join` makes the key, gets the certificate from the endpoint, writes
`client.pem` and `client-key.pem` into `tls_dir`, and enables and starts the service of the agent
with the same init system; after each renewal it sends the agent `SIGHUP` (`systemctl kill
--signal=HUP`, or `rc-service norca reload`, which the OpenRC service of the install script
has).

### systemd or OpenRC, decided at boot

One document serves every family of the install script: the last line of `runcmd` finds the
init system when the machine boots. It is one line of POSIX `sh`:

```sh
if [ -d /run/systemd/system ]; then rm -f /etc/init.d/norca-join && systemctl daemon-reload && systemctl enable --now norca-join; elif command -v rc-service >/dev/null 2>&1; then rc-update add norca-join default && rc-service norca-join start; else echo "norca bootstrap: neither systemd nor OpenRC runs on this machine: norca-join is not started" >&2; exit 1; fi
```

- **systemd** runs when `/run/systemd/system` exists (Debian, Ubuntu, RHEL and its rebuilds):
  `systemctl daemon-reload` and `systemctl enable --now`. In join mode the OpenRC service
  `/etc/init.d/norca-join` is removed first: `systemctl enable` hands a script of `/etc/init.d`
  with the name of the unit to `update-rc.d`, which refuses one without an LSB header.
- **OpenRC** otherwise, when `rc-service` is there (Alpine): `rc-update add <service> default`
  and `rc-service <service> start`.
- **Neither**: the line fails with the message above, and cloud-init reports the error of
  `runcmd` (`cloud-init status --long`, `/var/log/cloud-init-output.log`).

The basic mode has the same line for `norca`, without the removal. `norca-join` decides the same
way when it starts the agent: on a machine with neither systemd nor OpenRC it does not join, so
the one-time token is not spent.

The entries of `runcmd` are shell lines (YAML strings), not the list form `[cmd, arg]`, so both
bootstrappers of Alpine images run them:

- **cloud-init** (the `cloud-init` package; Alpine 3.22 has 24.3) runs them in its `sh` script
  and handles the whole document. The document passes `cloud-init schema`.
- **tiny-cloud** (3.2 on Alpine 3.22), the bootstrapper of the Alpine cloud images, runs each
  line with `sh -c` and writes `write_files` (with `b64` content and `permissions`). It does not
  read `hostname`: the machine keeps the host name of its metadata. The agent configuration
  names the node, so the node name is the same either way.

An example `norca-join.json`:

```json
{
  "instance": "ns-ng-scaled-3-1",
  "region": "ns",
  "urls": ["https://10.77.1.11:8490", "https://10.77.1.13:8490"],
  "endpoint": "spiffe://lab.norca/region/ns/nodescaler/join",
  "token": "<one-time token>",
  "ca_file": "/etc/norca.d/tls/ca.pem",
  "cert_file": "/etc/norca.d/tls/client.pem",
  "key_file": "/etc/norca.d/tls/client-key.pem",
  "state_file": "/etc/norca.d/tls/norca-join.state",
  "service": "norca",
  "key_store": "auto"
}
```

`urls` are the join endpoints, tried in order. `endpoint` is the only name the certificate of an
endpoint may carry (`spiffe://no-spiffe.invalid/...` without SPIFFE). `token` is removed once it
is used. `state_file` keeps the endpoints the operator last named and when the certificate was
issued, and `service` is the unit `norca-join` starts and sends `SIGHUP` after a renewal.
`key_store` is where the key is made: `file` (the default when it is missing), `tpm` or `auto`;
the operator sets it from its `join_key_store` or the `join.key_store` of the group.

**A key in the TPM.** With `tpm`, or `auto` on a machine with `/dev/tpmrm0`, `norca-join` makes
the key in the TPM 2.0 of the machine, and `client-key.pem` is a TSS2 key file
(`-----BEGIN TSS2 PRIVATE KEY-----`) that only this TPM can load; the agent signs with the TPM.
The install script needs nothing more: no TPM tools, no package. The units of `norca-join` and of
the agent run as root, which can open `/dev/tpmrm0`; an image that runs the agent as another
user must give it access to the device (the group `tss` on most distributions). Linux only.

## Where the keys are

| Key | Where | Who can read it |
| --- | --- | --- |
| The key of a node, basic mode | Made by the operator; it travels in the user data to the provider and stays in the user data of the machine in the cloud. On the node: `<tls_dir>/client-key.pem`, `0600`, root | The node's root; whoever can read the user data of the machine (the cloud account, and any process on the machine that can reach the metadata service, unless the cloud blocks it) |
| The key of a node, join mode | Made on the machine by `norca-join`; it never leaves the machine | The node's root |
| The key of a node, join mode with the TPM | Made in the TPM 2.0 of the machine; it never leaves the TPM. `<tls_dir>/client-key.pem` is a blob only this TPM can load | Nobody: the node's root can sign with it while it runs on the machine, but cannot copy it |
| The key of the node CA of the region | The bootstrap Variable `nodescaler/bootstrap/<region>` (item `node_ca_key`), encrypted at rest in Raft like every Variable | The job of the operator: give the namespace of the Variable (`node_scaling.namespace`, by default `norca-system`, the namespace of the operator modules that `apply` creates) to the operator only |
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
- A container has no TPM of its own. A privileged container sees the devices of the host: on a
  host with a TPM, `auto` and `tpm` make the keys of the container nodes in the TPM of the host
  (shared by all of them); on a host without one, `auto` keeps them in files and `tpm` does not
  join.
- A container that is cloned (`docker commit` and run again, for example) carries the identity of
  its node. The operator sees a second ready node with the name of an instance, reports it as
  **foreign** and makes it ineligible for new work, but the certificate it holds stays valid until
  it expires.

Use the docker provider for labs and hosts you trust. On a cloud, each node is a machine of its
own, built from an image with the install script, or installed by the script at boot.
