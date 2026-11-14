# Configuration Reference

A plan describes one region. It is a YAML (`.yaml`, `.yml`) or HCL (`.hcl`) file; both use the same keys. Relative paths in the plan (keys, `.deb` files, scripts, CA files, token files) are resolved against the directory of the plan.

Complete examples: [single-node.yaml](../examples/nodes/single-node.yaml), [cluster.yaml](../examples/nodes/cluster.yaml) and their [HCL versions](../examples).

## YAML and HCL

| YAML | HCL |
| --- | --- |
| `metadata:` with nested keys | `metadata { ... }` block |
| `masters:` list of objects | one `masters { ... }` block per entry |
| `drivers: [docker, exec]` | `drivers = ["docker", "exec"]` |
| `env: {A: "1"}` | `env { A = "1" }` |
| `inline: \|` block text | `inline = <<EOT ... EOT` heredoc |

An unknown key or a block repeated where only one is allowed is an error.

## Top level

| Key | Default | Description |
| --- | --- | --- |
| `version` | `norca.io/v1alpha1` | Plan format version |
| `kind` | `DeploymentPlan` | Document kind |
| `metadata` | | Cluster name, region, datacenter |
| `release` | | Norca binary to install |
| `cni` | | CNI plugins |
| `upgrade` | | Rolling restart settings |
| `paths` | | Directory layout per platform |
| `tuning` | | Kernel, firewall, service limits |
| `network` | | Addresses and ports |
| `security` | | TLS, gossip, PKI, ACLs |
| `backup` | | Built-in server backups |
| `metrics_store` | | Retentions and disk caps of the metrics history of the servers |
| `bus` | | Settings of the system bus of the servers |
| `federation` | | Multi-region settings |
| `consul` | | A Consul agent on every node; without it the agents do not use Consul |
| `host_packages` | | Debian packages |
| `scripts` | | Bash scripts |
| `client_defaults` | | Client settings merged into every node that runs a client |
| `node_pools` | | Node pool descriptions |
| `node_scaling` | | The bootstrap and the network ranges for the node scaling operator |
| `secrets` | | Secrets and Variables written after the cluster is up |
| `packs` | | Pack registries and releases; `packs.gitops` holds GitOps bindings |
| `gitops` | | GitOps bindings, the same as `packs.gitops` |
| `inventory` | | Servers and clients |

## metadata

| Key | Default | Description |
| --- | --- | --- |
| `name` | `norca-cluster` | Cluster name; the CN of a generated CA |
| `region` | `global` | Region of every node in the plan |
| `datacenter` | `dc1` | Default datacenter; `inventory.*.datacenter` overrides it |

## release

| Key | Default | Description |
| --- | --- | --- |
| `version` | `v0.1.0` | Release tag (`v1.2.3`) for `official` and `custom-url` |
| `source` | `official` | `official`: GitHub release archive verified with `SHA256SUMS`. `custom-url`: same layout under `download_base_url`. `local-path`: binary or archive from the operator machine. `host-package`: the binary comes from a Debian package installed by `host_packages` |
| `download_base_url` | GitHub releases of Norca | Base URL for `official` / `custom-url` |
| `local_binary_path` | | File for `local-path`: a binary or a `.tar.gz` / `.zip` archive; `{os}` and `{arch}` are replaced |
| `checksum_sha256` | | Optional digest the artifact must match |
| `package` | `norca` | Package name for `host-package`; it must be installed on every node |

## cni

| Key | Default | Description |
| --- | --- | --- |
| `install` | `true` | Install the CNI reference plugins on Linux nodes that run a client |
| `version` | `v1.9.1` | Plugins release |
| `source` | `official` | `official`, `custom-url` or `local-path` |
| `download_base_url` | GitHub releases of containernetworking/plugins | For `official` / `custom-url` |
| `local_archive_path` | | Archive for `local-path`; `{arch}` is replaced |
| `checksum_sha256` | | Optional digest |

