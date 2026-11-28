# The namespace comes from the path (namespaces/apps/): the job sets none.
job "api" {
  type        = "service"
  datacenters = ["*"]

  group "api" {
    count = 1

    task "api" {
      driver = "docker"

      config {
        image = "registry.internal/apps/api:1.4.2"
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }
  }
}
