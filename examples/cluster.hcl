# Production-style Norca cluster in HCL (same content as cluster.yaml):
# 3 servers and 3 clients (2 Linux with Docker, 1 Windows) in one region.
#
# HCL plans use the same keys as YAML plans. Objects are blocks; a list of
# objects is a repeated block (one "masters" block per server, one "install"
# block per package). Windows paths need doubled backslashes in HCL strings.
#
#   norca-deployer apply -c examples/cluster.hcl

version = "norca.io/v1alpha1"
kind    = "DeploymentPlan"

metadata {
  name       = "norca-prod"            # Cluster name; also the CN of the generated CA.
  region     = "eu-central"            # One plan = one region.
  datacenter = "dc1"                   # Default datacenter; nodes may override it.
}

release {
  version = "v0.2.0"
  source  = "official"                 # official | custom-url | local-path | host-package
  natsbridge = true                    # Also install norca-natsbridge on the clients (nats pack bridge).

  # Norca from a Debian package instead (Linux-only clusters); add the
  # package to host_packages:
  # source  = "host-package"
  # package = "norca"
}

cni {
  install = true                       # CNI plugins on the Linux clients.
  version = "v1.9.1"
  source  = "official"
}

upgrade {
  max_parallel_clients = 1             # Clients restarted at the same time.
  health_timeout       = 240           # Seconds a restarted agent has to become healthy.
}

paths {
  linux {
    bin_dir        = "/usr/local/bin"
    config_dir     = "/etc/norca.d"
    data_dir       = "/srv/norca/data"  # Raft data and allocation state on a data partition.
    log_dir        = "/var/log/norca"
    tls_dir        = "/etc/norca.d/tls"
    cni_bin_dir    = "/opt/cni/bin"
    cni_config_dir = "/opt/cni/config"
    apps_dir       = "/opt/norca/apps"
  }
  windows {
    data_dir = "D:\\norca\\data"
    apps_dir = "C:\\norca\\apps"
  }
}

tuning {
  apply_sysctl          = true         # Kernel parameters and bridge modules.
  apply_firewall_rules  = true         # Open 4646-4648.
  open_ephemeral_ports  = true         # Open 20000-32000 on the clients.
  setup_logrotate       = true         # Agent log file with rotation.
  windows_long_paths    = true         # LongPathsEnabled on Windows nodes.
  file_descriptor_limit = 131072       # LimitNOFILE of the agent service.

  firewall {                           # Who may reach the Norca ports (Linux nodes only).
    cluster_cidrs = ["10.8.0.0/24"]                  # RPC and Serf (and allocation ports by default).
    api_cidrs     = ["10.8.0.0/24", "192.0.2.0/24"]  # HTTP API and UI on the servers.
    extra_ports {                      # Static workload ports (NATS clients).
      port  = "4222"
      cidrs = ["10.8.0.0/24"]
      roles = ["client"]
    }
    default_deny  = true               # ufw on, everything else denied; SSH stays open.
  }
}

network {
  bind_address     = "0.0.0.0"
  interface_prefix = "wg"              # Advertise the address of the first "wg*" interface.

  ports {
    http = 4646
    rpc  = 4647
    serf = 4648
  }

  # dynamic_ports {                    # Allocation port range (default 20000-32000);
  #   min = 21000                      # the firewall opens the same range.
  #   max = 29000
  # }
}

security {
  tls_enabled            = true
  verify_server_hostname = true
  verify_https_client    = false
  gossip_encryption_key  = "AUTO_GENERATE"

  pki {
    generate_worker_pool_cert  = true                  # certs/client-pool.pem for clients added outside the deployer.
    server_additional_dns_sans = ["norca.example.com"] # Extra DNS names in the server certificates.
    server_additional_ip_sans  = ["10.8.0.100"]        # Extra IP addresses in the server certificates.

    spiffe {                           # SPIFFE IDs in the certificates (needed by OpenZiti).
      enabled      = true
      trust_domain = "prod.example.com" # Nodes: spiffe://prod.example.com/norca/eu-central/<server|client>/<hostname>
    }
  }

  acl {
    enabled                 = true
    auto_bootstrap          = true
    management_token_output = ".norca-deployer/management-token"

    policies {                         # Created after the bootstrap.
      name        = "natsbridge"
      description = "Event stream to NATS bridge"
      rules_file  = "policies/natsbridge.hcl"
      job {                            # Bound to the job's workload identity.
        namespace = "default"
        job_id    = "natsbridge"
      }
    }
  }
}

