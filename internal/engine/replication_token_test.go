package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// authoritativePlan is bootstrapPlan as the authoritative region of a
// federation.
var authoritativePlan = strings.Replace(bootstrapPlan, "release:", "federation: {authoritative_region: eu}\nrelease:", 1)

// aclCLI answers the acl commands of the first server: bootstrap (with the
// secret it reads from the body file, or a token of its own) and token self.
type aclCLI struct {
	conn          *connector.FakeConnector
	bootstrapped  string // the management token once bootstrapped
	bootstrapArgs []string
}

func newACLCLI() *aclCLI {
	a := &aclCLI{}
	a.conn = connector.NewFakeConnector()
	a.conn.Handler = func(cmd string) (string, error, bool) {
		switch {
		case strings.Contains(cmd, "'acl' 'bootstrap'"):
			a.bootstrapArgs = append(a.bootstrapArgs, cmd)
			if a.bootstrapped != "" {
				return "Error bootstrapping: Unexpected response code: 400 (ACL bootstrap already done (reset index: 7))", errors.New("exit 1"), true
			}
			a.bootstrapped = "00000000-0000-4000-8000-000000000001"
			if strings.Contains(cmd, "'-json' '/var/lib/norca-deployer/cli.body'") {
				a.bootstrapped = strings.TrimSpace(string(a.conn.Files["/var/lib/norca-deployer/cli.body"]))
			}
			return fmt.Sprintf(`{"AccessorID": "a", "SecretID": %q}`, a.bootstrapped), nil, true
		case strings.Contains(cmd, "'acl' 'token' 'self'"):
			env := string(a.conn.Files["/var/lib/norca-deployer/cli.env"])
			if a.bootstrapped != "" && strings.Contains(env, "NORCA_TOKEN='"+a.bootstrapped+"'") {
				return `{"Type": "management"}`, nil, true
			}
			return "Error: ACL token not found", errors.New("exit 1"), true
		}
		return "", nil, true
	}
	return a
}

func TestReplicationTokenAuthoritativeFirstApply(t *testing.T) {
	r, first, _ := bootstrapRunner(t, authoritativePlan)
	cli := newACLCLI()
	first.conn = cli.conn

	if err := r.resolveReplicationToken(); err != nil {
		t.Fatal(err)
	}
	secret := r.replicationToken
	if !uuidRe.MatchString(secret) || r.bootstrapSecret != secret {
		t.Fatalf("replication token %q, bootstrap secret %q", secret, r.bootstrapSecret)
	}
	tokenPath := r.plan.ResolveLocalPath(r.plan.Security.ACL.ManagementTokenOutput)
	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Fatalf("management token stored before the bootstrap: %v", err)
	}

	// The servers start with the chosen secret as their replication token.
	hcl := GenerateNorcaConfig(first.node, r.plan, first.paths, true, GenOptions{ReplicationToken: r.replicationToken})
	if !strings.Contains(hcl, fmt.Sprintf("replication_token = %q", secret)) {
		t.Fatalf("no replication token in the server config:\n%s", hcl)
	}

	// The bootstrap makes the secret the management token.
	token, err := r.aclBootstrap(first)
	if err != nil {
		t.Fatal(err)
	}
	if token != secret || cli.bootstrapped != secret {
		t.Fatalf("token %q, bootstrapped %q, want %q", token, cli.bootstrapped, secret)
	}
	if stored, ok, err := r.store.ReadSecret(tokenPath); err != nil || !ok || stored != secret {
		t.Fatalf("stored %q %v %v", stored, ok, err)
	}
	if _, err := os.Stat(tokenPath + ".pending"); !os.IsNotExist(err) {
		t.Fatalf("pending secret left: %v", err)
	}

	// The next apply reads the stored token and bootstraps nothing.
	r2, first2, _ := bootstrapRunner(t, authoritativePlan)
	r2.plan = r.plan
	r2.store = r.store
	first2.conn = cli.conn
	if err := r2.resolveReplicationToken(); err != nil {
		t.Fatal(err)
	}
	if r2.replicationToken != secret || r2.bootstrapSecret != "" {
		t.Fatalf("second apply: replication token %q, bootstrap secret %q", r2.replicationToken, r2.bootstrapSecret)
	}
	if token, err := r2.aclBootstrap(first2); err != nil || token != secret || len(cli.bootstrapArgs) != 1 {
		t.Fatalf("token %q, err %v, bootstraps %d", token, err, len(cli.bootstrapArgs))
	}
}

