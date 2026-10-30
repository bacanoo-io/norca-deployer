# Networking and Tuning

## Addresses

| Setting | Effect |
| --- | --- |
| `node.host` | SSH address of the node |
| `node.cluster_address` | Address the other nodes use (e.g. a VPN address). Used for `retry_join` and the server certificate. Default: `host` |
| `network.bind_address` | Listen address of HTTP, RPC and Serf. Empty (default): each node listens on its cluster address only, and HTTP also on `127.0.0.1` |
| `network.ports` | `http` 4646, `rpc` 4647, `serf` 4648 |

**Listen address.** Without `bind_address`, the agent gets an `addresses` block: RPC and Serf on the node's cluster address (or the matched interface, see below), HTTP on that address and on `127.0.0.1` (used by the local CLI and by the deployer). Nothing listens on other interfaces, such as a public one. The cluster address must then be an IP address; `validate` reports a DNS name. With `bind_address: 0.0.0.0` the agent listens on every interface, and only the firewall decides who can connect.

**Joining:** every server lists all servers of the plan on the Serf port; clients list the servers on the RPC port.

**Advertise address**, in this order:

1. `network.interface_prefix`: the agent advertises the IPv4 address of the first interface whose name starts with the prefix (e.g. `wg` for `wg0`). It is resolved by the agent at start, with a go-sockaddr template.
2. `network.interface_suffix`: the same, matching the end of the name.
3. `network.vpn` with `enabled: true`: `interface_prefix`, `interface_suffix` or the exact `interface_name`.
4. Otherwise `cluster_address` (or `host`).

`client_settings.network_interface` selects the interface the client uses for allocation addresses.

## Firewall

With `tuning.apply_firewall_rules: true` the deployer opens the Norca ports on **Linux** nodes. Windows firewalls are not configured; manage them yourself.

```yaml
tuning:
  apply_firewall_rules: true
  open_ephemeral_ports: true
  firewall:
    cluster_cidrs: [10.8.0.0/24]               # the node network
    api_cidrs: [10.8.0.0/24, 192.0.2.0/24]     # operators, load balancers
    ephemeral_cidrs: [10.8.0.0/24]             # default: cluster_cidrs
    default_deny: true
```

| Port | Nodes | Allowed from |
| --- | --- | --- |
| TCP 4646 (HTTP API, UI) | Servers | `api_cidrs` |
| TCP 4647 (RPC) | Servers | `cluster_cidrs` |
| TCP/UDP 4648 (Serf) | Servers | `cluster_cidrs` (servers of other regions must be in it) |
| TCP 4649 (system bus clients) | Servers | `cluster_cidrs` |
| TCP 4650 (system bus routes between the servers) | Servers | `cluster_cidrs` |
| TCP/UDP 20000-32000 (allocations) | Nodes running a client, with `open_ephemeral_ports` | `ephemeral_cidrs`, default `cluster_cidrs` |
| SSH | All, with `default_deny` | Any source |
| `extra_ports` | Nodes of their `roles` (default: nodes running a client) | Their `cidrs`, default `cluster_cidrs` |

`extra_ports` opens static ports of workloads, which are outside the allocation range, e.g. the client port 4222 of the NATS pack:

```yaml
tuning:
  firewall:
    extra_ports:
      - port: "4222"            # or a range "7422-7423"
        proto: tcp              # tcp (default), udp or both
        cidrs: [10.8.0.0/24]    # default cluster_cidrs
        roles: [client]         # default [client]
```

An empty list means **any source**; `plan` prints `ANY source` for such ports. Allocation ports are allowed both for host network mode (`INPUT`) and for bridge mode. In bridge mode the CNI port mapping forwards the traffic to the allocation on `cni0`, so it never reaches `INPUT`, and the forwarding rule sees the allocation's own port. The forwarding rule therefore limits the source (`ephemeral_cidrs`), not the port.

**`default_deny: true`** turns ufw on with incoming traffic denied unless a rule allows it (`ufw default deny incoming`, `allow outgoing`). Before that, the deployer allows the SSH port it connected on and every port `sshd` listens on, so it cannot lock itself out. A running firewalld already denies by default. Without ufw or a running firewalld, `default_deny` fails the node; install ufw with [host packages](host-packages-and-scripts.md).

**Backends**, in this order:

- **ufw**, when active (or installed, with `default_deny`): `ufw allow` rules with the source, `ufw route allow out on cni0` rules for bridge-mode allocations and `ufw route allow in on cni0` for their egress.
- **firewalld**, when running: ports open to any source, rich rules for restricted sources, `cni0` in the trusted zone with masquerading in that zone.
- **iptables** (fallback): `INPUT` and `FORWARD` rules with the source. They are not persistent across reboots, and if the `INPUT` policy is `ACCEPT` (the default on a plain Debian) they restrict nothing; the deployer warns about that. Use ufw with `default_deny` instead.

