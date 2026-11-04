# global/acl/policies/developer.hcl   (a native Norca ACL policy, applied as is)
namespace "apps" {
  policy       = "write"
  capabilities = ["submit-job", "read-logs", "alloc-exec"]
}

node {
  policy = "read"
}
