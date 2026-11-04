# norca-platform.hcl - the STARTUP configuration (design section 5.2).
# Read once when the program starts. It lives OUTSIDE the tree and is never committed:
# it holds the address, the tokens and what this process may do. A commit cannot change it.
#
#   norca-deployer platform plan --config ./norca-platform.hcl -d ./platform

region "eu-1" {
  authoritative = true                        # the one deployment target for now
  address       = "https://10.0.0.11:4646"
  token_env     = "NORCA_TOKEN"               # default token; never write a token here in clear
  ca_cert       = "./certs/ca.pem"

  consul_kv {                                 # where kv/ is written
    type    = "norca-compat"
    address = "http://10.0.0.21:8500"         # a client whose consul_compat.bind is reachable
    token_env = "NORCA_TOKEN_KV"
    backend = "variables"                     # declared, never "auto" (design 13.2)
  }

  scoped_token "foundation" {                 # a management token, used for the foundation group only
    token_env = "NORCA_TOKEN_FOUNDATION"
    for       = ["foundation"]
  }

  scoped_token "apps" {                       # only jobs, Variables and secrets of app-* namespaces
    token_env  = "NORCA_TOKEN_APPS"
    for        = ["job", "variable", "secret"]
    namespaces = ["app-*", "apps"]
  }

  scoped_token "readonly" {                   # plan, drift watch and status read with this one
    token_env = "NORCA_TOKEN_RO"
    for       = ["plan"]
  }
}

placement {
  type = "proxy"                              # auto | local | proxy | node. Here: outside the cluster, direct connection
}

state {
  dir    = ".norca-deployer/platform"
  ledger = "cluster"
}

limits {
  kinds      = ["foundation", "job", "variable", "secret", "service-intention", "kv-entry"]
  prune      = false                          # deletion is opt-in
  on_failure = "rollback"
  max_changes = 50
}
