variable "settings" {
  path = "web/settings"

  items {
    color    = var.color
    replicas = tostring(var.replicas)
  }
}
