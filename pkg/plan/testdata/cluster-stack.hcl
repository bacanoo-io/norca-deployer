rigger {
  name   = "base"                  # Recorded in every release as its owner.
  region = "eu-central"            # The region of this plan (metadata.region).
}

module "cache" {                   # The release "cache": one Redis server.
  source  = "norca/redis"          # A module of the global registry, by a version constraint.
  version = "~> 0.4"

  values = {                       # Variables of the module.
    datacenters = ["dc1", "dc2"]
    node_pool   = "apps"           # The clients are in the pool "apps" (client_defaults); jobs
                                   # without node_pool only run in the pool "default".
    memory      = 256
  }
}

# NATS cluster (JetStream) for the applications.
module "nats" {
  source  = "norca/nats"
  version = "~> 2.0"

  values = {
    datacenters       = ["dc1", "dc2"]
    node_pool         = "apps"
    count             = 1          # One server per client node (distinct_hosts): use 3 for HA
                                   # on 3 or more Linux clients with Docker.
    jetstream_enabled = true
    auth_mode         = "userpass" # Users in accounts, passwords from the secrets above.
    users = [
      {
        name            = "app"
        publish_allow   = ["app.>", "$JS.API.>"]
        subscribe_allow = ["app.>", "_INBOX.>"]
      },
      {
        name    = "ops"
        account = "SYS"            # nats server report, raft peer-remove.
      },
    ]
    # tls_enabled = true           # TLS: put tls_ca/tls_cert/tls_key into the Variable
                                   # nomad/jobs/nats (a secrets entry of type variable).
  }
}
