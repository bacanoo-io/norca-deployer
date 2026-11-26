# Host Packages and Scripts

Before Norca is configured, `apply` can install packages on Linux nodes and run scripts on Linux and Windows nodes. Packages are written in one block per package manager; a node takes only the block of its own package manager. Windows nodes take no packages; their scripts run in Windows PowerShell.

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
| `host_packages.apk` | package manager `apk`: Alpine 3.22+ (apk 2.14 and apk 3) | supported |

- A node whose own block selects nothing for it gets nothing.
- **A forgotten block is an error.** When a block of another manager selects packages for a node and the node's own manager has no block, `apply` fails before any node is changed: `node [10.0.0.5]: the node uses dnf, but host_packages has packages for its roles only in apt`.
- A node with `host_facts.package_manager: none` (a system without a package manager) must not get any package.
- `update_cache` and `lock_timeout` are common to all blocks.
- Each block has the keys of its own format. A key of another block's repositories (`suite` in `dnf`, `baseurl` in `apt`, `tagged` in `apt`) or an unknown key is an error of the plan, with its line.

A cluster with Debian, RHEL and Alpine nodes writes the three blocks:

```yaml
host_packages:
  apt:
    install:
      - { name: chrony }
  dnf:
    install:
      - { name: chrony }
  apk:
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

## apk repositories

```yaml
host_packages:
  apk:
    repositories:
      - name: community
        url: https://dl-cdn.alpinelinux.org/alpine/v3.22/community   # no key: signed by a key of Alpine
      - name: mycorp
        url: https://apk.mycorp.example/v3.22/main
        key_file: keys/packager-6165ee59.rsa.pub   # the key that signs its index
        tagged: true                    # packages are taken from it only when they name it
        roles: [client]
```

The repositories become:

| File on the node | Content |
| --- | --- |
| `/etc/apk/repositories.d/norca.list` | The repositories of the deployer, one line each (`@norca-<name> <url>` for a tagged one), written whole. `/etc/apk/repositories` stays yours. |
| `/etc/apk/keys/<key name>` | The key, under the name of the key file (or `key_name`) |

- **Location:** `url` is the repository: the `http`/`https` directory above `<arch>/APKINDEX.tar.gz`. apk reads the `.list` files of `/etc/apk/repositories.d` from apk 2.14 (Alpine 3.20) on, and apk 3.
- **Keys:** apk finds the key of a signed index by the name the signature carries, so a key keeps its own name: `key_file` is installed under its file name, and an inline `key` needs `key_name` (e.g. `packager-6165ee59.rsa.pub`, the name of the key that signed the index). The key is an RSA public key in PEM (`BEGIN PUBLIC KEY`), as apk keys are. Without a key the repository must be signed by a key apk has already (the keys of Alpine). Two different keys under one name are refused.
- **No credentials.** apk reads credentials only from the URL in its repositories files, which every user of the node can read, so `auth` and a user in the `url` are refused. Reach a private repository through local `.apk` files (`file`, below) or a proxy that adds the credentials.
- **Tagged repositories:** `tagged: true` writes the repository as `@norca-<name>`. apk takes a package from it only when the package names it (`repository: <name>` in `install`), so it cannot replace a package of Alpine.
- When a key or the repositories file changed, `apk update` reads the indexes, so a wrong URL or key fails at that step.
- **Dropped repositories:** the repositories file is written whole. The deployer records the keys it installed (`/var/lib/norca-deployer/apk-keys`) and removes the key of a repository the plan no longer gives the node, unless a package owns it (the keys of Alpine stay).
- A package of a tagged repository the plan dropped stays in `/etc/apk/world` as `name@norca-<name>`, and apk refuses every change while a tag of the world has no repository. The node stops with a message that names the tag: keep the repository, or remove its packages (`apk del`) first.

## apk packages

```yaml
host_packages:
  apk:
    install:
      - name: containerd
        version: "1.7.27-r0"            # version-rN; empty = what apk finds
        hold: true                      # the version stays in /etc/apk/world
        roles: [client]
      - name: mycorp-agent
        repository: mycorp              # from the tagged repository: mycorp-agent@norca-mycorp
      - name: mycorp-tool
        file: apks/mycorp-tool-1.0-r0.{arch}.apk   # local .apk, {arch} -> x86_64 | aarch64
