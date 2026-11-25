package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/gitops"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// resolveValue resolves a value of a secret item or of a GitOps binding: an
// encrypted value, a generated value or NKey, or (fileOK) "@path".
func (r *Runner) resolveValue(v string, fileOK bool) (string, error) {
	switch {
	case strings.HasPrefix(v, config.GeneratePrefix):
		name, size, err := config.ParseGenerated(v)
		if err != nil {
			return "", err
		}
		return r.store.GeneratedValue(name, size)
	case strings.HasPrefix(v, config.NKeySeedPrefix):
		return r.store.NKeySeed(strings.TrimPrefix(v, config.NKeySeedPrefix))
	case strings.HasPrefix(v, config.NKeyPublicPrefix):
		return r.store.NKeyPublic(strings.TrimPrefix(v, config.NKeyPublicPrefix))
	case fileOK && strings.HasPrefix(v, "@"):
		data, err := os.ReadFile(r.plan.ResolveLocalPath(v[1:]))
		return string(data), err
	}
	return r.resolveSecret(v)
}

// resolveValues resolves the strings of the values tree of a GitOps binding
// into a copy.
func (r *Runner) resolveValues(v any) (any, error) {
	switch t := v.(type) {
	case string:
		return r.resolveValue(t, false)
	case map[string]any:
		res := make(map[string]any, len(t))
		for k, e := range t {
			x, err := r.resolveValues(e)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			res[k] = x
		}
		return res, nil
	case []any:
		res := make([]any, len(t))
		for i, e := range t {
			x, err := r.resolveValues(e)
			if err != nil {
				return nil, err
			}
			res[i] = x
		}
		return res, nil
	}
	return v, nil
}

// marker returns the stored digest of an object the deployer wrote, and a
// function that records a new one.
func (r *Runner) marker(kind, key string) (string, func(string)) {
	sum := sha256.Sum256([]byte(key))
	path := r.store.Path(filepath.Join(kind, hex.EncodeToString(sum[:8])+".sha256"))
	prev, _ := os.ReadFile(path)
	return strings.TrimSpace(string(prev)), func(digest string) {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err == nil {
			_ = os.WriteFile(path, []byte(digest+"\n"), 0600)
		}
	}
}

func digest(v any) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// applyACLPolicies creates or updates the policies of security.acl.policies.
// Replica regions get them by replication from the authoritative region.
func (r *Runner) applyACLPolicies(first *target, token string) error {
	acl := r.plan.Security.ACL
	if acl == nil || len(acl.Policies) == 0 {
		return nil
	}
	if r.plan.IsReplicaRegion() {
		fmt.Printf("    ACL policies replicate from %s (not applied here)\n", r.plan.Federation.AuthoritativeRegion)
		return nil
	}
	if token == "" {
		fmt.Printf("    [WARN] No management token: %d ACL polic(ies) not applied\n", len(acl.Policies))
		return nil
	}
	cli := first.cli(r, token)
	for i := range acl.Policies {
		pol := &acl.Policies[i]
		rules, err := r.plan.PolicyRules(pol)
		if err != nil {
			return fmt.Errorf("ACL policy %s: %w", pol.Name, err)
		}
		body := map[string]any{"Name": pol.Name, "Description": pol.Description, "Rules": rules}
		if j := pol.Job; j != nil {
			body["JobACL"] = map[string]string{"Namespace": j.Namespace, "JobID": j.JobID, "Group": j.Group, "Task": j.Task}
		}
		d := digest(body)
		prev, save := r.marker("acl-policies", pol.Name)
		if prev == d {
			fmt.Printf("    ACL policy %s unchanged (skipped)\n", pol.Name)
			continue
		}
		data, _ := json.Marshal(body)
		if status, out, err := cli.api("POST", "/v1/acl/policy/"+url.PathEscape(pol.Name), data); err != nil || status != 200 {
			return apiError(fmt.Sprintf("ACL policy %s", pol.Name), status, out, err)
		}
		save(d)
		fmt.Printf("    [OK] ACL policy %s applied\n", pol.Name)
	}
	return nil
}

