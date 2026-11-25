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
| `host_packages.dnf` | RHEL 9+, Rocky Linux, AlmaLinux | planned; refused for now |
| `host_packages.apk` | Alpine | planned; refused for now |

- A node whose own block selects nothing for it gets nothing.
- **A forgotten block is an error.** When a block of another manager selects packages for a node and the node's own manager has no block, `apply` fails before any node is changed: `node [10.0.0.5]: the node uses dnf, but host_packages has packages for its roles only in apt`.
- A node with `host_facts.package_manager: none` (a system without a package manager) must not get any package.
- `update_cache` and `lock_timeout` are common to all blocks.

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
- The names and versions follow the rules of Debian; a key of another format in the `apt` block (such as `baseurl`) is not a key of the block.

### Local `.deb` files (no repository)

Not every site has an APT repository. A package with `file` is copied from the operator machine to `/var/lib/norca-deployer/debs/` and installed from there:

- `{arch}` in `file` is the Debian name of the node's architecture (`amd64`, `arm64`, `armhf`, `i386`);
- the file is copied again only when its SHA-256 differs;
- `dpkg-deb -f` checks the package name and version in the file;
- when a node gets only local files and no repository, `apt-get update` is skipped, so the node may be offline;
- dependencies must come from the node's existing sources or from more `file` entries.

`validate` checks that the file exists for every architecture of the selected nodes.

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
- `interpreter: sh` runs the script with the `/bin/sh` of the node (dash on Debian and Ubuntu, busybox ash on Alpine); write such a script without bash features.
- A `bash` script on a node without bash is an error of that node before anything is changed: install `bash` with `host_packages` (for a script after `pre_install`) or set `interpreter: sh`.
- `families` limits a script to the nodes of those families; other nodes skip it.
- Environment variables are written to a 0600 file, sourced by the script runner and deleted afterwards. They never appear on a command line.
- After a successful run, the SHA-256 of the script and its environment is stored in `/var/lib/norca-deployer/scripts/<name>.sha256`. The script runs again only when the body or the environment changed, or with `always: true`. Write scripts so that a second run is harmless anyway.

## Validation

`validate` and `plan` check package names, Debian versions, repository names, URIs (`http`/`https` only), suites, components, architectures, pin expressions, script names, interpreters, families and environment variable names. Because of these checks, no plan value can inject shell commands.

## Privileges

A non-root SSH user needs `connection.become: true`, with passwordless `sudo` or `doas` (`connection.become_method`), or `sudo` with `become_password`.
