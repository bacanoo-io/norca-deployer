# Single-node Norca deployment plan in HCL (same content as single-node.yaml).
#
# HCL plans use the same keys as YAML plans. Nested objects are blocks
# (metadata { ... }), and list entries of objects are repeated blocks
# (one "masters" block per server, one "install" block per package).
#
#   norca-deployer apply -c examples/single-node.hcl

version = "norca.io/v1alpha1"          # Plan format version (default; may be omitted).
kind    = "DeploymentPlan"             # Document kind (default; may be omitted).

metadata {
  name       = "norca-single"          # Cluster name; also the CN of the generated CA.
  region     = "global"                # Norca region of the node.
  datacenter = "dc1"                   # Norca datacenter of the node.
}

release {
  version = "v0.2.0"                   # Norca release tag to install.
  source  = "official"                 # Download and verify the GitHub release on the deployer machine.
  # source            = "local-path"   # Or copy a local binary/archive to the node:
  # local_binary_path = "dist/norca_{os}_{arch}.tar.gz"
  # source  = "host-package"           # Or Norca from a Debian package (see host_packages):
  # package = "norca"
}

cni {
  install = true                       # CNI reference plugins (bridge networking).
  version = "v1.9.1"
  source  = "official"
}

upgrade {
  health_timeout = 180                 # Seconds to wait for a restarted agent to become healthy.
}

tuning {
  apply_sysctl          = true         # Kernel parameters and bridge modules.
  apply_firewall_rules  = true         # Open the Norca ports in the firewall.
  open_ephemeral_ports  = true         # Also open 20000-32000 for the allocations.
  setup_logrotate       = true         # Agent log file with size-based rotation.
  file_descriptor_limit = 65536        # LimitNOFILE of the systemd unit.

  firewall {
    api_cidrs       = ["192.168.1.0/24"] # Only the local network may use the API and UI.
    ephemeral_cidrs = ["192.168.1.0/24"] # ...and the allocation ports.
  }
}

network {
  bind_address = "0.0.0.0"             # Address the agent listens on.

  ports {
    http = 4646                        # HTTP API and web UI.
    rpc  = 4647                        # Server RPC.
    serf = 4648                        # Serf gossip.
  }
}

security {
  tls_enabled            = true            # mTLS with an internal CA (certs/norca-ca.pem).
  verify_server_hostname = true            # Check the role names on RPC connections.
  verify_https_client    = false           # true: require client certificates on the HTTP API.
  gossip_encryption_key  = "AUTO_GENERATE" # Generated once into .norca-deployer/gossip.key.

  acl {
    enabled        = true                  # Turn on the ACL system.
    auto_bootstrap = true                  # Bootstrap once and keep the management token locally.
  }
}

host_packages {
  install {
    name = "jq"                        # From the APT sources already configured on the node.
  }
  install {
    name = "curl"
    hold = true                        # apt-mark hold.
  }
  # Norca itself as a Debian package (with release.source = "host-package"):
  # install {
  #   name    = "norca"
  #   version = "0.2.0-1"
  #   hold    = true
  #   # file  = "debs/norca_0.2.0-1_{arch}.deb"   # or a local .deb
  # }
}

scripts {
  name   = "motd"                      # Unique script name.
  phase  = "post_install"              # pre_install | post_packages | post_install
  inline = <<EOT
echo "Managed by norca-deployer ($CLUSTER)" > /etc/motd
EOT
  env {
    CLUSTER = "norca-single"           # Passed through a 0600 env file.
  }
}

inventory {
  masters {
    host     = "192.168.1.50"          # SSH address of the machine.
    hostname = "norca-single"          # Norca node name.
    platform = "linux"
    arch     = "amd64"

    connection {
      mode              = "ssh"
      ssh_user          = "root"
      ssh_key_path      = "~/.ssh/id_ed25519"
      host_key_checking = "tofu"       # Trust on first use, reject a changed key.
    }

    server_settings {
      bootstrap_expect = 1             # One server in the region.
    }

    client_settings {                  # The server also runs the client.
      drivers         = ["exec", "raw_exec"]
      enable_raw_exec = true
      # meta       = { environment = "dev" }   # Scheduling labels (${meta.<key>}).
      # node_class = "general"
    }
  }
}