## upgrade

| Key | Default | Description |
| --- | --- | --- |
| `max_parallel_clients` | `1` | Clients restarted at the same time |
| `health_timeout` | `180` | Seconds a restarted agent has to become healthy |

## paths

`paths.linux` and `paths.windows` take `bin_dir`, `config_dir`, `data_dir`, `log_dir`, `tls_dir`, `cni_bin_dir`, `cni_config_dir` and `apps_dir`. The defaults are listed in [Networking and tuning](networking-and-tuning.md#paths-and-volumes). A node can override them with `path_overrides`.

## tuning

| Key | Default | Description |
| --- | --- | --- |
| `apply_sysctl` | `false` | Kernel modules and sysctl parameters (Linux) |
| `apply_firewall_rules` | `false` | Open the agent ports (Linux nodes; Windows is not managed) |
| `open_ephemeral_ports` | `false` | Also open 20000-32000 on nodes running a client |
| `firewall.cluster_cidrs` | any | Sources for RPC and Serf (the node network) |
| `firewall.api_cidrs` | any | Sources for the HTTP API and UI |
| `firewall.ephemeral_cidrs` | `cluster_cidrs` | Sources for the allocation ports |
| `firewall.default_deny` | `false` | Turn ufw on and deny other incoming traffic; SSH stays open |
| `firewall.extra_ports[]` | | Static workload ports: `port`, `proto`, `cidrs`, `roles` |
| `setup_logrotate` | `false` | Agent log file in `log_dir` with rotation |
| `windows_long_paths` | `false` | `LongPathsEnabled` and execution policy on Windows |
| `file_descriptor_limit` | `65536` | `LimitNOFILE` of the systemd unit |
| `cni_mtu_mode` | `auto` | Reserved; not applied yet |

See [Networking and tuning](networking-and-tuning.md#firewall) for the firewall rules.

## network

| Key | Default | Description |
| --- | --- | --- |
| `bind_address` | | Listen address; empty: the cluster address of each node (HTTP also `127.0.0.1`) |
| `dynamic_ports.min` / `dynamic_ports.max` | 20000 / 32000 | Allocation port range; the firewall opens the same range |
| `ports.http` / `ports.rpc` / `ports.serf` | `4646` / `4647` / `4648` | Agent ports |
| `ports.bus` / `ports.bus_cluster` | `4649` / `4650` | Ports of the system bus of the servers (NATS embedded in Norca): consumers, and the routes between the servers |
| `interface_prefix` | | Advertise the address of the first interface whose name starts with this |
| `interface_suffix` | | Same, matching the end of the name |
| `vpn.enabled` | `false` | Use the VPN interface settings below for the advertise address |
| `vpn.interface_name` | | Exact interface name (e.g. `wg0`) |
| `vpn.interface_prefix` / `vpn.interface_suffix` | | Interface name match |
| `advertise_mode`, `vpn.cidr_filter`, `vpn.fallback_to_default` | | Reserved; not applied yet |

## security

| Key | Default | Description |
| --- | --- | --- |
| `tls_enabled` | `false` | mTLS for HTTP and RPC |
| `verify_server_hostname` | `false` | Check role names on RPC |
| `verify_https_client` | `false` | Require client certificates on the HTTP API; issues `certs/cli.pem` |
| `gossip_encryption_key` | | `AUTO_GENERATE`, a base64 key (16/24/32 bytes), an `!enc:` secret, or empty |
| `pki.ca_cert_path`, `pki.ca_key_path` | | Use an existing CA |
| `pki.generate_worker_pool_cert` | `false` | Issue `certs/client-pool.pem` |
| `pki.server_additional_dns_sans` | | Extra DNS names in server certificates |
| `pki.server_additional_ip_sans` | | Extra IP addresses in server certificates |
| `pki.spiffe.enabled` | `false` | SPIFFE IDs (URI SANs) in all certificates |
| `pki.spiffe.trust_domain` | `<metadata.name>.norca` | SPIFFE trust domain |
| `pki.distrusted_ca_files` | | PEM files of CA certificates the servers no longer trust (a node CA whose key was lost); see [security](security.md#shutting-out-a-node-ca) |
| `acl.enabled` | `false` | Enable ACLs |
| `acl.auto_bootstrap` | `false` | Bootstrap once and store the token |
| `acl.management_token_output` | `.norca-deployer/management-token` | Token file |
| `acl.policies[]` | | `name`, `description`, `rules` or `rules_file`, `job` (`namespace`, `job_id`, `group`, `task`); see [ACL policies](secrets-and-policies.md#acl-policies) |
| `auto_generate_certificates` | | Reserved; certificates are always generated when TLS is on |

See [Security](security.md).

## backup

Built-in snapshots of the server state, written to the server block of `norca.hcl`.

| Key | Description |
| --- | --- |
| `enabled` | Turn backups on |
| `schedule` | Cron expression |
| `prefer_observer` | The leader asks an observer server (`server_settings.role: observer`) to take the backups; the leader takes them when no observer can. A `directory` destination must then be shared storage on every server |
| `destination.name` | Destination name (required) |
| `destination.type` | `s3` (default) or `directory` |
| `destination.endpoint`, `bucket`, `prefix`, `force_path_style` | S3 settings; `bucket` is required for `s3` |
| `destination.path` | Absolute directory on the servers for `directory` |
| `encryption.recipients` | age recipients; at least one (Norca encrypts every backup) |
| `retention.keep_hourly`, `keep_daily`, `keep_weekly`, `keep_monthly` | Snapshots to keep |

## metrics_store

The metrics store of the servers: the history of the task group usage, the cluster capacity, the scheduler and the custom objects that every server keeps in its data directory, and in a second store with its own retention and disk cap the host metrics: the host statistics of the clients (`norca.node.*`), the Raft log and the RPC of the servers (`norca.server.*`) and their system bus (`norca.bus.*`). The deployer writes the same `metrics_store` block into the server block of `norca.hcl` on every server, so that the servers keep the same history. Without this section, or with no key set, no block is written and the defaults of Norca apply.

| Key | Default | Description |
| --- | --- | --- |
| `retention` | `30d` | How long the history is kept: `1h` to `30d` (`720h`), as a duration (`168h`) or in days (`7d`) |
| `max_disk_size` | `4GiB` | Disk cap of the store on each server (`512MiB`, `4GiB`); above it the oldest points go first, before their retention |
| `host_retention` | `7d` | How long the host metrics are kept, in the range and form of `retention` |
| `host_max_disk_size` | `1GiB` | Disk cap of the store of the host metrics on each server |

The plan is checked like Norca checks the block: a retention outside the range or a size that is not one is an error. A changed value is a changed configuration: `apply` restarts the servers one by one ([How apply works](how-apply-works.md)). Norca itself also applies all four values on a configuration reload (`SIGHUP`) without a restart. See the [`metrics_store` block](https://bacanoo-io.github.io/norca-doc/docs/configuration/metrics_store) of Norca.

```yaml
metrics_store:
  retention: 14d
  max_disk_size: 4GiB
  host_retention: 7d
  host_max_disk_size: 1GiB
```

## bus

Settings of the system bus of the servers (the embedded NATS). The deployer writes the same `bus` block into the server block of `norca.hcl` on every server. Without this section, or with no key set, no block is written and the defaults of Norca apply.

| Key | Default | Description |
| --- | --- | --- |
| `consul_kv_max_bytes` | `256MiB` | Size limit of the bucket of the Consul KV of the Consul-compatible API on the bus (`512MiB`, `1GiB`). A write beyond it fails; no key is dropped |

The plan checks that the value is a size. Whether it fits into the storage of the bus (4 GiB by default) together with the events, the pack push notices and the usage reports is checked by Norca at start, which names what each takes. **Do not set a `bus` block in `server_settings.extra_hcl` as well**: the server block would have two. A changed value is a changed configuration: `apply` restarts the servers one by one ([How apply works](how-apply-works.md)). See the [`bus` block](https://bacanoo-io.github.io/norca-doc/docs/configuration/bus) of Norca.

```yaml
bus:
  consul_kv_max_bytes: 512MiB
```

## federation

| Key | Description |
| --- | --- |
| `authoritative_region` | Region that owns the ACLs (required) |
| `join` | Servers of other regions: `host` or `host:serf_port` |
| `replication_token_file` | Management token of the authoritative region; required in a replica region with ACLs |

See [Multi-region](multi-region.md).

## consul

Norca does not need Consul. Without this section every agent gets `auto_advertise = false`, so it does not try to register itself in a Consul agent on `127.0.0.1:8500`. With it, the agents register in the Consul agent of their node, for example the one of the `consul` pack. The servers are always joined by the plan, never through Consul (`server_auto_join` and `client_auto_join` are always `false`).

| Key | Default | Description |
| --- | --- | --- |
| `address` | `127.0.0.1:8500` | HTTP API of the local Consul agent |
| `auto_advertise` | `true` | Register the agents as Consul services |
| `token_file` | | Local file (relative to the plan) with the Consul ACL token of the agents, for a Consul whose ACLs deny by default |

## host_packages

| Key | Default | Description |
| --- | --- | --- |
| `update_cache` | `true` | `apt-get update` before installing |
| `lock_timeout` | `300` | dpkg lock wait in seconds |
| `repositories[]` | | `name`, `uri`, `suite`, `components`, `architectures`, `key` or `key_file`, `roles`, `pin_priority`, `pin_packages`, `pin_release`, `auth.username`, `auth.password` |
| `install[]` | | `name`, `version`, `hold`, `file`, `sha256`, `roles`, `hosts` |

See [Host packages and scripts](host-packages-and-scripts.md).

## scripts[]

| Key | Default | Description |
| --- | --- | --- |
| `name` | | Unique name |
| `phase` | | `pre_install`, `post_packages` or `post_install` |
| `path` / `inline` | | Script file (relative to the plan) or body; exactly one |
| `roles` | all | `master`, `client` |
| `hosts` | all | Host addresses or hostnames |
| `env` | | Environment variables; values may be `!enc:` secrets |
| `timeout` | `600` | Seconds |
| `always` | `false` | Run on every apply |

## secrets

| Key | Default | Description |
| --- | --- | --- |
| `store` | `builtin` | `builtin` or `external` (secrets of the built-in store are then skipped) |
| `bootstrap[]` | | `type` (`secret`, `variable`), `path`, `namespace`, `items`, `overwrite`; see [Bootstrap secrets](secrets-and-policies.md) |

## packs

`registry` (one) or `registries` (several) take the keys below.

| Key | Default | Description |
| --- | --- | --- |
| `registry.name` | | Registry name in the cluster |
| `registry.type` | `git` | `git`, `oci` or `dir` |
| `registry.url` | | Source; `builtin` with `type: dir` for the packs of Norca |
| `registry.ref` | | Tag or branch |
| `registry.sync_interval` | | Periodic sync, e.g. `1h` |
| `deploy[].name` | | Release name |
| `deploy[].registry` | the only registry | Registry of the pack; required with several registries |
| `deploy[].pack` | | Pack name |
| `deploy[].version` | | Pack version |
| `deploy[].enabled` | `false` | Deploy this entry |
| `deploy[].values` | | Pack variables |

See [Packs](packs.md).

## node_scaling

Nodes that a node scaling operator (`norca-nodescaler` of `norca-platform`) creates by itself are
not installed by the deployer: the operator signs the certificate of each new node with the
**node CA** of the region and hands the machine a bootstrap. With `node_scaling.enabled` the
deployer writes, after `apply`, what the operator needs:

- the Variable `nodescaler/bootstrap/<region>`: the cluster CA (`ca`), the node CA and its key
  (`node_ca`, `node_ca_key`, from `pki issue-node-ca`), the agent configuration template of a
  scaled node (`agent_config`), the trust domain, the validity and the paths of the files, and
  with `machine` where `norca-join` is (`join_binary`) and the install script (`install_script`);
- the Variable `nodescaler/network/<region>`: `cluster_cidrs` and `api_cidrs` of
  `tuning.firewall`, against which the operator checks the network of a node group.

| Key | Default | Description |
| --- | --- | --- |
| `enabled` | `false` | Write the Variables |
| `namespace` | `default` | Namespace of the Variables. The bootstrap holds the key of the node CA: give read access to the operator only |
| `validity` | `60d` | Validity of a node certificate, in days (`60d`) or hours (`720h`); the node CA ends it at the latest |
| `client` | | Client settings of a scaled node (drivers, `enable_raw_exec`, `extra_hcl`, reserved resources, host networks), over `client_defaults` |
| `machine` | | What a scaled machine needs on its disk, see below |

`node_scaling.machine` describes the install script of a scaled machine
(`norca-deployer node-scaling install-script`); see
[Machines of the node scaling operator](node-scaling-machines.md). It is checked even when
`enabled` is `false`, since an image can be built before the operator runs.

| Key | Default | Description |
| --- | --- | --- |
| `install` | `image` | `image`: the script runs when the image of the provider is built. `boot`: `apply` writes the script into the bootstrap Variable (item `install_script`) and the bootstrap of the operator runs it at the first boot. `boot` needs a release and CNI plugins the machine can download (not `local-path`) and no APT repository with `auth` |
| `arch` | `amd64` | Architecture of the machines: `amd64`, `arm64`, `arm` or `386`. The script of the bootstrap Variable is for this one; `install-script --arch` overrides it |
| `join_binary` | `/usr/local/bin/norca-join` | Where `norca-join` is on the machines. Written into the bootstrap Variable (item `join_binary`) only when set |
| `join_download.url` | | Where the script downloads `norca-join` from (http or https). Without it the script only checks that the binary is there |
| `join_download.checksum_sha256` | | SHA-256 of that file (required with `url`) |

```yaml
node_scaling:
  enabled: true
  machine:
    install: image
    join_binary: /usr/local/bin/norca-join
    join_download:
      url: https://downloads.example.com/norca-join/v0.3.0/norca-join_linux_amd64
      checksum_sha256: 9f2c...  # 64 hex characters
```

The template is the configuration of a client of the plan without what belongs to one node: the
name, datacenter, pool, class and meta come from the node group, and the agent takes its own
private address (the address of a machine that does not exist yet is not known). The meta of
`client_defaults` is not used: a scaled node gets its meta from its node group. Consul is not
set up on a scaled node. It needs `security.tls_enabled` and `metadata.region`. Without a node
CA the bootstrap is not written and `apply` says so:

```shell
norca-deployer pki issue-node-ca -c cluster.yaml --region eu
norca-deployer apply -c cluster.yaml
```

```yaml
node_scaling:
  enabled: true
  validity: 60d
  client:
    drivers: [docker, exec]
```

A Variable is written again only when its items changed since the last `apply`.

## inventory

`inventory.masters` lists the servers (at least one), `inventory.clients` the dedicated clients, and `inventory.decommission` the nodes to remove (see below). Each server or client entry:

| Key | Default | Description |
| --- | --- | --- |
| `host` | | SSH address (required, unique) |
| `hostname` | | Node name (unique) |
| `platform` | `linux` | `linux` or `windows` |
| `arch` | `amd64` | `amd64`, `arm64`, `arm` or `386` |
| `cluster_address` | `host` | Address used by the other nodes |
| `datacenter` | `metadata.datacenter` | Datacenter of the node |
| `connection` | | See below |
| `server_settings` | | Servers only |
| `client_settings` | | Clients; on a server it also runs a client on that node |
| `path_overrides` | | Paths for this node |

### decommission

Nodes to take out of the cluster and uninstall; see [Adding and removing nodes](adding-and-removing-nodes.md).

| Key | Default | Description |
| --- | --- | --- |
| `hostname` / `host` | | The node; for a node in the installed list one of them is enough |
| `role` | from the installed list | `master` or `client`; required for a node not in the list |
| `connection` and other node keys | from the installed list | Required for a node not in the list |
| `drain_deadline` | `600` | Seconds a client's allocations have to move |
| `purge_data` | `false` | Also delete `data_dir`, `log_dir` and `apps_dir` |

### connection

| Key | Default | Description |
| --- | --- | --- |
| `mode` | `ssh` | `ssh` or `local` (the operator machine itself) |
| `ssh_user` | | SSH user |
| `ssh_port` | `22` | SSH port |
| `ssh_key_path` | | Private key file (`~` is expanded) |
| `ssh_private_key` | | Private key inline (may be `!enc:`) |
| `ssh_key_passphrase` | | Key passphrase (may be `!enc:`) |
| `ssh_password` | | Password (may be `!enc:`) |
| `become` | `false` | Use `sudo` (Linux only) |
| `become_password` | | sudo password (may be `!enc:`) |
| `host_key_checking` | `tofu` | `tofu`, `strict` or `off` |
| `known_hosts_file` | | Known hosts file for `strict` |

### server_settings

| Key | Default | Description |
| --- | --- | --- |
| `role` | `voter` | Role of the server in the Raft cluster: `voter`, `standby` (gets a vote only when a voter fails, with autopilot `target_voters`) or `observer` (never votes) |
| `bootstrap_expect` | number of masters with the role `voter` | Must equal the number of masters with the role `voter` |
| `raft_multiplier` | `1` | Raft timing multiplier |
| `log_level` | `INFO` | Agent log level |
| `extra_hcl` | | Lines appended inside the server block |

### client_settings

| Key | Default | Description |
| --- | --- | --- |
| `drivers` | `[docker, exec]` | `driver.allowlist` |
| `enable_raw_exec` | `false` | Enable the `raw_exec` plugin |
| `raw_exec_allowed_devices` | | Devices `raw_exec` tasks may use beyond the default ones (cgroups v2 filters them): paths under `/dev`, e.g. `[/dev/fuse]` for FUSE mounts, or `["*"]`; written as `allowed_devices` of the `raw_exec` plugin |
| `network_interface` | | Interface for allocation addresses |
| `gc_disk_usage_threshold` | | Allocation GC threshold in percent |
| `native_app_volumes.enable_defaults` | `false` | Native application host volumes under `apps_dir` |
| `custom_host_volumes[]` | | `name`, `path`, `read_only`, `owner`, `mode` |
| `node_class` | | `${node.class}` |
| `node_pool` | `default` | Node pool of the node |
| `meta` | | Labels `${meta.<key>}` |
| `host_networks[]` | | `name`, `interface` or `cidr`, `reserved_ports` |
| `reserved` | | `cpu`, `memory`, `disk`, `reserved_ports` |
| `drain_on_shutdown` | | `deadline` (default 1h), `force`, `ignore_system_jobs` |
| `extra_hcl` | | Lines appended inside the client block |
| `cni_subnet`, `cni_plugins` | | Reserved; not applied yet |

`client_defaults` takes the same keys; see [Node labels, pools and resources](node-labels-and-pools.md).

### node_pools[]

| Key | Description |
| --- | --- |
| `name` | Pool name (not `all`) |
| `description` | Description |
| `meta` | Pool meta |
