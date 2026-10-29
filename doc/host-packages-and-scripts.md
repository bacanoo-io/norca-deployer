# Host Packages and Scripts

Before Norca is configured, `apply` can install Debian packages and run bash scripts on Linux nodes. Both are skipped with a warning on Windows nodes.

Order on each node:

1. `pre_install` scripts
2. APT repositories and package installation (one `apt-get install` transaction)
3. `post_packages` scripts
4. Tuning, directories, Norca binary, CNI, certificates, `norca.hcl`, firewall, service
5. `post_install` scripts (after the agent was restarted and is healthy, or was unchanged)

An error in any step stops that node.

## APT repositories

```yaml
host_packages:
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

- Installation uses `apt-get install -y --no-install-recommends --allow-change-held-packages` with `--force-confdef --force-confold`, so local configuration files are kept. Afterwards `dpkg-query` checks the requested version.
- `hold: true` holds the package; when the plan no longer asks for the hold, the deployer releases it.
- `update_cache` (default `true`) runs `apt-get update` first; `lock_timeout` (default 300 s) waits for the dpkg lock.

### Local `.deb` files (no repository)

Not every site has an APT repository. A package with `file` is copied from the operator machine to `/var/lib/norca-deployer/debs/` and installed from there:

- the file is copied again only when its SHA-256 differs;
- `dpkg-deb -f` checks the package name and version in the file;
- when a node gets only local files and no repository, `apt-get update` is skipped, so the node may be offline;
- dependencies must come from the node's existing sources or from more `file` entries.

`validate` checks that the file exists for every architecture of the selected nodes.

## Scripts

```yaml
scripts:
  - name: prepare-data-disk
    phase: pre_install                # pre_install | post_packages | post_install
    path: scripts/prepare-data-disk.sh   # relative to the plan; or inline: |
    roles: [master]
    hosts: []
    env:
      DATA_DEVICE: /dev/sdb
      API_TOKEN: "!enc:v1:aes-gcm:..."
    timeout: 600                      # seconds (default 600)
    always: false                     # true: run on every apply
```

- Scripts run as root with `bash` under `timeout`; a non-zero exit fails the node.
- Environment variables are written to a 0600 file, sourced by the script runner and deleted afterwards. They never appear on a command line.
- After a successful run, the SHA-256 of the script and its environment is stored in `/var/lib/norca-deployer/scripts/<name>.sha256`. The script runs again only when the body or the environment changed, or with `always: true`. Write scripts so that a second run is harmless anyway.

## Validation

`validate` and `plan` check package names, Debian versions, repository names, URIs (`http`/`https` only), suites, components, architectures, pin expressions, script names and environment variable names. Because of these checks, no plan value can inject shell commands.

## Privileges

A non-root SSH user needs `connection.become: true`, with passwordless sudo or `become_password`.
