# global/namespaces/apps.hcl   (a native Norca namespace file; applied after the quota it names)
name        = "apps"
description = "Application workloads"
quota       = "apps"

meta {
  owner       = "app-team"
  environment = "production"
}
