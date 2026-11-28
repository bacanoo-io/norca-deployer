variable "api-config" {
  path = "apps/api/config"

  items {
    log_level = "info"
    region    = "eu-1"
  }
}
