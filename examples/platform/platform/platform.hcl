# Tree manifest: portable, no credentials (design section 5.1).
platform {
  name    = "example"
  version = "norca.io/platform/v1alpha1"   # format version of this tree (required)
}

requires {
  norca = ">=0.3.0 <1.0.0"                 # Norca versions these files were written for
}

graph {
  tiers        = ["platform", "apps"]      # applied in this order, deleted in reverse
  default_wait = "healthy"
}

defaults {
  job_apply = "plan-then-run"
  wait      = true
}
