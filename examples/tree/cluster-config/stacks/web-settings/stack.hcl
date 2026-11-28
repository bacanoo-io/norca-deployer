# A Rigger stack written by hand. Its name is its directory's; its namespace
# and region are those of the rigger block (a stack without a region is of
# global). The module ./modules/settings is not in this directory: the
# deployer copies it from the tree's modules/.
rigger {
  name      = "web-settings"
  namespace = "web"
  region    = "global"
}

module "settings" {
  source = "./modules/settings"

  values = {
    color    = var.color
    replicas = var.replicas
  }
}
