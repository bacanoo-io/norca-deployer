# Norca Deployer – Platform Management (Directory-Based GitOps for Norca Objects)

> **Status:** Design proposal (nothing implemented yet). Revised 2026-11-28 for Rigger, see the box below.
> **Component:** `norca-deployer` – new `tree` command group
> **Language:** English
> **Related:** [gitops-rigger-design.md](gitops-rigger-design.md) (stack bindings, section 7: the tree on Rigger), [git-operations-design.md](git-operations-design.md) (in-memory Git engine, webhook daemon), [norca-deployer-design.md](norca-deployer-design.md) (node provisioning), `norca/rigger/design/rigger-design.md` (modules, stacks, file names)

> **Revision 2026-11-28: the tree on Rigger.** The pack manager is gone (Rigger step 4.10) and Rigger releases now hold every kind this tree writes. Decided on 2026-11-28:
>
> - **Names.** The tree manifest is `tree.hcl`, the command group `norca-deployer tree …`, and the examples name the root `cluster-config/`. The word *platform* stays for the running Norca platform, the platform team and the `platform` namespace. It was dropped as a name because the deployer already uses it for host platforms (`internal/platform`, supported platforms).
> - **The layout of §4 stays** (`global/`, `namespaces/<ns>/`, `kv/`; `regions/<r>/` went with the multi-region design in the third round, and `shared/` was merged into `global/`). It gains a top-level `modules/` (local Rigger modules shared by the tree) and a top-level `stacks/` (Rigger stacks). Plain job files keep working under `namespaces/<ns>/jobs/`.
> - **The tree is applied as Rigger stacks** (gitops-rigger-design.md section 7). The deployer builds one generated stack from `global/`, one per namespace from the files under `namespaces/<ns>/`, and sends each written stack of `stacks/` as it is (§4.1). The release engine of the servers does the ordering, ownership, prune, drift and history.
> - **First round (2026-11-28):** §1, §2 (rows 22 and 23), §3, §4, §5.1, §6.2, §7.5 and the names everywhere.
> - **Second round (2026-11-28): the tree's own engine is gone.** The ledger (§7.4), the journal and before-images (§7.8), the local state (§7.9) and the unit graph with `unit.hcl` and `references.hcl` (§7.10) are replaced by the release engine: release owners, input digests, atomic applies and revisions, and Rigger's dependencies. Rewritten: §7.1 to §7.4, §7.6, §7.8 to §7.10, §7.14, §8, §10, §11, §12 and §14. Adjusted to them: §1, §5, §7.7, §7.12, §7.13, §7.15, §8.1, §9, §11a, §11b, §11c, §13 and §13a (new gaps G19 to G25). The implementation plan is cut again around steps G8a and G8b.
>
> **Choices of the first round, kept:** the extension decides the kind and the directory only sorts; plain job files go through the implicit module of their namespace stack, never through `job register`; kinds Rigger does not have yet are refused by `validate`. **New in the second round, accepted by the user on 2026-11-28:** `stack_order` may also name generated namespace stacks (§5.1, §7.10); a file removed from a release is guarded by the `--prune` flags although the server would delete it (§7.4); tokens are chosen per stack (§5.3).
>
> **Third round (2026-11-28): the multi-region design is removed.** For GitOps, several regions mean only that every region has its own endpoint, and a run goes to one region through it (§6.1). Gone: the scope decision of §1, the fan-out, `rollout_order`, region staging, the file overlays and `regions/<r>/`, `<region>.vars.hcl`, the replication check of the preflight, per-region watchers and promotion gating, and the deferred phase of the plan. Multi-region will be designed again when it is taken up.

---

## 1. Goal and Non-Goals

### Goal

Manage the **objects of a running Norca platform** – namespaces, quotas, ACL policies, jobs, secrets, Variables, network rules, custom objects and so on – from **a directory tree** (usually a Git repository), into **one region per run**, through that region's endpoint (§6.1), with one small command set: `validate`, `plan`, `apply`.

The tree is the source of truth. The tool makes the live platform match it and shows what it would change first. It keeps no state of its own: ownership, history and "unchanged" are in the releases on the servers (§7.9).

### Non-goals

- **Node and cluster installation.** Installing agents, TLS, gossip keys, firewall, host packages and rolling restarts is `norca-deployer apply` and stays there. `tree` starts where it ends: a reachable cluster API.
- **A second engine.** The tree has no engine of its own. Everything it writes goes through Rigger (§4.1): packages are Rigger modules and stacks, and the servers' release engine owns ordering, ownership, prune, drift and history. What the deployer adds is the shape of the tree, the generated stacks, the values and the behaviour of the controller (gitops-rigger-design.md section 7, point 3).
- **No Terraform clone.** No provider model, no state file with a lock backend. Expressions, `for_each` and modules exist only where Rigger has them: inside modules and stacks. The plain files of the tree (§4) are native Norca files without variables.
- **No new object schema where Norca already has one.** Files are the native Norca files (`.norca.hcl` jobs, the HCL of `norca namespace apply`, `norca quota apply`, `norca acl policy apply`, …). A file in the tree can always be applied by hand with the `norca` CLI. The deployer adds only a thin envelope (directory layout, a few small manifests, secret value forms).
- **Not one big platform.** One tree describes one platform. Several platforms are several trees.

### Regions

A run goes to one region, through the endpoint configured for that region; several regions are several runs, each against its own endpoint (§6.1). The multi-region design is not part of this document.

### Design principles

1. **Directory = object kind × scope.** Where a file lives says what it is and where it applies; the file content is native.
2. **Idempotent and boring.** `apply` twice changes nothing the second time. The servers skip an unchanged release by its input digest (§7.3).
3. **Everything shown before it happens.** `plan` is read-only and exact; `apply` re-checks.
4. **Deletion is opt-in.** Nothing is ever removed without `--prune`, and only what a release of the tree owns (§7.4).
5. **Secrets never in plaintext in Git.** Same value forms as the existing plan (`!enc:`, `!generate:`, `@file`).
6. **One binary, same configuration style.** Reuses the master key, the token handling, the Git fetcher, the webhook daemon and the stack client of the stack bindings of `norca-deployer`.
7. **HCL first, YAML second.** Every file the deployer defines is written, documented and generated in HCL, like the Norca objects themselves (§4 *File formats*). YAML is an equivalent secondary form with the same keys, for teams that want it; nothing is YAML-only.

---

## 2. What Norca Offers (Object Inventory)

Derived from the `norca` CLI (`norca/norca/command`), the example policies and the Norca documentation. **Scope** says where the object lives: `region` = stored in one region's Raft; `global` = written to the authoritative region and replicated by Norca to the other regions of the federation. **Verified in the Norca source (`norca/norca/leader.go`, `leader_quota.go`):** only ACL policies, roles, auth methods, binding rules and tokens, namespaces, node pools and quota specs replicate (plus built-in secrets that are marked `replicate`). Admission policies, network policies, object kinds and everything else are **per region**. What this means for a tree run in several regions is not designed now (§6.1). *Why* differs per kind: for admission policies it is a **documented limitation** ("stored per region; apply them in every region of a federation", `features/admission-control.md`); for object kinds replication is **reserved but not implemented** (`replicated = true` is rejected by the servers, `features/operator-framework`); for network policies nothing is documented, the endpoints simply never forward to the authoritative region (`network_policy_endpoint.go`).

| # | Object | Norca CLI / API | Native file | Scope | Notes |
|---|---|---|---|---|---|
| 1 | **ACL policy** | `acl policy apply` | `.hcl` rules | global | optional job binding (workload identity: `job_id`, `group`, `task`) |
| 2 | **ACL role** | `acl role create` | flags → policy list | global | no file format in the CLI; deployer needs a tiny manifest |
| 3 | **ACL auth method / binding rule** | `acl auth-method create`, `acl binding-rule create` | flags / JSON | global | OIDC/JWT SSO |
| 4 | **ACL token** | `acl token create` | – | global/region | **not managed** as files: secrets by nature (§9, rule 3) |
| 5 | **Namespace** | `namespace apply` | `.hcl` (`name`, `quota`, `meta`, capabilities) | global | `meta.environment=production` makes releases atomic (§7.8) |
| 6 | **Quota** | `quota apply` | `.hcl` with `limit { region }` per region, `task_defaults` | global | one `limit` block per region |
| 7 | **Node pool** | `node pool apply` | `.hcl` | global | scheduler config, meta |
| 8 | **Scheduler config** | `operator scheduler set-config` | JSON | region | preemption, algorithm |
| 9 | **Autopilot config** | `operator autopilot set` | flags | region | server health thresholds |
| 10 | **Admission policy (CEL)** | `admission-policy apply` | `.hcl` (`match`, `rule`) | region (not replicated) | `action = deny / warn` |
| 11 | **Network policy** | `network-policy apply` | `.hcl` (`selector`, `egress`, `ingress`) | region, namespaced | level A/B |
| 12 | **Admin network policy** | `network-policy apply -admin` | `.hcl` (`priority`, `namespaces`) | region (not replicated) | evaluated before namespace policies, `deny` is final |
| 13 | **Object kind** (CRD equivalent) | `object-kind apply` | `.hcl` (schema, CEL, webhooks, limits) | region (not replicated) | defines custom types |
| 14 | **Custom object** | `object apply` | `.hcl` (`<group>/<Kind>`) | region | spec written by users, status by controllers |
| 15 | **Job** | `job run`, `job plan` | `.norca.hcl` + vars | region (or multiregion) | the largest group; periodic, parameterized, system, batch |
| 16 | **Job scaling policy** | inside the job (`scaling` block) | in the job | region | typed, validated; autoscaler is built in |
| 17 | **Variables** | `var put` | HCL/JSON spec (`var init`) | region, namespaced | also Consul-KV replacement |
| 18 | **Secrets (built-in store)** | `secret put`, `secret import` | items (`k=v`, `k=@file`) | region, namespaced | versioned, audited; `secret` block in jobs |
| 19 | **Volumes** | `volume create/register` (**CSI**; dynamic host volumes have no API, 13.2) | `.hcl` | region, namespaced | CSI plugins are jobs; static host volumes are node configuration (`apply`); snapshots are runtime, not managed |
| 20 | **Service intention** | `service intention create` | flags: source, destination, action | region | native mTLS allow/deny |
| 21 | **External service** | `service register` | flags / HCL | region | address or DNS alias with checks |
| 22 | **Rigger registry** | `rigger registry add` (object `rigger.norca.io/Registry`) | name + source | region | **not managed by `tree`** for now: synced by the operator or the `rigger` section of the plan; a written stack (§4.1) needs its registries synced |
| 23 | **Rigger release** | `rigger stack apply`, `PUT /v1/rigger/stack/apply` | stack + values | region | **the carrier of the tree**: every object of the tree is written as a release of a generated or written stack (§4.1) |
| 24 | **Backup schedule / S3 target** | agent config, `operator backup` | agent HCL | region | **node config → stays in `apply`** (§3) |
| 25 | **Multiregion deployment** | `deployment run` | in the job (`multiregion`) | spans regions | staged rollout, `on_failure` |
| 26 | **Keyring, snapshots, Raft ops, gossip keys, seal** | `operator …` | – | region | **operations, not objects – never managed** |
| 27 | **Consul KV entry** | Consul-compatible `/v1/kv` of a client (`consul_compat`) or a real Consul (the `consul` module) | file per key | region (not replicated; per endpoint) | value = file content; written with CAS (`ModifyIndex`), §7.12 |

**Edges of the inventory** *(review item G14)*. So a reader can tell what was left out on purpose from what was forgotten, these are explicitly **not managed** by `tree`:

| Thing | Where it lives | Why not here |
|---|---|---|
| **Job scaling policies** | inside the job (`scaling` block) | part of the job file; the autoscaler owns `count` (§7.14) |
| **Node meta**, node **eligibility** and **drain** (`node meta apply`, `node eligibility`, `node drain`) | per node | node-level operations on a running machine, not declarative platform objects; node install and decommission stay in `apply` (§3) |
| **CSI plugins** | jobs (plugin tasks) | managed as ordinary jobs; **volumes** are managed (row 19), snapshots and volume attachments are runtime |
| **ACL tokens** | the cluster | secrets by nature (§9, rule 3) |
| **Stacks of the deployer plan** (`rigger` section) and their bindings | the plan | one owner per stack (§3) |
| **Allocations, evaluations, deployments, periodic launches** | runtime | results of the platform, not input to it |
| **Snapshots, keyring, Raft, gossip, seal, license** | operator operations | operations, not objects (row 26) |
| **Agent configuration** (backup, TLS, tuning) | node files | `apply` (§3) |

New Norca object types that appear in a release become kinds through the registry of §8.1, and the kind-support matrix (§7.7) shows which Norca version supports which; anything not in the registry is *unknown to the tool*, never silently ignored in `import`: `import` lists the object types it found but does not manage.

Things Norca gives us for free that the design leans on:

- **`job plan`** returns a real diff and a check-and-set index → exact `plan` for jobs.
- **Check-and-set** on Variables and secrets → "create once" semantics already used by `bootstrapSecrets`.
- **Rigger releases are server-side objects** with an owner (the stack), revision history, atomic upgrade, drift detection, rollback, hooks and tests, and a stack apply runs as an operation with a log → the tree is applied as stacks (§4.1), so ownership, prune, history and ordering are the servers'.
- **Admission control and quotas** run in the cluster → a bad file is rejected by the platform itself, not only by our validator.
- **Job meta, namespace meta and object labels** allow ownership tagging for jobs/namespaces/objects.

---

## 3. Boundary with the Existing Deployer Plan

| Concern | Owner | Why |
|---|---|---|
| SSH, node install, agent HCL, TLS, gossip, firewall, tuning, host packages, decommission | `apply` (`cluster-deploy.yaml`) | needs node access |
| ACL **bootstrap** (first management token) | `apply` | chicken-and-egg: needs the cluster before the API is usable |
| Backup/seal/server settings in agent config | `apply` | agent configuration |
| `consul_compat` listener of the clients (bind address, KV backend), firewall for it | `apply` (agent config, `extra`, firewall rules) | node configuration; `tree` only needs the address |
| **Stacks of the plan** (`rigger` section: a stack in the plan, a stack directory, stack bindings) | the `rigger` section and `rigger.gitops` | already built (doc/rigger.md, doc/gitops.md); a stack has one owner |
| **Rigger registries** | the operator, or the `rigger` section | the tree only needs them synced (§2, row 22) |
| **Every other API object** (table in §2) | `tree`, through stacks (§4.1) | needs only the API |

The existing post-cluster sections `security.acl.policies` and `secrets.bootstrap` keep working and can be moved into the tree step by step.

- `norca-deployer tree import` (phase 4) reads the live cluster and writes the tree for the kinds `tree` manages, so an existing platform can be adopted. Releases of other stacks are not imported: they keep their owner.
- A policy or secret defined both in the plan (`security.acl.policies`, `secrets.bootstrap`) and in the tree is a `validate` error, so two tools never fight over one object.
- **A stack has one owner.** A stack name used both by the `rigger` section of the plan and by the tree (a written stack of `stacks/`, or a generated name, §4.1) is a `validate` error. A release another stack owns is not taken over: the servers refuse it unless the apply adopts it.
- **Order when both run together** (the optional `tree:` line of the plan makes `apply` call `tree apply` as a stage): the `rigger` section of the plan first, then the tree in the order of §4.1.
- The same secret path must not be written by both `secrets.bootstrap` and `tree`; `validate` checks that too.
- Long-term, `security.acl.policies` and `secrets.bootstrap` can be deprecated in favour of one line in the plan: `tree { path = "./cluster-config" }`.

---

## 4. Directory Layout

One repository (or one directory) = one tree = one platform. The examples name its root `cluster-config/`. The root has a small `tree.hcl` and these trees:

- `modules/`: local Rigger modules the stacks of the tree share.
- `stacks/`: Rigger stacks written by hand.
- `global/`: cluster-wide objects: ACL, namespaces, quotas, node pools, admission policies, admin network policies, object kinds.
- `namespaces/<ns>/`: the plain objects of an application namespace.
- `kv/`: Consul KV entries.

Kinds marked *(no Rigger kind yet)* are refused by `validate` until Rigger has them (§4.1, *Kinds*).

```text
cluster-config/
├── tree.hcl                       # tree manifest: format version, requires, namespace of the generated global stack, stack order (§5.1)
├── README.md
├── .gitattributes                 # written by `tree init` (§4, file-system hazards)
│
├── modules/                       # local Rigger modules shared by the stacks of the tree
│   └── web-service/
│       ├── metadata.hcl
│       ├── variables.hcl
│       ├── outputs.hcl
│       └── web.norca.hcl
│
├── stacks/                        # Rigger stacks written by hand: one directory, one stack, any number of namespaces
│   ├── data-platform/
│   │   ├── stack.hcl              # rigger { name = "data-platform", namespace = "platform", registry, trust }
│   │   ├── rigger.lock.hcl        # rigger stack sync: versions and digests of the registry modules
│   │   ├── variables.hcl
│   │   ├── outputs.hcl
│   │   ├── secrets.hcl
│   │   ├── keys/registry.pub
│   │   ├── modules/               # optional: modules of this stack alone
│   │   └── values/
│   │       └── default.vars.hcl   # values of the stack; the run adds its own (§4.1)
│   └── observability/
│
├── global/                        # cluster-wide objects (into the stack tree-global)
│   ├── acl/
│   │   ├── policies/              # developer.acl.hcl, ci-deploy.acl.hcl, readonly.acl.hcl
│   │   ├── roles/                 # (no Rigger kind yet)
│   │   ├── auth-methods/          # (no Rigger kind yet)
│   │   └── binding-rules/         # (no Rigger kind yet)
│   ├── namespaces/                # apps.namespace.hcl, platform.namespace.hcl, monitoring.namespace.hcl
│   ├── quotas/                    # apps.quota.hcl (one limit{} per region)
│   ├── node-pools/                # (no Rigger kind yet)
│   ├── admission-policies/        # baseline.admission.hcl
│   ├── network-policies/          # (no Rigger kind yet) admin policies
│   └── object-kinds/              # loadbalancer.kind.hcl, postgres-cluster.kind.hcl
│
├── namespaces/                    # plain objects, by namespace: native Norca files without variables
│   ├── apps/
│   │   ├── jobs/                  # plain job files, applied as they are
│   │   │   ├── api.norca.hcl
│   │   │   └── worker.norca.hcl
│   │   ├── variables/             # api-config.var.hcl (a Norca Variable)
│   │   ├── secrets/               # api-db.hcl: secret blocks of the generated stack (items: !enc / !generate / @file)
│   │   ├── services/              # payments.external.hcl (external services)
│   │   ├── objects/               # <name>.object.hcl custom objects
│   │   ├── network-policies/      # (no Rigger kind yet)
│   │   ├── intentions/            # (no Rigger kind yet)
│   │   └── volumes/               # (no Rigger kind yet)
│   └── monitoring/ …
│
├── kv/                            # (no Rigger kind yet) Consul KV: file path = key, file content = value (§7.12)
│   ├── _kv.hcl
│   └── app/config/db.json
│
└── .tree/                         # optional: ignore patterns, lint rules
```

**Where what goes:**

- **A plain object** (a job without variables, a namespace, a policy) is one native file under `global/` or `namespaces/<ns>/`. It is applied as it is, and can also be applied by hand with the `norca` CLI.
- **Anything with variables, `for_each`, outputs or registry modules** is a module and a stack. The module lives in `modules/`, or in the stack's own `modules/` when no other stack uses it. The stack lives in `stacks/<name>/`. A module that more than one repository uses belongs in a Rigger registry and is referred to by address and version. A `variable` block in a plain job file is refused by `validate`, which points to the module form.
- **`stacks/` and `modules/` are at the top, not under `namespaces/<ns>/`.** A stack belongs to a region, not to a namespace: each module of a stack may set its own `namespace` (rigger-design.md 6.4), and the servers write each release with the caller's token in the release's namespace. A shared module has no namespace at all.

### Why namespace-first for applications

An application team owns a namespace: its jobs, its secrets, its intentions and its network policies move together and are reviewed together (`CODEOWNERS` per `namespaces/<ns>/`). The platform team owns `global/`. That maps cleanly onto Git permissions and ACL policies, and it is the unit people actually think in. Stacks and shared modules are owned per directory (`CODEOWNERS` per `stacks/<name>/` and `modules/<name>/`). A stack that writes into several namespaces needs a token with release write in each of them (§4.1, *Tokens*).

### File formats: HCL first

| Files | Primary | Secondary |
|---|---|---|
| Native Norca objects (jobs, policies, namespaces, quotas, ...) | `<name>.<kind>.hcl`, jobs `<name>.norca.hcl` (naming rules below) | none: they are module files |
| Deployer manifests (`tree.hcl`, secrets of `namespaces/<ns>/secrets/`) | **`.hcl`** | `.yaml`/`.yml` with the **same keys** |
| Rigger files (modules, stacks, values, locks) | the file names of rigger-design.md 4.5 | none: Rigger reads HCL only |
| Values of a stack | `values/*.vars.hcl` | none |

Rules:

- **HCL is the reference format.** All examples, `tree init` output, `tree import` output and the documentation use HCL. A YAML file is accepted wherever a manifest is, and is never required.
- **One decode path, like the deployer plan.** `pkg/plan/hcl.go` already loads HCL plans by shaping them into the YAML decoder, so both formats share keys, defaults and validation. The platform manifests reuse that mechanism; nothing can exist in one format only. Nested objects are blocks, lists of objects are repeated blocks, and a **block label is the `name`** (`region "eu-1" { ... }` is a YAML list entry with `name: eu-1`).
- **Never both for the same object.** `tree.hcl` and `tree.yaml` side by side is a validation error (ambiguous), not a merge. Mixing formats between *different* files is fine.
- **Order is explicit.** The decoder returns labelled blocks sorted by label, so anything where order matters is a list attribute (`layers`, `stack_order`), never the order of blocks in the file.
- **Conversion.** `tree convert --to hcl|yaml [-d DIR]` rewrites the manifests (never the native Norca files), so a team can move either way; comments are lost when converting from YAML. `tree fmt` formats HCL.

