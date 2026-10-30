# Configuration Reference

A plan describes one region. It is a YAML (`.yaml`, `.yml`) or HCL (`.hcl`) file; both use the same keys. Relative paths in the plan (keys, `.deb` files, scripts, CA files, token files) are resolved against the directory of the plan.

Complete examples: [single-node.yaml](../examples/single-node.yaml), [cluster.yaml](../examples/cluster.yaml) and their [HCL versions](../examples).

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
| `federation` | | Multi-region settings |
| `consul` | | A Consul agent on every node; without it the agents do not use Consul |
| `host_packages` | | Debian packages |
| `scripts` | | Bash scripts |
| `client_defaults` | | Client settings merged into every node that runs a client |
| `node_pools` | | Node pool descriptions |
| `secrets` | | Secrets and Variables written after the cluster is up |
| `packs` | | Pack registries and releases |
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
| `natsbridge` | `false` | Also install `norca-natsbridge` from the release archive on the nodes that run a client (with `host-package` the package must provide it) |
| `natsbridge_local_path` | | `norca-natsbridge` binary for `local-path` when `local_binary_path` is a plain binary |

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
| `destination.name` | Destination name (required) |
| `destination.type` | `s3` (default) or `directory` |
| `destination.endpoint`, `bucket`, `prefix`, `force_path_style` | S3 settings; `bucket` is required for `s3` |
| `destination.path` | Absolute directory on the servers for `directory` |
| `encryption.recipients` | age recipients; at least one (Norca encrypts every backup) |
| `retention.keep_hourly`, `keep_daily`, `keep_weekly`, `keep_monthly` | Snapshots to keep |

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
| `bootstrap_expect` | number of masters | Must equal the number of masters |
| `raft_multiplier` | `1` | Raft timing multiplier |
| `log_level` | `INFO` | Agent log level |
| `extra_hcl` | | Lines appended inside the server block |

### client_settings

| Key | Default | Description |
| --- | --- | --- |
| `drivers` | `[docker, exec]` | `driver.allowlist` |
| `enable_raw_exec` | `false` | Enable the `raw_exec` plugin |
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
