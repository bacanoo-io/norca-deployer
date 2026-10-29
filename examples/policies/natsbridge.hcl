# ACL policy of the NATS bridge (the natsbridge job of the nats pack). The
# deployer binds it to the job (security.acl.policies[].job), so the bridge
# runs with its workload identity and needs no static token.
#
# The forwarded topics need read-job in the namespaces of the events (Job,
# Deployment, Evaluation, Allocation, Service, Scaling) and the node read
# policy (Node). Topics not covered here (ACL objects, node pools, "*") need
# a management token.

# Events of all namespaces (the bridge subscribes with -namespace "*").
# With bridge_sync_pushes or bridge_sync_regions the bridge also reads and
# syncs the cluster pack registries:
#   object "packs.norca.io/Registry" { capabilities = ["read", "write"] }
namespace "*" {
  capabilities = ["read-job"]
}

# The namespace of the bridge job: its checkpoint and leader election lease
# under the job's own Variables path.
namespace "default" {
  capabilities = ["read-job"]

  variables {
    path "nomad/jobs/natsbridge/*" {
      capabilities = ["list", "read", "write"]
    }
  }
}

node {
  policy = "read"
}
