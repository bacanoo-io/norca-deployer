package engine

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/connector"
	"github.com/bacanoo-io/norca-deployer/pkg/pki"
	"github.com/bacanoo-io/norca-deployer/pkg/platform/storage"
)

func loadPlan(t *testing.T, yaml string) *config.DeploymentPlan {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0600); err != nil {
		t.Fatal(err)
	}
	plan, err := config.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

const spiffePlan = `
metadata: {name: prod, region: eu}
release: {version: v0.2.0}
security:
  tls_enabled: true
  pki:
    spiffe: {enabled: true, trust_domain: prod.example}
inventory:
  masters:
    - host: 10.0.0.1
      hostname: s1
  clients:
    - host: 10.0.0.2
      hostname: w1
`

func certSPIFFE(t *testing.T, data []byte) []string {
	t.Helper()
	block, _ := pem.Decode(data)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return pki.SPIFFEIDs(cert)
}

func TestNodeCertificatesCarrySPIFFEIDs(t *testing.T) {
	plan := loadPlan(t, spiffePlan)
	r := NewRunner(plan, nil)
	if err := r.initPKI(); err != nil {
		t.Fatal(err)
	}
	if got := pki.SPIFFEIDs(r.ca.Certificate); len(got) != 1 || got[0] != "spiffe://prod.example" {
		t.Fatalf("new CA SPIFFE IDs = %v", got)
	}

	for i, want := range map[int]string{0: "spiffe://prod.example/norca/eu/server/s1", 1: "spiffe://prod.example/norca/eu/client/w1"} {
		var node *config.NodeConfig
		isMaster := i == 0
		if isMaster {
			node = &plan.Inventory.Masters[0]
		} else {
			node = &plan.Inventory.Clients[0]
		}
		conn := connector.NewFakeConnector()
		tg := &target{node: node, isMaster: isMaster, paths: storage.ResolveNodePaths(node, plan), conn: conn}
		changed, err := r.provisionCertificates(tg)
		if err != nil || !changed {
			t.Fatalf("first issue: changed=%t err=%v", changed, err)
		}
		name := "client.pem"
		if isMaster {
			name = "server.pem"
		}
		if got := certSPIFFE(t, conn.Files["/etc/norca.d/tls/"+name]); len(got) != 1 || got[0] != want {
			t.Fatalf("%s SPIFFE IDs = %v, want %s", name, got, want)
		}
		// Unchanged: the certificate is reused, the agent not restarted.
		if changed, err := r.provisionCertificates(tg); err != nil || changed {
			t.Fatalf("second run: changed=%t err=%v", changed, err)
		}
		// Another trust domain re-issues it.
		r.trustDomain, _ = pki.TrustDomain("other.example")
		if changed, _ := r.provisionCertificates(tg); !changed {
			t.Fatal("trust domain change did not re-issue the certificate")
		}
		r.trustDomain, _ = pki.TrustDomain("prod.example")
	}
}

func TestIssueIntermediateForZiti(t *testing.T) {
	plan := loadPlan(t, spiffePlan)
	if _, err := IssueIntermediate(plan, IntermediateOptions{Name: "ziti", Years: 5, MaxPathLen: 1}); err == nil ||
		!strings.Contains(err.Error(), "run apply first") {
		t.Fatalf("expected a missing CA error, got %v", err)
	}
	if err := NewRunner(plan, nil).initPKI(); err != nil {
		t.Fatal(err)
	}
	files, err := IssueIntermediate(plan, IntermediateOptions{Name: "ziti", Years: 5, MaxPathLen: 1})
	if err != nil {
		t.Fatal(err)
	}
	chain, _ := os.ReadFile(files[2])
	if strings.Count(string(chain), "BEGIN CERTIFICATE") != 2 {
		t.Fatalf("chain must hold the intermediate and the root:\n%s", chain)
	}
	inter, _ := os.ReadFile(files[0])
	if got := certSPIFFE(t, inter); len(got) != 1 || got[0] != "spiffe://prod.example" {
		t.Fatalf("intermediate SPIFFE IDs = %v", got)
	}
	if info, _ := os.Stat(files[1]); info.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("key mode %v", info.Mode())
	}
	if _, err := IssueIntermediate(plan, IntermediateOptions{Name: "ziti", Years: 5, MaxPathLen: 1}); err == nil {
		t.Fatal("an existing intermediate was replaced without --force")
	}
	if _, err := IssueIntermediate(plan, IntermediateOptions{Name: "Ziti CA", Years: 5}); err == nil {
		t.Fatal("invalid name accepted")
	}
}

func loadPlanFile(t *testing.T, path string) *config.DeploymentPlan {
	t.Helper()
	plan, err := config.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
