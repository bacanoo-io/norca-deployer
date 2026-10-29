package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
)

// resolveValue resolves a value of a secret item or a pack value: an
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

// resolveValues resolves the strings of a pack values tree into a copy.
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
			return fmt.Errorf("ACL policy %s: HTTP %d: %v %s", pol.Name, status, err, out)
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

// deployPacks creates the cluster registries and installs the enabled packs
// with the server-side deploy. A pack is only deployed again when its
// settings changed since the last successful apply.
func (r *Runner) deployPacks(first *target, token string) error {
	pk := r.plan.Packs
	if pk == nil || len(pk.Deploy) == 0 {
		return nil
	}
	if r.plan.Security.ACL != nil && r.plan.Security.ACL.Enabled && token == "" {
		fmt.Printf("    [WARN] No management token: %d pack(s) not deployed\n", len(pk.Deploy))
		return nil
	}
	cli := first.cli(r, token)

	regs := pk.AllRegistries()
	for _, reg := range regs {
		body, _ := json.Marshal(map[string]any{
			"Name": reg.Name,
			"Spec": map[string]any{
				"type":         reg.Type,
				"source":       reg.URL,
				"defaultRef":   reg.Ref,
				"syncInterval": reg.SyncInterval,
			},
		})
		if status, out, err := cli.api("POST", "/v1/pack-registry", body); err != nil || status != 200 {
			return fmt.Errorf("pack registry %q: HTTP %d: %v %s", reg.Name, status, err, out)
		}
		if status, out, err := cli.api("POST", "/v1/pack-registry/"+reg.Name+"/sync", nil); err != nil || status != 200 {
			return fmt.Errorf("pack registry %q sync: HTTP %d: %v %s", reg.Name, status, err, out)
		}
		fmt.Printf("    Pack registry %q synced\n", reg.Name)
	}

	for _, d := range pk.Deploy {
		if !d.Enabled {
			continue
		}
		registry := d.Registry
		if registry == "" {
			registry = regs[0].Name
		}
		values, err := r.resolveValues(d.Values)
		if err != nil {
			return fmt.Errorf("pack %q values: %w", d.Name, err)
		}
		req := map[string]any{
			"Registry": registry,
			"Pack":     d.Pack,
			"Version":  d.Version,
			"Values":   values,
			"Wait":     true,
			"Timeout":  int64(10 * time.Minute),
		}
		body, err := json.Marshal(req)
		if err != nil {
			return fmt.Errorf("pack %q: %w", d.Name, err)
		}
		sum := sha256.Sum256(body)
		marker := r.store.Path(filepath.Join("packs", d.Name+".sha256"))
		if prev, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(prev)) == hex.EncodeToString(sum[:]) {
			fmt.Printf("    Pack %s unchanged (skipped)\n", d.Name)
			continue
		}
		fmt.Printf("    Deploying pack %s (%s/%s)...\n", d.Name, registry, d.Pack)
		status, out, err := cli.api("POST", "/v1/release/"+d.Name+"/deploy-pack", body)
		if err != nil || status != 200 {
			return fmt.Errorf("pack %q: HTTP %d: %v %s", d.Name, status, err, out)
		}
		if err := os.MkdirAll(filepath.Dir(marker), 0700); err == nil {
			_ = os.WriteFile(marker, []byte(hex.EncodeToString(sum[:])+"\n"), 0600)
		}
		fmt.Printf("    [OK] Pack %s deployed\n", d.Name)
	}
	return nil
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
			return fmt.Errorf("node pool %s: HTTP %d: %v %s", np.Name, status, err, out)
		}
		save(d)
		fmt.Printf("    [OK] Node pool %s applied\n", np.Name)
	}
	return nil
}
