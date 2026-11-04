# namespaces/apps/references.hcl
# Things this tree depends on but does NOT manage (design section 7.10).
# Pack releases belong to the existing `packs` section, so they are only referenced here.
reference "pack/postgres" {
  type    = "pack-release"
  release = "postgres"
  version = ">=0.1 <1.0"
  wait    = "healthy"
  timeout = "10m"
}