// bootstrapSecrets writes the secrets and Variables of secrets.bootstrap.
// Without overwrite an entry is only created (check-and-set with index 0:
// an existing secret or Variable is left as it is, e.g. after a rotation);
// with overwrite a new version is written when the items in the plan
// changed since the last apply.
func (r *Runner) bootstrapSecrets(first *target, token string) error {
	s := r.plan.Secrets
	if s == nil || len(s.Bootstrap) == 0 {
		return nil
	}
	if acl := r.plan.Security.ACL; acl != nil && acl.Enabled && token == "" {
		fmt.Printf("    [WARN] No management token: %d bootstrap secret(s) not written\n", len(s.Bootstrap))
		return nil
	}
	cli := first.cli(r, token)
	skipped := 0
	for i := range s.Bootstrap {
		b := &s.Bootstrap[i]
		if b.Type == config.SecretTypeSecret && s.Store == config.SecretStoreExternal {
			skipped++
			continue
		}
		label := fmt.Sprintf("%s %s/%s", b.Type, b.Namespace, b.Path)
		items := map[string]string{}
		for k, v := range b.Items {
			val, err := r.resolveValue(v, true)
			if err != nil {
				return fmt.Errorf("%s item %s: %w", label, k, err)
			}
			items[k] = val
		}

		endpoint := "/v1/secret/" + b.Path
		body := map[string]any{"Items": items}
		if b.Type == config.SecretTypeVariable {
			endpoint = "/v1/var/" + b.Path
			body = map[string]any{"Namespace": b.Namespace, "Path": b.Path, "Items": items}
		}
		query := url.Values{"namespace": {b.Namespace}}
		d := digest(body)
		prev, save := r.marker("secrets", b.Type+"|"+b.Namespace+"|"+b.Path)
		if b.Overwrite && prev == d {
			fmt.Printf("    %s unchanged (skipped)\n", label)
			continue
		}
		if !b.Overwrite {
			query.Set("cas", "0")
		}
		data, _ := json.Marshal(body)
		status, out, err := cli.api("PUT", endpoint+"?"+query.Encode(), data)
		switch {
		case err != nil:
			return fmt.Errorf("%s: %w", label, err)
		case status == 409 && !b.Overwrite:
			fmt.Printf("    %s exists (kept)\n", label)
		case status != 200:
			return fmt.Errorf("%s: HTTP %d: %s", label, status, redact(out, items))
		default:
			save(d)
			fmt.Printf("    [OK] %s written\n", label)
		}
	}
	if skipped > 0 {
		fmt.Printf("    secrets.store is external: %d secret(s) of the built-in store skipped (set them up in the external store)\n", skipped)
	}
	return nil
}

// redact removes item values from an error response.
func redact(out string, items map[string]string) string {
	for _, v := range items {
		if len(v) >= 4 {
			out = strings.ReplaceAll(out, v, "<redacted>")
		}
	}
	return out
}

// applyNodePools creates or updates the node pools of the plan (description
// and meta). A pool that a node names is created anyway when the node
// registers; this sets its description. Node pools replicate from the
// authoritative region.
func (r *Runner) applyNodePools(first *target, token string) error {
	pools := r.plan.NodePools
	if len(pools) == 0 {
		return nil
	}
	if r.plan.IsReplicaRegion() {
		fmt.Printf("    Node pools replicate from %s (not applied here)\n", r.plan.Federation.AuthoritativeRegion)
		return nil
	}
	if acl := r.plan.Security.ACL; acl != nil && acl.Enabled && token == "" {
		fmt.Printf("    [WARN] No management token: %d node pool(s) not applied\n", len(pools))
		return nil
	}
	cli := first.cli(r, token)
	for _, np := range pools {
		body := map[string]any{"Name": np.Name, "Description": np.Description, "Meta": np.Meta}
		d := digest(body)
		prev, save := r.marker("node-pools", np.Name)
		if prev == d {
			fmt.Printf("    Node pool %s unchanged (skipped)\n", np.Name)
			continue
		}
		data, _ := json.Marshal(body)
		if status, out, err := cli.api("PUT", "/v1/node/pools", data); err != nil || status != 200 {
			return apiError(fmt.Sprintf("node pool %s", np.Name), status, out, err)
		}
		save(d)
		fmt.Printf("    [OK] Node pool %s applied\n", np.Name)
	}
	return nil
}

