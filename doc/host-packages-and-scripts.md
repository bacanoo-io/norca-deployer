# Host Packages and Scripts

Before Norca is configured, `apply` can install packages and run scripts on Linux nodes. Packages are written in one block per package manager; a node takes only the block of its own package manager. Windows nodes take no packages and skip the scripts with a warning.

Order on each node:

1. `pre_install` scripts
2. Repositories and package installation (one install transaction)
3. `post_packages` scripts
4. Tuning, directories, Norca binary, CNI, certificates, `norca.hcl`, firewall, service
5. `post_install` scripts (after the agent was restarted and is healthy, or was unchanged)

An error in any step stops that node.

## Which block a node takes

Before it changes anything, the deployer finds out what every node is (see [`host_facts`](configuration.md#host_facts)): its family (Debian and Ubuntu are the `debian` family) and its package manager.

| Block | Nodes | Status |
| --- | --- | --- |
| `host_packages.apt` | package manager `apt`: Debian 11+, Ubuntu 20.04+ | supported |
| `host_packages.dnf` | package manager `dnf`: RHEL 9+, Rocky Linux 9+, AlmaLinux 9+ (dnf 4 and dnf5) | supported |
| `host_packages.apk` | Alpine | planned; refused for now |

- A node whose own block selects nothing for it gets nothing.
- **A forgotten block is an error.** When a block of another manager selects packages for a node and the node's own manager has no block, `apply` fails before any node is changed: `node [10.0.0.5]: the node uses dnf, but host_packages has packages for its roles only in apt`.
- A node with `host_facts.package_manager: none` (a system without a package manager) must not get any package.
- `update_cache` and `lock_timeout` are common to all blocks.
- Each block has the keys of its own format. A key of the other block's repositories (`suite` in `dnf`, `baseurl` in `apt`) or an unknown key is an error of the plan, with its line.

A cluster with Debian and RHEL nodes writes both blocks:

```yaml
host_packages:
  apt:
    install:
      - { name: chrony }
  dnf:
    install:
      - { name: chrony }
```

Plans of earlier deployer versions had `repositories` and `install` directly under `host_packages`; such a plan is refused with a message that names `host_packages.apt`. Move both lists into `apt` unchanged.

## APT repositories

```yaml
host_packages:
  apt:
    repositories:
      - name: docker
        uri: https://download.docker.com/linux/debian
        suite: bookworm
        components: [stable]
        architectures: [amd64]
        key_file: keys/docker.asc
        pin_priority: 600
        roles: [client]
```

Each repository becomes:

| File on the node | Content |
| --- | --- |
| `/etc/apt/sources.list.d/norca-<name>.sources` | deb822 source with `Signed-By` |
| `/etc/apt/keyrings/norca-<name>.asc` or `.gpg` | The key (ASCII-armored or binary), uploaded from the operator machine |
| `/etc/apt/preferences.d/norca-<name>.pref` | Pin, when `pin_priority` is set |
| `/etc/apt/auth.conf.d/norca-<name>.conf` (0600) | Credentials, when `auth` is set |

**Keys:** exactly one of `key` (inline) or `key_file` is required; unsigned repositories are not supported. The node never downloads a key.

**Flat repositories:** `suite: "./"` without `components`.

**Pins:** `pin_priority` writes an APT preference for the repository's packages (`pin_packages`, default all). Without `pin_release` the pin is `Pin: origin "<host>"`, which applies to **every** repository on that host. Use `pin_release` (e.g. `n=bookworm-backports` or `o=MyCorp,a=stable`) to target one repository. A priority above 1000 also allows downgrades.

**Credentials:** only in `auth` (the password may be an `!enc:` secret). Credentials in the URI are rejected because source files are world-readable.

## Packages

```yaml
host_packages:
  apt:
    install:
      - name: docker-ce
        roles: [client]                 # master | client; empty = all nodes
      - name: mycorp-agent
        version: "2.4.1-1"              # exact Debian version; empty = candidate
        hold: true                      # apt-mark hold
        hosts: [norca-worker-01]        # host or hostname; empty = all nodes of the roles
      - name: mycorp-tool
        file: debs/mycorp-tool_1.0-1_{arch}.deb   # local .deb, {arch} -> amd64 | arm64 | armhf | i386
        sha256: ""                      # optional; only for a file name without {arch}
```

- Installation uses `apt-get install -y --no-install-recommends --allow-change-held-packages` with `--force-confdef --force-confold`, so local configuration files are kept. Afterwards the installed versions are checked against the requested ones.
- `hold: true` holds the package; when the plan no longer asks for the hold, the deployer releases it.
- `update_cache` (default `true`) runs `apt-get update` first; `lock_timeout` (default 300 s) waits for the dpkg lock.
- The names and versions follow the rules of Debian.

### Local `.deb` files (no repository)

Not every site has an APT repository. A package with `file` is copied from the operator machine to `/var/lib/norca-deployer/debs/` and installed from there:

- `{arch}` in `file` is the Debian name of the node's architecture (`amd64`, `arm64`, `armhf`, `i386`);
- the file is copied again only when its SHA-256 differs;
- `dpkg-deb -f` checks the package name and version in the file;
- when a node gets only local files and no repository, `apt-get update` is skipped, so the node may be offline;
- dependencies must come from the node's existing sources or from more `file` entries.

`validate` checks that the file exists for every architecture of the selected nodes.

## dnf repositories

```yaml
host_packages:
  dnf:
    repositories:
      - name: docker
        baseurl: https://download.docker.com/linux/rhel/$releasever/$basearch/stable
        key_file: keys/docker.asc
        priority: 10                    # 1-99, lower wins; dnf's default is 99
        include: [containerd.io, docker-ce, docker-ce-cli]   # includepkgs
        exclude: []                     # excludepkgs
        roles: [client]
```

Each repository becomes:

| File on the node | Content |
| --- | --- |
| `/etc/yum.repos.d/norca-<name>.repo` | The repository `norca-<name>`: `baseurl` or `metalink`, `enabled=1`, `gpgcheck=1`, `gpgkey=file://…`, `priority`, `includepkgs`, `excludepkgs`, and the credentials of `auth` (then the file is 0600) |
| `/etc/pki/rpm-gpg/norca-<name>.asc` | The key, uploaded from the operator machine |

- **Location:** exactly one of `baseurl` or `metalink`, an `http`/`https` URL; the dnf variables `$releasever` and `$basearch` may be used.
- **Keys:** exactly one of `key` or `key_file`, an ASCII-armored OpenPGP public key (rpm imports no binary key). Every repository is checked with its key (`gpgcheck=1`); unsigned repositories are not supported.
- **Credentials:** only in `auth` (the password may be an `!enc:` secret and must be one line). dnf reads them from the repository file, which is then readable by root only. Credentials in the URL are rejected. A repository with credentials cannot be used by the install script of a scaled machine at boot (see [node scaling machines](node-scaling-machines.md)).
- When the key or the file of a repository changed, `dnf makecache --repo=norca-<name>` reads its metadata, so a wrong URL or key fails at that step.
- The files of the deployer's repositories that the plan no longer gives a node (`norca-*.repo` and their keys) are removed from it, so a dropped repository does not stay enabled.

## dnf packages

```yaml
host_packages:
  dnf:
    install:
      - name: containerd.io
        version: "1.7.27-3.1.el9"       # [epoch:]version-release; empty = what dnf finds
        hold: true                      # a versionlock entry
        roles: [client]
      - name: docker-ce
        version: "3:27.3.1-1.el9"       # with the epoch
      - name: mycorp-tool
        file: rpms/mycorp-tool-1.0-1.{arch}.rpm   # local .rpm, {arch} -> x86_64 | aarch64
```

- Installation uses `dnf -y install --setopt=install_weak_deps=False` (no weak dependencies, as apt installs no recommended packages), with `--refresh` while `update_cache` is on and a repository is used. Afterwards the installed versions are checked with `rpm -q`.
- **Versions** are `[epoch:]version-release`. A version without an epoch matches the installed version of any epoch (`27.3.1-1.el9` matches `3:27.3.1-1.el9`).
- **A package without a version is installed once.** `dnf install` does not upgrade an installed package (unlike `apt-get install`), so such a package stays at the version it was installed with; pin a `version` to move it with the plan.
- **`hold: true`** adds a versionlock entry for the installed version; when the plan no longer asks for the hold, the deployer deletes it. The versionlock command is installed by a step before the holds when a package is held: the `python3-dnf-plugin-versionlock` plugin of dnf 4 (RHEL 9 and 10), nothing on dnf5, which has its own. A held package whose version changes in the plan is released for the install and held again afterwards.
- **`lock_timeout`** (default 300 s): before every dnf call the deployer waits until no other `dnf`, `yum` or `rpm` process runs, at most this long, and then fails the node. dnf 4 waits for its own lock without an end, and dnf and rpm stop at once on the lock of the rpm database, so this wait is the deployer's.
- **Local `.rpm` files** work as local `.deb` files do: copied to `/var/lib/norca-deployer/rpms/` when their SHA-256 differs, checked with `rpm -qp` (name and version), installed with `dnf install /path/file.rpm` (dnf does not check the signature of a local file; the SHA-256 of the plan does). `{arch}` is the RPM name of the architecture (`x86_64`, `aarch64`). A node with only local files gets no `--refresh`, but dnf still reads the metadata of the node's enabled repositories when it has none cached.
- The names and versions follow the rules of RPM.

### SELinux

On a RHEL node where SELinux is enforcing or permissive, `apply` restores the SELinux labels of what it wrote (`restorecon`), after the service and before the agent is started, when it changed something on the node. The deployer uploads a file next to its place and moves it there, and a moved file keeps the label of where it came from, which systemd refuses for the unit and the binary. The labels of the binary, the unit, the configuration and certificates, the CNI directories, the tuning and DNS files and the dnf repository files are restored with everything below them; the data, log and application volume directories get only their own label (their contents belong to the agent and the allocations).

Under SELinux enforcing `apply` prints one warning per node and goes on: Norca under SELinux enforcing is not verified yet; set SELinux to permissive if the agent or its drivers are denied.

## Norca as a host package

With `release.source: host-package` the norca binary comes from a package of `host_packages` (`release.package`, default `norca`). Every node must install it from the block of its own package manager; `apply` checks this for every node before it changes anything.

## Scripts

```yaml
scripts:
  - name: prepare-data-disk
    phase: pre_install                # pre_install | post_packages | post_install
    interpreter: bash                 # bash (default) | sh
    families: [debian]                # optional: debian | rhel | alpine | windows
    path: scripts/prepare-data-disk.sh   # relative to the plan; or inline: |
    roles: [master]
    hosts: []
    env:
      DATA_DEVICE: /dev/sdb
      API_TOKEN: "!enc:v1:aes-gcm:..."
    timeout: 600                      # seconds (default 600)
    always: false                     # true: run on every apply
```

- Scripts run as root with their `interpreter` under `timeout`; a non-zero exit fails the node.
- `interpreter: sh` runs the script with the `/bin/sh` of the node (dash on Debian and Ubuntu, bash on RHEL, busybox ash on Alpine); write such a script without bash features.
- A `bash` script on a node without bash is an error of that node before anything is changed: install `bash` with `host_packages` (for a script after `pre_install`) or set `interpreter: sh`.
- `families` limits a script to the nodes of those families; other nodes skip it.
- Environment variables are written to a 0600 file, sourced by the script runner and deleted afterwards. They never appear on a command line.
- After a successful run, the SHA-256 of the script and its environment is stored in `/var/lib/norca-deployer/scripts/<name>.sha256`. The script runs again only when the body or the environment changed, or with `always: true`. Write scripts so that a second run is harmless anyway.

## Validation

`validate` and `plan` check package names and versions (Debian rules in `apt`, RPM rules in `dnf`), repository names, URIs and base URLs (`http`/`https` only), suites, components, architectures, pin expressions, dnf priorities and package filters, script names, interpreters, families and environment variable names. Because of these checks, no plan value can inject shell commands or repository options.

## Privileges

A non-root SSH user needs `connection.become: true`, with passwordless `sudo` or `doas` (`connection.become_method`), or `sudo` with `become_password`.
