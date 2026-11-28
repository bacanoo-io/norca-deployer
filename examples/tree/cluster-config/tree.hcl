# The manifest of the tree: what the files mean. How the program reaches the
# cluster is in the startup configuration (../norca-tree.hcl), outside the tree.
tree {
  name        = "example"
  version     = "norca.io/tree/v1alpha1"
  environment = "dev"
}

# Applied right after tree-global; the written stack web-settings follows.
stack_order = ["tree-ns-apps"]
