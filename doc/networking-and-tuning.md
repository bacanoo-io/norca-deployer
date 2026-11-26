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
    leafnode_cidrs: [198.51.100.7/32]          # leaf nodes of the bus; default: api_cidrs
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
| TCP 4652 (system bus leaf nodes, with `bus.leafnodes`) | Servers | `leafnode_cidrs`, default `api_cidrs` |
| TCP/UDP 20000-32000 (allocations) | Nodes running a client, with `open_ephemeral_ports` | `ephemeral_cidrs`, default `cluster_cidrs`; **host network mode only**, see below |
| SSH | All, with `default_deny` | Any source |
| `extra_ports` | Nodes of their `roles` (default: nodes running a client) | Their `cidrs`, default `cluster_cidrs` |
| TCP/UDP 53 (DNS stub of the host on docker0) | Linux nodes with `consul.dns_forwarding.docker` | The subnet of `docker0` (see [DNS of the hosts](#dns-of-the-hosts-the-consul-domain)) |

`extra_ports` opens static ports of workloads, which are outside the allocation range, e.g. the client port 4222 of the `nats` module:

```yaml
tuning:
  firewall:
    extra_ports:
      - port: "4222"            # or a range "7422-7423"
        proto: tcp              # tcp (default), udp or both
        cidrs: [10.8.0.0/24]    # default cluster_cidrs
        roles: [client]         # default [client]
```

An empty list means **any source**; `plan` prints `ANY source` for such ports. Allocation ports of host network mode are allowed from `ephemeral_cidrs` (`INPUT`).

> [!IMPORTANT]
> **With ufw, firewalld or iptables, `ephemeral_cidrs` does not limit allocations in bridge mode.** There the CNI port mapping forwards the traffic to the allocation on the bridge of the client (`nomad`, the default of `bridge_network_name`), so it never reaches `INPUT`, and the forwarding chain of the client accepts it from every source before the rule of the deployer is looked at. **A mapped port of an allocation in bridge mode answers every network the node has an address in**, also with `default_deny`. Keep nodes that run such allocations off public networks, or filter in front of them. The deployer still writes its forwarding rule (`ufw status` shows it), and it lets the allocations reach the network.
>
> The **nftables** backend with `default_deny` is the exception: its own table drops new connections into the bridge that do not come from `ephemeral_cidrs`, and a drop of one table is final whatever the chains of the client accept.

**`default_deny: true`** denies incoming traffic unless a rule allows it: the nftables table of the deployer gets the policy `drop` (it still lets in loopback, replies, ICMP and DHCP replies), or ufw is turned on with `ufw default deny incoming` and `allow outgoing` when the node has ufw but no `nft`. Before that, the deployer allows the SSH port it connected on and every port `sshd` listens on, so it cannot lock itself out (without `ss`, as on Alpine without `iproute2`: the ports of `sshd -T` that a socket of `/proc/net/tcp` listens on). A running firewalld already denies by default. Without `nft`, ufw or a running firewalld, `default_deny` fails the node; install `nftables` (or ufw) with [host packages](host-packages-and-scripts.md).

**Backends**, in this order:

- **ufw**, when active: `ufw allow` rules with the source, `ufw route allow out on nomad` rules for bridge-mode allocations and `ufw route allow in on nomad` for their egress.
- **firewalld**, when running (the default on RHEL): ports open to any source, rich rules for restricted sources, the bridge `nomad` in the trusted zone with masquerading in that zone.
- **nftables**, when `nft` is installed (Debian 12 and RHEL ship it): the deployer owns the table `inet norca` (IPv4 and IPv6) and writes it whole on every apply to `/etc/nftables.d/norca.nft`, loaded with one `nft -f`. Its `input` chain holds the rules with their sources; its `forward` chain keeps the policy `accept` (Docker and other bridges are left alone) and, with `default_deny`, drops new connections into the client's bridge from other sources than those of `ephemeral_cidrs`. The unit `norca-nftables.service` loads the file again at boot (systemd hosts); on Alpine the OpenRC service `/etc/init.d/norca-nftables` of the boot runlevel does it (after the `nftables` service, before the network). The deployer does not enable the `nftables` service of Alpine: its default ruleset drops every input it does not list, SSH included. Without `default_deny` the policy is `accept`, so the rules restrict nothing; the deployer warns about that.
- **ufw**, when installed but not active, with `default_deny` and without `nft`: it is turned on as above.
- **iptables** (fallback without `nft`): `INPUT` and `FORWARD` rules with the source. They are not persistent across reboots, and if the `INPUT` policy is `ACCEPT` (the default on a plain Debian) they restrict nothing; the deployer warns about that.

A node that has `nft` and no active ufw (a Debian 12 node, for example) uses the nftables backend; the iptables rules an earlier deployer version made there stay in the running ruleset until the next reboot (they are `ACCEPT` rules and were never persisted). When ufw or firewalld filters a node later, the deployer removes its nftables table, file and boot unit or service, whose `drop` policy would still filter what the other firewall lets in. A restart of `nftables.service` with a ruleset that starts with `flush ruleset` removes the table until the next `apply` or boot.

**Rule management.** Every rule carries an identifier (`norca:<port>/<proto>:<source>`, as a ufw, iptables or nftables comment). A later `apply` adds the rules the plan asks for first, then removes its own rules the plan no longer asks for (for example after a CIDR change), including the open rules of earlier deployer versions; the nftables backend replaces its whole table in one transaction. Rules it did not create are not touched; with firewalld it only manages the Norca ports.

**Notes**

- Earlier deployer versions set `DEFAULT_FORWARD_POLICY="ACCEPT"` in `/etc/default/ufw`. Forwarded traffic is then not filtered; the deployer warns, and you should set it back to `DROP`.
- Ports published by the Docker daemon itself (not by Norca's bridge mode) bypass ufw through Docker's own iptables chains, and so do the mapped ports of Norca's bridge mode (see above). Keep such services off public interfaces.
- Firewall problems other than a failed `default_deny` are warnings and do not stop the node.

## Kernel tuning (Linux)

`tuning.apply_sysctl: true` writes `/etc/modules-load.d/norca-bridge.conf` (`bridge`, `br_netfilter`) and `/etc/sysctl.d/99-norca-production.conf`, then runs `sysctl --system` (on Alpine `sysctl -p` of the file, and the `modules` and `sysctl` services of OpenRC, which read both files at boot, are put into the boot runlevel when they are in none):

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

## DNS of the hosts: the consul domain

With a Consul agent on every node (for example the `consul` module), `consul.dns_forwarding` makes the Linux nodes resolve `*.consul` through the DNS interface of the local agent (`127.0.0.1:8600` by default):

```yaml
consul:
  address: 127.0.0.1:8500
  dns_forwarding:
    enabled: true            # false or missing: the files written earlier are removed
    address: 127.0.0.1:8600  # the DNS interface of the Consul agent
    domain: consul
    method: auto             # auto, systemd-resolved, dnsmasq
    docker: false            # containers on docker0 resolve the domain too (systemd-resolved only)
```

This is **conditional forwarding**: the `consul` domain is answered by Consul, every other name keeps being answered by the resolver the host already has. The deployer does not make Consul the resolver of the host and sets no recursors in Consul.

> [!NOTE]
> **systemd-resolved also asks Consul for other names.** The server of the drop-in is a global `DNS=` server, and systemd-resolved sends a name that matches no routing domain to the servers of every link with `DefaultRoute=` *and* to the global ones, in parallel (systemd-resolved.service(8); seen on Debian 12, systemd 252). Consul refuses such a name (`REFUSED`, it has no recursors) and the answer of the servers of the host is used, so names resolve as before; the Consul agent only sees the extra queries. dnsmasq sends only the domain to Consul.

**The deployer never installs a resolver.** It configures the one the node has, before the directories of the agent are made (after the host packages and the tuning):

- **`systemd-resolved`**, when it runs and the lookups of the host go through it: `127.0.0.53` (or `127.0.0.54`) in `/etc/resolv.conf`, or `resolve` in the `hosts` line of `/etc/nsswitch.conf`. It needs systemd 246 or newer for a port other than 53 in `DNS=`.
- **`dnsmasq`**, when it runs, `/etc/resolv.conf` names `127.0.0.1` (or `::1`), and it reads `/etc/dnsmasq.d` (the unit of Debian passes it; elsewhere `conf-dir=/etc/dnsmasq.d/,*.conf` in `/etc/dnsmasq.conf`, which the package of Alpine has). Alpine nodes (OpenRC) have only this one: see [Alpine: dnsmasq](#alpine-dnsmasq).

`auto` takes the first that applies; an explicit `method` checks the same conditions for that one. A node where neither applies is an error of that node, found before anything is changed on it, with what was found:

```text
consul.dns_forwarding: 10.77.0.10 has no local resolver the deployer can forward the consul domain
from: systemd-resolved not installed, dnsmasq not installed, /etc/resolv.conf names 10.0.0.2.
Install systemd-resolved or dnsmasq with host_packages, or set consul.dns_forwarding.enabled = false
```

Debian 12 has neither. Install one with [host packages](host-packages-and-scripts.md), which run earlier in the same `apply`:

```yaml
host_packages:
  apt:
    install:
      - name: systemd-resolved   # takes over /etc/resolv.conf (127.0.0.53)
```

> [!WARNING]
> **Debian with ifupdown and DHCP:** the package points `/etc/resolv.conf` to the stub of systemd-resolved at once, but systemd-resolved gets the DNS servers of the DHCP lease only with the next lease (dhclient hands them over through `resolvconf`). Until then names outside the `consul` domain do not resolve; the deployer warns (`systemd-resolved has no DNS server but the Consul agent`) on every `apply` until `resolvectl dns` shows a server of the network. Renew the lease (`ifdown`/`ifup` of the interface, or a reboot), or give systemd-resolved its servers with `DNS=` in a file of `/etc/systemd/resolved.conf.d`. Hosts with systemd-networkd or NetworkManager hand the servers over themselves.

**What it writes** (each file starts with `# Managed by norca-deployer. Do not edit.`, or `;` in the trust anchor):

| Method | File | Content |
| --- | --- | --- |
| systemd-resolved | `/etc/systemd/resolved.conf.d/norca-consul.conf` | `[Resolve]`, `DNS=127.0.0.1:8600`, `Domains=~consul` (and `DNSStubListenerExtra=` with `docker`) |
| systemd-resolved | `/etc/dnssec-trust-anchors.d/norca-consul.negative` | `consul`: a negative trust anchor, so that DNSSEC validation (`DNSSEC=yes` or `allow-downgrade`) skips the domain, whose answers Consul does not sign; DNSSEC stays as it is for every other name. A `.negative` file replaces the built-in negative trust anchors of systemd-resolved (the private zones: `10.in-addr.arpa`, `local`, `home.arpa`, ...), so the file lists them too (the list of systemd 252) |
| dnsmasq | `/etc/dnsmasq.d/norca-consul.conf` | `server=/consul/127.0.0.1#8600` |

A file is written only when its content differs, and the resolver is restarted (`systemctl restart systemd-resolved` or `dnsmasq`; on Alpine `rc-service dnsmasq restart`) only when a file changed or was removed; a second `apply` restarts nothing. The agent is not restarted for it. The work is done by a POSIX `sh` script that the deployer uploads to `/var/lib/norca-deployer/consul-dns.sh` and runs; the install script of [scaled machines](node-scaling-machines.md) runs the same functions.

**Docker containers** (`docker: true`, systemd-resolved only, systemd 247 or newer): containers do not use the stub on `127.0.0.53`, so systemd-resolved also listens on the address of `docker0` (`DNSStubListenerExtra=`, read from `ip -4 addr show dev docker0`, never assumed to be `172.17.0.1`), and the Docker daemon gives that address to its containers: `"dns": ["<docker0 address>"]` is merged into `/etc/docker/daemon.json` (every other key kept; the file is made when it is missing; a file that does not parse is refused; the merge uses `jq` or `python3`). Every name of a container then goes through systemd-resolved, so other names resolve as on the host. When `daemon.json` changed, the deployer restarts Docker and warns: **running containers are restarted unless `live-restore` is on**. A node without `docker0` is an error. With `tuning.apply_firewall_rules` the deployer allows TCP and UDP 53 from the `docker0` subnet (rule `norca:53/udp:<subnet>`), which `default_deny` would drop. This covers the default bridge; containers on other Docker networks come from other subnets.

**Turning it off** (`enabled: false` or no block) removes the files above and restarts the resolver when it removed one. The `dns` key of `daemon.json` stays; the deployer warns, because containers then ask an address nobody answers on: remove the key and restart Docker. Decommissioning a node leaves the files, as it leaves the other host settings.

**How to check** on a node:

```bash
resolvectl query consul.service.consul        # systemd-resolved: answered by the Consul agent
getent hosts consul.service.consul            # the resolver of the host (NSS)
dig @127.0.0.1 -p 8600 consul.service.consul  # Consul itself
getent hosts deb.debian.org                   # other names still resolve
docker run --rm busybox nslookup consul.service.consul   # with docker: true
```

Consul answers DNS queries with the rights of its DNS token: with ACLs that deny by default (the `consul` module) the answers stay empty until the agents have a token that may read nodes and services (`consul acl set-agent-token dns <token>` on every agent).

### Alpine: dnsmasq

Alpine runs OpenRC and has no systemd-resolved, so an Alpine node forwards the domain with dnsmasq: `auto` takes it, and `method: systemd-resolved` or `docker: true` is an error of the plan before anything is changed on any node. The deployer drives dnsmasq through OpenRC: `rc-service dnsmasq status` tells whether it runs, and `rc-service dnsmasq restart` makes it read its new file (the `reload` of the Alpine service only clears the cache). The conditions are those of dnsmasq above: started, named in `/etc/resolv.conf` as `127.0.0.1`, and reading `/etc/dnsmasq.d`, which the `/etc/dnsmasq.conf` of the Alpine package does.

Two things of an Alpine host need a setup that the plan does, because the deployer does not manage `/etc/resolv.conf`:

- **dnsmasq needs servers other than those of `/etc/resolv.conf`.** dnsmasq asks the servers `/etc/resolv.conf` names. Once that names `127.0.0.1`, dnsmasq skips it as its own address (`ignoring nameserver 127.0.0.1 - local interface` in the log) and refuses every name it cannot answer itself. Give it the servers of the network in a file of their own (`resolv-file=`) or as `server=` lines. The deployer warns when dnsmasq has neither: `dnsmasq has no DNS server for names outside the consul domain`.
- **The DHCP client rewrites `/etc/resolv.conf`.** With DHCP (`iface eth0 inet dhcp` in `/etc/network/interfaces`, the default of Alpine and of its cloud images) ifupdown-ng starts dhcpcd when it is installed, as on the Alpine cloud images, and else the udhcpc of busybox. Both write the servers of each lease into `/etc/resolv.conf`, and so drop `127.0.0.1` at the next renewal or boot. udhcpc writes them to another file with `RESOLV_CONF` in `/etc/udhcpc/udhcpc.conf`: the one dnsmasq reads. dhcpcd leaves `/etc/resolv.conf` alone with `nohook resolv.conf` in `/etc/dhcpcd.conf`, and its hook `/etc/dhcpcd.exit-hook` writes the servers to that file.

This plan installs dnsmasq on the Alpine nodes and sets it up that way, in a script that runs after the host packages and before the deployer forwards the domain:

```yaml
consul:
  dns_forwarding:
    enabled: true
host_packages:
  apk:
    install:
      - name: dnsmasq
scripts:
  - name: alpine-dnsmasq
    phase: post_packages
    interpreter: sh
    families: [alpine]
    inline: |
      set -eu
      # The servers of the network, once: those /etc/resolv.conf names now.
      up=/etc/resolv.dnsmasq
      if [ ! -s "$up" ]; then
        grep -E '^nameserver[[:space:]]' /etc/resolv.conf |
          grep -vE '^nameserver[[:space:]]+(127\.0\.0\.1|::1)[[:space:]]*$' >"$up.new" || true
        if [ ! -s "$up.new" ]; then
          rm -f "$up.new"
          echo "/etc/resolv.conf names no DNS server of the network: write them to $up" >&2
          exit 1
        fi
        mv "$up.new" "$up"
      fi
      # udhcpc writes the servers of the next leases there, not to resolv.conf.
      conf=/etc/udhcpc/udhcpc.conf
      if ! grep -qxF "RESOLV_CONF=$up" "$conf" 2>/dev/null; then
        mkdir -p /etc/udhcpc
        { grep -v '^RESOLV_CONF=' "$conf" 2>/dev/null || true; echo "RESOLV_CONF=$up"; } >"$conf.new"
        mv "$conf.new" "$conf"
      fi
      # dhcpcd (ifupdown-ng takes it when it is installed, as on the cloud
      # images): its resolv.conf hook off, and its exit hook writes the
      # servers of each lease there. A running dhcpcd reads nohook only when
      # it starts again; until then the exit hook names dnsmasq in
      # resolv.conf again after its resolv.conf hook.
      if [ -x /sbin/dhcpcd ]; then
        if ! grep -qxF 'nohook resolv.conf' /etc/dhcpcd.conf 2>/dev/null; then
          echo 'nohook resolv.conf' >>/etc/dhcpcd.conf
        fi
        cat >/etc/dhcpcd.exit-hook.new <<'EOF'
      # Written by the plan (alpine-dnsmasq): dnsmasq asks the servers of the
      # lease, /etc/resolv.conf names dnsmasq.
      if [ -n "$new_domain_name_servers" ]; then
        printf 'nameserver %s\n' $new_domain_name_servers >/etc/resolv.dnsmasq.new &&
          mv -f /etc/resolv.dnsmasq.new /etc/resolv.dnsmasq
      fi
      if [ "$(grep -E '^nameserver' /etc/resolv.conf)" != "nameserver 127.0.0.1" ]; then
        { grep -E '^(search|domain|options)[[:space:]]' /etc/resolv.conf
          echo "nameserver 127.0.0.1"; } >/etc/resolv.conf.new
        cat /etc/resolv.conf.new >/etc/resolv.conf
        rm -f /etc/resolv.conf.new
      fi
      EOF
        if cmp -s /etc/dhcpcd.exit-hook.new /etc/dhcpcd.exit-hook; then
          rm -f /etc/dhcpcd.exit-hook.new
        else
          mv /etc/dhcpcd.exit-hook.new /etc/dhcpcd.exit-hook
        fi
      fi
      # dnsmasq answers this host only, from the servers of the network.
      want="listen-address=127.0.0.1
      bind-interfaces
      resolv-file=$up"
      restart=0
      if [ "$(cat /etc/dnsmasq.d/local.conf 2>/dev/null)" != "$want" ]; then
        printf '%s\n' "$want" >/etc/dnsmasq.d/local.conf
        restart=1
      fi
      rc-update add dnsmasq default >/dev/null
      if [ -e /run/openrc/softlevel ]; then # OpenRC runs (not an image build)
        if [ "$restart" = 1 ]; then
          rc-service dnsmasq restart
        elif ! rc-service dnsmasq status >/dev/null 2>&1; then
          rc-service dnsmasq start
        fi
      fi
      # The host asks dnsmasq; search and options lines stay.
      if [ "$(grep -E '^nameserver' /etc/resolv.conf)" != "nameserver 127.0.0.1" ]; then
        { grep -E '^(search|domain|options)[[:space:]]' /etc/resolv.conf || true
          echo "nameserver 127.0.0.1"; } >/etc/resolv.conf.new
        cat /etc/resolv.conf.new >/etc/resolv.conf
        rm -f /etc/resolv.conf.new
      fi
```

The script:

- writes the servers that `/etc/resolv.conf` names on its first run to `/etc/resolv.dnsmasq` (it stops with an error when that names no server but dnsmasq), and sets `RESOLV_CONF=/etc/resolv.dnsmasq` in `/etc/udhcpc/udhcpc.conf`, so udhcpc writes the servers of later leases there;
- with dhcpcd, adds `nohook resolv.conf` to `/etc/dhcpcd.conf` and writes `/etc/dhcpcd.exit-hook`, which dhcpcd runs after its own hooks at every lease: it writes the servers of the lease to `/etc/resolv.dnsmasq` and makes `/etc/resolv.conf` name `127.0.0.1` again when the `resolv.conf` hook of a dhcpcd that started before the change rewrote it;
- writes `/etc/dnsmasq.d/local.conf`: dnsmasq asks the servers of `/etc/resolv.dnsmasq` (and reads the file again when the DHCP client changes it), and answers on `127.0.0.1` only (`listen-address`, `bind-interfaces`; the `local-service` of the Alpine `dnsmasq.conf` would answer every host of the local subnets);
- puts dnsmasq into the `default` runlevel and starts it, or restarts it when its file changed (not while OpenRC does not run: an image build);
- makes `/etc/resolv.conf` name `127.0.0.1` only, with its `search`, `domain` and `options` lines.

A second run changes nothing. A host with a static address (`iface eth0 inet static`) has no DHCP client rewriting `/etc/resolv.conf`; the same script works there. A host with another DHCP client (dhclient) must keep that client from writing `/etc/resolv.conf` in the client's own way. The install script of [scaled machines](node-scaling-machines.md) does not run the scripts of the plan: build the image with this setup.

**How to check** on an Alpine node:

```sh
nslookup consul.service.consul    # through dnsmasq on 127.0.0.1: answered by the Consul agent
nslookup dl-cdn.alpinelinux.org   # other names still resolve
rc-service dnsmasq status
cat /etc/resolv.dnsmasq           # the servers of the network that dnsmasq asks
```

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
