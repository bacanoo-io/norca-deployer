# namespaces/apps/references.hcl
# Things this tree depends on but does NOT manage (design section 7.10).
# Releases are installed by the Rigger stack of the `rigger` section, so they are only referenced here.
reference "release/postgres" {
  type    = "release"
  release = "postgres"
  version = ">=0.1 <1.0"
  wait    = "healthy"
  timeout = "10m"
}
