# kv/_kv.hcl   (Consul KV: the file path under kv/ is the key, the file content is the value)
prefix "app/" {
  mode  = "sync"            # the tree wins
  prune = true              # needs --prune-data to delete keys that are not in the tree
}

prefix "feature/" {
  mode = "create-only"      # seeded once, applications may change them later
}

entry "feature/dark-mode" {
  value = "false"
}
