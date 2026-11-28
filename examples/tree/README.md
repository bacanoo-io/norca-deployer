# Tree example

A small tree for `norca-deployer tree` ([doc/tree.md](../../doc/tree.md)), experimental:

- `cluster-config/global/`: the namespaces `apps` and `web` and an ACL policy, applied as the stack
  `tree-global`,
- `cluster-config/namespaces/apps/`: a job, a Variable and a secret, applied as the stack `tree-ns-apps`,
- `cluster-config/stacks/web-settings/`: a Rigger stack written by hand, which uses the shared module
  `cluster-config/modules/settings/` and takes `replicas` from the startup configuration,
- `norca-tree.hcl`: the startup configuration, outside the tree.

Against a development agent (`norca agent -dev`):

```sh
export NORCA_DEPLOYER_EXPERIMENTAL=tree APP_DB_PASSWORD=example
cd examples/tree
norca-deployer tree validate -d cluster-config
norca-deployer tree plan -d cluster-config --detailed-exitcode   # 2: changes pending
norca-deployer tree apply -d cluster-config
norca-deployer tree plan -d cluster-config --detailed-exitcode   # 0: in sync
norca-deployer tree status -d cluster-config
```

The job uses the Docker driver and an image of a private registry: replace the image, or expect the
release `tree-apps` to wait for a deployment that never becomes healthy.