### Naming rules

- **The extension decides the kind; the directory only sorts.** A plain file is named as in a Rigger module (rigger-design.md 4.5): `<name>.norca.hcl` a job, `.namespace.hcl`, `.quota.hcl`, `.acl.hcl`, `.var.hcl`, `.kind.hcl`, `.admission.hcl`, `.external.hcl`, `.object.hcl`. That way a file moves into a module unchanged. `validate` warns when the directory names another kind (an `.acl.hcl` under `quotas/`), and refuses a file whose extension names no kind. The exceptions are the deployer manifests under `secrets/` and the files of `kv/`.
- **One object per file, and the file name before the extension is the name of the object** (Rigger's rule). A job's ID is its `id` attribute, or the label when it has none.
- **Namespace comes from the path** (`namespaces/<ns>/…`). If the file also sets `namespace`, it must equal the path; mismatch = validation error. This prevents "copy a folder, forget the namespace". Inside `stacks/` the namespace is the stack's (its `rigger` block and the `namespace` of each module), not the path.
- `.yaml`/`.yml` is the secondary form of the **deployer manifests only** (see *File formats* above). JSON plain files are not taken: Rigger reads HCL.
- Files and directories starting with `_` or `.` are ignored (drafts, shared snippets), **except** the two named control files `kv/_kv.hcl` (§7.12) and `_ignore.hcl` (§7.14).

### File-system hazards

*(Review item G10.)* The tree is checked out on Linux servers, Windows laptops and in CI. Four things differ between them and are handled by rule, not left to chance:

| Hazard | Rule |
|---|---|
| **Line endings** (`core.autocrlf`) change a KV value or a secret file between machines | Values are compared **byte for byte**. `tree init` writes a `.gitattributes` (`kv/** -text`, `**/secrets/** -text`, `**/*.pem -text`, `**/*.der -text`) so Git never converts those files. The only normalisation is the explicit per-key `trim_trailing_newline` (§7.12). `validate` warns when a file under these paths contains CRLF but the tree has no such `.gitattributes`. |
| **Case-insensitive file systems** (`App/` and `app/`, `Job.hcl` and `job.hcl`) | `validate` rejects two paths that differ only by case, and two objects whose names differ only by case in the same kind and namespace, even on a case-sensitive machine, so a commit that works on Linux cannot break a Windows checkout. |
| **Characters illegal on Windows** in a file or directory name (`<>:"|?*`, trailing dot or space, reserved names such as `CON`, `NUL`) | `validate` rejects them in paths. A KV key that needs such a character is declared in a `kv` manifest entry (`key = "a:b"`) with a neutral file name; keys are otherwise the relative path with `/` as separator. |
| **Long paths** (Windows 260-character limit) | `validate` warns above 200 characters of relative path and errors above 240; the tool itself reads the tree with extended-length path prefixes on Windows. In Git mode the tree is read in memory, so only the operator's checkout is affected. |

`validate` runs these checks on the raw file list **before** it reads any content, so a hostile or accidental path never reaches the parser. Symbolic links in the tree are rejected (the tool never follows them), and a path that leaves the tree (`..`) is an error (the path-traversal fuzzing of implementation plan P6-1).

### 4.1 How the tree is applied: stacks

The deployer turns the tree into Rigger stacks in memory and sends each one as files (`PUT /v1/rigger/stack/plan`, then `…/apply`, the request of the stack bindings, doc/gitops.md). The servers evaluate them, and their release engine applies them. There is no second engine (gitops-rigger-design.md section 7).

| Part of the tree | Becomes | Namespace of the stack | Releases |
| --- | --- | --- | --- |
| `global/` | the generated stack `tree-global` | `tree.namespace` of `tree.hcl` (default `default`, which always exists) | one release `tree-global`, from one implicit module of the files |
| `namespaces/<ns>/` | the generated stack `tree-ns-<ns>` | `<ns>` | one release `tree-<ns>`, from one implicit module of the files |
| `stacks/<name>/` | that stack, as written | its `rigger` block | its module blocks |
| `modules/<name>/` | nothing of its own: copied into each stack that uses it | – | – |

**Generated stacks.**

- The implicit module of a generated stack is the plain files of the directory (after the layers of §6.2), flattened into one module directory. Two files with the same kind and name in one stack are a `validate` error.
- The deployer writes a `metadata.hcl` (`module "tree-<ns>"`, no version) and a `stack.hcl` with one `module` block for that implicit module. The `region` of the stack is the region of the run. The secrets of `namespaces/<ns>/secrets/` become the stack's `secret` blocks.
- A generated stack has no registry modules, so it has no lock and no `trust`.
- The names `tree-global`, `tree-ns-*` and `tree-*` releases are reserved. A written stack or a stack of the plan that uses one is a `validate` error.
- Every release names its stack as owner, so prune is the servers': a file removed from `namespaces/apps/` is a resource removed from the release `tree-apps`, and its object goes with the next apply. A namespace directory removed altogether leaves the stack `tree-ns-apps` without its directory: with `prune` the deployer uninstalls its release, without it the plan says so.

**Written stacks.**

- A directory of `stacks/` is a plain Rigger stack: `stack.hcl`, `rigger.lock.hcl` (from `rigger stack sync`), `variables.hcl`, `outputs.hcl`, `secrets.hcl`, `values/`.
- A module of the tree's `modules/` is used as `source = "./modules/<name>"`, the stack's own module path. The deployer adds `modules/<name>/` from the tree to the files it sends when the stack directory does not have it. A module that is in both places is a `validate` error, so it is always clear which one runs.
- Outside the deployer (`rigger stack plan` in the stack directory) the module must be found too. An option of the `rigger` program that names the tree's `modules/` directory is an open point for Rigger (§13a).
- Registry modules are fetched by the servers from the registries they have synced (doc/rigger.md). The deployer syncs no registry.

**Values of a written stack.** `values/default.vars.hcl` of the stack, then the values of the run: the `values` of the `region` block in the startup configuration, or of the binding (§5.2, doc/gitops.md `values`); a later source wins per variable. The deployer sends the merged values as the values file of the request (`RiggerStackRequest.Values`).

**Order.** `tree-global` first. Then the stacks named in `stack_order` of `tree.hcl` (§5.1), in that order: written stacks and generated namespace stacks alike. Then the other `tree-ns-*` stacks in name order, then the other written stacks in name order. One stack at a time; the first stack that fails stops the run, and the plan of the rest is reported, not applied. Inside a stack the servers order the releases by their references and kinds (rigger-design.md 6.5, the fixed kind order of section 5).

**Tokens.** The servers check release write in the namespace of the request and write each release with the caller's token in the release's own namespace (`norca/rigger_operation.go` `authorizeRiggerStack`, `norca/rigger_host.go`). A token therefore needs object `write` on `rigger.norca.io/Release` and `read` on `rigger.norca.io/Operation` in every namespace a stack writes to. A written stack that also uses registry modules needs `read` on `rigger.norca.io/Registry`. The scoped tokens of §5.3 map to stacks: the token of `tree-ns-<ns>` is the one scoped to `<ns>`.

**Kinds.** A generated stack carries only the kinds Rigger releases hold today: quota, namespace, ACL policy, Variable, object kind, admission policy, external service, object and job (rigger-design.md section 5). The directories marked *(no Rigger kind yet)* in §4 are refused by `validate` with "not supported yet". They are ACL roles, auth methods and binding rules, node pools, network policies, service intentions, CSI volumes and Consul KV. Scheduler and autopilot settings have no directory yet; it is chosen when Rigger registers them. Each becomes supported when Rigger registers its kind (rigger-design.md section 11; gitops-rigger-design.md step G8b), not by a path of its own in the deployer.

---

## 5. Configuration: Tree Manifest and Startup Configuration

Two files with two jobs. Keeping them apart lets the tree live in Git without credentials, and stops a commit from widening what the program may do. *(Decision on review item G1: the machine and credential settings are given when the program starts.)*

| | **Tree manifest** `tree.hcl` | **Startup configuration** |
|---|---|---|
| Lives | in the tree, in Git | outside Git: on the machine, or in the job, that runs the program |
| Says | what the files mean | how this process runs and what it may do |
| Holds | format version, `requires`, the namespace of `tree-global`, `stack_order`, `layers` | the cluster (address, tokens per stack), `placement` and `proxy`, `limits`, `watch`, the Git binding, values of written stacks |
| Read | with the tree, on every run | **once when the program starts** (the reloadable parts again on `SIGHUP`) |
| Changed by | a commit | the operator |
| Can a commit change it? | yes | **no** |

### 5.1 Tree manifest: `tree.hcl`

Portable and free of credentials. (`tree.yaml` is accepted as the equivalent secondary form.)

```hcl
tree {
  name      = "acme-prod"
  version   = "norca.io/tree/v1alpha1"       # format version of this tree (required)
  environment = "prod"                      # optional: the name of the environment (6.2)
  namespace = "platform"                     # namespace of the generated stack tree-global (§4.1); default "default"
}

requires {                                   # versions the files were written for (7.7)
  norca = ">=0.3.0 <0.5.0"
}

stack_order = ["data-platform", "tree-ns-apps", "observability"]   # applied first after tree-global, in this order (§4.1)
```

The YAML form has the same keys. Environments are layers of trees (6.3); `layers` belongs here.

- `tree.environment` is optional and only informs (decided 2026-11-28): it names the environment in the plan heading, the JSON output and the binding status, so a reader sees which environment a plan is for. It changes nothing that is applied. With `environment = "prod"` (or `production`), `validate` warns for every namespace the tree writes whose meta `environment` is not `production` or `prod`, because the releases there are not atomic by default (§7.8).
- `tree.namespace` must be a namespace that exists before the first apply, or one of the namespaces `tree-global` itself creates: the deployer then applies the namespace in `default` first. Which of the two is simpler is decided at implementation.
- `stack_order` names written stacks (directories of `stacks/`) and generated namespace stacks (`tree-ns-<ns>` of a directory `namespaces/<ns>/`). Any other name, and `tree-global`, is a `validate` error.
- Registries and `trust` are not in `tree.hcl`: a written stack names them in its own `rigger` block, and the generated stacks use no registry.
- The `graph` and `defaults` blocks of the earlier draft are gone. Ordering is §4.1, and whether a stack waits for healthy deployments is the `atomic`/`timeout` of its modules (rigger-design.md 6.4).

**Format version** *(review item G9)*. `tree.version` names the format of the tree (`norca.io/tree/v1alpha1`), independent of the `requires` block (which is about Norca versions) and of the tool version.

- **Required.** A tree without it is a `validate` error with the value to add; `tree init` writes it.
- **Unknown version = refusal.** `validate`, `plan` and `apply` stop with `tree format norca.io/tree/v2 is not supported by this tool (supports v1alpha1)`; nothing is guessed. A tool that is *newer* than the tree reads older versions it still supports and says so.
- **Evolution.** Additive changes (new optional keys) keep the version. A breaking change gets a new version; the tool supports the previous one for at least one release and `tree convert --to-version vN` rewrites the manifests (not the native Norca files), like the HCL/YAML conversion of §4. While the version is `v1alpha1` the format may still change, which is also why the feature has an experimental gate (implementation plan, P6-6).
- **Layers and trees from several repositories** (6.3) must all carry versions the tool supports; mixed versions are fine as long as each is supported.
- **The startup configuration** (5.2) is versioned by the tool release, not by a field: it is not a committed format and an unknown key in it is an error.

### 5.2 Startup configuration

A file read when the program starts (`norca-tree.hcl`), or the same settings as flags and environment variables for simple cases (`--server`, `NORCA_ADDR`, `NORCA_TOKEN` as in the existing `clusterFlags`). It is **not part of the tree** and not committed.

```hcl
# norca-tree.hcl - read when the program starts; never committed

# One `region` block per region, each with its own endpoint (6.1). A run goes to one of them (--region).
region "eu-1" {
  address       = "https://10.0.0.11:4646"
  token         = "!enc:v1:aes-gcm:..."      # the default token; or token_file / token_env, see 5.3
  ca_cert       = "./certs/ca.pem"
  values        = { replicas = 3 }           # values of the written stacks for runs in this region (4.1)

  # Optional: more tokens, each used only for the stacks it is declared for (5.3).
  scoped_token "foundation" {
    token_env = "NORCA_TOKEN_FOUNDATION"     # a management token: tree-global writes namespaces and ACL policies
    stacks    = ["tree-global"]              # stack names or globs (4.1)
  }
  scoped_token "apps" {
    token_file = "/etc/norca-deployer/apps.token"
    stacks     = ["tree-ns-app-*"]           # the namespace stacks of the app-* namespaces
  }
  scoped_token "readonly" {
    token_env = "NORCA_TOKEN_RO"
    for       = ["plan"]                     # plans, drift watch and status (read only): a stack plan needs
                                             # release read and read on what it compares (13a, G25)
  }

  consul_kv {                                # where `kv/` is written (7.12); omit if no KV is managed
    type    = "norca-compat"                 # norca-compat | consul
    address = "http://10.0.0.21:8500"        # a client whose consul_compat.bind is reachable, or a Consul agent/server
    token   = "!enc:v1:aes-gcm:..."          # Norca ACL token (norca-compat) or Consul ACL token (consul)
    backend = "variables"                    # for the offline limit checks: variables | bus | consul
  }
}

# Optional: take the endpoints from an existing deployer plan instead of
# repeating them (addresses, CA, ACL token file).
# from_deploy_plan = "../cluster-deploy.hcl"

placement {                                  # how this program reaches the cluster (11b)
  type    = "proxy"                          # auto (default) | local | proxy | node
  proxy   = "bastion"
  inbound = "none"                           # direct | none: can a webhook reach the watcher (11a)
}

proxy "bastion" {
  type              = "ssh"                  # ssh | http | socks5
  host              = "bastion.example.com"
  ssh_user          = "deploy"
  ssh_key_path      = "~/.ssh/norca_deployer"
  host_key_checking = "strict"
}

sync_registries = ["global"]                 # registries the servers sync before the plans (gitops-rigger-design.md 5.10)

limits {                                     # what this process may do; a commit cannot raise it
  kinds          = ["acl", "namespace", "quota", "node-pool", "job", "secret"]   # identifiers and groups (8.1); all when omitted
  prune          = false                     # deletion is opt-in; see 7
  on_failure     = "rollback"                # rollback | stop (7.8): atomic applies of the generated stacks
  rollback_scope = "release"                 # release | run
  max_changes    = 50                        # a run that would change more objects needs --yes-really
  test           = "off"                     # off | after-apply: run the release tests after each stack (7.8)
  read_parallelism = 4                       # API read tuning (7.15): also `rate`, `stale_reads`, `plan_time_budget`
}
```

`watch { ... }` (11a) and `gitops { ... }` (11) are further blocks of the same file.

Rules:

- **A commit can never widen the process.** The kinds it may write, whether it may prune, which tokens and proxies it uses: all of that is decided by the operator at start. The tree manifest has no key that raises any of it, and `validate` rejects a tree that tries (a `limits` or `region` block in `tree.hcl`).
- **Precedence:** flags, then environment, then the startup file, then built-in defaults. The tree manifest never overrides a startup setting.
- **Location:** `--config FILE`, else `NORCA_DEPLOYER_PLATFORM_CONFIG`, else `./norca-tree.hcl`. `tree service install` (11a) writes `/etc/norca-deployer/norca-tree.hcl` (Windows: `%ProgramData%\norca-deployer\norca-tree.hcl`) with mode `0600`. `tree init` adds the file name to `.gitignore` and writes a commented template.
- **Safety checks at start:** a startup file inside the tree it applies is a `validate` warning; a file readable by group or others triggers a warning (it can hold `!enc:` tokens and proxy passwords).
- **Reload:** `SIGHUP` re-reads `watch`, `limits`, tokens and proxy profiles. Listen address and placement type need a restart.
- **As a Norca job** (11a): the file is delivered from the built-in secret store as a file (the `secret` block), never put in the job file.
- Values may be `!enc:`, `!env:` or `@file` (§9), like everywhere else.

### 5.3 The token and its permissions

*(Decision on review item G2: the token is part of the startup configuration.)* The program gets the token when it starts, from the first of: `token_env` (default `NORCA_TOKEN`), `token_file`, an inline `!enc:` value, or, with `node` placement and a Norca job, the **workload identity** of the job (11b). A tree and a commit never carry a token.

#### Permission table per kind

*(Work package P0-9, read from the Norca server source: the ACL package `norca/acl` and the RPC endpoints in `norca/norca/*_endpoint.go`. To be confirmed on a lab cluster by the P0-7 spikes before P1 relies on it.)* One row per kind identifier (§8.1). "Policy" is what an ACL policy of the token must contain; **management** means a management token is required and no policy can replace it.

| Kind | Read (for `plan`, drift) | Write / delete | Level | Source |
|---|---|---|---|---|
| `acl-policy` | **management** sees all; any other token lists only *its own* policies | **management** | management | `ACL.UpsertPolicies`, `DeletePolicies`, `ListPolicies` |
| `acl-role` | management sees all; other tokens only their linked roles | **management** | management | `ACL.UpsertRoles`, `ListRoles` |
| `acl-auth-method` | **any token lists** name, type, default and hash; the **configuration** (get) is management (lab, 13.3). With the proposed `acl` read rule (issue-norca-readonly-plan) the configuration is read through an allowlist that excludes `OIDCClientSecret`. **The client secret is never read back**: the plan compares it by the ledger digest of the last written value (§9 rule 5); the stub `Hash` (it covers the secret) only signals an out-of-band change | **management** | management for writes and for the configuration | `ACL.ListAuthMethods`, `GetAuthMethod`, `UpsertAuthMethods` |
| `acl-binding-rule` | **management** | **management** | management | `ACL.GetBindingRules`, `UpsertBindingRules` |
| `namespace` | any capability in the namespace: `ListNamespaces` returns the namespaces the token has a capability in, `GetNamespace` answers per name (so a policy with `namespace "*" { capabilities = ["list-jobs"] }` can read all; `GetNamespaces` is the replication RPC between regions, not used by the tool) | **management** (upsert and delete) | management for writes | `Namespace.ListNamespaces`, `GetNamespace`, `UpsertNamespaces`, `DeleteNamespaces` |
| `quota` | `quota` policy `read` | `quota` policy `write` | policy | `ACL.AllowQuotaRead/Write` |
| `node-pool` | `node_pool` capability `read` | `node_pool` capabilities `write`, `delete` (per pool name or glob) | policy | `NodePool.GetNodePool`, `UpsertNodePools`, `DeleteNodePools` |
| `scheduler-config`, `autopilot-config` | `operator` `read` | `operator` `write` | policy | `Operator.SchedulerSetConfiguration`, `AutopilotSetConfiguration` |
| `admission-policy` | `admission` `read` | `admission` `write` | policy | `ACL.AllowAdmissionRead/Write` |
| `admin-network-policy` | `operator` `read` | `operator` `write` | policy | `allowNetworkPolicyOperator` |
| `object-kind` | `operator` `read`, or any object capability on that kind | `operator` `write` | policy | `ACL.AllowObjectKindRead/Write` |
| `network-policy` | namespace capability `read-network-policy` | namespace capability `write-network-policy` | policy, per namespace | `AllowNetworkPolicyRead/Write` |
| `custom-object` | object capability `read` for `<group>/<Kind>` in the namespace | object capability `write` (status and events have their own: `write-status`, `write-events`; finalizers `manage-finalizers`) | policy, per namespace and kind | `ACL.AllowObjectOperation` |
| `job` | `read-job`, `list-jobs`; `plan` needs **`submit-job`** (see below); `validate` needs `read-job`; the HCL parse (`/v1/jobs/parse`) needs `parse-job` **or** `submit-job` | `submit-job` (register, stop, revert), `scale-job`; `host_volume` `mount-readonly`/`mount-readwrite` for jobs that mount host volumes, `csi-register-plugin` for plugin jobs | policy, per namespace | `Job.Register`, `Plan`, `Validate`, `Deregister`, `Revert` |
| `variable` | `variables` path capability `read`, `list` | `variables` path capabilities `write`, `destroy` | policy, per namespace and path | `Variables.Apply` |
| `secret` | namespace capabilities `read-secret`, `list-secrets` (path-scoped) | `write-secret`, `destroy-secret` (path-scoped) | policy, per namespace and path | `Secret.Read`, `Upsert`, `Destroy` |
| `kv-entry` (Consul-compatible endpoint) | `variables` capabilities `read`, `list` on the KV path | `write`, `destroy` | policy | `ConsulKV.Get`, `Put`, `Delete` |
| `volume` (CSI only, see 13.2) | `csi-read-volume`, `csi-list-volume` | `csi-write-volume` (register, create, deregister, delete) | policy, per namespace | `CSIVolume.Register`, `Create`, `Deregister`, `Delete` |
| `service-intention` | `read-job` | `write-intention` | policy, per namespace | `ServiceIntention` endpoint |
| `external-service` | `read-job` | `register-external-service`; **and `operator` `write`** when the registration has no node and no TTL check (`node` `write` for a node-bound one) | policy, per namespace (+ operator) | `ServiceExternal` endpoint |
| releases and operations (every stack) | object `read` on `rigger.norca.io/Release` and `rigger.norca.io/Operation` in the namespace of the stack | object `write` on `rigger.norca.io/Release` in every namespace the stack writes to (a plan needs only `read`, and `read` on what it compares: 13a, G25); `read` on `rigger.norca.io/Operation`; `read` on `rigger.norca.io/Registry` for a written stack with registry modules | policy, per namespace | §4.1 *Tokens*; `authorizeRiggerStack`, `rigger_host.go` |
| version preflight (7.7) | `agent` read (`/v1/agent/self`) **and `node` read** (`/v1/agent/members`, `/v1/nodes`; lab, 13.3: `agent` read alone is 403 on members) | none | policy | `Agent.Members`, `Node.List` |
| any run: the token's own rights | `acl token self` (any token may read its own token and policies) | none | any | `ACL.GetToken` |

**A release checks each resource** with the right of its kind's endpoint (`allowReleaseResourceWrite` in `norca/norca/release_apply.go`), so the rows above still say what a stack's token needs for the kinds the stack holds. A resource the release owns and leaves unchanged needs no right: a team can upgrade its namespace stack although an operator wrote something into it that the team may not write.

**What the table decides**

1. **A management token is unavoidable for exactly two groups:** the ACL kinds (`acl-policy`, `acl-role`, `acl-auth-method`, `acl-binding-rule`) and `namespace`. Nothing in a policy can replace it. Every other kind has a policy form: `quota`, `node-pool`, `scheduler-config`, `autopilot-config`, `admission-policy`, `admin-network-policy` and `object-kind` through the `quota`, `node_pool`, `operator` and `admission` rules, and all namespaced kinds through namespace capabilities.
2. **For the tree this means:** `tree-global` holds namespaces and ACL policies, so its token is a management token. If that is too wide for the other foundation kinds (quotas, admission policies, object kinds), they can go into a written stack of their own, applied with a policy token.
3. **A "read only" token cannot plan every kind** (state of the Norca source that was read). The ACL kinds answer fully only to a management token (other tokens see only their own policies and roles; auth methods and binding rules are management only), and `Job.Plan` needs `submit-job`, a write capability. **Namespaces are not a problem**: a token with any capability in every namespace lists and reads them all. Decisions (proposals):
   - `plan` for `acl-*` uses the management token for **read calls only**. The tool performs only `GET`s in `plan` (a property the tool guarantees and tests, not one the cluster enforces); the log shows which token read. If that is not wanted, `plan` reports those kinds as *not plannable with this token*, never as "unchanged".
   - For jobs the plan method is chosen by the token: with `submit-job` the server-side `job plan` of §7.3; **without it the tool falls back to a read-back compare** (`GET` of the job, parse of the file through `/v1/jobs/parse`, normalised diff). The fallback loses the server's placement preview and the check-and-set index, and says so in the plan output.
   - So a truly read-only `plan` token can plan everything except the ACL kinds, and jobs by read-back.
   - **A Norca ticket would remove both exceptions** with two small, optional capabilities, an `acl` read rule and a `plan-job` capability ([issue-norca-readonly-plan.md](issue-norca-readonly-plan.md), draft). The tool detects them by Norca version and token policy (kind-support matrix, §7.7): on a Norca that has them, a `for = ["plan"]` token plans every kind with server-side job plan; on an older one the fallbacks above apply. The design never depends on the ticket.
4. **Preflight (stage A of §7.8)** reads `acl token self` and the policies of every configured token and checks each row of the table that is in scope, with the message form `token "apps" lacks write-intention in namespace apps`. The path-scoped rows (`variable`, `secret`, `kv-entry`) are checked for the paths the tree uses.
5. **Namespace-scoped tokens work for the namespaced kinds only.** `job`, `variable`, `secret`, `network-policy`, `custom-object`, `volume`, `service-intention` and `external-service` (the last with the operator exception) can be written with a policy limited to the namespaces of one team, which is what a token per namespace stack (next subsection) relies on. `external-service` is the one namespaced kind that can still need an `operator` rule: that is documented as a reason to keep it out of an application team's token.
6. **Checked in the source (13.2):** every capability name above is accepted by the policy parser (`acl/policy.go`); dynamic host volumes have **no** API (the `volume` kind covers CSI volumes); a workload identity can never be a management token. **Still to check on a lab cluster:** that `norca acl policy apply` accepts the example policies below as written.

An example policy for the application token of the previous example (`scoped_token "apps"`), as a native Norca policy:

```hcl
# policy "platform-apps"  (for scoped_token "apps")
namespace "apps" {
  capabilities = [
    "list-jobs", "read-job", "submit-job", "parse-job", "scale-job",
    "write-intention", "write-network-policy", "read-network-policy",
    "read-secret", "list-secrets", "write-secret",
  ]

  object "rigger.norca.io/Release" {         # the release tree-apps of the stack tree-ns-apps
    capabilities = ["read", "write"]
  }

  object "rigger.norca.io/Operation" {       # follow the operation of the stack
    capabilities = ["read"]
  }

  variables {
    path "*" {
      capabilities = ["read", "list", "write"]
    }
  }
}
```

#### Several tokens: one per stack

*(Follow-up to G2, revised 2026-11-28.)* One token for everything is the default and stays the simple case. A release is written with one token, the caller's, so a token is chosen **per stack**: the startup file declares named tokens with the stacks they may write (names or globs), and one token may be the read token of `status` and the drift watch:

```hcl
region "eu-1" {
  token = "!enc:..."                          # default: used where no scoped token matches

  scoped_token "foundation" {
    token_env = "NORCA_TOKEN_FOUNDATION"
    stacks    = ["tree-global"]
  }
  scoped_token "apps" {
    token_file = "/etc/norca-deployer/apps.token"
    stacks     = ["tree-ns-app-*"]
  }
  scoped_token "readonly" {
    token_env = "NORCA_TOKEN_RO"
    for       = ["plan"]
  }
}
```

Rules:

- **Choice per stack.** For every stack the tool picks the token whose `stacks` match its name; the most specific wins (a name over a glob). No match: the default `token`; no default: the stack is blocked, never written with a stronger token.
- **`plan` scope.** Plans, `status` and the drift watch use the token with `for = ["plan"]` if present: a stack plan needs release `read` and read on what it compares (13a, G25). A resource it may not write shows `Permission denied` in the plan. Writes never use the read token.
- **Preflight check** (stage A of 7.8, before any write): `acl token self` is read for every declared token and checked against the rows of the table for the kinds in each stack's plan and the namespaces it writes to. A missing capability is reported as `token "apps" cannot write job in namespace app-x (stack tree-ns-app-x)`, and the run stops with zero writes.
- **Narrower, never wider.** `limits.kinds` is still the outer boundary: a stack whose plan writes a kind outside it is blocked, whatever its token could do.
- **In the tree.** The tree carries no token: a `scoped_token` or `token` in `tree.hcl` is rejected (§5.2 rules). Which team's directory is written with which token is the operator's decision, which is also how the namespace-scoped tokens of G15 are realised.
- **Watcher.** A stack with no usable token is planned and reported only. `SIGHUP` reloads tokens.
- **Logs** record the token **name** (`foundation`), never the secret or its accessor.
- **The safe first rollout** is a management token for `tree-global` and scoped tokens for the namespace stacks.

---

## 6. Regions and Environments

*(Revised 2026-11-28: the multi-region design is removed. It will be defined again when multi-region is taken up.)*

### 6.1 One endpoint per region

- **A run targets one region**, through that region's endpoint: address, CA, tokens and placement in a `region` block of the startup configuration (§5.2). `--region R` picks the block; with one block it is the default.
- **Several regions:** each region gets its own `region` block with its own endpoint, and the tree is run against each of them separately: one `tree apply --region R` per region, or one tree binding per region (§11). The runs of different regions know nothing of each other.
- **Differences between regions** go through the values given with the run: the `values` of the `region` block, or of the binding (§4.1, *Values of a written stack*).
- **Not designed now:** what Norca's replication between the regions of a federation means for a tree applied in several regions, an order or staging between regions, reaching a region through another region's API, and per-region watchers. None of it is assumed by the rest of this document.

### 6.2 Environments (dev/stage/prod)

Environments are **separate trees**, not a flag inside one. Reasons: different tokens, different reviewers, different promotion pace. A tree may name its environment with the optional `tree.environment` (§5.1); the name only informs. Promotion between environments is a pull request (gitops-rigger-design.md section 9, question 3).

Two supported patterns:

1. **Repo per environment** (simplest; promotion = a pull request between repos or a cherry-pick).
2. **One repo, one directory per environment**, with shared pieces pulled in by layers:

```hcl
# envs/prod/tree.hcl
layers = [
  "../../common",         # lowest priority
  ".",                    # this directory wins
]
```

Layers resolve by **relative path, later layer wins per file**. No merging.

### 6.3 Multiregion jobs

A job with a `multiregion` block is an ordinary job file for the tool; Norca's own staged rollout handles its regions. How such a job fits a tree that is run in several regions belongs to the multi-region design (§6.1).

---

## 7. Reconciliation Model

### 7.1 The loop

*(Revised 2026-11-28 for Rigger.)*

```text
validate  →  load tree  →  build stacks  →  resolve values  →  plan every stack  →  (apply stack by stack)
```

1. **Load**: walk the tree, parse files, compute the effective file set (layers, §6.2).
2. **Build stacks**: the generated stacks `tree-global` and `tree-ns-<ns>`, and the written stacks of `stacks/` with the shared modules they use (§4.1).
3. **Resolve**: `!enc:`, `!env:`, `@file` values (§9) and the merged values of each written stack. Secrets are only decrypted in memory and sent as secret values of the request.
4. **Plan**: `PUT /v1/rigger/stack/plan` for every stack, in the order of §4.1, before anything is written. The servers answer per release: `unchanged`, `install`, `upgrade` with the create, update and delete of each resource, `remove`, and per stack secret `create`, `keep` or `overwrite`.
5. **Guard**: the deployer checks the plans against the startup limits and the flags of the run (§7.4, §7.6). A stack whose plan breaks a rule is not applied.
6. **Apply**: `PUT /v1/rigger/stack/apply` stack by stack, following each operation through its log. The first stack that fails stops the run. Inside a stack the servers apply the releases in dependency order and stop at the first failed release (rigger-design.md 9.2).

A run repeated after a failure continues where it stopped: what is done plans as `unchanged`, because the servers compare the input digest of each release (rigger-design.md 9.3).

Around these steps (gitops-rigger-design.md 5.10): before the first plan the servers sync the registries named in `sync_registries`, and a failed sync stops the run; after each stack's operation succeeds, the release tests run when `limits.test` is `after-apply` or `--test` is given (§7.8), and the outputs of the stack are printed without the sensitive ones, an output `next_steps` as `rigger` prints it after an install.

### 7.2 Plan output

```text
Tree acme-prod (environment prod), region eu-1, commit 3f2a91c
Stack tree-global (namespace platform)
  release tree-global        upgrade
    + namespace      monitoring
    ~ quota          apps
    = acl-policy     developer
Stack tree-ns-apps (namespace apps)
  release tree-apps          upgrade
    ~ job            api          (rolling update, 3 allocs)
    - job            old-batch    removed from the tree; purged by the apply (needs --prune --prune-jobs)
  secret api/db                   keep
Stack data-platform (namespace platform)
  release rustfs             unchanged
  release forgejo            upgrade 0.2.0 -> 0.2.1
Releases outside the tree's stacks
  release tree-legacy        its directory namespaces/legacy/ is gone; kept (use --prune)
Summary: 3 stacks to apply; 3 upgrade, 1 unchanged, 1 not pruned; blocked: tree-ns-apps (a job delete needs --prune-jobs)
```

The per-resource lines are the servers' plan. The deployer adds only the headings, the guard notes and the summary. Exit codes are defined for every command in §10 (*Exit codes and machine output*). For `plan --detailed-exitcode` they mirror `terraform plan -detailed-exitcode` (0 nothing, 2 changes, 1 error), which is also what `rigger stack plan` returns.

### 7.3 How "changed" is decided

*(Revised 2026-11-28.)* The deployer compares nothing itself. The servers decide:

- **Per release:** the digest of the module, the values and the settings against the input digest of the release's current revision. Equal means `unchanged`, and the release is skipped. This is right from any machine, because the digest is in the cluster (rigger-design.md 9.3).
- **Per resource**, for a release that differs: the server's plan. Jobs and objects are compared by their own endpoints (the job plan of Norca), the other kinds by the release engine against their live state (`norca/norca/release_apply.go`).
- **Secrets of a stack:** `create` when the path is missing, `keep` when it exists, `overwrite` only when the `secret` block says so. Values are never compared and never printed.
- **Drift** (a live object changed outside the release) is the server's drift check: the release gets the `Drifted` condition and a `ReleaseDrifted` event (`norca/norca/release_drift.go`). `plan` shows it per release, and the next apply writes the release's content again.

Of the earlier draft only the rule "values never print" is left. The read-back compare per kind, the digest markers of secrets and Variables, and the local digest files are gone.

### 7.4 Deletion and ownership

*(Revised 2026-11-28: the release owner replaces the ledger.)*

**Ownership is in the cluster, on the releases.** Every release names its stack as owner (`ReleaseOwner`: kind `stack`, namespace, name), and every resource belongs to exactly one release. A live object that no release owns is not taken over: the servers refuse an apply that names it, unless the apply adopts it (`tree import --adopt`, §3). So nothing outside the tree's releases can be changed or deleted by a run, and no ledger, marker or local file is needed for that.

There are two kinds of removal, and they behave differently on the server:

| What is removed from the tree | What the servers do | What the deployer adds |
| --- | --- | --- |
| **A file** of a generated stack, or a resource of a module | The release upgrade **deletes** the resource. This is not optional on the server: a revision is the whole content of the release (`release_apply.go`). A job is deregistered **with purge**, so its history goes too. | A guard. The plan lists the deletes, and `apply` refuses a stack whose plan deletes anything without `--prune`, a job without `--prune-jobs` as well, and a Variable without `--prune-data` as well. The stack is then *blocked* (exit code 3), not half applied: a release cannot be applied without its deletes. The way out is to restore the file or to give the flag. |
| **A whole release**: a namespace directory removed (the release `tree-<ns>`), or a `module` block removed from a written stack | The release stays installed. It is shown as `remove` and uninstalled only with `--prune` (rigger-design.md 9.3). | For a generated stack the deployer finds the release by its reserved owner name (`tree-ns-*`) and uninstalls it with `--prune`. |
| **A written stack directory** | Nothing. The servers do not know that the stack is gone. | `plan` and `status` list releases whose owner is a stack the tree no longer has, as a warning. To retire a written stack, run `tree destroy --stack S` (by hand only), or remove its `module` blocks and apply with `--prune`; then delete the directory. A marker of the tree on the owner would let the deployer prune it in one step (§13a, G21). |
| **A stack secret** | Never deleted. A `secret` block removed from a stack leaves the secret in the store (rigger-design.md 6.3). | Nothing: a secret is removed by hand. |

**Renames.** A renamed file is a delete of the old resource and a create of the new one, inside the same release. The deployer helps the reader: a delete and a create of the same kind with the same content apart from the name, in one release plan, are shown as `looks like a rename`. The servers apply creates before deletes, so a renamed job starts before the old one is purged. A `moved` hint is a Rigger question (gitops-rigger-design.md section 9, question 2), not the deployer's.

**A kind that cannot be removed while in use** (a namespace with jobs, a quota used by a namespace) is refused by the server with its own message, and the release apply fails. The deployer never forces it.

**Never removed by the tree:** the `default` namespace and node pool, the built-in `anonymous` policy, every token, and every object that no release of the tree owns. A plan that would delete one of the first three blocks its stack, whatever the flags.

### 7.5 Ordering

*(Revised 2026-11-28.)* Two levels, neither of them the deployer's own kind list:

1. **Between stacks**, the order of §4.1: `tree-global`, then the stacks of `stack_order`, then the other `tree-ns-*`, then the other written stacks.
2. **Inside a stack**, the servers' order: releases by their references (`module.<a>.<output>`, rigger-design.md 6.5), and the resources of a release in the fixed kind order of the release engine. That order is quota, namespace, ACL policy, Variable, object kind, admission policy, external service, object, job (rigger-design.md section 5). What a release no longer has is deleted after what it has was applied.

The kinds without a Rigger kind yet (§4.1, *Kinds*) get their place in the servers' order when Rigger registers them. The deployer does not slot them in.

Every stack of a run goes to the region of the run (§6.1).

`validate` checks the references that cross stacks before anything is sent: a namespace a file or module names exists in `global/namespaces/` or in the cluster; a quota a namespace names exists; a job's `node_pool` exists; a `secret` block path exists in `secrets/` of its namespace or in the cluster. A reference to something only a later stack creates is an error, with the stack that would create it.

### 7.6 Safety rails

- **Dry-run first**: `apply` always plans every stack and prints the plan; `--yes` for non-interactive use.
- **Blast-radius guard**: if the plans of a run would delete or update more than N resources together (`limits.max_changes`, default 50), it refuses without `--yes-really`. The count comes from the servers' plans.
- **Delete guards**: the `--prune`, `--prune-jobs` and `--prune-data` rules of §7.4, checked on the servers' plans before the first write.
- **Kinds the process may write**: a stack whose plan writes a kind outside `limits.kinds` is blocked, not applied in part (a release is applied whole).
- **Admission and quotas still apply**: a resource the cluster rejects fails its release with the cluster's message.
- **ACL least privilege**: the tool can run with scoped tokens (§5.3), one per stack, instead of management; a management token is needed only for a stack that writes ACL policies or namespaces (in practice `tree-global`).
- **One operation per stack**: the servers run at most one operation per stack (409 otherwise, gitops-rigger-design.md 2.2). A second run of the same tree waits for the stack, then plans it again. The deployer holds no lock of its own. Whether a whole tree needs a lock across its stacks is the open question 5 of gitops-rigger-design.md.

### 7.7 Version Check and Reconciliation with Norca

Every `plan`/`apply` starts with a **preflight version check**, per region, before anything is read or written. The tree must never be applied blindly to a cluster that is mid-upgrade or older than the files expect.

**What is read** (read-only calls; exact endpoints to confirm against Norca, the CLI prints `Norca v0.3.0 (Nomad API 1.6.5 compatible)`):

| Item | Source | Used for |
|---|---|---|
| Server versions, one per server | agent members / `agent self` per server | mixed-version detection |
| Client (node) versions | node list | warn when a kind needs a newer client (e.g. network rules need nftables-capable nodes) |
| Region and leader | agent self, status leader | the endpoint answers for the region its `region` block names |
| Deployer binary version and the API level it was built against | build info | the deployer's own compatibility window |
| Releases of the tree's stacks (revision, owner, `Ready`, `Drifted`) and registry sync state | release + registry API, read-only | the status table, orphan releases (§7.4), whether the registries of a written stack are synced |
| Object kind schema versions | object-kind API | custom object compatibility |

**Checks, in order:**

1. **Reachability and identity**: the region is reachable the way `placement` says (§11b; the probes are cached in `placement.json`), and the region name returned by the cluster equals the one in the startup configuration.
2. **Consistency**: all servers of a region on the same `major.minor` (patch may differ). Mixed versions mean an upgrade is running → `apply` is **refused** unless `--allow-mixed-versions`; `plan` only warns. This is the same reason `apply` of the node installer rolls servers one at a time.
3. **Tree requirement**: `tree.hcl` may declare the Norca versions the files were written for:

   ```hcl
   requires {
     norca    = ">=0.3.0 <0.5.0"   # semver range, checked per region
     deployer = ">=0.9.0"
   }
   ```

   Outside the range → error (with the actual versions in the message). Inside → proceed.
4. **Kind support matrix**: the deployer carries a small table `kind → minimum Norca version` (generated from the Norca feature status: e.g. network rules, operator framework, secrets store, native mTLS intentions). A kind with files in the tree but unsupported by a region is an **error before any write**, not a late 404. An unknown newer Norca version is **not** blocked (the table is a floor, never a ceiling); it prints a note.
5. **Capability probe as safety net**: for kinds not in the table, a read call (`list`) is made first; 404/501 marks the kind unsupported for that region with a clear message.
6. **Upgrade ordering hint**: if the tree requires a newer Norca than the region runs, the message says "upgrade the cluster first with `norca-deployer apply`" – the platform tool never upgrades nodes (non-goal).

**Reconciling with the node plan.** When `from_deploy_plan` is set (§5.2), the platform tool also compares the **Norca version the cluster plan installs** (`artifacts`) with the **observed versions**: a difference means an `apply` of the node plan is pending or failed, and the tree run stops at preflight with that explanation. So the two tools cannot disagree silently about what version the cluster is.

**Reporting.** `tree status --versions` shows the whole picture in one table:

```text
Region  Servers (versions)     Clients            Node plan   Tree requires   State
eu-1    3 × 0.3.0              9 × 0.3.0          0.3.0       >=0.3 <0.5      ok
eu-2    2 × 0.3.0, 1 × 0.2.4   6 × 0.2.4          0.3.0       >=0.3 <0.5      MIXED – upgrade running, apply blocked

Stacks of the tree
Stack           Releases   Ready   Drifted   Last operation           Commit
tree-global     1          1       0         Succeeded 10:42          3f2a91c
tree-ns-apps    1          1       1         Succeeded 10:43          3f2a91c
data-platform   2          1       0         Failed 10:45 (forgejo)   3f2a91c
```

Version drift of **workloads** (job images) is reported, not enforced: the tree is the intent, the cluster is the fact, the table shows both. Releases owned by stacks the tree no longer has are listed below the table (§7.4); releases of other stacks are not shown.

### 7.8 Validation Stages and Rollback

*(Revised 2026-11-28: release revisions and atomic applies replace the journal and the before-images.)* Two separate protections stay: **nothing is written if the tree is invalid**, and **a failed write is undone where Norca allows it**. The second one is now the release engine's.

#### Stage A – all-or-nothing preflight (no cluster change)

Everything that can be known before the first write is checked for **every stack** first. Any error aborts the run with **zero changes**.

| # | Check | Example failure |
|---|---|---|
| 1 | **Syntax**: every file parses as HCL, with file, line and column in the message. The full evaluation (module schemas, expressions, the job decoder) is the servers' plan of check 5; offline, without a cluster, `rigger stack validate` on the built stacks gives the same answer when the `rigger` program is installed (`tree validate --rigger`). Whether the deployer imports the Rigger core instead is open (§13a, G23) | `namespaces/apps/jobs/api.norca.hcl:42:7: unclosed block` |
| 2 | **Shape**: the naming rules of §4 (extension, one object per file, namespace from the path), unique identities per stack, reserved names (§4.1), supported kinds | namespace in path ≠ namespace in file |
| 3 | **Cross references** (§7.5) | namespace names quota `apps` that is not in the tree or live |
| 4 | **Values**: `!enc:` decrypts, `@file` exists, `!env:` set, plaintext-secret scan (§9); the values of each written stack against its `variables.hcl` | master key missing |
| 5 | **The servers' plan of every stack** (`StackPlan`), which evaluates every module, validates every resource and plans the jobs. A stack that only a stack before it makes valid (it writes into a namespace `tree-global` creates in the same run) cannot be planned by the server yet; it is planned as `install` from the offline checks and planned again just before its apply (§13a, G20) | job rejected by the server's plan |
| 6 | **Version preflight** (§7.7) and **guards** (§7.4, §7.6) | a job delete without `--prune-jobs` |

A bad file is therefore found **before any stack is applied**, including a bad file in the *last* stack. This is the main defence; rollback is the second line.

`--skip-invalid` does not exist on purpose: a half-applied tree is exactly what the tool must not produce. To apply part of a broken tree, narrow it explicitly (`--stack`, `--namespace`); the *selected* stacks must be valid.

#### Stage B – what undoes a failed release

The deployer keeps no journal. A release does it on the server:

- **Atomic apply.** A release applied with `atomic = true` remembers the live state of each resource just before it changes it. When a resource fails, or the deployments do not become healthy within the release `timeout`, the release restores what it changed in reverse order: changed resources get their earlier content back, created ones are deleted, deleted ones are created again. A resource that someone changed after the operation wrote it is left as it is, with a warning in the operation log (`restore` in `norca/norca/release_apply.go`).
- **Default.** `atomic` is on by default in a namespace whose meta says `environment = production` (or `prod`), or whose meta key `pack.atomic` is true (`ReleaseAtomicDefault`), and a module may set it (rigger-design.md 6.4). For the generated stacks the deployer sets it from `limits.on_failure` (below). A written stack is sent as it is, with its own settings.
- **Revisions.** Every apply is a revision of the release with its content and owner, kept up to `max_history`. `tree rollback` re-applies an earlier revision (`Release.Rollback`). Git covers everything older: `git revert` and a normal apply.

#### Stage C – what is not undone

| Thing | Why |
| --- | --- |
| **Releases of earlier stacks of the same run** | They succeeded. They stay applied unless `--rollback-scope run` (below) |
| **Stack secrets** | Created once and never deleted. An `overwrite` writes a new version, which a rollback does not take back; the store keeps earlier versions up to `max_versions` for a manual restore |
| **Side effects of a job** (a migration it ran) | That is the job's concern |
| **Object kind schema changes with data migration** | The release restores the kind, not the objects migrated meanwhile |
| **A whole release removed with `--prune`** | Uninstalling is final, which is why it needs the flag |

#### Stage D – policy: when does it roll back

```text
--on-failure=rollback   (default for apply)   generated stacks are applied atomic; the failed release is restored by the server
--on-failure=stop                             generated stacks are applied without atomic; the failed release stays as far as it got
--rollback-scope=release (default) | run      run: also roll back the releases this run already applied, newest first
--keep-going                                  after a failed stack, go on with the remaining namespace and written stacks (never after tree-global)
```

- **Tests** (gitops-rigger-design.md 5.10): with `limits.test = "after-apply"` or `--test`, the tests of every release a stack's operation installed or upgraded run after the operation succeeds (`PUT /v1/release/<name>/test`). A failed test fails the stack and stops the run; with `--on-failure=rollback` those releases are rolled back to the revision they had before the run. A release without `test` hooks counts as passed.
- **Scope `release`** (default): the failed release is restored (if atomic), the run stops, and the releases applied before it stay. The report says which.
- **Scope `run`**: the deployer also rolls back every release this run changed before the failure, newest first, to the revision each had before the run (it reads those revisions from the plan). It keeps that list in memory only: if the deployer itself stops in the middle, the scope `run` is lost for that run, and the next run simply continues forward. The report says so.
- **A failure** is what the servers report for a release: an API error, a rejected resource, deployments that do not become healthy within the release timeout. How a job's own `auto_revert` counts is the release engine's (§13, question 10).
- **A restore that fails** is reported in the operation log and by the deployer (exit code 4). There is no open journal to block the next run: the next `plan` shows the live state against the tree, like after any other change outside the tree.

#### Invalid input in the GitOps path

For the controller (§11) the same rules hold, with one addition: a commit that fails stage A is **never applied**, the binding stays on its **last good commit** (recorded in the binding status, gitops-rigger-design.md 5.6), the failure is reported, and the same commit is **not retried** until a new push arrives, so a broken commit cannot loop.

### 7.9 No Local State

*(Revised 2026-11-28.)* The earlier draft kept an optional state directory for the journal, before-images, the ledger copy and the run history. None of it is needed on Rigger:

| Earlier local file | Now |
| --- | --- |
| `applied.json`, digests | the input digest of each release (§7.3) |
| `ledger.json` and the ledger Variable | the release owners (§7.4) |
| `journal/`, `snapshots/` | atomic applies and release revisions (§7.8) |
| `runs.jsonl` | the operations of the servers (`rigger.norca.io/Operation`, with their log on the bus for 30 days) and the binding status |
| `!generate:` values in `.norca-deployer/generated/` | the generated values of each stack, kept by the servers (`rigger/stacks/<stack>/generated`) |
| `lock` | one operation per stack on the servers (§7.6) |

So `--state-dir`, `--state none`, `NORCA_DEPLOYER_STATE_DIR` and the `state` block of the startup configuration are gone. A fresh CI runner plans exactly what the usual machine plans.

The one file the tool still writes is a **cache**: the probe results of the placement (§11b, `placement.json`) under `.norca-deployer/tree/`, next to the tree root or in the watcher's working directory. Deleting it costs one probe run, never correctness.

### 7.10 Dependencies and Update Order

*(Revised 2026-11-28: the unit graph, `unit.hcl` and `references.hcl` are dropped.)* The earlier draft had a graph of its own over jobs and stacks, with references to things outside the tree, gates and tiers. Rigger already has what it needs, so the tree uses that:

| Need | Earlier draft | Now |
| --- | --- | --- |
| Order between groups of objects | units and tiers | the stack order of §4.1, with `stack_order` in `tree.hcl` (§5.1). `stack_order` may name written stacks and the generated namespace stacks `tree-ns-<ns>`; `tree-global` is always first |
| "B after A is healthy" | `depends_on` with a gate | inside a stack, `module.<a>.<output>` or `depends_on = [module.a]`: Rigger waits until release `a` is ready (its deployments healthy) before it applies `b` (rigger-design.md 6.5) |
| A dependency on something the tree does not install | `references.hcl` (pack releases, jobs, services, secrets, Variables of others) | for a module installed by another stack: `requires { module "<name>" { version = "..." } }` in the module's `metadata.hcl`, checked at plan. For a namespace, quota, node pool or secret: the cross-reference check of `validate` (§7.5) |
| Gates `ready` (service readiness) and `tested` (last release test) | four gates | Rigger's gate is "ready". `ready` by service checks and `tested` are ideas for Rigger, not for the deployer |
| Update order `reverse` (consumers first, expand/contract) | `update.mode` | none. An incompatible change is made in two commits: the consumers first, then the provider |
| A plain job that must wait for a data service | a unit with `depends_on` | put the job into a written stack next to the data service, or order the stacks: `stack_order = ["data-platform", "tree-ns-apps"]` |

Inside a release the order is the fixed kind order of the release engine (§7.5). The deployer schedules nothing itself and has no ready queue.

`tree graph` stays, smaller: it prints the order of the stacks and, per written stack, the dependencies between its releases as Rigger's plan reports them (text, Graphviz `dot`, or Mermaid for pull requests).

### 7.11 (not in scope for now)

Running external programs and dispatching follow-up jobs after a run is **not part of this design for now**. Nothing in the other chapters depends on it. The section number stays so that the references to 7.12 to 7.15 keep their meaning.

### 7.12 Consul KV Entries

> **Not a kind yet (2026-11-28).** Consul KV has no Rigger resource kind, so `validate` refuses `kv/` for now (§4.1, *Kinds*). This section stays as the specification of the future kind: layout, prefixes and modes, CAS writes, backend limits. Where it names the run journal, before-images or foundation steps, the release engine takes their place once the kind is registered (gitops-rigger-design.md step G8b).

Applications that read the Consul KV (`consul-template`'s `key`/`tree`, Traefik, Spring Cloud) get their configuration from keys. `tree` manages those keys from the tree, in the Consul-compatible KV of the clients (`consul_compat`, keys in Norca Variables or in the system bus bucket) **or** in a real Consul (the `consul` pack). Both speak the same HTTP API (`GET`/`PUT`/`DELETE /v1/kv/<key>`, `recurse`, `cas`, `flags`), so there is one implementation with two endpoint types.

#### Tree: a file per key

```text
kv/
├── _kv.hcl                      # which prefixes the tree owns, and how
├── app/config/db.json           # key  app/config/db.json   value = the file content
├── app/config/limits            # key  app/config/limits
└── feature/new-ui               # key  feature/new-ui
```

```hcl
# kv/_kv.hcl
trim_trailing_newline = true        # default: remove one final newline from files (editors add it)

prefix "app/" {
  mode  = "sync"                    # sync: the tree wins | create-only: seed once, never overwrite
  prune = true                      # delete keys under app/ that are not in the tree (needs --prune-data)
}

prefix "feature/" {
  mode = "create-only"              # applications may flip these at run time
}

entry "feature/dark-mode" {         # a small inline value instead of a file
  value = "false"
  flags = 0
}
```

Rules:

- **The path under `kv/` is the key, the file content is the value** (binary safe; keys use `[A-Za-z0-9._/-]`, no leading slash, no `..`).
- **Every key in the tree must lie under a declared `prefix`.** A key outside any prefix is a validation error: ownership is always stated, never an accident of a stray file.
- **Ownership is by prefix.** Under a `sync` prefix with `prune = true` the tree is the whole truth: other keys there are removed on `--prune-data`. Keys **outside** the declared prefixes are never read, written or deleted. Do not share a managed prefix with an application that writes its own keys; use `create-only` or a separate prefix for those.
- **Not a secret store.** Secrets belong in `secrets/` (built-in store). If a value must not be readable in Git, a file whose whole content is an `!enc:v1:...` token is decrypted with the master key when written (it then sits in the KV in clear, like any KV value). The plaintext-secret scan of §9 warns for keys that look like `*password*`, `*secret*`, `*token*`.

#### Reconcile

1. **Read:** one `GET /v1/kv/<prefix>?recurse` per declared prefix (values and `ModifyIndex`/`Flags` of every key).
2. **Diff:** by value and flags. `plan` shows key, action and a short diff; values longer than 1 KiB or from `!enc:` show size and digest only.
3. **Write:** `PUT /v1/kv/<key>?cas=<ModifyIndex>` (`cas=0` to create). A rejected CAS means someone changed the key since the plan (the server answers **HTTP 200 with the body `false`**, so the writer reads the body, never only the status): the key is reported and the run stops for that key unless a re-plan is allowed (`--replan-on-conflict`).
4. **Delete:** only under `prune = true` prefixes, with `--prune-data`, after all writes of the run succeeded.

`create-only` prefixes only create missing keys; an existing key is left as it is and is never reported as drift.

#### Where it runs in the order

KV entries belong before the jobs in the kind order of the release engine (where Rigger places the kind), so a job that reads a key finds it. Keep in mind that a changed key can make a task restart (`consul-template` with `change_mode = "restart"`), so a KV write is a deployment event. The KV tree exists only at the root of the tree (`kv/`); a stack has no KV of its own.

#### Limits and checks (stage A of §7.8)

The limits depend on the backend declared in the region's `consul_kv` block:

| `backend` | Key | Value | Flags |
|---|---|---|---|
| `variables` | Variables path, at most 128 characters (other characters stored as `~XX`) | 64 KB | kept |
| `bus` | any | 512 KB | **rejected** (HTTP 400) |
| `consul` | any | 512 KB (Consul default) | kept |

**`ModifyIndex` is an opaque token.** On the `variables` backend it is the Raft modify index of the Variable; on `bus` it is the per-key revision of the NATS KV (and `CreateIndex` equals it). The tool only ever **compares it for equality for the same key** (the CAS value of the plan) and never orders or compares indexes across keys or backends. A backend change (`/v1/operator/consul-kv/migrate`) invalidates every remembered index, so the next run re-reads before it writes.

On the `variables` backend a value above about 64 KB is answered by the server with an **HTTP 500**, not with a clear 4xx (lab, 13.3), so stage A rejects it itself. An entry that breaks the declared limits, or sets `flags` on `bus`, fails validation **before any write**; the preflight (§7.7) then checks that the real endpoint is reachable and answers `/v1/kv` with the same backend. Sessions, locks (`acquire`/`release`) and transactions are not used (the compat layer answers `501` for them).

#### Access

- `type = "norca-compat"`: the `token` is a **Norca ACL token**; keys live below the Variables path `consul/kv/<key>` and the usual Variables rules apply, so the deployer's policy needs `write` on that path (and `list`/`read` for the recurse). The compat API listens on `127.0.0.1:8500` by default; the deployer must reach a client's non-loopback `bind` (node configuration through `apply`, plus a firewall rule) or run next to a client (§11a, job form). **Clients that share the same `namespace` and `path` of their `kv` block share their keys**, so any of them is the same key space; clients with another `namespace`/`path` are a different one (that is one `consul_kv` endpoint per key space).
- `type = "consul"`: the `token` is a **Consul ACL token** with `key_prefix "<prefix>" { policy = "write" }`; the `consul` module installs the official Consul (BSL 1.1, accepted by the user at install; the platform tool ships and installs none of it).

#### Rollback and drift

A failed write is restored like any resource of an atomic release (§7.8). The drift watch reads the prefixes with the same `recurse` call (one request per prefix); drift is **reported** by default and healed only if `watch.heal` names `kv-entry`.

### 7.13 Saved Plans and Machine-Readable Output

*(Accepted review item G3.)* A pull request shows a `plan`; the merge applies a fresh computation that can differ because the cluster changed in between. A saved plan makes **what was approved** the same as **what runs**.

```text
norca-deployer tree plan  --out plan.json [--format text|json]     # compute, print, save
norca-deployer tree apply plan.json                                  # apply exactly that, or refuse
```

- **The plan file** records the tree (commit SHA and a hash of the effective tree), the version of the tool and the Norca versions, and per stack the servers' plan: per release its current revision and action, per resource its kind, name and operation, and the digest of the built stack. It contains **no secret value**, only digests; values are resolved again from the tree at apply time.
- **`apply plan.json` recomputes and compares.** It builds the plan from the current tree and cluster and requires it to be the **same set of changes** (kind, name, operation, new-content digest). If so, it applies it; if not, it refuses with the difference ("stale plan: `quota/apps` changed in the cluster since the plan"), exit code 3 (blocked, §10). The file is never executed as instructions, so editing it can only make `apply` refuse. `--replan` shows the new plan next to the approved one and exits without writing.
- **Also checked:** the tree hash (the same commit), the tool and Norca versions (same major.minor), the age (`--max-plan-age`, default 24 h), and that `--prune*` flags are not wider than at plan time.
- **Waits stay live.** Rigger's wait for a release to be ready (§7.10) happens at apply time; it is not part of what is approved.
- **The watcher uses the same file.** A plan that the watcher computes for kinds outside `auto_apply` is kept as a **pending plan** with a fingerprint in the binding status; `tree status --plan` shows it, `tree approve <fingerprint>` (or `POST /v1/tree/approve`) lets the watcher apply exactly that plan. In every run record the plan fingerprint is logged.
- **Pull-request flow:** CI runs `validate` and `plan --out plan.json --format json` on the PR (a bot posts the summary); the approver reads it; the merge job runs `apply plan.json` with the saved file (or lets the watcher compute and compare the fingerprint).
- **Machine-readable output:** `--format json` on `plan`, `apply`, `status`, `runs`, `diff` and `graph`, with stable field names (kind id, name, operation, result, fingerprint, timestamps); `text` stays the default for people. Secrets and values never appear in either format.

### 7.14 Fields Owned by Something Else

*(Accepted review item G4; revised 2026-11-28.)* Some fields are changed by the platform itself, not by the tree: above all the group `count` set by the **built-in autoscaler** or `job scale`. If the file says `count = 3` and the autoscaler made it 7, an apply that resets it fights the autoscaler and the drift never settles.

The tree no longer writes jobs itself, so the rule belongs to the release engine (gitops-rigger-design.md 6.3):

- **Jobs, default rule.** A task group that an enabled scaling policy scales has its `count` owned by the autoscaler: `Job.Register` and `Job.Plan` keep the live count in a new version of the job (`keepScaledCounts`), and since norca `28c3bcdd8` (branch `feat/rigger-gitops-prep`) the drift check leaves that count out. A group created for the first time takes the count of the file. Once that branch is merged, `heal` may name `job` like any kind, and no `validate` warning is needed (§13a, G24).
- **Other fields.** An ignore list (the `ignore` of `unit.hcl` and the `_ignore.hcl` files of the earlier draft) would be a release feature too: attribute paths per resource that the plan leaves out and the apply takes from the live object. It is a proposal for Rigger and the release engine, not built in the deployer. The file name `_ignore.hcl` stays reserved (§4, naming rules).
- **Plan output** says when a field is ignored, so a reader sees why a live value differs from the file.

### 7.15 API Load and Read Strategy

> **Revised 2026-11-28.** The servers plan the stacks, so most reads of the earlier draft (every kind, every object) now happen inside the servers' plan. What the deployer itself reads is small: the stack plans, the releases of the tree's stacks for `status` and orphans, the operation logs and the versions of the preflight. The rules below apply to those reads and to the controller's polls; list-then-fetch and blocking queries per kind matter again only for the Consul KV reads of a later kind.

*(Review item G16.)* Every `plan` and every drift tick reads many kinds; on a large platform that is thousands of requests. The read side is designed to be cheap by default and bounded by configuration (a startup setting, §5.2, `limits`):

```hcl
limits {
  read_parallelism = 8                       # concurrent API reads per run (default 4)
  rate             = "50/s"                  # requests per second to the cluster (default 20/s)
  stale_reads      = true                    # allow stale reads for drift ticks (default true)
}
```

- **List, then fetch only what the tree names.** Where the API has a list endpoint with stubs (jobs, variables, policies, namespaces), the tool lists once per kind and fetches the full object only for names that exist in the tree **and** whose stub (index, version, modify time) differs from the one remembered from the last run. Nothing is fetched for unchanged objects.
- **Parallel with a limit.** Kinds are read in parallel (they are independent for reads) up to `read_parallelism`, with the global `rate` as the ceiling; writes follow the stack order of §4.1.
- **Change detection by blocking queries.** For kinds whose endpoints support blocking queries (index-based `?index=` waits), the drift loop waits on the index instead of polling on a timer; the timer remains as the fallback. Per kind, a changed index triggers a re-read of that kind only.
- **`stale` reads for drift.** The drift watch (11a) may use `?stale=true` so any server answers, not only the leader; **plan before apply and every write path read from the leader** (no stale reads), because a stale plan is a wrong plan.
- **Backoff.** On `429`, `5xx` or a leader election, reads back off exponentially with jitter and the run is marked `degraded` in `status`, not failed; a run never retries writes blindly.
- **Budget visible.** `status` and the metrics (11a) show requests per run, per kind, the slowest kind and the time spent; a plan that needs more than `limits.plan_time_budget` (default 2 minutes) says so and names the kind, so the first remedy is a narrower `--kind` or a longer drift interval.
- **KV** (§7.12): one recursive read per declared prefix; a prefix with many keys is the main cost, so a prefix limit is part of the stage A checks.

---

## 8. Kind Table (Implementation Contract)

*(Revised 2026-11-28.)* The earlier draft gave each kind a Go type of the deployer (`Load`, `Read`, `Diff`, `Apply`, `Delete`). On Rigger the deployer reads, compares and writes no object itself, so that interface is gone. A kind of the tree is a **file extension that maps to a Rigger resource kind**. Adding a kind is a registration in Rigger (rigger-design.md section 11) plus one row of the deployer's registry (§8.1). The engine does not change.

| Kind id | File in the tree | Rigger resource kind | Status |
|---|---|---|---|
| `quota` | `global/quotas/<name>.quota.hcl` | quota | supported |
| `namespace` | `global/namespaces/<name>.namespace.hcl` | namespace | supported |
| `acl-policy` | `global/acl/policies/<name>.acl.hcl` | ACL policy | supported |
| `admission-policy` | `global/admission-policies/<name>.admission.hcl` | admission policy | supported |
| `object-kind` | `global/object-kinds/<name>.kind.hcl` | object kind | supported |
| `variable` | `namespaces/<ns>/variables/<name>.var.hcl` | Variable | supported |
| `external-service` | `namespaces/<ns>/services/<name>.external.hcl` | external service | supported |
| `custom-object` | `namespaces/<ns>/objects/<name>.object.hcl` | object | supported |
| `job` | `namespaces/<ns>/jobs/<name>.norca.hcl` | job | supported |
| `secret` | `namespaces/<ns>/secrets/<path>.hcl` (deployer manifest) | none: a `secret` block of the generated stack (rigger-design.md 6.3) | supported |
| `acl-role`, `acl-auth-method`, `acl-binding-rule` | `global/acl/{roles,auth-methods,binding-rules}/` | to be registered | not yet |
| `node-pool` | `global/node-pools/` | to be registered | not yet |
| `admin-network-policy`, `network-policy` | `global/network-policies/`, `namespaces/<ns>/network-policies/` | to be registered | not yet |
| `service-intention` | `namespaces/<ns>/intentions/` | to be registered | not yet |
| `volume` (CSI) | `namespaces/<ns>/volumes/` | to be registered | not yet |
| `kv-entry` | `kv/` (§7.12) | to be registered | not yet |
| `scheduler-config`, `autopilot-config` | chosen with the kind | to be registered; singletons, possibly in a small module of their own (gitops-rigger-design.md section 7, point 2) | not yet |

The extensions of the kinds that are not registered yet are chosen when Rigger registers them; the deployer takes Rigger's. Until then `validate` refuses such files with "not supported yet" (§4.1, *Kinds*).

The secret manifest stays a deployer file, because a secret is not a release resource. The deployer turns each file into a `secret` block of the stack of its namespace:

```hcl
# namespaces/apps/secrets/api/db.hcl          -> secret path api/db of the stack tree-ns-apps
items = {
  user     = "api"
  password = "!generate:api-db-password"      # a generated value of the stack, created once, kept
  ca       = "@../../../certs/db-ca.pem"      # file outside Git (see 9)
}

overwrite = false                             # create once (default); true writes a new version when the value changes
```

`replicate = true` of the earlier draft is not carried yet: a stack `secret` block has no such setting today. A replicated secret is written by hand or waits for the setting in Rigger.

The small manifests the earlier draft proposed for roles and intentions (a role as `name`, `description`, `policies`; an intention as `source`, `destination`, `action`) are input for the Rigger registration of those kinds, not formats of the deployer.

### 8.1 Kind Identifiers

> **Two meanings of "kind".** In this document a *kind* is a type of object that the tool manages (namespace, quota, job, ...). It is **not** the Norca *ObjectKind*, which is the type of a custom object (`group/Kind`, `/v1/object-kinds`). The two meet in one place only: `object-kind` is one of the kind identifiers (the Norca object that defines custom object types), and `custom-object` is another (the instances of such a type). Where the Norca meaning is intended the text says *object kind* or *ObjectKind*.

*(Accepted review item G7.)* The first column of the table above is the **kind identifier**. It is the same word everywhere a kind is named: the allow-list `limits.kinds`, `--kind` (a guard on the plans, §10), the policy lists `auto_apply` and `heal` (§11), the columns of `plan`, the JSON output, the kind-support matrix (§7.7), the permission table (§5.3) and the metric label `drift_objects{kind}`. The mapping to Rigger's resource kinds is the third column of §8.

**Grammar**

- **R0 – a kind is something outside the tool.** An identifier names an object that exists in the cluster (or in its KV endpoint) and that the tool can write and read back. Concepts of the tool and of Rigger are never kinds: *stack*, *module*, *release*, *plan*, *run*, *layer*.
- **R1 – form.** Lower case ASCII, singular, kebab-case, no dot.
- **R2 – shape** `[family-]noun[-config]`: a family prefix where Norca groups several objects (`acl-`), the suffix `-config` for a **singleton** (one per region, no name).
- **R3 – the name is Norca's own, as in its CLI and API** (`node pool` is `node-pool`, `service intention` is `service-intention`). Abbreviations and words that would be ambiguous in a tool that manages "objects" are written out: `var` is `variable`, `object` is `custom-object`, `kv` is `kv-entry`, `autopilot` is `autopilot-config`.
- **R4 – a variant keeps Norca's phrase:** `admin-network-policy` next to `network-policy`.
- **R5 – frozen.** An identifier is written into startup configurations, bindings and CI scripts; once released it is never changed without an alias.

**The identifiers**

| Id | Object | Norca name (CLI / API) | Directory | Groups |
|---|---|---|---|---|
| `acl-policy` | ACL policy | `acl policy` | `global/acl/policies` | `acl` |
| `acl-role` | ACL role | `acl role` | `global/acl/roles` | `acl` |
| `acl-auth-method` | ACL auth method | `acl auth-method` | `global/acl/auth-methods` | `acl` |
| `acl-binding-rule` | ACL binding rule | `acl binding-rule` | `global/acl/binding-rules` | `acl` |
| `quota` | quota spec | `quota` | `global/quotas` | |
| `namespace` | namespace | `namespace` | `global/namespaces` | |
| `node-pool` | node pool | `node pool` | `global/node-pools` | |
| `scheduler-config` | scheduler configuration (singleton) | `operator scheduler` | chosen with the kind | |
| `autopilot-config` | autopilot configuration (singleton) | `operator autopilot` | chosen with the kind | |
| `admission-policy` | CEL admission policy | `admission-policy` | `global/admission-policies` | |
| `admin-network-policy` | cluster-wide network policy | `network-policy -admin` | `global/network-policies` | `network` |
| `object-kind` | custom object type | `object-kind` | `global/object-kinds` | |
| `network-policy` | namespaced network policy | `network-policy` | `namespaces/<ns>/network-policies` | `network` |
| `variable` | Variable | `var` | `namespaces/<ns>/variables` | `data` |
| `secret` | built-in secret | `secret` | `namespaces/<ns>/secrets` | `data` |
| `kv-entry` | Consul KV entry | `/v1/kv` (Consul-compatible API or Consul) | `kv/` | `data` |
| `volume` | volume | `volume` | `namespaces/<ns>/volumes` | |
| `service-intention` | service intention | `service intention` | `namespaces/<ns>/intentions` | |
| `external-service` | external (registered) service of the catalog | `service register` | `namespaces/<ns>/services` | |
| `job` | job | `job` | `namespaces/<ns>/jobs` | |
| `custom-object` | object of a custom kind | `object` | `namespaces/<ns>/objects` | |

Directory names stay plural, as folders usually are; the identifiers are singular. Every row names a Norca object except `kv-entry`, which names an entry in an external KV system; none names a concept of this tool (R0).

**Groups** are shorthand for several identifiers and work **only in selectors** (`limits.kinds`, `--kind`, `watch.auto_apply`, `watch.heal`, and the `auto_apply` and `heal` of a binding), never in output: `acl` (the four `acl-*`), `network` (both network policies), `data` (`variable`, `secret`, `kv-entry`), `foundation` (every identifier except `job` and `custom-object`), `all`.

**Strict by default.** An unknown identifier is an **error** at start and in `validate`, with a suggestion ("`namespaces` is not a kind; did you mean `namespace`?"). A plural, a typo or an old spelling is never accepted silently: a silently ignored word in `auto_apply` or `heal` would switch a safety setting off.

**One setting, one place per process.** `kinds` and `prune` are in `limits`; `auto_apply` and `heal` are in the `policy` of a tree binding (§11), or in `watch` for the standalone `tree watch` (§11a). The `tree {}` block of a binding has no keys.

**In the code** one registry (`kinds.Registry`: identifier, aliases, groups, directory, minimum Norca version, **the Norca API name**, the file extension and the Rigger resource kind) and one selector parser serve every place above; `import` (live objects to files) uses the same mapping in the other direction, and a later Norca rename would change one registry entry, not the trees. Tests: every identifier used in the documents and examples resolves, and a golden list fails when an identifier is renamed by accident.

**Spellings that changed** from earlier drafts of this document: `namespaces` to `namespace`, `quotas` to `quota`, `node-pools` to `node-pool`, `autopilot` to `autopilot-config`, `object` to `custom-object`, `intention` to `service-intention`, `kv` to `kv-entry`, `jobs` to `job`, `secrets` to `secret`.

---

## 9. Secrets and Sensitive Values

Reuse the value forms already implemented in `pkg/engine/postcluster.go` (`resolveValue`); no new secret machinery.

| Form | Where | Meaning |
|---|---|---|
| `!enc:v1:aes-gcm:…` | any string | encrypted with the deployer master key – safe to commit |
| `!generate:<name>[:<bytes>]` | secrets of a stack, values of written stacks | random value created once and kept by the servers as a generated value of the stack (`rigger/stacks/<stack>/generated`) |
| `!nkey-seed:` / `!nkey-public:` | same | NATS NKeys |
| `@path` | secret / variable items | file content from the operator machine (outside the repo, CI secret mount) |
| `!env:NAME` *(new)* | same | environment variable – the natural fit for CI secret injection |
| `!file:path` | the `verify.public_keys` of a GitOps binding (§11) | the content of a file next to the startup configuration, read when the process starts; not a secret form |

Rules:

1. **Plaintext secret values in the tree are a validation error** for files under `secrets/` and for Variables, unless the item is explicitly marked `plain = ["name"]` (non-secret config). This catches accidents.
2. **Values never appear in `plan` output or logs**; they print as `<hidden>`/digest prefix only.
3. **ACL tokens are not managed.** They are issued secrets. Instead the tree manages policies and roles; workload identity covers jobs. Operator tokens are created by hand/auth methods (OIDC) and stored outside Git. (If really needed later: `acl token` declared with `store_to = "<file or Variable>"` and create-once.)
4. **`!generate:` values belong to their stack.** The servers create a generated value once per stack and keep it; another stack, or the same stack in another region, has its own. A value two stacks must share is created by one stack's `secret` block and read from the secret store by the other.
5. **"Unchanged" lives in the cluster, not on the operator's disk.** A release is skipped by its input digest (§7.3) and a stack secret by its existence (`keep`), so a fresh CI runner sees the same "unchanged" as the usual machine. The `OIDCClientSecret` of an `acl-auth-method` (once that kind is registered) is a write-only value: a sensitive value of the release, never stored in a revision (rigger-design.md 6.3), printed as `<hidden>`.
6. **Master key lifecycle.** *Rotation:* `tree rekey --old-key-env X --new-key-env Y` re-encrypts the `!enc:` values of the tree in the working copy (the result is a normal commit). *Loss:* values encrypted with the lost key cannot be recovered; they are entered again (`!enc:` re-created). Nothing on the servers depends on the key: values are sent decrypted, and secrets and generated values live in the store. The key itself is never stored in the tree.

---

## 10. CLI

*(Revised 2026-11-28.)* New command group, same global flags (`--secret-key`, `--secret-key-file`) and same style as `apply`.

```text
norca-deployer tree validate [-d DIR] [--changed-since REF] [--rigger] [--format text|json]   # offline checks; touched directories; --rigger: rigger stack validate on the built stacks
norca-deployer tree build    [-d DIR] --out DIR                                   # write the built stacks to a directory (to read, or to run rigger on)
norca-deployer tree plan     [-d DIR] [--stack S] [--namespace NS] [--kind K] [--detailed-exitcode]
                                 [--out FILE] [--format text|json]                  # the servers' plan of every stack (§7.2); saved plan (§7.13)
norca-deployer tree apply    [PLAN_FILE] [-d DIR] [--stack S] [--namespace NS] [--kind K]
                                 [--prune] [--prune-jobs] [--prune-data]
                                 [--yes] [--yes-really] [--keep-going] [--dry-run]
                                 [--on-failure rollback|stop] [--rollback-scope release|run]
                                 [--allow-mixed-versions] [--test]
norca-deployer tree connect  test                            # read-only probes through the configured path (§11b)
norca-deployer tree graph    [-d DIR] [--format text|dot|mermaid]                   # stack order and the release dependencies of each stack (§7.10)
norca-deployer tree diff     [-d DIR] [--format text|json]  # drift only: the drifted releases of the tree's stacks
norca-deployer tree rollback --stack S [--release R] [--revision N]                 # re-apply an earlier revision (default: the one before the current)
norca-deployer tree runs     [--stack S]                     # the operations of the tree's stacks
norca-deployer tree test     [--stack S]                     # run the release tests of the tree's stacks (§7.8)
norca-deployer tree cancel   [--stack S]                     # cancel the running operation of a stack; the servers stop before its next release
norca-deployer tree destroy  --stack S [--yes]               # uninstall every release of one stack; never from a watcher or a push
norca-deployer tree import   [-d DIR] [--kind K] [--adopt]   # live cluster → tree; --adopt lets the releases take the objects over
norca-deployer tree watch    [-d DIR | --repo …] [--listen :9096] [--mode plan-only|apply]   # standalone controller (§11a)
norca-deployer tree service  install|uninstall|status [--name N]                         # systemd / Windows service
norca-deployer tree sync | pause | resume [--addr URL]                                    # control a running watcher
norca-deployer tree approve  <fingerprint> [--addr URL]      # let a watcher apply its pending plan (§7.13)
norca-deployer tree status   [--versions] [--plan]           # stacks, releases, last operations, orphan releases; versions table
norca-deployer tree convert  --to hcl|yaml [-d DIR]          # manifests only; `tree fmt` formats HCL
norca-deployer tree init     [-d DIR]                        # skeleton tree + tree.hcl + commented norca-tree.hcl template + .gitignore
```

Common flags: `--config FILE` (startup configuration, §5.2), `-d/--dir` (tree, default `./cluster-config`), `--placement auto|local|proxy|node` (default `auto`) and `--proxy NAME` (§11b). `--kind` takes kind identifiers or groups, comma separated (§8.1); an unknown one is an error.

**Narrowing a run.** `--stack` and `--namespace` (the stack `tree-ns-<ns>`) select whole stacks; the selected stacks must be valid, and the order of §4.1 is kept among them. `--kind` cannot cut a release: a release is applied whole, and a release sent without some of its files would delete their resources. So `--kind` is a guard: a selected stack whose plan changes a kind outside the list is blocked (exit code 3). A hot fix to one job:

```text
norca-deployer tree apply --namespace apps --kind job
```

applies `tree-ns-apps` when its plan changes only jobs, and refuses otherwise.

**Further flags and commands** named in the chapters: `apply` also takes `--replan` and `--max-plan-age` (§7.13); `tree rekey --old-key-env NAME --new-key-env NAME` (§9); `tree fmt [-d DIR]` and `tree convert --to hcl|yaml` or `--to-version vN` (§4, §5.1); `tree pause [--reason TEXT]`, `resume` and `sync` (§11a); `tree watch` takes `--listen` and `--mode`; `tree service install` takes `--user` (§11a). `--region R` picks the `region` block, and so the endpoint, of a run (§6.1); every command that talks to the cluster takes it.

Gone with the tree's own engine: `--purge` (a release purges a job it deletes), `--rollback-purge`, `--ignore-open-journal`, `--unit`, `--with-deps`, `--only`, `--update-order`, `--ignore-unhealthy-deps`, `--replan-on-conflict`, `--state-dir` and `--state`, and `--on-failure continue` (now `--keep-going`).

#### Exit codes and machine output

*(Review item G13.)* CI and wrappers get one rule for all commands:

| Code | Meaning | Where |
|---|---|---|
| `0` | success; for `plan --detailed-exitcode`: nothing to change | all |
| `1` | error: invalid input, API failure, a failed apply | all |
| `2` | changes pending | `plan --detailed-exitcode`, `apply --dry-run`, `diff` (drift present), `status --plan` |
| `3` | **blocked, not failed**: a guard stopped a stack (a delete without its `--prune` flag, a kind outside `limits.kinds` or `--kind`, a stack with no usable token), a stack waits for a manual apply (§11), a saved plan is **stale**, a pending plan awaits `approve`, or another operation of a stack is still running after the wait | `plan`, `apply`, `status` |
| `4` | a release failed and the server restored it (atomic), the run was rolled back (`--rollback-scope run`), or a restore failed | `apply`, `rollback` |

`plan` without `--detailed-exitcode` returns `0` whenever it could compute the plan, so a plain `plan` in a pipeline never fails on "there are changes". `--format text|json` is available on `plan`, `apply`, `status`, `runs`, `diff` and `graph`; the JSON shape is versioned (`"schema": "norca.io/tree/plan/v1"`, see §7.13) and is the stable interface, while the text is for people. Errors in JSON mode are objects (`kind`, `name`, `file`, `line`, `message`) on stdout, logs stay on stderr, and no secret value ever appears in either form.

`validate` has two levels:

1. **Offline** (no cluster): parse all files (HCL), name/path rules, duplicate identities, reserved names, supported kinds, cross references (§7.5), plaintext-secret scan; with `--rigger`, `rigger stack validate` on the built stacks.
2. **Online** (with `plan`): the servers' plan of every stack, which evaluates the modules and validates every resource.

---

## 11. GitOps Integration

*(Revised 2026-11-28.)* A tree in Git is applied by the GitOps controller of [gitops-rigger-design.md](gitops-rigger-design.md) section 5, through a **tree binding**: a stack binding whose `subpath` points at the root of a tree (`tree.hcl`) instead of one stack directory. One run applies every stack of the tree in the order of §4.1. The controller's loop, polling, retries, circuit breaker, drift reaction, notifications and status are the ones of gitops-rigger-design.md section 5; this section adds only what is particular to a tree.

```hcl
rigger {
  gitops {
    enabled = true

    binding "cluster" {
      region = "eu-1"

      forgejo {
        url         = "https://git.example.com/acme/platform"
        branch      = "main"
        subpath     = "cluster-config"           # the root of the tree
        auth_secret = "!enc:v1:aes-gcm:..."
      }

      tree {}                                    # this binding applies a whole tree (§4.1), not one stack

      verify {
        public_keys = ["!file:keys/ops.asc"]     # required in mode apply (gitops-rigger-design.md 6.4)
      }

      webhook {
        secret_key      = "!enc:v1:aes-gcm:..."
        filter_branches = ["main"]
      }

      policy {
        mode          = "apply"                  # apply | plan-only
        poll_interval = "60s"
        prune         = false
        auto_apply    = ["job", "variable", "external-service", "custom-object"]   # kind ids and groups (8.1)
        heal          = ["admission-policy"]
        window        = "Mon-Fri 07:00-19:00"
        max_failures  = 3
        test          = "after-apply"            # off | after-apply (§7.8)
      }

      sync_registries = ["global"]               # synced before the plans
    }
  }
}
```

A tree binding has no `namespace` of its own: each stack has its namespace (§4.1). Its tokens are the scoped tokens of §5.3, one per stack.

Behaviour, on top of gitops-rigger-design.md section 5:

- **Push or poll** → fetch in memory → verify → stage A for every stack (§7.8) → plan every stack → apply the stacks the policy allows, in order → the binding status records the commit and the operation of each stack.
- **`auto_apply` per kind, applied per stack.** A release is applied whole, so the policy is decided per stack: a stack whose plan changes only kinds in `auto_apply` is applied; a stack whose plan touches any other kind (an `acl-policy`, a `quota`) is planned, reported and left for a manual `tree apply`, and the stacks after it in the order wait too, because they may rely on it. An empty `auto_apply` is `plan-only` for every stack. This is point 3 of gitops-rigger-design.md section 7, the per-kind policy on stacks.
- **`heal` per kind, decided per release.** On a `ReleaseDrifted` event the controller re-applies the release of the last good commit only when every drifted resource of it is of a kind in `heal`. Jobs are not healed by default (an operator's emergency `job scale` or `job stop`), and fields owned by the autoscaler must not count as drift (§7.14).
- **Who may change what** *(review item G15)*. `tree validate --changed-since REF` lists the **directories a commit touched** and the stacks and kinds they map to, as text or JSON, so a reviewer sees at once that a change under `namespaces/apps/` also changed `global/`. The documented rule is a `CODEOWNERS` file that gives `global/` and `modules/` to the platform team, `namespaces/<ns>/` to the owning team and `stacks/<name>/` to its owner. With one scoped token per stack (§5.3) a team's commit can only ever be written with the team's token, whatever it contains. A stack with no usable token is `blocked` (exit code 3), never applied with a stronger token.
- **Pull requests**: `tree validate` and `tree plan --detailed-exitcode --format json` in CI (gitops-rigger-design.md 5.7); the controller may comment the plan.
- **Provenance**: the commit goes into the releases (`Commit` of each release, gitops-rigger-design.md 6.1) and into the operation (6.2), and the binding status holds the last applied commit. Nothing is written into the objects, so an unchanged object is never touched.
- **Failure handling** is §7.8: an invalid commit is not applied and the binding stays on its last good commit; a failed release is restored by the server when atomic; the binding backs off and, after `max_failures`, is `Degraded`.
- **The additions of gitops-rigger-design.md 5.10** work on a tree binding as on a stack binding: registry sync, outputs and `next_steps`, `gitops cancel` and `cancel_on_shutdown`, `gitops destroy` (every stack of the tree, by hand only), `gitops rollback` to the last good commit, and the tests of `policy.test`. Values come from the binding's `values` and the stacks' own `values/` (§4.1), so a tree binding takes no `values_files`.
- **Rollback** = atomic restore for a failed apply, `tree rollback` (a release revision) or the controller's "back to the last good commit" (gitops-rigger-design.md section 4, R11), and `git revert` for anything older.

Without GitOps (laptop, CI job) the same tree is simply a local directory: `tree plan` and `tree apply` send the same stacks, and nothing in the model requires Git.

---

## 11a. Service Mode: Continuous Watching

> **Revised 2026-11-28.** The service mode is the GitOps controller of gitops-rigger-design.md section 5 with one tree binding (§11). Where this chapter still describes an engine of its own, the controller holds: the drift watch is the servers' drift check with the `ReleaseDrifted` events (gitops-rigger-design.md 5.5), the status is the binding status (5.6) instead of a state directory and a status Variable, a failed apply is restored by the release engine instead of a journal, and high availability is the operator form of step G6 instead of a leader-lease Variable. What this chapter still adds: the per-kind policies (applied per stack and per release, §11), the self-management guard, the two forms (OS service and Norca job) and the topology.

`plan`/`apply` are one-shot. The same engine can also run as a **long-lived service** that keeps watching and keeps the platform on the tree. There is no second engine: the service is a loop around the exact `validate → plan → apply` path of §7, including the version preflight and the guards.

```text
norca-deployer tree watch [-d DIR | --repo …] [--listen :9096]
```

It extends the process of the existing `gitops daemon` (webhook listener, HMAC verification, debounce, ring log, `:9095` API) rather than adding a new kind of daemon: one binary, one HTTP server, stack bindings and tree bindings side by side. `norca-deployer gitops daemon` keeps working unchanged.

### Two watch loops

| Loop | Watches | Default | Cost |
|---|---|---|---|
| **Source watch** | the tree: Git commit of the branch (provider API/`ls-remote` returns the SHA) or the mtime/hash of a local directory | every `interval` (60 s) **and** on webhook push | cheap; the tree is fetched only when the SHA changed |
| **Drift watch** | the `Drifted` condition of the tree's releases (the servers check drift; `ReleaseDrifted` events are hints), plus versions (§7.7) | every `drift_interval` (5 min) and on events | a list of releases, no tree fetch |

Triggers that start a run, all funnelled into one **coalescing queue** (never more than one run per region at a time, bursts merge into one run):

1. Source change (poll or webhook, after the `debounce` window).
2. Drift found by the drift watch, if the kind's drift policy says `heal` (below).
3. Manual: `POST /v1/tree/sync` or `norca-deployer tree sync`. (`SIGHUP` only reloads the configuration, §5.2.)
4. *(Later, optional)* cluster event stream: subscribe to the topics of the managed kinds so drift is noticed in seconds instead of minutes; polling stays as the safety net. The source (13.2) shows the topics `ACLPolicy`, `ACLRole`, `ACLToken`, `ACLAuthMethod`, `ACLBindingRule`, `NodePool`, `Job`, `Variables`, `Secret`, `Quota`, `AdmissionPolicy`, `NetworkPolicy`, `CustomObject`, `ServiceIntention`, `Service`, but **no `Namespace` topic**, and a broker buffer of 1000 events in memory only, so a stream is never trusted to be complete: the watcher subscribes with `exact=true` (HTTP 410 means "re-list"), treats every event as a hint to re-read that kind, and keeps the timer for `namespace` and as the safety net.

### What the watcher does with what it finds

```text
tick ─► source changed?  ──yes──► fetch in memory ─► validate (stage A) ─► plan ─► apply allowed kinds ─► record
   │                                     │ invalid                                   │ failure
   │                                     └► report, stay on last good commit         └► rollback per §7.8, report
   └► no ─► drift watch due? ─► compare ─► per-kind drift policy: report | heal | ignore
```

Per-kind policy keeps the dangerous things manual:

```hcl
watch {
  mode           = "apply"       # apply | plan-only  (plan-only: watch, report, never write)
  interval       = "60s"
  jitter         = "10s"         # spreads the polls of many watchers
  drift_interval = "5m"
  debounce       = "10s"
  auto_apply     = ["job", "variable", "external-service"]          # decided per stack (§11); acl-*, namespace and quota stay manual on purpose
  heal           = ["admission-policy"]                             # decided per release: re-applied when all its drift is of these kinds
  # everything else: drift is only reported (variables, secrets, Consul KV, volumes, object kinds, ...)
  prune          = false         # the watcher never prunes unless this is set explicitly
  window         = "Mon-Fri 07:00-19:00"   # optional change window for auto_apply; outside it runs queue up
  listen         = ":9096"                  # the watcher's own API and metrics (also --listen)
  max_failures   = 3                        # circuit breaker: consecutive failed runs before `degraded`
  # notify { webhook = "..." }              # post failures and `degraded` transitions (see below)
}
```

- **`plan-only` is the recommended first stage** of rolling the service out: it shows what *would* happen on every commit and on every manual edit in the cluster, with no change to the cluster, and with a token that can't apply (release `read` and read on what the plan compares; §13a, G25).
- **`heal`** re-applies the tree over out-of-band edits. Reasonable for access and guard-rail objects (policies, quotas, admission, network rules), where drift is a security problem; **not** for jobs by default, where an operator's emergency `job scale` or `job stop` should not be undone within five minutes. A job drift is reported with the diff; the next commit or a manual `sync` resolves it.
- **Secrets and Variables are never healed** unless the entry says `overwrite = true` (§8).
- **Freeze / pause**: `tree pause [--reason …]` (or a `freeze` file in the tree root, or a window) stops *applies* but keeps watching and reporting. Needed during incidents and upgrades; the version preflight also pauses it automatically while a region is on mixed versions (§7.7).

### Failure behaviour of the loop

- **Blocked stacks** (§7.4, §7.6, §11): a stack stopped by a guard, or waiting for a manual apply because its plan touches a kind outside `auto_apply`, is `blocked`, not failed; it does not count towards the circuit breaker, and the status lists why.
- **Backoff**: a failed run is retried with exponential backoff (1 min → 30 min cap) – only for transient causes (API unreachable, no leader). A rejected object or an invalid tree is **not retried**; it waits for a new commit (same rule as §7.8).
- **Circuit breaker**: after `max_failures` (default 3) consecutive failed runs, the state becomes `degraded`: auto-apply stops, the watcher keeps reporting, `tree resume` or a new green commit clears it. This is what prevents a flapping rollout loop.
- **A failed restore** of a release is reported loudly (exit code 4, the binding status). It does not block the next run, which plans the live state against the tree (§7.8).
- **Restart**: the last good commit and the last applied commit are read from the binding status (gitops-rigger-design.md 5.6). The service resumes without re-applying an unchanged commit, and an unchanged stack plans as unchanged anyway.

### Forms the service can take

The *form* (OS service or job) is independent of the *placement* (local, proxy, client node, §11b); §11b lists which combinations make sense.

#### 1. Operating system service (systemd, Windows)

The simplest form, and the right one for the **break-glass watcher**: it lives outside the cluster it manages, so it still runs when Norca is down and can be used to fix it.

```text
norca-deployer tree service install   --name norca-platform [--user norca-deployer]
norca-deployer tree service uninstall --name norca-platform
norca-deployer tree service status
```

Generates and enables a unit (not started automatically the first time, so the first run is `--mode plan-only`): a dedicated user, `CacheDirectory=` (for `placement.json`), `EnvironmentFile=` for the master key and the API token (`0600`), `Restart=on-failure`, standard hardening (`NoNewPrivileges`, `ProtectSystem=strict`, `ReadWritePaths=` the cache directory). The existing deployer already runs on Linux, macOS and Windows; the Windows form is a native service registration (details: open question 11).

Good for: one watcher per region endpoint, a small platform, the bootstrap period.

#### 2. A Norca job (the platform watches itself from inside)

The watcher as a **service job** on the cluster it manages: scheduled, restarted and updated by Norca like any workload, registered in the built-in service catalog with a health check, so it is also visible in the Norca UI.

```hcl
# illustrative; adjust to the Norca job specification
job "norca-platform" {
  namespace   = "platform"
  datacenters = ["*"]
  type        = "service"

  group "watch" {
    count = 1                      # one instance; HA is the operator form (below)

    network {
      port "http" {
        to = 9096
      }
    }

    service {
      name = "norca-platform"
      port = "http"
      check {
        type     = "http"
        path     = "/healthz"
        interval = "10s"
        timeout  = "2s"
      }
    }

    task "watch" {
      driver = "docker"            # or exec2 with the single static binary
      config {
        image = "registry.example.com/norca-deployer:0.9"
        args  = ["tree", "watch", "--config", "/secrets/norca-tree.hcl", "--listen", ":9096"]
      }
      identity {
        env = true                 # workload identity: no static management token
      }
      # master key and Git credentials come from the built-in secret store (`secret` block),
      # delivered as files/env – never in the job file
      resources {
        cpu    = 200
        memory = 256
      }
    }
  }
}
```

Design points for this form:

- **Authentication by workload identity.** An ACL policy bound to this job (`job_id`, `group`, `task` binding of ACL policies) gives it exactly the capabilities of the kinds in `watch.auto_apply`; no long-lived management token exists. `plan-only` needs only read capabilities (§13a, G25).
- **Secrets from the built-in store** (`secret` block) for the master key, Git token and webhook secret; the job file in Git contains only references.
- **State**: none is needed. The binding status in the cluster holds the last good commit, so a restarted or rescheduled job resumes from it.
- **HA**: high availability is the operator form of the controller (gitops-rigger-design.md step G6, leader election as `norca-dns` and `norca-acme`). The standalone job runs with `count = 1`. Two instances would both apply; the servers' one operation per stack only keeps them from applying the same stack at the same moment.
- **Network**: the webhook endpoint is exposed through the service catalog / a load balancer; mTLS through Norca's native mTLS if the Git server is inside the platform. Polling works with no inbound port at all, so a webhook is optional.

#### Self-management and failure domains

Two traps with form 2, each with a fixed rule:

1. **The tree contains the watcher's own job.** The watcher **never auto-applies, heals or prunes its own job** (identified by `job_id` + the `self` guard); a change to it is *planned and reported*, and applied only by a human `tree apply` or by a standby instance during a hand-over. Otherwise a bad commit could stop the only thing able to repair it.
2. **The watcher dies with its cluster.** A watcher in Norca cannot repair Norca. Recommended topology: **an OS-service watcher outside the cluster (or in the management network) as the break-glass path, and/or the in-cluster job for convenience.** Both can run at the same time only if one of them is `plan-only`, until the operator form gives a leader election.

### Interface of the running service

The existing `:9095` surface (HMAC webhook, token-protected logs and status, TLS option, loopback-only without a token) is reused:

| Endpoint | Purpose |
|---|---|
| `POST /v1/tree/webhook` | push notification (same HMAC providers as GitOps) |
| `GET  /healthz` | liveness for the Norca service check / systemd watchdog |
| `GET  /v1/tree/status` | state of the binding: `idle`, `running`, `paused`, `degraded`; last good commit; versions (§7.7); pending plan summary; blocked stacks |
| `GET  /v1/tree/runs`, `/logs` | run history, ring-buffer log (the one in `pkg/gitops`) |
| `GET  /v1/tree/drift` | current drift list with diffs (values hidden) |
| `GET  /v1/tree/graph` | the stack order and the release dependencies of the pending run (§7.10) |
| `POST /v1/tree/sync`, `/pause`, `/resume` | manual control (token required) |
| `GET  /metrics` | Prometheus: `runs_total{result}`, `last_success_timestamp`, `drift_objects{kind}`, `pending_changes`, `version_skew`, `state` |

CLI clients use the same API: `tree status`, `tree sync`, `tree pause|resume` talk to a running service when `--addr` is given, and read the cluster otherwise (releases, operations, binding status).

**Visibility inside Norca.** The binding status becomes a custom object with the operator form (gitops-rigger-design.md 5.6), so it shows in the Norca UI; until then the operations and releases of the tree's stacks show there. Optional `notify.webhook` posts failures and `degraded` transitions to a chat/alert endpoint (gitops-rigger-design.md 5.8).

### What stays the same

- The tree is the only input; the service has no extra configuration language.
- Every apply goes through Stage A validation, the version preflight and the guards: the service gets no shortcuts a human `apply` does not have.
- Deletion is opt-in; the watcher defaults to `prune = false` and `plan-only` until explicitly raised.
- Layers: reads (drift watch) < plan (`plan-only`) < apply of selected kinds < heal < prune. Each raise is a config line, reviewed in Git like everything else.

---

## 11b. Placement: Where the Tool Runs and How It Reaches the Cluster

The tool talks to the Norca HTTP API (and the KV endpoint, the Git provider, and receives webhooks). *Where it runs* decides what is reachable, how, and which design of the rest makes sense. There are **three placements**:

| Placement | Where the tool runs | How it reaches the cluster |
|---|---|---|
| **`local`** | on the machine where a Norca agent runs (a local installation: dev box, single node, a server used for break-glass) | the local agent, `127.0.0.1` |
| **`proxy`** | on an operator workstation, a CI runner or a management host, **outside** the cluster network | over a network path with **zero or more hops**: direct, HTTP `CONNECT` proxy, SOCKS5, SSH bastion, or a reverse proxy/load balancer in front of the API |
| **`node`** | **on a Norca client node** of the cluster: an OS service there, or a Norca job constrained to client nodes (§11a, job form) | the node's own agent at `127.0.0.1` (the client agent forwards API calls to the servers) |

`local` is the existing meaning of the deployer's `connection.mode: local` (the tool and the agent share the host), seen from the API side. A **direct** remote connection is simply `proxy` with no hop.

### What the placement fixes


| | `local` | `proxy` | `node` |
|---|---|---|---|
| Norca API | `http(s)://127.0.0.1:4646` | the address of a server or of a load balancer, through the hops | `http(s)://127.0.0.1:4646` (client agent) |
| Consul-compatible KV (§7.12) | `127.0.0.1:8500` of the local client (if this is a client) | a client's **non-loopback** `bind`, through the hops; or a Consul | `127.0.0.1:8500`, no node change needed |
| Token | the local file written by the deployer's ACL bootstrap (`management_token_output`) | `!enc:` in the startup configuration, a token file or an environment variable | **workload identity** (job form) or a local token file |
| Inbound webhooks (§11) | loopback bind, or a reverse proxy on the same host (token required) | only if an inbound path exists (public address, reverse proxy, tunnel); otherwise **polling only** | through the node's address or a service/load balancer; polling works with no inbound port |
| Cache (`placement.json`) | local disk | disk of the management host | the task's directory |
| Failure domain | the host | the management host; survives a cluster outage | the cluster; dies with it (§11a *self-management*) |
| Typical use | development, single node | operations, CI, central watcher over all regions, break-glass | in-cluster convenience watcher, one per region |

### Configuration

A default placement for the process and an optional override per region. **Proxy profiles** are named and referenced, so the settings are written once and remembered by name:

```hcl
# norca-tree.hcl (startup configuration)
placement {
  type  = "proxy"                       # local | proxy | node   (default: auto, see below)
  proxy = "bastion"                     # a proxy profile below; omit for a direct connection
  inbound = "none"                      # can a webhook reach the watcher? direct | none (none = polling only)
}

proxy "bastion" {
  type              = "ssh"             # ssh | http | socks5
  host              = "bastion.example.com"
  ssh_port          = 22
  ssh_user          = "deploy"
  ssh_key_path      = "~/.ssh/norca_deployer"
  host_key_checking = "strict"          # tofu | strict | off, as in the deployer plan
  known_hosts_file  = "./known_hosts"
}

proxy "corp" {
  type         = "http"                 # HTTP CONNECT
  url          = "http://proxy.corp.example:3128"
  username     = "svc-norca"
  password     = "!enc:v1:aes-gcm:..."
  no_proxy     = ["127.0.0.1", "*.internal"]
  ca_cert      = "./certs/proxy-ca.pem" # only for a TLS-inspecting proxy
  idle_timeout = "60s"                  # what the proxy is known to cut; omit to have it probed
  max_body     = "1MiB"                 # the proxy's request size limit, if any
}

region "eu-1" {
  address = "https://10.0.0.11:4646"
  # ...

  placement {                           # how this region's endpoint is reached
    type            = "proxy"
    proxy           = "corp"
    tls_server_name = "server.eu-1.norca"   # when the address is a load balancer or a tunnel end
  }

  consul_kv {
    address = "http://10.0.0.21:8500"
    proxy   = "corp"                    # the KV flow may use another hop (or "none")
  }
}
```

- **`auto` (default):** inside a Norca task (`NORCA_ALLOC_ID` set) → `node`; the address is loopback and a local agent answers → `local`; anything else → `proxy` (with the profile if one is configured). `validate` rejects a contradiction: `local` with a non-loopback address, or `node` where the local agent is not a client (`/v1/agent/self`).
- **Per flow:** every outbound flow can name its own hop: the Norca API of a region, the KV endpoint, and the Git provider of a GitOps binding (`proxy = "..."` or `"none"`); the Git server is often outside the proxy that the cluster needs. Without a name the flow inherits the placement of its region.
- **`use_env = true`** on a profile reads `HTTPS_PROXY`/`NO_PROXY` of the process instead of fixed values; the default is **off**, so a run does not change behaviour with the shell it was started from.
- **Credentials** (`password`, private keys) follow §9: `!enc:`, `@file` or `!env:`; they are never logged.
- **Not supported:** NTLM/Kerberos proxy authentication and PAC scripts (Basic only; use an SSH bastion or a reverse proxy instead).

### Proxy types and what they mean for the design

| Hop | Wire | Consequences to design around |
|---|---|---|
| **none (direct)** | TLS to the server/LB | the server certificate names `server.<region>.norca`; by IP or LB name set `tls_server_name` |
| **`http` (CONNECT)** | TLS stays end-to-end through the tunnel; the proxy sees only `host:port` | many proxies allow `CONNECT` to **443 only**: the API must then be exposed on 443 (a load balancer), and the KV endpoint likewise; long-poll limits of the proxy (`idle_timeout`) apply; no inbound path |
| **`socks5`** | TCP tunnel, optional remote DNS | as `http`, but any port; set whether names resolve at the proxy (`remote_dns`) |
| **`ssh` (bastion)** | an SSH `direct-tcpip` channel to `address` as the bastion sees it | the existing connector and host-key policy are reused; no extra ports; each request multiplexes on one SSH session (keepalive configured); the API address is the one **from the bastion's network**; no inbound path |
| **reverse proxy / LB in front of the API** (not a hop: a different `address`) | TLS terminated or passed through | passthrough keeps `server.<region>.norca`; termination needs the LB certificate and `tls_server_name`; blocking queries and the event stream need long timeouts and **no response buffering**; the token header must be forwarded; request size limits cap job and KV values |

### Probes and remembered settings

`tree connect test [--region R]` (and step 1 of the preflight, §7.7) runs **read-only probes** through the configured path and records what it finds:

| Probe | Why it matters | Effect |
|---|---|---|
| TLS name check | wrong `tls_server_name` is the most common failure behind an LB | error with the exact fix |
| API identity and version | the region is who the startup configuration says (§7.7) | as in §7.7 |
| **Blocking query** (`?index=...&wait=...` for a few seconds beyond `idle_timeout`) | proxies cut idle responses | the watcher uses `wait` below the observed cut, or plain polling at `drift_interval` |
| **Event stream** (long-lived chunked response) | buffering proxies swallow it | event-triggered drift (§11a, later) only if it works; otherwise polling |
| KV endpoint | key space and backend (§7.12) | as in §7.12 |
| Inbound path | cannot be probed from inside; set `placement.inbound` to `direct` or `none` | `none` forces polling for source changes |

The results are **remembered** in the cache file (`placement.json`: type, hops, the effective values above and the time) and in the log of every run, so `tree status` shows *how* the last run reached its region, and the watcher re-reads them at start instead of guessing. A changed `placement` block or a failing probe invalidates the record and the next run probes again. Probes never write (no test keys, no test objects).

### Design by placement

| Decision | `local` | `proxy` | `node` |
|---|---|---|---|
| Watcher form (§11a) | OS service or none (CLI use) | OS service on the management host (the break-glass watcher) | Norca job, or an OS service on that node |
| Source changes | webhook on loopback / reverse proxy, or polling | **polling** unless an inbound path exists | polling, or webhook through the node/LB |
| HA | not applicable | the operator form (G6) | the operator form (G6) |
| Secrets | local token file | `!enc:` values in the startup configuration on the host | workload identity, secrets from the built-in store |
| What to record | token file path | the full `proxy` profiles and `placement` | the node constraint (`node_pool`, class) of the job |
| Main risk | the host is the single point | proxy limits (timeouts, body, ports) | the watcher dies with the cluster |

### The watcher's own API behind a proxy

The watcher's API (`/v1/tree/...`, §11a) answers only on loopback without a token, and a client behind a reverse proxy on the same host *looks* local. So with `proxy` or `node` placement behind any reverse proxy, **a token is mandatory** (`NORCA_GITOPS_API_TOKEN`-style, the setting of the GitOps daemon): `validate` refuses a non-loopback `watch.listen` without it.

---

## 11c. Runbook: From an Empty Cluster to a Running Watcher

*(Review item G12.)* The order in which a new platform is brought up. Each step has a check, and each can be repeated. The first run of a new tool is where people get hurt, so the sequence goes from read-only to write-enabled in small steps.

| Step | What | Check before going on |
|---|---|---|
| 0. **Cluster** | `norca-deployer apply` installs the nodes (existing flow, §3) and does the ACL bootstrap; the management token is kept outside Git | `norca status` shows a healthy cluster, all servers on the same version (7.7) |
| 1. **Tokens** | Create the policies for the tool (the per-kind table of §5.3): a read-only token for `status` and the drift watch, a token per namespace stack if wanted, a management token for `tree-global`. Put them in the startup configuration as `scoped_token` entries | `tree connect test` passes through the intended placement and proxy (11b); the preflight of stage A reports every token's capabilities |
| 2. **Tree** | `tree init` writes the skeleton, `tree.hcl` with its `version`, `.gitattributes` and `.gitignore`. For an existing platform: `tree import` writes what is live, then `import --adopt` lets the releases take the objects over **without changing them** | `tree validate` is clean offline; `tree plan` against the adopted cluster shows **no changes** (or only the differences the team accepts) |
| 3. **CI** | Add `tree validate` and `plan --detailed-exitcode --format json` to pull requests (§11); add the `CODEOWNERS` rule of §11 | A test PR with a deliberate error fails; a PR with a change shows the plan |
| 4. **Watcher, read only** | `tree watch --mode plan-only` as an OS service or a Norca job (11a), `limits.kinds` restricted to the safe kinds | The watcher reports the plan of each commit and the drift, writes nothing; metrics show the read budget (7.15) |
| 5. **First apply, foundation** | A manual `tree apply --stack tree-global` with `--prune` **off** | The operation shows what was created; a second `apply` plans `unchanged` |
| 6. **Workloads** | Manual `tree apply --namespace NS` for one namespace, then the others, then the written stacks; try one deliberate failure in an atomic namespace to see the restore (7.8) | A failed release is restored; `tree rollback` re-applies a revision |
| 7. **Raise `auto_apply`** | Step by step: first one non-sensitive kind in `watch.auto_apply`, then more; sensitive kinds (`acl-*`, `quota`) stay manual unless the team decides otherwise | Each step runs a few days before the next; any surprise returns the watcher to `plan-only` (`tree pause`) |
| 8. **Prune** | Only after the plans are trusted: `--prune` first, `--prune-jobs` and `--prune-data` last and by hand | The plan lists exactly the deletes the team expects |

What goes wrong most often, and the answer: a plan that shows changes right after `import` (normalisation differences: they are findings for the release engine, §7.14; never patch the cluster to hide them), a watcher that cannot reach the API (placement probes, §11b), a token that is too narrow (the preflight names the missing capability), and a commit that changes more than expected (`max_changes`, §7.6). The runbook is the base of the adoption guide of implementation plan P4-6.

---

## 12. Implementation Plan

*(Revised 2026-11-28 around steps G8a and G8b of gitops-rigger-design.md.)* The work breakdown (work packages, sizes, acceptance criteria, test strategy, risks, the code that exists today) is in [platform-management-implementation-plan.md](platform-management-implementation-plan.md); this section keeps the summary.

Code layout: new packages under `internal/tree/` (the deployer keeps its code in `internal/`; `internal/platform` holds the host platforms), `internal/values/` (value forms, extracted from the engine), and `cmd/norca-deployer/tree_cmd.go`. The stack client is the one the stack bindings already use (`internal/gitops/stack.go`).

Phases (each shippable on its own):

| Phase | Step | Content | Done when |
|---|---|---|---|
| **T0 – Foundations** | – | `internal/values`, the stack client made reusable outside a binding (files in, plan, apply, follow), the experimental gate, the doc-example test | the pieces are tested; nothing user-visible changed |
| **T1 – Tree to stacks** | G8a | `tree.hcl` and the startup configuration, the tree loader with the naming and file-system rules, generated stacks (implicit module, secrets), written stacks (shared modules, values), `tree init|validate|plan|apply|status` for the supported kinds, guards, atomic by `on_failure`, scoped tokens per stack | a tree with `global/`, two namespaces and one written stack plans, applies, applies again with no change, and an invalid file changes nothing |
| **T2 – Safe operations** | G8a | prune rules (§7.4), uninstall of removed generated stacks, orphan report, `max_changes`, rename hint, `--rollback-scope run`, `tree rollback`, version preflight (§7.7), saved plans and JSON (§7.13), `validate --changed-since`, `tree graph` | deletions only with their flags; a stale saved plan is refused |
| **T3 – Tree binding and adoption** | G8a | the `tree {}` binding (§11) with `auto_apply` and `heal` per stack, `tree import` and `--adopt`, conflict checks with the plan's sections, the runbook (§11c) | a push applies the stacks the policy allows; an existing platform is adopted with no change |
| **T4 – Connection layer** | – | placements `local`/`proxy`/`node`, proxy hops (HTTP CONNECT, SOCKS5, SSH), probes, `tree connect test` | the same tree plans identically directly and through each proxy type |
| **T5 – More kinds** | G8b | each kind Rigger registers (roles, auth methods, binding rules, node pools, network policies, intentions, CSI volumes, Consul KV, scheduler and autopilot): its directory accepted by `validate`, its example, its e2e | the whole inventory of §2 is covered |
| **T6 – Service forms** | G8b | `tree watch` as a standalone controller with one tree binding, `tree service install` (systemd, Windows), the self-management guard, the example Norca job; the operator form is G6 | the watcher runs as a service and as a Norca job and survives a restart |
| **T7 – Hardening and release** | – | security review, large trees, documentation `doc/tree-*.md`, `convert` and `fmt`, removal of the experimental gate | the feature is released |

T1 depends only on G1, which is built (the stack bindings, `internal/gitops/stack.go`). T3 needs G2 (polling, binding status) for the controller behaviour and G5 for `heal`. T5 waits for each Rigger kind. T6 builds on G2 and G6.

Testing: every ` ```hcl ` example of this document is parsed by a Go test, and so is `examples/cluster-config/`; the built stacks of the examples are checked with `rigger stack validate` in CI; an end-to-end test applies a tree to `norca agent -dev` and to a region of the lab.

---

## 13. Open Questions (to settle before Phase 1)

Every question has an answer in the table *13.1* below the list, **accepted as the design** (the *decide* answers are decisions; the *spike* answers are the working assumption until the spike says otherwise). *Decide* means the answer is final; *spike* means a short check against the Norca source or a lab cluster (work package P0-7 of the implementation plan) settles it, with the fallback already designed; *deferred* belongs to the multi-region phase (§1).

1. *(Removed 2026-11-28 with the multi-region design: which kinds Norca replicates is a fact of §2; what it means for a tree in several regions is designed with multi-region.)*
2. *(Obsolete 2026-11-28: no ledger; ownership is the release owner, §7.4.)*
3. **Role and auth-method file format.** The CLI has no file input for them; the small HCL manifests (§8) are the proposal. Alternatively add `norca acl role apply <file>` to Norca itself – cleaner, but a change in the other repository.
4. *(Obsolete 2026-11-28: secrets are stack secrets, created once, §8.)*
5. **Daemon location.** Run the tree binding in the same `gitops daemon` process (proposed) or in the operator form of the controller (gitops-rigger-design.md step G6).
6. *(Obsolete 2026-11-28: a plain job file has no variables; a job with variables is a module of a stack, §4.)*
7. **Delete semantics of namespaces.** A namespace with jobs cannot be deleted; `--prune` must stop on that with a clear message instead of cascading.
8. **Version endpoints.** Confirm which Norca endpoints expose per-server and per-client versions and the API level (agent self/members, node list) and whether Norca publishes a machine-readable feature list. If not, the `kind → minimum version` table is maintained in the deployer and has to be updated with every Norca release (a CI check against the Norca repo's feature status would keep it honest).
9. *(Obsolete 2026-11-28: an atomic release restores Variables itself; stack secrets are not rolled back, §7.8.)*
10. **Auto-revert interplay.** A job with `auto_revert = true` is reverted by Norca during the release's wait for healthy deployments. Whether the release then counts as failed is the release engine's rule; the deployer reports what the release reports and never issues a revert itself.
11. **Windows service.** The deployer runs on Windows; decide between a native service registration inside the binary (`golang.org/x/sys/windows/svc`) and documenting a wrapper. Linux systemd units can simply be generated.
12. **Event stream for drift.** Which event topics cover each managed kind (the Variables topic exists; policies, namespaces, quotas and jobs must be checked) and whether the stream resumes reliably after a watcher restart. Until confirmed, polling is the only drift mechanism.
13. **Workload identity scope.** Confirm that an ACL policy bound to a job (`job_id`/`group`/`task`) can carry the management-like capabilities the watcher needs (namespace write, ACL policy write, Variables) without a management token, or whether some kinds (ACL, quotas, namespaces) need a token held by the OS-service watcher only.
14. **System bus subject** for run events, and whether a job may publish to it from a task (credentials and subject namespace of the embedded NATS).
15. *(Resolved 2026-11-28: Rigger has `requires { module }` in `metadata.hcl`, rigger-design.md 6.5.)*
16. *(Obsolete 2026-11-28: the gates went with the unit graph; Rigger waits until a release is ready, §7.10.)*
17. *(Obsolete 2026-11-28: there are no units; a file belongs to exactly one stack and a resource to exactly one release, §7.4.)*
18. **Release ownership (decided 2026-11-28).** The tree owns the releases of its own stacks: the generated `tree-global` and `tree-ns-<ns>` and the written stacks of `stacks/`. It never takes over a release of another stack (the plan's `rigger` section, another binding, a person's `rigger stack apply`); the servers refuse it, and `tree import` does not import it. Objects that no release owns are taken over only with `--adopt`. Registries stay with the operator or the plan (§2, row 22).
19. **Reaching the Consul-compatible KV** (the placements of §11b decide: `local` and `node` use loopback, `proxy` needs the client's `bind` through the hop). The `consul_compat` listener is on loopback by default and belongs to a client. Decide how `tree` reaches it (a reachable `bind` on the cluster address through `apply`, or the watcher as a job next to a client), and check that `/v1/kv` with `recurse` and `cas` behaves identically on the `variables` and the `bus` backend (including ModifyIndex semantics).
20. **Backend discovery.** Whether the active KV backend (`auto` resolves at the first request) can be read from the API, so `consul_kv.backend` can be verified by the preflight instead of only declared.
21. *(Removed: external programs and actions are not in scope for now.)*
22. *(Removed with question 21.)*
23. **CONNECT and ports.** Whether the proxy profiles must support a fixed port allow-list (`CONNECT` to 4646/8500) or whether the documented answer is always "expose the API and the KV on 443 behind a load balancer".
24. *(Removed 2026-11-28 with the multi-region design.)*
25. **Node placement detection.** The reliable test that the local agent is a client (and which region/node pool it is in) and what a Norca job may call on `127.0.0.1:4646` with workload identity.
26. **SSH tunnel behaviour.** Keepalive, reconnect and the number of concurrent channels for long blocking queries over one SSH session; whether the existing connector can expose a `direct-tcpip` dialer.
27. *(Removed 2026-11-28 with the multi-region design.)*
28. *(Removed with question 21.)*

### 13.1 Answers (accepted)

| # | Proposal | Settled by | Needed before |
|---|---|---|---|
| 1 | Removed with the multi-region design | removed | none |
| 2 | Obsolete: no ledger (§7.4) | obsolete | none |
| 3 | **HCL manifests for roles and auth methods** (§8) now. A `norca acl role apply <file>` in Norca is a nice-to-have ticket, not a dependency | decide | P1 |
| 4 | Obsolete: stack secrets, created once (§8) | obsolete | none |
| 5 | **Same `gitops daemon` process**: the tree binding is one more binding form. `tree watch` stays available standalone; the operator form is step G6 | decide | T3 |
| 6 | Obsolete: plain job files have no variables (§4) | obsolete | none |
| 7 | Decided in G11: a namespace (or quota, policy) that is still in use refuses cleanly and names its dependents; never a cascade | resolved (G11) | P3 |
| 8 | Read `GET /v1/agent/members`, `/v1/nodes` and `/v1/agent/self` for the versions. If Norca publishes no machine-readable feature list, the **`kind -> minimum version` table lives in the deployer** and a CI check against the Norca feature status keeps it current (P6-3) | spike, fallback designed | P1 |
| 9 | Obsolete: the release restores, stack secrets are not rolled back (§7.8) | obsolete | none |
| 10 | The deployer reports the release's outcome and never reverts a job itself; the release engine's rule decides | decide | T1 |
| 11 | **Native Windows service in the binary** (`golang.org/x/sys/windows/svc`, same flags as the systemd unit generator); a documented wrapper is the fallback. Linux/macOS: generated unit | decide | P5 |
| 12 | **Polling is the baseline; blocking queries (§7.15) are the first improvement; event streams are an optimisation** added per kind after the spike shows topic coverage and reliable resume | spike | P5 |
| 13 | **Assume no** (now backed by the permission table of §5.3): writing ACL policies, roles, auth methods, binding rules and namespaces needs a management token, so those kinds are applied by the OS-service watcher or a person with the management `scoped_token`; a watcher running as a Norca job is limited to the kinds that have a policy form, with its workload identity. The spike may widen what the job can do, never narrow the rule | spike, default designed | P5 |
| 14 | Run events are **not** published on the system bus in the first release: they go to the log, the metrics and the binding status. Bus publishing is a later option once the subject namespace and the task credentials are known | decide (defer feature) | P5 |
| 15 | Resolved by Rigger's `requires { module }` | resolved | none |
| 16 | Obsolete with the unit graph; Rigger's wait until ready (§7.10) | obsolete | none |
| 17 | Obsolete: a file belongs to one stack, a resource to one release | obsolete | none |
| 18 | **The tree owns its own stacks' releases only**; other stacks' releases are never taken over; `--adopt` for objects no release owns | decide (2026-11-28) | T1 |
| 19 | The KV endpoint is **configured explicitly** in `consul_kv.address` and reached through the same placement and proxy as the API (§11b). A reachable `bind` is the operator's job through `apply`. The spike checks that `recurse` and `cas` behave identically on the `variables` and the `bus` backend; until then only the backend that passed is listed as supported in the kind-support matrix | decide + spike | P3b |
| 20 | `consul_kv.backend` is **declared**. If the API shows the active backend, the preflight verifies it; if not, it checks the CAS and ModifyIndex behaviour on a key under a managed prefix on first use and stops on a mismatch | spike, fallback designed | P3b |
| 21 | *Removed: external programs and actions are not in scope for now* | removed | none |
| 22 | *Removed with question 21* | removed | none |
| 23 | The proxy **does not enforce a port allow-list itself**; the documented rule is "expose the API and the KV on 443 behind a load balancer, or let the operator's proxy allow 4646 and 8500". `connect test` reports which ports are reachable through the hop | decide | P1b |
| 24 | Removed with the multi-region design | removed | none |
| 25 | **The operator declares `placement.type = node`**; the probe only verifies it: `GET /v1/agent/self` on the loopback must show a client with its region and node pool. What a job may call with workload identity is checked in the spike | decide + spike | P1b |
| 26 | One SSH session per proxy profile, keepalive 30 s, reconnect with backoff, a pool of at most 10 channels for blocking queries; if the existing connector cannot give a `direct-tcpip` dialer, a thin wrapper around `golang.org/x/crypto/ssh` is added in P1b | spike | P1b |
| 27 | Removed with the multi-region design | removed | none |
| 28 | *Removed with question 21* | removed | none |

**Summary of the table (questions 21, 22 and 28 were removed with the actions):** 9 questions are plain *decide* (2, 3, 5, 10, 11, 14, 15, 17, 23), 3 are *decide + small spike* (6, 19, 25), 7 are *spike* with the fallback already in the design (8, 9, 12, 13, 16, 20, 26), 4 are resolved (1, 4, 7, 18) and 2 are deferred (24, 27). All proposals were accepted by the project owner. The spikes were read from the source on 2026-11 and are in 13.2; the changes they caused are marked there. **Only the lab checks left in 13.2 can still change the design**, and none of them blocks P1: each has a fallback that is already written.

**Phases and obsolete rows (2026-11-28).** Questions 2, 4, 6, 9, 16 and 17 went with the tree's own engine, 15 is answered by Rigger, 18 is decided anew. The *Needed before* column of the other rows names the phases of the earlier plan; the revised plan (§12) maps them as P0 → T0, P1 → T1, P1b → T4, P2 → T1 (jobs, Variables and secrets come with the generated stacks), P2b → dropped, P3 → T2 and T5, P3b → T5, P4 → T3, P5 → T6, P6 → T7. Questions 1, 24 and 27 went with the multi-region design.

### 13.2 Spike results (P0-7, from the Norca source)

Read from the Norca source in `norca/norca` (`main`, 2026-11): the RPC endpoints, the HTTP layer of `command/agent`, the ACL package and `client/consulcompat`. **Nothing was run on a cluster**; items marked *open* still need a lab check. The first column is the question of §13.

| # | Result | Consequence for the design |
|---|---|---|
| 4 | **Yes, partly.** A secret's metadata (`GET /v1/secret/<path>?metadata`, needs only `list-secrets`) returns `CurrentVersion`, `ModifyIndex`, `ItemKeys`, `ValueHash` and the version list, without any value. The write reply returns the new version, and `?cas=<ModifyIndex>` makes writes conditional. `ValueHash` is a **keyed** hash: it cannot be computed locally. Variables have only `ModifyIndex` and CAS | The ledger records the **version and ModifyIndex the tool wrote**; a different live `CurrentVersion` is drift detected with a metadata read, with no value read. Whether the *tree* value changed is still decided from the digest in the ledger (G5). A read-only token needs only `list-secrets` for this |
| 6 | **Confirmed, with limits.** `POST/PUT /v1/jobs/parse` is **HTTP-only** (the agent parses, no RPC), takes `JobHCL`, `Variables` (one string with the content of a variables file, HCL2), `HCLv1`, `Canonicalize`, and needs `parse-job` **or** `submit-job`. The parse runs with `AllowFS: false` | The merged var string is built by the tool as decided. **A job that reads local files at parse time (`file()` and similar) cannot be parsed server-side**; `validate` reports it with the function name, and such values must come from variables or from `!enc:`/`@file` values of the tree |
| 8 | **Partly, no feature list.** Versions: `/v1/agent/self` (`norca_version`, `nomad_version`), `/v1/agent/members` (serf tags `norca_vsn`, `build`, `raft_vsn`, `region`), node attributes `norca.version` and `nomad.version`. `ServerHealth.Version` is the **Nomad** build, not the Norca version. No endpoint lists features or an API level | The tool uses `norca_vsn` (servers) and `norca.version` (clients) for the `requires` check and for the kind-support matrix, which stays maintained in the deployer with a CI check (P6-3). The autopilot health version is **not** used |
| 9 | **Secrets: yes, partly.** Versions are kept up to `max_versions`, an old version can be read (`?version=n`), soft delete and undelete exist, but there is **no promote-version call**. **Variables: no history** | Secret rollback = read version n, write it as a new version (Stage C, new row), no on-disk before-image. Variables keep the encrypted before-image |
| 12 | **Partly.** Topics exist for ACL policies, roles, tokens, auth methods and binding rules, node pools, jobs, Variables, secrets, quotas, admission policies, network policies, custom objects, service intentions and services; **none for namespaces**, and releases are probably `CustomObject` events (*open*). The buffer is 1000 events in memory. Without `exact=true` an old index silently skips events; with `exact=true` the server answers 410 | Polling stays the baseline (§11a step 4 now states the rules). Events are hints per kind; namespaces always by timer |
| 13 | **Confirmed.** A policy bound to a job (`job_id`/`group`/`task`) can contain any rule the parser accepts, and the resulting ACL is **never management**. ACL policy/role/token upserts and namespace upsert/delete need management | A watcher running as a Norca job can manage the kinds that have a policy form (`quota`, `node-pool`, `admission-policy`, `scheduler-config`, `autopilot-config`, `admin-network-policy`, `object-kind`, and the namespaced kinds) if its job-bound policy carries those rules, and **never** `acl-*` or `namespace`. Such a policy gives the same powers to every task of that job, which is the argument for a separate job for the watcher (§11a) |
| 16 | **Yes.** A service registration has `Ready` (derived from the startup and readiness checks), `Health`, the per-check state and `HealthUpdatedAt`; a check has a `Role`. A release has `status.phase`, `conditions` (`Ready`, `HooksSucceeded`, `Drifted`) and `status.lastTest` (`revision`, `succeeded`, hooks) | `wait = "ready"` reads `Ready` of the service registration; `wait = "tested"` reads `lastTest.succeeded` **for the current revision**; `wait = "healthy"` on a pack release reads the `Ready` condition. All four gates are feasible; the kind-support matrix still carries the minimum Norca version |
| 19 | **No, not identical.** The Consul-compatible KV has routes `/v1/kv/...` with `recurse`, `keys`, `raw`, `flags`, `cas`, blocking `index`/`wait`, a value limit of 512 KiB and no sessions. **Flags exist only on the `variables` backend**; `ModifyIndex` is the Raft index on `variables` and the per-key revision on `bus`/`nats`. The listener is **off by default** (`consul_compat.enabled`) and binds `127.0.0.1:8500` by default; the source suggested it exists in the server and the client config, **the lab shows it is configured in the `client` block only (13.3)** | Stage A rejects flags on `bus` (already so in 7.12), and ModifyIndex is treated as an opaque equality token (7.12). Reaching the listener stays an `apply` (agent config) matter. A backend switch needs the migrate endpoint of the operator, not the tool |
| 20 | **No.** `/v1/agent/self` shows the *configured* `consul_compat.kv.backend`, but `auto` is not shown resolved, and no header or status field names the active backend | `consul_kv.backend` is declared; for `auto` the tool probes behaviour (a `flags` write on a canary key under a managed prefix, once) or the operator declares `variables` or `bus` explicitly. **Recommendation: never `auto`** for a platform that manages KV |
| 25 | **Partly.** `/v1/agent/self` (needs `agent:read`) returns the config (`Client.Enabled`, region, datacenter, node pool), the `stats` of the running client and server, and the versions; `member` is only set on servers. *Open:* what a task's workload identity may call there | `placement.type = node` is declared by the operator, verified by `Client.Enabled` or a `client` entry in `stats`; the agent-read capability is a documented requirement of the token used by the node placement |
| 26 | **Answered in the deployer.** `pkg/connector/ssh.go` opens one `ssh.Client` with `ssh.Dial` and a dial timeout, **without keepalive** and **without exposing a dial method** (the client is private). The module `golang.org/x/crypto` is already a direct dependency | P1b adds `Dial(network, addr)` to the connector (`ssh.Client.Dial` is the `direct-tcpip` channel), a keepalive loop (`keepalive@openssh.com`, 30 s) and reconnect with backoff; no new module |
| 11 | **Answered in the module file.** No `x/sys/windows/svc` use exists yet; `golang.org/x/sys` is in `go.mod` as an *indirect* dependency (it contains `windows/svc`) | The native Windows service needs no new module, only `x/sys` promoted to a direct dependency (P5-6) |
| (5) | **Dynamic host volumes: no API.** Only static client-config host volumes exist, checked in the job ACL. CSI has register, create, deregister, delete and snapshot routes, all `csi-write-volume` | The `volume` kind is **CSI volumes only** (§2 row 19 corrected); host volumes are node configuration (`apply`) |
| (cap.) | **Capability names confirmed** in `acl/policy.go`. Syntax: `secret "<path>" { capabilities = [...] }`, `object "<group>/<Kind>" { capabilities = [...] }`, `network_policy { policy = "read"|"write" }`, `admission { policy = ... }`, `variables { path "..." { capabilities = [...] } }` | The permission table (§5.3) is correct as written; the path- and kind-scoped forms are available for tighter tokens |

**Open after the spikes:** the lab (13.3) confirmed the example policies, the version sources, job plan and parse, the secret metadata, the KV behaviour and the events; **still open** are the workload identity and `agent:read` (question 25), the release `lastTest` and `conditions` fields (question 16, release part) and `CustomObject` events for releases, the stream after a server restart (question 12), and the resolution of `backend = "auto"` (question 20).

**Norca-side tickets that came out of the spikes** (drafts, not filed): [issue-norca-replication-scope.md](issue-norca-replication-scope.md) (which kinds replicate), [issue-norca-readonly-plan.md](issue-norca-readonly-plan.md) (`acl` read rule and `plan-job`) [issue-norca-features-endpoint.md](issue-norca-features-endpoint.md) (a machine-readable features list incl. replication scope and the resolved KV backend, Norca version in autopilot health), [issue-norca-namespace-event-topic.md](issue-norca-namespace-event-topic.md), [issue-norca-secret-restore.md](issue-norca-secret-restore.md) (restore by version) and [issue-norca-service-register-panic.md](issue-norca-service-register-panic.md) (a CLI bug). The design depends on none of them: every item has a fallback in the tool, and the tool switches to the new form by Norca version once it exists.

**Note on the module layout:** the deployer already has a package `pkg/platform` (node configuration: firewall, packages, scripts, storage, tuning). The implementation plan therefore names the new package `pkg/tree`, which avoids a clash.

### 13.3 Lab results (P0-7)

Run on 2026-11-03 against a **throwaway dev cluster** (`norca agent -dev` with ACLs, one server and client in one process, Norca 0.3.0, Nomad API 1.6.5 compatible, built from `main`), with a management token, a read-only token and an application token. The scripts and the cluster were scratch material outside the repository; nothing in the repository was changed by the lab. Where a lab result **contradicts the source reading of 13.2**, the lab wins and is marked *correction*.

| # | Check | Result | Consequence |
|---|---|---|---|
| 1 | The example policies of §5.3 and the other forms (`secret "<path>"`, `object "<group>/<Kind>"`, `network_policy`, `admission`, `quota`, `operator`, `node_pool`, `agent`, `node`) | **All accepted** by `norca acl policy apply` as written | the permission table and the example policies stand |
| 2 | What a read-only token (read-job in all namespaces, quota/operator/admission/node-pool/agent read) can read | ACL policies: **only its own** (`GET /v1/acl/policies` returns one entry, another policy is 403); roles: 200 (filtered); binding rules: **403**; auth methods: the **list works for any token** (name, type, default, hash; the full config is 403); namespaces: **all listed** with a capability in every namespace; quotas, scheduler and autopilot configuration, node pools, jobs: 200 | *correction/refinement:* auth methods can be **listed** by a plain token, only their configuration needs management. The tool can compare name, type and `Default` of auth methods with a read-only token, not the configuration. ACL policies, roles and binding rules stay management for a full plan (as in 13.2) |
| 3 | `job plan` and friends with different tokens | `job plan` with **read-job: 403**; with **submit-job: 200**. `job validate` with read-job: 200. `jobs/parse` without `parse-job`: 403; with `parse-job` and `submit-job`: authorised | confirms the read-only plan gap (and the Norca ticket `plan-job`) and the job fallback of §5.3 |
| 4 | Version sources | `/v1/agent/self`: `norca_version` 0.3.0, `nomad_version` 1.6.5; serf tags `norca_vsn`, `build`, `region`, `dc`; node attributes `norca.version`, `nomad.version`. **`/v1/agent/members` is 403 with `agent` read alone and 200 with `node` read** (also `/v1/nodes`) | the plan token needs **`node` read** for the version preflight (7.7), not only `agent` read. Added to the permission table (§5.3) |
| 5 | `/v1/agent/self` for placement | `config.Client.Enabled` true, `config.Client.NodePool`, `config.Region`, `config.Datacenter`, `stats.client.node_id`; `member` is set on servers only | placement detection (question 25) works as designed |
| 6 | `jobs/parse` | `file("...")` fails with `filesystem function disabled`; `Variables` as one string with a variables file works; `Canonicalize` works | question 6 confirmed |
| 7 | Secret metadata with only `list-secrets` | `GET /v1/secret/<path>?metadata` returns `CurrentVersion`, `ModifyIndex`, `ItemKeys`, `ValueHash` (`hmac-sha256:...`) and the versions; the value read is **403** | question 4 confirmed: drift of a secret is detectable without reading the value |
| 8 | Variables under `nomad/` | a Variable path under the top-level `nomad/` directory is only valid below `nomad/jobs` or `nomad/job-templates` (HTTP 500 otherwise) | the ledger, lock and status Variables use `norca-deployer/tree/...` (not under `nomad/`), which is correct; the `nomad/jobs/<job>` Variables of the examples are also valid |
| 9 | Consul-compatible KV | **`consul_compat` is configured in the `client` block** (`client { consul_compat { ... } }`) and is rejected at the top level or in the server block. Token header `X-Consul-Token`. `flags` are kept, `recurse` works, `X-Consul-Index` is set, the key is stored as the Variable `consul/kv/<key>` in the **`default`** namespace. **A failed CAS answers HTTP 200 with the body `false`** (a successful one `true`), not an error code. A value above 512 KiB: 413. On the `variables` backend a **70 KB value answers 500** (the 64 KB Variable limit, no clean 4xx). The active backend is shown only in `/v1/agent/self` (`Client.ConsulCompat.KV.Backend`, as configured), not in a header or in `/v1/operator/bus` | *correction:* the listener belongs to the client agent only (13.2 had "server and client"). The KV writer must read the CAS result from the **body**, and stage A must reject values above the backend limit itself (64 KB on `variables`) because the server's answer is an unclean 500. §7.12 states both |
| 10 | Event stream | live topics seen: `ACLAuthMethod`, `ACLPolicy`, `ACLToken`, `Node`, `NodePool`, `Quota`, `Variables`. **No event for a namespace upsert** (the `Namespace` topic stream stayed empty), and **none for a secret put** (the `Secret` topic stream stayed empty) | namespaces **and secrets** must be polled; the watcher does not rely on events for them (7.15 and 11a already poll namespaces; secrets are added) |
| 11 | Service registration (external service registered through the API) | `Ready` true, `Health` `passing`, `HealthUpdatedAt`, `Checks[]` with `Role` and `Status`, `Namespace` | the `ready` gate fields (question 16) are confirmed for services |
| 12 | `norca service register` CLI | **panics** at start (`service register flag redefined: address`): the command's `-address` flag collides with the global `-address` of the server | a Norca bug; the tool uses the HTTP API (`PUT /v1/service/external/<name>`) and not the CLI. Added to the Norca ticket draft |

**Not tested in the lab** (needs more than a dev agent): the **workload identity** of a task and what it may call on the loopback (`agent:read`, question 25 remainder), the **release API** (`status.lastTest`, `conditions`, no pack in the lab), the event stream **after a server restart** and `exact=true` with an index older than the buffer (question 12 remainder), the resolution of `backend = "auto"` (the lab used `variables`). These stay open and need a cluster with a pack and a running job.

## 13a. Review Findings: Gaps Still to Design

Found by reading the whole document again after the scope decision. The stale multi-region texts and the old YAML-style notation were corrected in place; what remains are **real omissions**. Each needs a decision or a short design before the phase that depends on it (last column).

| # | Gap | Why it matters | Proposal | Before |
|---|---|---|---|---|
| G1 | **Two levels of configuration.** *Decided:* the machine and credential settings are **given when the program starts** (startup configuration, §5.2); the tree keeps only the portable manifest (§5.1). A commit can never widen what the process may do | Credentials stay out of Git; the watcher does not need the tree to know where the tree is | Done in §5 | P1 |
| G2 | **The tool's own ACL permissions.** *Decided:* the token is part of the startup configuration (§5.3), tokens are **chosen per stack** with `scoped_token` (revised 2026-11-28; first per group of kinds), and the **per-kind permission table is written** (§5.3, from the Norca source): management is required only for the ACL kinds and `namespace`; a read-only token cannot plan the ACL kinds and (server-side) jobs, so `plan` has a defined fallback, and a Norca ticket (draft) would remove it | The token needs are known per kind | §5.3 | P1 |
| G3 | **Approved plan vs applied plan.** *Accepted:* saved plan, `apply` recomputes and compares, pending plans and `approve` for the watcher, JSON output | Reviewers approve exactly what runs | §7.13 | P1 |
| G4 | **Fields owned by something else** (autoscaler `count`). *Accepted, moved 2026-11-28:* the rule belongs to the release engine (§7.14, gitops-rigger-design.md 6.3) and is not built there yet (G24) | Applies no longer fight the autoscaler | §7.14 | P2 |
| G5 | **Secret digests without local state.** *Superseded 2026-11-28:* a release is skipped by its input digest and a stack secret by its existence; the deployer keeps no digest anywhere | – | §7.3, §9 | – |
| G6 | **Provenance meta on jobs.** *Accepted:* the commit SHA is **not** written into job meta (it would change the job on every commit); it is recorded in the releases (`Commit`), the operation and the binding status (§11). Jobs carry only the constant `meta.managed-by`, written once, after the spike confirms it causes no new version | A commit that changes nothing else never restarts a workload | §11 (provenance) | P2 |
| G7 | **One vocabulary for kinds.** *Accepted:* one identifier per kind, one grammar, strict selectors, one place for `kinds`/`prune`/`auto_apply`/`heal` | Typos no longer disable a policy silently | §8.1 | P1 |
| G8 | **Name clash** of `external`. *Decided:* the reference to something the tree does not manage is called **`reference`** (`references.hcl`). *Obsolete 2026-11-28:* `references.hcl` went with the unit graph (§7.10); the record below is kept for the vocabulary | No confusion with the `external-service` object or `secrets.store = external` | §7.10, decision record below | P2b |
| G9 | **Format version** of the tree. *Accepted:* `tree.version = "norca.io/tree/v1alpha1"` required; unknown versions are refused; evolution rules and `convert --to-version` | The tree format can evolve, an unknown one is refused | §5.1 | P1 |
| G10 | **File-system hazards.** *Accepted:* byte-for-byte compare, `.gitattributes` from `init`, case-collision and illegal-character checks on the raw file list, long-path limits, no symlinks | The same commit gives the same values on every machine | §4 (*File-system hazards*) | P1 |
| G11 | **Renames, moves and removal.** *Revised 2026-11-28:* rename hint in the plan, creates before deletes inside a release, `--prune`, `--prune-jobs` and `--prune-data` as guards on the servers' plans (a release purges a job it deletes), `moved` is a Rigger question | A rename never silently stops a workload without a warning | §7.4 | P3 |
| G12 | **Day-0 and day-1 runbook.** *Accepted:* nine steps from cluster to prune, each with a check | The first run of the tool is guided | §11c | P4 |
| G13 | **Exit codes and machine output.** *Accepted:* codes `0`-`4` for every command, `--format text|json` with a versioned JSON schema | CI and wrappers can rely on them | §10 | P1 |
| G14 | **Inventory edges.** *Accepted:* explicit *not managed* list (scaling policies, node meta/eligibility/drain, CSI plugins, tokens, packs, runtime objects, operator operations) and the rule for new object types | A reader can tell excluded from forgotten | §2 | P1 |
| G15 | **Tenant isolation.** *Accepted:* a token per namespace stack (§5.3) plus `validate --changed-since` listing touched directories and the documented `CODEOWNERS` rule | A team's commit cannot be written with a stronger token | §5.3, §11 | P3 |
| G16 | **API load.** *Accepted:* list-then-fetch, parallel reads with a limit and a rate, blocking-query change detection, `stale` reads for drift only, backoff, visible read budget | A large tree does not make the watcher heavy for the servers | §7.15 | P6 |
| G17 | **Shared modules outside the deployer** *(2026-11-28)*. A written stack uses a module of the tree's `modules/` as `./modules/<name>`, and the deployer adds it to the files it sends. Plain `rigger stack plan` in the stack directory does not find it. | A stack of the tree should also be planned with the `rigger` program alone | An option of `rigger stack` that names further module directories (`--modules ../../modules`), or a `modules_path` in the `rigger` block of the stack; Rigger-side, to be decided with Rigger | G8a |
| G18 | **Size of a generated namespace stack** *(2026-11-28)*. One release `tree-<ns>` holds every plain file of a namespace. A change to one job plans the whole release, and a failure of one job fails it. | Large namespaces, `atomic` rollback of unrelated jobs | Keep one release per namespace first. If needed, one release per job file (`tree-<ns>-<job>`) and one for the rest | G8a |
| G19 | **The namespace of `tree-global`** *(2026-11-28)*. `tree.namespace` must exist before the first apply, or be one that `tree-global` itself creates | The first apply on a new cluster | Allow only `default` (always there) until T1 shows a need for another; then apply the namespace alone in `default` first | T1 |
| G20 | **Planning a stack before its namespace exists** *(2026-11-28)* | `tree-ns-apps` writes into `apps`, which `tree-global` creates in the same run, so the servers cannot plan it before `tree-global` is applied | Stage A plans such a stack as `install` from the offline checks and plans it again just before its apply (§7.8); the run reports that its plan was partial | T1 |
| G21 | **A marker of the tree on the release owner** *(2026-11-28)* | A written stack removed from `stacks/` is not found for prune: the owner names only the stack | An optional tree name on `ReleaseOwner` (it has `Source` for published stacks), written by the deployer; a Norca and Rigger change. Until then: empty the stack, apply with `--prune`, then delete it (§7.4) | T2 |
| G22 | **`--kind` cannot narrow a release** *(2026-11-28)* | A release is applied whole; sending part of it deletes the rest | `--kind` is a guard on the plans (§10). A real narrowing needs smaller releases (G18) | T1 |
| G23 | **Offline evaluation** *(2026-11-28)* | Without a cluster `tree validate` stops at syntax and the tree's rules; the full evaluation needs the servers or the `rigger` program | `tree build` writes the stacks and `tree validate --rigger` runs the installed `rigger stack validate` on them. Importing the Rigger core into the deployer would also need Norca's job decoder; not proposed | T1 |
| G24 | **The autoscaler's `count` in releases** *(2026-11-28)* | A scaled job showed as drifted (an apply never reset its count: `keepScaledCounts`) | **Built 2026-11-28**, norca `28c3bcdd8` on `feat/rigger-gitops-prep`: the drift hash leaves the scaled count out (gitops-rigger-design.md 6.3) | T3 |
| G25 | **A read-only stack plan** *(2026-11-28)* | The servers require object `write` on `rigger.norca.io/Release` for a stack plan as for an apply (`authorizeRiggerStack` in `norca/norca/rigger_operation.go`), so `plan-only` and pull-request plans need a write token | **Built 2026-11-28**, norca `871c9298b` on `feat/rigger-gitops-prep`: a stack plan, a stack status and the plan of a release need `read` on releases and read on what they compare; objects the token may not write are not compared (gitops-rigger-design.md 6.7) | T3 |

**Decisions of 2026-11-28 (the user, all as proposed):** G18 one release per namespace first, measured on the lab; G19 only `default` for `tree.namespace` at first; G20 partial plan and a second plan before the apply; G22 `--kind` is a guard; G23 `tree validate --rigger` with the installed `rigger`, no Rigger core in the deployer. Changes asked of Norca and Rigger: G24 (the autoscaler's `count`) and G25 (a read-only stack plan) go ahead now; G17 (a module path option of `rigger stack`) and G21 (a tree marker on the release owner) come later.

### G8: decision record

**Problem.** The word `external` was used for three unrelated things: the declaration of a dependency that the tree does not manage, the Norca object *external service* (`norca service register`, kind `external-service`, directory `services/`), and `secrets.store = external` of the node plan (plus a sentence in 7.5 about a secret path "marked external"). Two of them sat in the same directory, and an error message "external not found" would not say which.

**Candidates:**

| Name | Reads as | For | Against |
|---|---|---|---|
| **`reference`** | "this tree refers to something that exists elsewhere" | neutral, short, says read-only | generic; Norca already has an *owner reference* (`ObjectOwnerReference`), a different meaning in a different context |
| `unmanaged` | "not managed by this tool" | states the contract | sounds like an exclusion list; Norca has "managed jobs" |
| `provided` | "the cluster provides it" | reads well | close to the secret *provider* wording |
| `needs` | "this unit needs it" | very short | collides in meaning with `depends_on` |

**Decision: `reference`.** File `references.hcl` (one per namespace), block `reference "<name>" { type = ..., ... }`, graph layer *References (read-only)*. Types: `pack-release`, `job`, `service`, `secret`, `variable`. The block that adds options to an edge stays `dependency "..." {}`. References are not a kind (R0 of §8.1). `external-service` keeps its meaning and its directory; `secrets.store = external` of the node plan is untouched.

**Why not rename the Norca side instead.** Considered and rejected. Norca's *external service* is Norca's own addition (so the Nomad 1.6.5 API promise does not cover it), but the term is wired into many places: the HTTP paths `/v1/service/external/...` and `/v1/services/external`, the ACL capability string `register-external-service` (stored in policies and used in the example policies and the documentation, 16 places), the public Go types of the `api` package (`ExternalService`, `RegisterExternal`, ...), about 30 Go files, the Consul-compatible layer, the web UI, the CLI help and the docs. Renaming only the words in docs, UI and help is harmless but changes nothing for tools; renaming the wire names (paths, capability, JSON fields) breaks existing policies and clients and needs aliases and a deprecation period. It would also not remove the other two uses of `external` (the node plan's `secrets.store = external`, and the plain English word in Norca's own "external secret provider"). The kind identifiers are the tool's vocabulary, mapped to the Norca API names in the registry (§8.1), so a later Norca rename would change one registry entry and nothing in the trees.

---

## 14. Summary

| Question | Answer |
|---|---|
| What does it manage? | API objects of a running Norca platform, written as Rigger releases. Today: quotas, namespaces, ACL policies, admission policies, object kinds, Variables, external services, custom objects, jobs and secrets. As Rigger registers them: ACL roles, auth methods and binding rules, node pools, network policies, intentions, CSI volumes, Consul KV, scheduler and autopilot settings |
| What does it not do? | Install nodes (that is `apply`), manage tokens, operate keyrings/Raft, take over releases of other stacks |
| Engine | None of its own. The tree is built into Rigger stacks (`tree-global`, `tree-ns-<ns>`, the written stacks of `stacks/`) and the servers' release engine applies them (§4.1) |
| File format | Native Norca files named by Rigger's extensions (`<name>.<kind>.hcl`), Rigger modules and stacks, and the HCL deployer manifests (`tree.hcl`, secrets); YAML is a secondary form of the manifests only |
| Layout | `cluster-config/` with `tree.hcl`, `modules/`, `stacks/`, `global/`, `namespaces/<ns>/`, `kv/` |
| Regions | One run per region, through that region's endpoint; several regions are several runs (§6.1). The multi-region design is not part of this document |
| State | None local. Ownership, digests, history and generated values are the releases', operations' and stacks' on the servers (§7.9); only a placement cache is written |
| Versions | Preflight: servers consistent, tree `requires` range, kinds supported, node-plan version match (§7.7) |
| Dependencies | Stack order (`tree-global`, then `stack_order`, then the rest); inside a stack Rigger's references and `depends_on` with its wait until ready; inside a release the fixed kind order (§7.5, §7.10) |
| Failure | Invalid input stops before any write; a failed release is restored by the server when atomic; `--rollback-scope run` also rolls back the releases the run changed; `tree rollback` re-applies a release revision (§7.8) |
| Deletion | A file removed is a resource deleted by the release upgrade, guarded by `--prune`, `--prune-jobs` and `--prune-data`; a release removed is uninstalled only with `--prune`; secrets are never deleted (§7.4) |
| GitOps | A tree binding of the GitOps controller (gitops-rigger-design.md section 5): one run applies every stack of the tree; `auto_apply` and `heal` per kind, decided per stack and per release (§11) |
| Configuration | Two files: the **tree manifest** `tree.hcl` (in Git: format version, `requires`, the namespace of `tree-global`, `stack_order`, `layers`) and the **startup configuration** `norca-tree.hcl` (outside Git: cluster, tokens per stack, placement, limits, watch). A commit can never widen what the process may do |
| Plans | The servers' plan of every stack, `plan --out` and `apply FILE` with stale-plan refusal, `--format json`, exit codes `0`-`4` (§7.13, §10) |
| Placement | `local`, `proxy` (direct, HTTP CONNECT, SOCKS5, SSH bastion, reverse proxy), `node` (§11b) |
| Day 0 | A written runbook from an empty cluster to a watcher that applies (§11c) |
| Not Terraform because | No state backend, no providers, no expressions outside Rigger modules, plain files stay usable with the `norca` CLI, and the cluster itself holds ownership and history |
