# namespaces/apps/jobs/api/unit.hcl   (optional: how this job fits into the graph)
depends_on = ["pack/postgres"]      # wait until the postgres release is healthy

tier = "apps"

update {
  mode = "follow"                   # providers before consumers on update
}
