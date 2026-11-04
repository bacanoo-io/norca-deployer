# namespaces/apps/jobs/api/api.norca.hcl   (a normal Norca job; the namespace comes from the path)
variable "image_tag" {
  type = string
}

variable "count" {
  type    = number
  default = 2
}

job "api" {
  datacenters = ["dc1"]
  type        = "service"
  node_pool   = "general"

  group "web" {
    count = var.count

    network {
      port "http" {
        to = 8080
      }
    }

    task "server" {
      driver = "docker"

      config {
        image = "registry.example.com/apps/api:${var.image_tag}"
        ports = ["http"]
      }

      resources {
        cpu    = 200
        memory = 256
      }
    }
  }
}