**Rule management.** Every rule carries an identifier (`norca:<port>/<proto>:<source>`, as a ufw or iptables comment). A later `apply` adds the rules the plan asks for first, then removes its own rules the plan no longer asks for (for example after a CIDR change), including the open rules of earlier deployer versions. Rules it did not create are not touched; with firewalld it only manages the Norca ports.

**Notes**

- Earlier deployer versions set `DEFAULT_FORWARD_POLICY="ACCEPT"` in `/etc/default/ufw`. Forwarded traffic is then not filtered; the deployer warns, and you should set it back to `DROP`.
- Ports published by the Docker daemon itself (not by Norca's bridge mode) bypass ufw through Docker's own iptables chains. Use `network { mode = "bridge" }` in jobs, or keep such services off public interfaces.
- Firewall problems other than a failed `default_deny` are warnings and do not stop the node.

## Kernel tuning (Linux)

`tuning.apply_sysctl: true` writes `/etc/modules-load.d/norca-bridge.conf` (`bridge`, `br_netfilter`) and `/etc/sysctl.d/99-norca-production.conf`, then runs `sysctl --system`:

```text
net.ipv4.ip_forward = 1
net.ipv6.conf.all.forwarding = 1
net.bridge.bridge-nf-call-iptables = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.bridge.bridge-nf-call-arptables = 1
fs.file-max = 2097152
fs.inotify.max_user_watches = 524288
fs.inotify.max_user_instances = 8192
vm.max_map_count = 262144
vm.overcommit_memory = 1
vm.swappiness = 1
net.core.somaxconn = 32768
net.ipv4.tcp_max_syn_backlog = 16384
net.ipv4.neigh.default.gc_thresh1 = 1024
net.ipv4.neigh.default.gc_thresh2 = 2048
net.ipv4.neigh.default.gc_thresh3 = 4096
```

Other tuning keys:

- `file_descriptor_limit` (default 65536): `LimitNOFILE` of the agent service.
- `setup_logrotate`: the agent writes `log_dir/norca.log` and rotates it itself (100 MB, 10 files).
- `windows_long_paths`: see [Windows nodes](windows.md).

## CNI plugins

Nodes that run a client get the CNI reference plugins in `paths.cni_bin_dir` (default `/opt/cni/bin`):

```yaml
cni:
  install: true
  version: v1.9.1
  source: official            # official | custom-url | local-path
  # download_base_url: https://mirror.example.com/containernetworking/plugins/releases/download
  # local_archive_path: dist/cni-plugins-linux-{arch}-v1.9.1.tgz
  # checksum_sha256: "<64 hex>"
```

The archive is downloaded on the operator machine, verified against its `.sha256` file and cached. It is unpacked on a node again only when its digest changed; this does not restart the agent.

## Drivers

`client_settings.drivers` becomes the client's `driver.allowlist`. The default is `[docker, exec]`. `raw_exec` runs tasks without isolation, so it must also be enabled with `enable_raw_exec: true`. The Docker engine itself is not installed by the driver setting; install it with [host packages](host-packages-and-scripts.md).

## Paths and volumes

Paths are set per platform in `paths.linux` / `paths.windows`, and per node in `path_overrides`. Defaults:

| Key | Linux | Windows |
| --- | --- | --- |
| `bin_dir` | `/usr/local/bin` | `C:\Program Files\norca\bin` |
| `config_dir` | `/etc/norca.d` | `C:\ProgramData\norca\config` |
| `data_dir` | `/var/lib/norca` | `C:\ProgramData\norca\data` |
| `log_dir` | `/var/log/norca` | `C:\ProgramData\norca\logs` |
| `tls_dir` | `/etc/norca.d/tls` | `C:\ProgramData\norca\config\tls` |
| `cni_bin_dir` | `/opt/cni/bin` | `C:\Program Files\norca\cni` |
| `cni_config_dir` | `/opt/cni/config` | `C:\ProgramData\norca\cni\config` |
| `apps_dir` | `/opt/norca/apps` | `C:\norca\apps` |

**Native application volumes:** `client_settings.native_app_volumes.enable_defaults: true` creates `bin/` (0755), `work/` (0770), `config/` (0750) and `data/` (0750) under `apps_dir` and registers them as host volumes `native-apps-bin`, `native-apps-work`, `native-apps-config` (read-only) and `native-apps-data`. Jobs using `exec` or `raw_exec` can mount them.

**Custom host volumes:** `client_settings.custom_host_volumes` entries are created with their `mode` (default 0755) and `owner` (Linux) and registered as host volumes. The agent refuses to start with a missing host volume directory, so the deployer creates them first.
