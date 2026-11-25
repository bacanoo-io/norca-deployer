package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
	"github.com/bacanoo-io/norca-deployer/internal/state"
)

// apiRecorder answers the API calls of the first server and records the
// method, the path and the body of every call.
type apiRecorder struct {
	mu       sync.Mutex
	calls    []string
	bodies   map[string]string
	conflict map[string]bool   // paths answering 409
	answers  map[string]string // "METHOD path": the verbose output of the answer
	// queued holds answers given once each, in order, before answers.
	queued map[string][]string
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
		if q := a.queued[m[1]+" "+path]; len(q) > 0 {
			a.queued[m[1]+" "+path] = q[1:]
			return q[0], nil, true
		}
		if out, ok := a.answers[m[1]+" "+path]; ok {
			return out, nil, true
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
	api := &apiRecorder{bodies: map[string]string{}, conflict: map[string]bool{}, answers: map[string]string{}, queued: map[string][]string{}}
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

// The values of a GitOps binding are resolved into a copy of the same form:
// a generated value is replaced, a list stays a list and a number a number.
func TestResolveValuesKeepsTheTree(t *testing.T) {
	r, _, _ := bootstrapRunner(t, bootstrapPlan)

	values := map[string]any{
		"bridge_nkey": "!nkey-public:bridge",
		"count":       3,
		"users":       []any{map[string]any{"name": "app", "password": "!generate:nats-app"}},
	}

	resolved, err := r.resolveValues(values)
	if err != nil {
		t.Fatal(err)
	}

	got, _ := resolved.(map[string]any)

	wantPub, err := r.store.NKeyPublic("bridge")
	if err != nil {
		t.Fatal(err)
	}

	if pub, _ := got["bridge_nkey"].(string); !strings.HasPrefix(pub, "U") || pub != wantPub {
		t.Errorf("bridge_nkey = %q, want the public NKey %q", got["bridge_nkey"], wantPub)
	}

	if got["count"] != 3 {
		t.Errorf("count = %#v, want 3", got["count"])
	}

	users, ok := got["users"].([]any)
	if !ok || len(users) != 1 {
		t.Fatalf("users = %#v, want a list of one user", got["users"])
	}

	user, _ := users[0].(map[string]any)
	if password, _ := user["password"].(string); user["name"] != "app" || len(password) < 20 || strings.HasPrefix(password, "!") {
		t.Errorf("users[0] = %#v, want the name kept and the password generated", user)
	}

	// The plan keeps what it said.
	if values["bridge_nkey"] != "!nkey-public:bridge" {
		t.Errorf("the values of the plan were changed: %#v", values)
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

const notFound = "< HTTP/1.1 404 Not Found\nNamespace not found"

// namespaceBody decodes the body of a namespace write.
func namespaceBody(t *testing.T, api *apiRecorder, call string) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal([]byte(api.bodies[call]), &body); err != nil {
		t.Fatalf("%s: body %q: %v", call, api.bodies[call], err)
	}
	return body
}

func TestNamespacesCreatedOnce(t *testing.T) {
	plan := strings.Replace(bootstrapPlan, "inventory:", "namespaces:\n  - {name: apps, description: Applications, meta: {owner: platform}}\ninventory:", 1)
	r, first, api := bootstrapRunner(t, plan)
	api.answers["GET /v1/namespace/norca-system"] = notFound
	api.answers["GET /v1/namespace/apps"] = notFound
	if err := r.applyNamespaces(first, "token"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /v1/namespace/norca-system", "PUT /v1/namespace/norca-system?cas=0",
		"GET /v1/namespace/apps", "PUT /v1/namespace/apps?cas=0",
	}
	if got := strings.Join(api.calls, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", got)
	}
	if body := namespaceBody(t, api, "PUT /v1/namespace/norca-system?cas=0"); body["Name"] != "norca-system" || body["Description"] != "Norca operators" {
		t.Fatalf("norca-system body: %v", body)
	}
	body := namespaceBody(t, api, "PUT /v1/namespace/apps?cas=0")
	if meta, _ := body["Meta"].(map[string]any); body["Name"] != "apps" || body["Description"] != "Applications" || meta["owner"] != "platform" {
		t.Fatalf("apps body: %v", body)
	}

	// Second apply: nothing changed in the plan, nothing is called.
	api.calls = nil
	if err := r.applyNamespaces(first, "token"); err != nil || len(api.calls) != 0 {
		t.Fatalf("unchanged namespaces applied again: %v %v", api.calls, err)
	}
}

func TestNamespacesExisting(t *testing.T) {
	plan := strings.Replace(bootstrapPlan, "inventory:", "namespaces:\n  - {name: apps, description: Applications, meta: {owner: platform}}\n  - {name: billing, description: Billing}\ninventory:", 1)
	r, first, api := bootstrapRunner(t, plan)
	// norca-system is not in the plan: its description is not the plan's.
	api.answers["GET /v1/namespace/norca-system"] = "< HTTP/1.1 200 OK\n" +
		`{"Name":"norca-system","Description":"set by hand","Meta":null,"CreateIndex":5,"ModifyIndex":5}`
	// apps is as the plan describes it.
	api.answers["GET /v1/namespace/apps"] = "< HTTP/1.1 200 OK\n" +
		`{"Name":"apps","Description":"Applications","Meta":{"owner":"platform"},"Quota":"","CreateIndex":7,"ModifyIndex":7}`
	// billing differs and has settings the plan does not describe.
	api.answers["GET /v1/namespace/billing"] = "< HTTP/1.1 200 OK\n" +
		`{"Name":"billing","Description":"old","Meta":{"team":"finance"},"Quota":"small",` +
		`"Capabilities":{"EnabledTaskDrivers":["docker"],"DisabledTaskDrivers":null},` +
		`"NodePoolConfiguration":{"Default":"apps","Allowed":null,"Denied":null},` +
		`"Hash":"AAEC","CreateIndex":9,"ModifyIndex":11}`
	if err := r.applyNamespaces(first, "token"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /v1/namespace/norca-system",
		"GET /v1/namespace/apps",
		"GET /v1/namespace/billing", "PUT /v1/namespace/billing?cas=11",
	}
	if got := strings.Join(api.calls, "\n"); got != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", got)
	}
	body := namespaceBody(t, api, "PUT /v1/namespace/billing?cas=11")
	caps, _ := body["Capabilities"].(map[string]any)
	pools, _ := body["NodePoolConfiguration"].(map[string]any)
	if body["Description"] != "Billing" || body["Meta"] != nil || body["Quota"] != "small" || caps == nil || pools["Default"] != "apps" {
		t.Fatalf("billing body: %v", body)
	}
	for _, k := range []string{"Hash", "CreateIndex", "ModifyIndex"} {
		if _, ok := body[k]; ok {
			t.Fatalf("billing body keeps %s: %v", k, body)
		}
	}

	api.calls = nil
	if err := r.applyNamespaces(first, "token"); err != nil || len(api.calls) != 0 {
		t.Fatalf("namespaces looked up again: %v %v", api.calls, err)
	}
}

const casConflict = "< HTTP/1.1 409 Conflict\ncheck-and-set conflict: the namespace changed"

// TestNamespacesConflictRetried: a namespace created or changed by somebody
// else between the read and the check-and-set write is read again, and the
// write that follows keeps what the other writer set.
func TestNamespacesConflictRetried(t *testing.T) {
	plan := strings.Replace(bootstrapPlan, "inventory:", "namespaces:\n  - {name: apps, description: Applications}\ninventory:", 1)
	const other = `"Quota":"small","Capabilities":{"EnabledTaskDrivers":["docker"]},"NodePoolConfiguration":{"Default":"gpu"}`
	for name, tc := range map[string]struct {
		firstGET, secondGET string
		want                []string
		quota, pool         string // in the body of the last write; "" when there is none
	}{
		"created meanwhile": {
			firstGET:  notFound,
			secondGET: `{"Name":"apps","Description":"by hand",` + other + `,"CreateIndex":21,"ModifyIndex":21}`,
			want: []string{
				"GET /v1/namespace/apps", "PUT /v1/namespace/apps?cas=0",
				"GET /v1/namespace/apps", "PUT /v1/namespace/apps?cas=21",
			},
			quota: "small", pool: "gpu",
		},
		"changed meanwhile": {
			firstGET:  `{"Name":"apps","Description":"old","Quota":"","CreateIndex":3,"ModifyIndex":11}`,
			secondGET: `{"Name":"apps","Description":"old",` + other + `,"CreateIndex":3,"ModifyIndex":12}`,
			want: []string{
				"GET /v1/namespace/apps", "PUT /v1/namespace/apps?cas=11",
				"GET /v1/namespace/apps", "PUT /v1/namespace/apps?cas=12",
			},
			quota: "small", pool: "gpu",
		},
		"created meanwhile as the plan describes it": {
			firstGET:  notFound,
			secondGET: `{"Name":"apps","Description":"Applications",` + other + `,"CreateIndex":21,"ModifyIndex":21}`,
			want: []string{
				"GET /v1/namespace/apps", "PUT /v1/namespace/apps?cas=0",
				"GET /v1/namespace/apps",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, first, api := bootstrapRunner(t, plan)
			api.answers["GET /v1/namespace/norca-system"] = "< HTTP/1.1 200 OK\n" +
				`{"Name":"norca-system","Description":"Norca operators","CreateIndex":2,"ModifyIndex":2}`
			firstAnswer := tc.firstGET
			if firstAnswer != notFound {
				firstAnswer = "< HTTP/1.1 200 OK\n" + firstAnswer
			}
			api.queued["GET /v1/namespace/apps"] = []string{firstAnswer}
			api.answers["GET /v1/namespace/apps"] = "< HTTP/1.1 200 OK\n" + tc.secondGET
			api.queued["PUT /v1/namespace/apps"] = []string{casConflict}
			if err := r.applyNamespaces(first, "token"); err != nil {
				t.Fatal(err)
			}
			want := append([]string{"GET /v1/namespace/norca-system"}, tc.want...)
			if got := strings.Join(api.calls, "\n"); got != strings.Join(want, "\n") {
				t.Fatalf("calls:\n%s", got)
			}
			if tc.quota != "" {
				body := namespaceBody(t, api, want[len(want)-1])
				pools, _ := body["NodePoolConfiguration"].(map[string]any)
				caps, _ := body["Capabilities"].(map[string]any)
				if body["Description"] != "Applications" || body["Quota"] != tc.quota || pools["Default"] != tc.pool || caps == nil {
					t.Fatalf("apps body: %v", body)
				}
				if _, ok := body["ModifyIndex"]; ok {
					t.Fatalf("apps body keeps ModifyIndex: %v", body)
				}
			}

			// The namespace was settled: the next apply skips it.
			api.calls = nil
			if err := r.applyNamespaces(first, "token"); err != nil || len(api.calls) != 0 {
				t.Fatalf("namespaces looked up again: %v %v", api.calls, err)
			}
		})
	}
}

// TestNamespacesConflictPersistent: a namespace whose check-and-set write
// keeps conflicting fails the step after a bounded number of rounds, and
// the next apply tries it again.
func TestNamespacesConflictPersistent(t *testing.T) {
	plan := strings.Replace(bootstrapPlan, "inventory:", "namespaces:\n  - {name: apps, description: Applications}\ninventory:", 1)
	r, first, api := bootstrapRunner(t, plan)
	api.answers["GET /v1/namespace/norca-system"] = "< HTTP/1.1 200 OK\n" +
		`{"Name":"norca-system","Description":"Norca operators","CreateIndex":2,"ModifyIndex":2}`
	api.answers["GET /v1/namespace/apps"] = "< HTTP/1.1 200 OK\n" +
		`{"Name":"apps","Description":"old","CreateIndex":3,"ModifyIndex":5}`
	api.answers["PUT /v1/namespace/apps"] = casConflict
	err := r.applyNamespaces(first, "token")
	if err == nil || !strings.Contains(err.Error(), "namespace apps: still changed by another writer after 3 check-and-set attempts") ||
		!strings.Contains(err.Error(), "check-and-set conflict: the namespace changed") {
		t.Fatalf("err %v", err)
	}
	puts := 0
	for _, c := range api.calls {
		if c == "PUT /v1/namespace/apps?cas=5" {
			puts++
		}
	}
	if puts != namespaceAttempts || len(api.calls) != 1+2*namespaceAttempts {
		t.Fatalf("calls %v", api.calls)
	}

	// Nothing was recorded for apps: the next apply looks again.
	delete(api.answers, "PUT /v1/namespace/apps")
	api.calls = nil
	if err := r.applyNamespaces(first, "token"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(api.calls, "\n"); got != "GET /v1/namespace/apps\nPUT /v1/namespace/apps?cas=5" {
		t.Fatalf("calls:\n%s", got)
	}
}

func TestNamespacesSkipped(t *testing.T) {
	for name, tc := range map[string]struct {
		plan, token string
	}{
		"replica region": {strings.Replace(bootstrapPlan, "release:", "federation: {authoritative_region: us, replication_token_file: repl.token}\nrelease:", 1), "token"},
		"no token":       {bootstrapPlan, ""},
	} {
		t.Run(name, func(t *testing.T) {
			r, first, api := bootstrapRunner(t, tc.plan)
			if err := r.applyNamespaces(first, tc.token); err != nil || len(api.calls) != 0 {
				t.Fatalf("calls %v, err %v", api.calls, err)
			}
		})
	}
}

func TestNamespaceAPIError(t *testing.T) {
	r, first, api := bootstrapRunner(t, bootstrapPlan)
	api.answers["GET /v1/namespace/norca-system"] = "< HTTP/1.1 403 Forbidden\nPermission denied"
	err := r.applyNamespaces(first, "token")
	if err == nil || !strings.Contains(err.Error(), "namespace norca-system: HTTP 403") {
		t.Fatalf("err %v", err)
	}
	// Nothing was recorded: the next apply looks again.
	delete(api.answers, "GET /v1/namespace/norca-system")
	api.calls = nil
	if err := r.applyNamespaces(first, "token"); err != nil || len(api.calls) != 1 {
		t.Fatalf("calls %v, err %v", api.calls, err)
	}
}
