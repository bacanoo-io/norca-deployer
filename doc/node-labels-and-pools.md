# Node Labels, Pools and Resources

The scheduler places allocations by the attributes of the nodes. The deployer sets the attributes that belong to the node's configuration (`norca.hcl`); changing them restarts the client agents one at a time, and the allocations keep running.

## Labels: meta and node class

Norca has no separate tag field: node labels are `meta` entries, key and value. Jobs use them in `constraint`, `affinity`, `spread` and `separate` blocks as `${meta.<key>}`, the class as `${node.class}`:

```yaml
inventory:
  clients:
    - host: 10.8.0.20
      hostname: worker-01
      client_settings:
        node_class: general
        meta:
          rack: r1
          storage: ssd
```

```hcl
# in a job
constraint {
  attribute = "${meta.storage}"
  value     = "ssd"
}
spread {
  attribute = "${meta.rack}"
}
```

Meta keys may contain letters, digits, `_`, `.` and `-`. The datacenter (`datacenter`) and the region are attributes of their own (`${node.datacenter}`).

## Defaults for all clients

`client_defaults` takes the same keys as `client_settings` and is merged into every node that runs a client (the clients, and the servers with `client_settings`):

- a value of the node wins when it is set;
- `meta` entries are merged by key, the node's entries win;
- lists (`drivers`, `host_networks`, `custom_host_volumes`) and blocks (`reserved`, `drain_on_shutdown`, `native_app_volumes`) come from the defaults only when the node has none;
- booleans (`enable_raw_exec`) are on when either turns them on.

```yaml
client_defaults:
  node_pool: apps
  meta:
    environment: production
  reserved:
    cpu: 200
    memory: 512
```

`norca-deployer plan` prints the resulting labels of every node.

## Node pools

`client_settings.node_pool` puts a node into a pool; a pool is created when the first node of it registers. `node_pools` adds a description and meta:

```yaml
node_pools:
  - name: apps
    description: Linux application nodes
    meta:
      owner: platform
```

The pools are written after the cluster is up (in the authoritative region; they replicate to the others), and again only when they change. Scheduler settings of a pool (`scheduler_config`) are not supported by Norca.

**Jobs only run in their pool.** A job without `node_pool` runs in the pool `default`. When the clients are in another pool, set `node_pool` in the jobs and in the pack values (most packs have a `node_pool` variable), as the cluster example does.

## Host networks

A node with several networks (for example a public and a private interface) can name them, and job ports choose one:

```yaml
client_settings:
  host_networks:
    - name: public
      interface: eth0          # or cidr: 203.0.113.0/24
      reserved_ports: "22"
```

```hcl
# in a job
network {
  port "http" {
    static       = 443
    host_network = "public"
  }
}
```

## Reserved resources

`reserved` keeps CPU (MHz), memory and disk (MB) and ports for the operating system and for the services outside Norca (Docker, sshd, monitoring agents); the scheduler does not offer them to allocations:

```yaml
client_settings:
  reserved:
    cpu: 200
    memory: 512
    disk: 1024
    reserved_ports: "22,9100"
```

## Draining on shutdown

With `drain_on_shutdown`, stopping the agent (for example for a reboot) drains the node first, so its allocations move before it goes away:

```yaml
client_settings:
  drain_on_shutdown:
    deadline: 30m            # default 1h
    force: false
    ignore_system_jobs: false
```

The deployer's own restarts are rolling and do not need it; it is meant for reboots and maintenance. Note that with it a client restart by `apply` also drains the node.

## Dynamic ports

`network.dynamic_ports` sets the range of the dynamic allocation ports on all clients, and the firewall opens the same range ([Networking and tuning](networking-and-tuning.md#firewall)):

```yaml
network:
  dynamic_ports:
    min: 21000
    max: 29000
```

Without it the agents keep their default range 20000-32000 and the configuration is not changed.

## Settings the deployer does not model

`client_settings.extra_hcl` and `server_settings.extra_hcl` are appended inside the client or server block of `norca.hcl`, for keys such as `max_kill_timeout`, `cpu_total_compute` or `num_schedulers`:

```yaml
client_settings:
  extra_hcl: |
    max_kill_timeout  = "60s"
    cpu_total_compute = 8000
```

The deployer does not check these lines; a mistake shows up when the agent starts (and stops the rollout).
