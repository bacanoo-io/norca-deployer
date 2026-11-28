# The startup configuration of the example: how this process reaches the
# cluster and what it may do. It lives outside the tree (cluster-config/), so
# a commit can never widen it, and it holds no token itself: NORCA_TOKEN.
region "global" {
  address   = "http://127.0.0.1:4646"
  token_env = "NORCA_TOKEN"

  # Over values/default.vars.hcl of the written stacks.
  values = {
    replicas = 3
  }
}

values {
  env = ["APP_*"]
}

limits {
  max_changes = 20
}