// applyNamespaces creates the namespaces the secrets and the releases need:
// norca-system, the namespace of the operator modules, and those of the plan.
// A namespace is looked up only when its entry changed since the last apply.
// A missing one is created. An existing one is left as it is when its
// description and meta are the plan's, and for norca-system when the plan
// does not list it; otherwise its description and meta are set to the
// plan's, and its other settings (quota, capabilities, node pools) are kept.
// The deployer never deletes a namespace. Namespaces replicate from the
// authoritative region.
func (r *Runner) applyNamespaces(first *target, token string) error {
	namespaces := r.plan.EnsuredNamespaces()
	if r.plan.IsReplicaRegion() {
		fmt.Printf("    Namespaces replicate from %s (not applied here)\n", r.plan.Federation.AuthoritativeRegion)
		return nil
	}
	if acl := r.plan.Security.ACL; acl != nil && acl.Enabled && token == "" {
		fmt.Printf("    [WARN] No management token: %d namespace(s) not applied\n", len(namespaces))
		return nil
	}
	// The reads and the writes go to the local agent of the first server,
	// in the region of the plan: the authoritative one here, which holds
	// the indexes the check-and-set compares.
	cli := first.cli(r, token)
	for _, ns := range namespaces {
		d := digest(map[string]any{"Name": ns.Name, "Description": ns.Description, "Meta": ns.Meta, "Implicit": ns.Implicit})
		prev, save := r.marker("namespaces", ns.Name)
		if prev == d {
			fmt.Printf("    Namespace %s unchanged (skipped)\n", ns.Name)
			continue
		}
		result, err := applyNamespace(cli, &ns)
		if err != nil {
			return err
		}
		save(d)
		if result == "kept" {
			fmt.Printf("    Namespace %s exists (kept)\n", ns.Name)
		} else {
			fmt.Printf("    [OK] Namespace %s %s\n", ns.Name, result)
		}
	}
	return nil
}

// namespaceAttempts bounds the read-decide-write rounds of one namespace
// whose check-and-set write keeps conflicting with other writers.
const namespaceAttempts = 3

// applyNamespace reads the namespace and writes it when it is missing or
// differs from the plan, and returns "kept", "created" or "updated". The
// write is a check-and-set (?cas=0 for a create, ?cas=<ModifyIndex read>
// for an update): a namespace created or changed by somebody else between
// the read and the write answers 409, and the namespace is read and decided
// again instead of being overwritten.
func applyNamespace(cli *remoteCLI, ns *config.EnsuredNamespace) (string, error) {
	what := "namespace " + ns.Name
	path := "/v1/namespace/" + url.PathEscape(ns.Name)
	for attempt := 1; ; attempt++ {
		status, out, err := cli.api("GET", path, nil)
		if err != nil || (status != 200 && status != 404) {
			return "", apiError(what, status, out, err)
		}
		body := map[string]any{"Name": ns.Name, "Description": ns.Description, "Meta": ns.Meta}
		var index uint64
		verb := "created"
		if status == 200 {
			var live map[string]any
			if err := json.Unmarshal([]byte(out), &live); err != nil {
				return "", fmt.Errorf("%s: %w", what, err)
			}
			if ns.Implicit || namespaceMatches(live, &ns.NamespaceConfig) {
				return "kept", nil
			}
			var idx struct{ ModifyIndex uint64 }
			if err := json.Unmarshal([]byte(out), &idx); err != nil {
				return "", fmt.Errorf("%s: %w", what, err)
			}
			if idx.ModifyIndex == 0 {
				return "", fmt.Errorf("%s: the answer has no ModifyIndex for the check-and-set write: %s", what, strings.TrimSpace(out))
			}
			// The write replaces the whole namespace: keep what the plan
			// does not describe.
			delete(live, "Hash")
			delete(live, "CreateIndex")
			delete(live, "ModifyIndex")
			live["Description"], live["Meta"] = ns.Description, ns.Meta
			body, verb, index = live, "updated", idx.ModifyIndex
		}
		data, err := json.Marshal(body)
		if err != nil {
			return "", fmt.Errorf("%s: %w", what, err)
		}
		status, out, err = cli.api("PUT", path+"?cas="+strconv.FormatUint(index, 10), data)
		switch {
		case err != nil || (status != 200 && status != 409):
			return "", apiError(what, status, out, err)
		case status == 409 && attempt >= namespaceAttempts:
			return "", fmt.Errorf("%s: still changed by another writer after %d check-and-set attempts, run apply again: %s",
				what, namespaceAttempts, strings.TrimSpace(out))
		case status == 409:
			fmt.Printf("    Namespace %s changed since it was read (check-and-set conflict), reading it again\n", ns.Name)
			continue
		}
		return verb, nil
	}
}