// An apply that stopped after the bootstrap, before it stored the token,
// finds the pending secret again and stores it once the cluster accepts it.
func TestReplicationTokenBootstrapDoneBeforeStored(t *testing.T) {
	r, first, _ := bootstrapRunner(t, authoritativePlan)
	cli := newACLCLI()
	first.conn = cli.conn
	if err := r.resolveReplicationToken(); err != nil {
		t.Fatal(err)
	}
	secret := r.replicationToken
	cli.bootstrapped = secret // bootstrapped by the earlier apply

	r2, first2, _ := bootstrapRunner(t, authoritativePlan)
	r2.plan, r2.store = r.plan, r.store
	first2.conn = cli.conn
	if err := r2.resolveReplicationToken(); err != nil {
		t.Fatal(err)
	}
	if r2.replicationToken != secret {
		t.Fatalf("pending secret not reused: %q, want %q", r2.replicationToken, secret)
	}
	token, err := r2.aclBootstrap(first2)
	if err != nil || token != secret {
		t.Fatalf("token %q, err %v", token, err)
	}
	tokenPath := r.plan.ResolveLocalPath(r.plan.Security.ACL.ManagementTokenOutput)
	if stored, ok, _ := r.store.ReadSecret(tokenPath); !ok || stored != secret {
		t.Fatalf("stored %q", stored)
	}
}

// Bootstrapped by someone else: the chosen secret is not the management
// token, nothing is stored.
func TestReplicationTokenBootstrappedByOthers(t *testing.T) {
	r, first, _ := bootstrapRunner(t, authoritativePlan)
	cli := newACLCLI()
	cli.bootstrapped = "00000000-0000-4000-8000-000000000099"
	first.conn = cli.conn
	if err := r.resolveReplicationToken(); err != nil {
		t.Fatal(err)
	}
	token, err := r.aclBootstrap(first)
	if err != nil || token != "" {
		t.Fatalf("token %q, err %v", token, err)
	}
	tokenPath := r.plan.ResolveLocalPath(r.plan.Security.ACL.ManagementTokenOutput)
	if _, ok, _ := r.store.ReadSecret(tokenPath); ok {
		t.Fatal("a token was stored")
	}
}

func TestReplicationTokenOtherPlans(t *testing.T) {
	for name, tc := range map[string]struct {
		plan         string
		replicaToken bool // the replica region's token file exists
		wantToken    string
	}{
		"single region": {plan: bootstrapPlan},
		"replica region": {
			plan:         strings.Replace(bootstrapPlan, "release:", "federation: {authoritative_region: us, replication_token_file: repl.token}\nrelease:", 1),
			replicaToken: true,
			wantToken:    "repl-secret",
		},
		"authoritative without auto bootstrap": {
			plan: strings.Replace(authoritativePlan, "auto_bootstrap: true", "auto_bootstrap: false", 1),
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, _, _ := bootstrapRunner(t, tc.plan)
			if tc.replicaToken {
				if err := os.WriteFile(filepath.Join(r.plan.BaseDir(), "repl.token"), []byte("repl-secret\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.resolveReplicationToken(); err != nil {
				t.Fatal(err)
			}
			if r.replicationToken != tc.wantToken || r.bootstrapSecret != "" {
				t.Fatalf("replication token %q, bootstrap secret %q", r.replicationToken, r.bootstrapSecret)
			}
		})
	}
}
