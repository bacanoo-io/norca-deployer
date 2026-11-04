# global/quotas/apps.hcl   (one limit block per region)
name        = "apps"
description = "Resource limits of the apps namespace"

limit {
  region = "eu-1"

  region_limit {
    cpu       = 8000
    memory_mb = 16384
  }
}