// namespaceMatches reports whether a namespace as the API returned it has
// the description and the meta of the plan.
func namespaceMatches(live map[string]any, want *config.NamespaceConfig) bool {
	if desc, _ := live["Description"].(string); desc != want.Description {
		return false
	}
	meta, _ := live["Meta"].(map[string]any)
	if len(meta) != len(want.Meta) {
		return false
	}
	for k, v := range want.Meta {
		if s, ok := meta[k].(string); !ok || s != v {
			return false
		}
	}
	return true
}

// deployGitOps runs the GitOps bindings: every apply fetches the stack of
// each and has the servers plan it. They compare it with the releases of
// the cluster, so the deployer keeps no marker: a stack that is in sync is
// left alone, another is applied (or, by its policy, only planned).
func (r *Runner) deployGitOps(first *target, token string) error {
	if len(r.plan.AllGitOpsBindings()) == 0 {
		return nil
	}
	bindings, err := r.plan.ResolvedGitOpsBindings(r.resolveSecret)
	if err != nil {
		return err
	}
	if r.plan.Security.ACL != nil && r.plan.Security.ACL.Enabled && token == "" {
		fmt.Printf("    [WARN] No management token: %d GitOps binding(s) not run\n", len(bindings))
		return nil
	}
	cli := first.cli(r, token)
	if cli.isWindows() {
		return fmt.Errorf("gitops: a stack can't be sent through a Windows server; run the first master on Linux or use gitops run")
	}
	runner := gitops.Runner{Client: remoteAPI{cli}, Report: func(e gitops.Event) {
		fmt.Printf("    [%s] %s\n", e.Phase, e.Details)
	}}

	for i := range bindings {
		b := &bindings[i]
		resolved, err := r.resolveValues(b.Values)
		if err != nil {
			return fmt.Errorf("gitops binding %s: values: %w", b.Name, err)
		}
		values, _ := resolved.(map[string]any)

		if err := gitops.CheckBinding(b); err != nil {
			return err
		}
		fmt.Printf("    Running the GitOps binding %s (%s, %s, mode %s)...\n",
			b.Name, b.EffectiveProvider(), gitops.SanitizeURL(b.EffectiveRepo().URL), b.Mode())
		// The errors of a run name the binding, the stack, the commit and
		// the operation.
		res, err := runner.Run(r.abortCtx(), b, gitops.FetchOptionsFor(b), values)
		if err != nil {
			return err
		}
		fmt.Printf("    [OK] GitOps binding %s: %s\n", b.Name, res)
	}

	return nil
}

// remoteAPI calls the cluster through the CLI of a server. A call that has
// started is not cut by its context; one is not started with a done one.
type remoteAPI struct{ cli *remoteCLI }

func (a remoteAPI) API(ctx context.Context, method, path string, body []byte) (int, string, error) {
	if err := ctx.Err(); err != nil {
		return 0, "", err
	}
	return a.cli.api(method, path, body)
}
