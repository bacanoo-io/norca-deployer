# global/node-pools/general.hcl
node_pool "general" {
  description = "General purpose clients"

  meta {
    tier = "standard"
  }
}