backup {
  enabled  = false                     # Enable after filling in a real destination and age recipient.
  schedule = "0 */2 * * *"

  destination {
    name             = "s3-primary"
    type             = "s3"            # s3 or directory
    endpoint         = "https://s3.example.com:9000"
    bucket           = "norca-backups"
    prefix           = "clusters/eu-central"
    force_path_style = true
  }

  encryption {
    recipients = ["age1examplerecipientreplacewithyourownpublickeyxxxxxxxxxxxxxxxxxxxxxxxxxx"]
  }

  retention {
    keep_hourly  = 24
    keep_daily   = 7
    keep_weekly  = 4
    keep_monthly = 6
  }
}

host_packages {
  update_cache = true
  lock_timeout = 300

  repositories {
    name          = "docker"           # -> /etc/apt/sources.list.d/norca-docker.sources
    uri           = "https://download.docker.com/linux/debian"
    suite         = "bookworm"
    components    = ["stable"]
    architectures = ["amd64"]
    key_file      = "keys/docker.asc"  # Signed-By keyring uploaded from the deployer machine.
    pin_priority  = 600
    roles         = ["client"]         # Only on the clients.
  }

  # With release.source host-package: Norca itself as a Debian package.
  # install {
  #   name    = "norca"
  #   version = "0.2.0-1"
  #   hold    = true
  #   # file  = "debs/norca_0.2.0-1_{arch}.deb"   # or a local .deb
  # }
  install {
    name  = "docker-ce"
    roles = ["client"]
  }
  install {
    name  = "docker-ce-cli"
    roles = ["client"]
  }
  install {
    name  = "containerd.io"
    roles = ["client"]
  }
  install {
    name = "chrony"                    # Time sync on every node.
  }
  install {
    name = "ufw"                       # Firewall for default_deny.
  }
}

scripts {
  name    = "prepare-data-disk"
  phase   = "pre_install"              # Before packages, directories and the agent.
  path    = "scripts/prepare-data-disk.sh"
  roles   = ["master"]
  timeout = 300
  env {
    DATA_DEVICE = "/dev/sdb"
  }
}

scripts {
  name   = "docker-log-limits"
  phase  = "post_packages"             # After Docker is installed.
  roles  = ["client"]
  inline = <<EOT
cat > /etc/docker/daemon.json <<'EOF'
{"log-driver": "json-file", "log-opts": {"max-size": "50m", "max-file": "3"}}
EOF
systemctl restart docker
EOT
}

secrets {                              # Written after the cluster is up, before the packs.
  store = "builtin"                    # external: secrets come from an external store (skipped here).

  # NATS user passwords (generated once, kept in .norca-deployer/generated/).
  bootstrap {
    path  = "nats/users/app"
    items = { password = "!generate:nats-app" }
  }
  bootstrap {
    path  = "nats/users/ops"
    items = { password = "!generate:nats-ops" }
  }
  # NKey seed of the bridge's NATS user; the public key goes into the pack.
  bootstrap {
    path  = "nats/users/natsbridge"
    items = { seed = "!nkey-seed:natsbridge" }
  }
  # bootstrap {
  #   path      = "apps/mailer"
  #   items     = { user = "mailer", password = "!enc:v1:aes-gcm:...", ca = "@certs/smtp-ca.pem" }
  #   overwrite = true
  # }
  # bootstrap {
  #   type  = "variable"
  #   path  = "nomad/jobs/billing"
  #   items = { api_url = "https://billing.example.com" }
  # }
}

packs {
  registries {
    name = "builtin"
    type = "dir"                       # git (default) | oci | dir
    url  = "builtin"                   # The packs that ship with Norca.
    ref  = "latest"
  }
  registries {
    name = "norca"                     # The Norca pack registry (nats, postgres, ...).
    type = "git"
    url  = "https://github.com/bacanoo-io/norca-registry"
    ref  = "v0.1.0"
  }

  deploy {
    name     = "cache"                 # Release name.
    registry = "builtin"
    pack     = "redis"
    enabled  = true
    values {                           # Pack variables.
      datacenters = ["dc1", "dc2"]
      node_pool   = "apps"             # The clients are in the pool "apps" (client_defaults).
      count       = 1
      memory      = 256
    }
  }

  # NATS cluster (JetStream) and the event stream -> NATS bridge.
  deploy {
    name     = "nats"
    registry = "norca"
    pack     = "nats"
    enabled  = true
    values {
      datacenters       = ["dc1", "dc2"]
      node_pool         = "apps"
      count             = 1            # 3 for HA on 3 or more Linux clients with Docker.
      jetstream_enabled = true
      auth_mode         = "userpass"
      users = [
        { name = "app", publish_allow = ["app.>", "$JS.API.>"], subscribe_allow = ["app.>", "nomad.>", "_INBOX.>"] },
        { name = "ops", account = "SYS" },
      ]
      bridge_enabled = true
      bridge_nkey    = "!nkey-public:natsbridge"
      bridge_driver  = "exec"
      bridge_binary  = "/usr/local/bin/norca-natsbridge"
    }
  }
}

