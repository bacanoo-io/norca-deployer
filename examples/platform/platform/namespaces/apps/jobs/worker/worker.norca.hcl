# namespaces/apps/jobs/worker/worker.norca.hcl
job "worker" {
  datacenters = ["dc1"]
  type        = "batch"
  node_pool   = "general"

  periodic {
    crons            = ["*/15 * * * *"]
    prohibit_overlap = true
  }

  group "work" {
    task "run" {
      driver = "docker"

      config {
        image   = "registry.example.com/apps/worker:2.0.0"
        command = "/bin/run-once"
      }
    }
  }
}
