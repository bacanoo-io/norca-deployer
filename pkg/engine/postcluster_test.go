package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/bacanoo-io/norca-deployer/pkg/connector"
	"github.com/bacanoo-io/norca-deployer/pkg/platform/storage"
	"github.com/bacanoo-io/norca-deployer/pkg/state"
)

// apiRecorder answers the API calls of the first server and records the
// method, the path and the body of every call.
type apiRecorder struct {
	mu       sync.Mutex
	calls    []string
	bodies   map[string]string
	conflict map[string]bool // paths answering 409
}

var apiCallRe = regexp.MustCompile(`'-X' '([A-Z]+)' '([^']+)'`)

func (a *apiRecorder) conn() *connector.FakeConnector {
	f := connector.NewFakeConnector()
	f.Handler = func(cmd string) (string, error, bool) {
		m := apiCallRe.FindStringSubmatch(cmd)
		if m == nil {
			return "", nil, true
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		call := m[1] + " " + m[2]
		a.calls = append(a.calls, call)
		a.bodies[call] = string(f.Files["/var/lib/norca-deployer/cli.body"])
		path, _, _ := strings.Cut(m[2], "?")
		if a.conflict[path] {
			return "< HTTP/1.1 409 Conflict\ncas conflict", nil, true
		}
		return "< HTTP/1.1 200 OK\n{}", nil, true
	}
	return f
}

const bootstrapPlan = `
metadata: {name: prod, region: eu}
release: {version: v0.2.0}
security:
  acl:
    enabled: true
    auto_bootstrap: true
    policies:
      - name: audit
        rules_file: bridge.hcl
        job: {job_id: audit}
secrets:
  bootstrap:
    - path: nats/users/app
      items: {password: "!generate:nats-app"}
    - path: nats/users/audit
      items: {seed: "!nkey-seed:bridge"}
    - type: variable
      path: nomad/jobs/nats
      items: {tls_ca: "@ca.pem", client_token: "!generate:nats-app"}
      overwrite: true
packs:
  registries:
    - {name: builtin, type: dir, url: builtin}
    - {name: norca, url: https://github.com/bacanoo-io/norca-registry, ref: v0.1.0}
  deploy:
    - name: nats
      registry: norca
      pack: nats
      enabled: true
      values:
        bridge_enabled: true
        bridge_nkey: "!nkey-public:bridge"
        users: [{name: app}]
inventory:
  masters:
    - {host: 10.0.0.1, hostname: s1}
`

func bootstrapRunner(t *testing.T, yaml string) (*Runner, *target, *apiRecorder) {
	t.Helper()
	dir := t.TempDir()
	for name, data := range map[string]string{"bridge.hcl": "node { policy = \"read\" }\n", "ca.pem": "CA-PEM"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "plan.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	plan := loadPlanFile(t, path)
	r := NewRunner(plan, nil)
	store, err := state.Open(plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.store = store
	api := &apiRecorder{bodies: map[string]string{}, conflict: map[string]bool{}}
	m := &plan.Inventory.Masters[0]
	return r, &target{node: m, isMaster: true, paths: storage.ResolveNodePaths(m, plan), conn: api.conn()}, api
}

func TestBootstrapSecretsCreateOnceAndGenerate(t *testing.T) {
	r, first, api := bootstrapRunner(t, bootstrapPlan)
	if err := r.bootstrapSecrets(first, "token"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PUT /v1/secret/nats/users/app?cas=0&namespace=default",
		"PUT /v1/secret/nats/users/audit?cas=0&namespace=default",
		"PUT /v1/var/nomad/jobs/nats?namespace=default",
	}
	if got := strings.Join(api.calls, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", got)
	}

	var secret struct{ Items map[string]string }
	json.Unmarshal([]byte(api.bodies[want[0]]), &secret)
	password := secret.Items["password"]
	if len(password) != 43 { // 32 random bytes, base64url
		t.Fatalf("generated password %q", password)
	}
	var variable struct {
		Namespace, Path string
		Items           map[string]string
	}
	json.Unmarshal([]byte(api.bodies[want[2]]), &variable)
	if variable.Path != "nomad/jobs/nats" || variable.Items["tls_ca"] != "CA-PEM" || variable.Items["client_token"] != password {
		t.Fatalf("variable body: %+v (the same generated name must give the same value)", variable)
	}
	json.Unmarshal([]byte(api.bodies[want[1]]), &secret)
	if !strings.HasPrefix(secret.Items["seed"], "SU") {
		t.Fatalf("NKey seed %q", secret.Items["seed"])
	}

	// Second apply: existing secrets are kept (409), the unchanged
	// overwrite entry is not written again.
	api.calls = nil
	api.conflict["/v1/secret/nats/users/app"] = true
	api.conflict["/v1/secret/nats/users/audit"] = true
	if err := r.bootstrapSecrets(first, "token"); err != nil {
		t.Fatal(err)
	}
	if len(api.calls) != 2 {
		t.Fatalf("second apply calls: %v", api.calls)
	}
}

func TestExternalStoreSkipsSecretsButWritesVariables(t *testing.T) {
	r, first, api := bootstrapRunner(t, strings.Replace(bootstrapPlan, "secrets:\n", "secrets:\n  store: external\n", 1))
	if err := r.bootstrapSecrets(first, "token"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(api.calls, "\n"); got != "PUT /v1/var/nomad/jobs/nats?namespace=default" {
		t.Fatalf("calls with an external store:\n%s", got)
	}
}

func TestACLPolicyBoundToJob(t *testing.T) {
	r, first, api := bootstrapRunner(t, bootstrapPlan)
	if err := r.applyACLPolicies(first, "token"); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Name, Rules string
		JobACL      map[string]string
	}
	json.Unmarshal([]byte(api.bodies["POST /v1/acl/policy/audit"]), &body)
	if body.Name != "audit" || !strings.Contains(body.Rules, "node") || body.JobACL["JobID"] != "audit" || body.JobACL["Namespace"] != "default" {
		t.Fatalf("policy body: %+v", body)
	}
	api.calls = nil
	if err := r.applyACLPolicies(first, "token"); err != nil || len(api.calls) != 0 {
		t.Fatalf("unchanged policy applied again: %v %v", api.calls, err)
	}
}

func TestPacksFromSeveralRegistriesWithGeneratedValues(t *testing.T) {
	r, first, api := bootstrapRunner(t, bootstrapPlan)
	if err := r.deployPacks(first, "token"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(api.calls, "\n")
	for _, want := range []string{"POST /v1/pack-registry/builtin/sync", "POST /v1/pack-registry/norca/sync", "POST /v1/release/nats/deploy-pack"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s:\n%s", want, joined)
		}
	}
	var req struct {
		Registry string
		Values   map[string]any
	}
	json.Unmarshal([]byte(api.bodies["POST /v1/release/nats/deploy-pack"]), &req)
	pub, _ := req.Values["bridge_nkey"].(string)
	seed, err := r.store.NKeySeed("bridge")
	if err != nil {
		t.Fatal(err)
	}
	wantPub, _ := r.store.NKeyPublic("bridge")
	if req.Registry != "norca" || !strings.HasPrefix(pub, "U") || pub != wantPub || seed == pub {
		t.Fatalf("deploy request: registry %q, bridge_nkey %q", req.Registry, pub)
	}
	if users, ok := req.Values["users"].([]any); !ok || len(users) != 1 {
		t.Fatalf("list values must stay lists: %#v", req.Values["users"])
	}
}

func TestNodePools(t *testing.T) {
	r, first, api := bootstrapRunner(t, strings.Replace(bootstrapPlan, "inventory:", "node_pools:\n  - {name: gpu, description: GPU nodes, meta: {owner: ml}}\ninventory:", 1))
	if err := r.applyNodePools(first, "token"); err != nil {
		t.Fatal(err)
	}
	var body struct {
		Name, Description string
		Meta              map[string]string
	}
	json.Unmarshal([]byte(api.bodies["PUT /v1/node/pools"]), &body)
	if body.Name != "gpu" || body.Description != "GPU nodes" || body.Meta["owner"] != "ml" {
		t.Fatalf("pool body: %+v", body)
	}
	api.calls = nil
	if err := r.applyNodePools(first, "token"); err != nil || len(api.calls) != 0 {
		t.Fatalf("unchanged pool applied again: %v %v", api.calls, err)
	}
}