# Defaults for every node that runs a client; the node's own settings win.
client_defaults {
  node_pool = "apps"
  meta = {
    environment = "production"
  }
  reserved {
    cpu            = 200
    memory         = 512
    reserved_ports = "22"
  }
  drain_on_shutdown {
    deadline = "30m"
  }
}

node_pools {
  name        = "apps"
  description = "Linux application nodes"
  meta        = { owner = "platform" }
}
node_pools {
  name        = "windows"
  description = "Windows nodes for native .exe workloads"
}

inventory {
  masters {
    host            = "203.0.113.10"
    hostname        = "norca-server-01"
    cluster_address = "10.8.0.10"      # Address the other nodes use (VPN).

    connection {
      mode         = "ssh"
      ssh_user     = "devops"
      ssh_key_path = "~/.ssh/norca_deployer"
      become       = true              # sudo for a non-root user.
    }

    server_settings {
      bootstrap_expect = 3
      raft_multiplier  = 1
      log_level        = "INFO"
    }
  }

  masters {
    host            = "203.0.113.11"
    hostname        = "norca-server-02"
    cluster_address = "10.8.0.11"

    connection {
      mode         = "ssh"
      ssh_user     = "devops"
      ssh_key_path = "~/.ssh/norca_deployer"
      become       = true
    }

    server_settings {
      bootstrap_expect = 3
    }
  }

  masters {
    host            = "203.0.113.12"
    hostname        = "norca-server-03"
    cluster_address = "10.8.0.12"
    arch            = "arm64"          # Each node gets the binary of its architecture.

    connection {
      mode         = "ssh"
      ssh_user     = "devops"
      ssh_key_path = "~/.ssh/norca_deployer"
      become       = true
    }

    server_settings {
      bootstrap_expect = 3
    }
  }

  clients {
    host            = "203.0.113.20"
    hostname        = "norca-worker-01"
    cluster_address = "10.8.0.20"

    connection {
      mode              = "ssh"
      ssh_user          = "devops"
      ssh_key_path      = "~/.ssh/norca_deployer"
      become            = true
      host_key_checking = "strict"     # The host key must already be known.
      known_hosts_file  = "~/.ssh/known_hosts"
    }

    client_settings {
      network_interface       = "wg0"
      drivers                 = ["docker", "exec"]
      gc_disk_usage_threshold = 85
      node_class              = "general"
      meta = {
        rack    = "r1"
        storage = "ssd"
      }

      host_networks {
        name           = "public"
        interface      = "eth0"
        reserved_ports = "22"
      }

      native_app_volumes {
        enable_defaults = true         # native-apps-bin/work/config/data under apps_dir.
      }

      custom_host_volumes {
        name      = "shared-data"
        path      = "/srv/shared"
        read_only = false
        owner     = "1000:1000"
        mode      = "0750"
      }
    }
  }

  clients {
    host            = "203.0.113.21"
    hostname        = "norca-worker-02"
    cluster_address = "10.8.0.21"
    datacenter      = "dc2"            # Another datacenter of the region.

    connection {
      mode         = "ssh"
      ssh_user     = "devops"
      ssh_key_path = "~/.ssh/norca_deployer"
      become       = true
    }

    client_settings {
      network_interface = "wg0"
      drivers           = ["docker", "exec"]
      node_class        = "general"
      meta = {
        rack    = "r2"
        storage = "hdd"
      }
    }

    path_overrides {                   # Paths for this node only.
      data_dir = "/data/norca"
    }
  }

  clients {
    host            = "203.0.113.30"
    hostname        = "norca-win-01"
    platform        = "windows"        # PowerShell; packages and scripts are skipped.
    cluster_address = "10.8.0.30"

    connection {
      mode         = "ssh"
      ssh_user     = "Administrator"   # No become on Windows.
      ssh_key_path = "~/.ssh/norca_deployer"
    }

    client_settings {
      drivers         = ["raw_exec"]
      enable_raw_exec = true
      node_pool       = "windows"
      node_class      = "windows"

      native_app_volumes {
        enable_defaults = true
      }
    }
  }
}

# Multi-region: a replica region adds
# federation {
#   authoritative_region   = "eu-central"
#   join                   = ["10.8.0.10", "10.8.0.11", "10.8.0.12"]
#   replication_token_file = "../eu-central/.norca-deployer/management-token"
# }