```

- Installation is `apk add` of `name[@norca-<repository>][=version]` (with `-U` while `update_cache` is on and a repository is used) when a package is missing, not at its version, or its entry in `/etc/apk/world` is not the one the plan asks for. Afterwards the installed versions are checked (from `/lib/apk/db/installed`). `apk add` does not upgrade an installed package, so a package without a version is installed once; pin a `version` to move it with the plan.
- **Versions** are `version-rN` (`1.7.27-r0`, `1.1.1w-r0`, `0.3.0_rc1-r2`).
- **Holds are apk's own:** the version constraint of a package in `/etc/apk/world` keeps `apk upgrade` from moving it. `hold: true` keeps `name=version` there (the installed version when the plan names none). A package with a version and without `hold` is installed at that version and then released (its entry is the name alone), so `apk upgrade` may move it, as `apt upgrade` may move a package that is not held; the next `apply` installs the plan's version again.
- **`lock_timeout`** (default 300 s): every apk call waits for the lock of apk this long (`apk --wait`), then fails the node.
- **Local `.apk` files** work as local `.deb` files do: copied to `/var/lib/norca-deployer/apks/` when their SHA-256 differs and checked (the name and version of their `.PKGINFO`; packages of the format abuild builds). They are installed with `apk add --allow-untrusted --no-network`: apk refuses a file that no key of `/etc/apk/keys` signed, the file is the plan's own (its SHA-256), and the option holds for every package of the call, so the call takes nothing from the network. **The dependencies of a local file must be installed already**, from the repositories of the block (installed in the step before) or the node. apk holds a package of a local file at that file (its world entry names the file's checksum). `{arch}` is the name of Alpine (`x86_64`, `aarch64`).
- The names and versions follow the rules of apk.

## Norca as a host package

With `release.source: host-package` the norca binary comes from a package of `host_packages` (`release.package`, default `norca`). Every node must install it from the block of its own package manager; `apply` checks this for every node before it changes anything.

## Scripts

```yaml
scripts:
  - name: prepare-data-disk
    phase: pre_install                # pre_install | post_packages | post_install
    interpreter: bash                 # bash | sh | powershell (default: bash on Linux, powershell on Windows)
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

- Scripts run as root (on Windows: as the administrator of the connection) with their `interpreter` under `timeout`; a non-zero exit fails the node.
- **A script is written for Linux or for Windows.** `bash` and `sh` run on Linux nodes, `powershell` (Windows PowerShell 5.1, `powershell.exe`) on Windows nodes. A script whose interpreter is of the other platform is skipped on a node without a warning: a `powershell` script on Linux nodes, a `bash` or `sh` script on Windows nodes.
- **Without `interpreter`** a script runs with `bash` on Linux nodes and with `powershell` on Windows nodes. A plan with Linux and Windows nodes must therefore tell which one a script is for: `validate` refuses a script without `interpreter` and `families` whose `roles` and `hosts` select nodes of both platforms. Set `interpreter`, or `families` (`[windows]`, or Linux families).
- `families` limits a script to the nodes of those families; other nodes skip it. Families must fit the interpreter: `interpreter: bash` or `sh` with `families: [windows]`, `interpreter: powershell` with a Linux family, and `families` naming `windows` and a Linux family without an interpreter are errors of `validate`.
- `interpreter: sh` runs the script with the `/bin/sh` of the node (dash on Debian and Ubuntu, bash on RHEL, busybox ash on Alpine); write such a script without bash features. Alpine has no bash unless `host_packages.apk` installs it.
- A `bash` script on a node without bash is an error of that node before anything is changed: install `bash` with `host_packages` (for a script after `pre_install`) or set `interpreter: sh`.
- Environment variables are written to a private file (Linux: mode 0600; Windows: a directory only the Administrators and SYSTEM can open), read by the script runner and deleted afterwards. They never appear on a command line.
- After a successful run, the SHA-256 of the script and its environment is stored in `/var/lib/norca-deployer/scripts/<name>.sha256` (Windows: `C:\ProgramData\norca-deployer\scripts\<name>.sha256`). The script runs again only when the body or the environment changed, or with `always: true`. Write scripts so that a second run is harmless anyway.

How a script runs on a Windows node (the environment file, the exit code, the timeout): [Windows nodes](windows.md#scripts).

## Validation

`validate` and `plan` check package names and versions (Debian rules in `apt`, RPM rules in `dnf`, apk rules in `apk`), repository names, URIs and base URLs (`http`/`https` only), suites, components, architectures, pin expressions, dnf priorities and package filters, apk keys and their names, the tagged repositories of apk packages, script names, interpreters, families and environment variable names. Because of these checks, no plan value can inject shell commands or repository options. The values of script environments are not restricted: they are quoted for the shell, or as PowerShell literals on Windows, and reach the script as they are.

## Privileges

A non-root SSH user needs `connection.become: true`, with passwordless `sudo` or `doas` (`connection.become_method`; Alpine often has only `doas`: `permit nopass <user> as root`), or `sudo` with `become_password`.
