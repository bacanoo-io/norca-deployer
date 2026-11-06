package engine

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/crypto"
	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
	"github.com/bacanoo-io/norca-pki"
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

func TestIssueIntermediate(t *testing.T) {
	plan := loadPlan(t, spiffePlan)
	if _, err := IssueIntermediate(plan, IntermediateOptions{Name: "mesh", Years: 5, MaxPathLen: 1}); err == nil ||
		!strings.Contains(err.Error(), "run apply first") {
		t.Fatalf("expected a missing CA error, got %v", err)
	}
	if err := NewRunner(plan, nil).initPKI(); err != nil {
		t.Fatal(err)
	}
	files, err := IssueIntermediate(plan, IntermediateOptions{Name: "mesh", Years: 5, MaxPathLen: 1})
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
	if _, err := IssueIntermediate(plan, IntermediateOptions{Name: "mesh", Years: 5, MaxPathLen: 1}); err == nil {
		t.Fatal("an existing intermediate was replaced without --force")
	}
	if _, err := IssueIntermediate(plan, IntermediateOptions{Name: "Mesh CA", Years: 5}); err == nil {
		t.Fatal("invalid name accepted")
	}
}

func TestIssueNodeCA(t *testing.T) {
	plan := loadPlan(t, spiffePlan)
	if _, err := IssueNodeCA(plan, NodeCAOptions{Years: 5}); err == nil || !strings.Contains(err.Error(), "run apply first") {
		t.Fatalf("expected a missing CA error, got %v", err)
	}
	if err := NewRunner(plan, nil).initPKI(); err != nil {
		t.Fatal(err)
	}
	files, err := IssueNodeCA(plan, NodeCAOptions{}) // the region of the plan, the default validity
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(filepath.ToSlash(files[0]), "certs/node-ca-"+plan.Metadata.Region+".pem") {
		t.Fatalf("files = %v", files)
	}
	chain, _ := os.ReadFile(files[2])
	if strings.Count(string(chain), "BEGIN CERTIFICATE") != 2 {
		t.Fatalf("chain must hold the node CA and the root:\n%s", chain)
	}
	block, _ := pem.Decode(chain)
	node, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if left := time.Until(node.NotAfter); left < (DefaultNodeCADays-1)*24*time.Hour || left > (DefaultNodeCADays+1)*24*time.Hour {
		t.Fatalf("default validity: expires in %v, want %d days", left, DefaultNodeCADays)
	}
	if want := []string{"client." + plan.Metadata.Region + ".norca", "localhost"}; !reflect.DeepEqual(node.PermittedDNSDomains, want) {
		t.Fatalf("permitted DNS = %v, want %v", node.PermittedDNSDomains, want)
	}
	if !reflect.DeepEqual(node.PermittedURIDomains, []string{"prod.example"}) {
		t.Fatalf("permitted URI domains = %v", node.PermittedURIDomains)
	}
	if info, _ := os.Stat(files[1]); info.Mode().Perm()&0o077 != 0 && os.PathSeparator == '/' {
		t.Fatalf("key mode %v", info.Mode())
	}
	if _, err := IssueNodeCA(plan, NodeCAOptions{Years: 5}); err == nil {
		t.Fatal("an existing node CA was replaced without --force")
	}
	if _, err := IssueNodeCA(plan, NodeCAOptions{Years: 5, Force: true}); err != nil {
		t.Fatalf("--force: %v", err)
	}
	if _, err := IssueNodeCA(plan, NodeCAOptions{Region: "us", Years: 5}); err != nil {
		t.Fatalf("another region: %v", err)
	}
	if _, err := IssueNodeCA(plan, NodeCAOptions{Region: "eu west", Years: 5}); err == nil {
		t.Fatal("invalid region accepted")
	}
}

func TestIssueNodeCAValidityOptions(t *testing.T) {
	plan := loadPlan(t, spiffePlan)
	if err := NewRunner(plan, nil).initPKI(); err != nil {
		t.Fatal(err)
	}
	expires := func(files []string) time.Duration {
		t.Helper()
		data, _ := os.ReadFile(files[0])
		block, _ := pem.Decode(data)
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return time.Until(c.NotAfter)
	}

	files, err := IssueNodeCA(plan, NodeCAOptions{Days: 90, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if left := expires(files); left < 89*24*time.Hour || left > 91*24*time.Hour {
		t.Fatalf("--days 90: expires in %v", left)
	}
	files, err = IssueNodeCA(plan, NodeCAOptions{Years: 2, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if left := expires(files); left < 729*24*time.Hour || left > 732*24*time.Hour {
		t.Fatalf("--years 2: expires in %v", left)
	}
	for name, o := range map[string]NodeCAOptions{
		"days and years":    {Days: 30, Years: 1, Force: true},
		"negative days":     {Days: -1, Force: true},
		"beyond cluster CA": {Days: 365 * 100, Force: true},
	} {
		if _, err := IssueNodeCA(plan, o); err == nil {
			t.Errorf("%s was accepted", name)
		}
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

func testVault(t *testing.T) *crypto.Vault {
	t.Helper()
	_, key, err := crypto.GenerateMasterKey()
	if err != nil {
		t.Fatal(err)
	}
	v, err := crypto.NewVault(key)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func caKeyFile(plan *config.DeploymentPlan) string {
	_, key, _ := caPaths(plan)
	return key
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// With a master key the key of the CA the deployer generates is stored
// encrypted, and the next apply loads it and reuses the CA.
func TestCAKeyIsEncryptedWithAMasterKey(t *testing.T) {
	plan := loadPlan(t, spiffePlan)
	vault := testVault(t)
	if err := NewRunner(plan, vault).initPKI(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, caKeyFile(plan)); !strings.HasPrefix(got, crypto.EncryptedPrefix) || strings.Contains(got, "PRIVATE KEY") {
		t.Fatalf("CA key file is not encrypted:\n%s", got)
	}
	certBefore := readFile(t, plan.BaseDir()+"/certs/norca-ca.pem")

	r := NewRunner(plan, vault)
	if err := r.initPKI(); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if r.ca == nil || r.ca.PrivateKey == nil {
		t.Fatal("the encrypted CA was not loaded")
	}
	if readFile(t, plan.BaseDir()+"/certs/norca-ca.pem") != certBefore {
		t.Fatal("a new CA was generated instead of reusing the encrypted one")
	}
}

// Without a master key the key stays in plain text (with a warning); a master
// key that appears later encrypts it in place, and the CA stays the same.
func TestPlainCAKeyIsEncryptedWhenAMasterKeyAppears(t *testing.T) {
	plan := loadPlan(t, spiffePlan)
	if err := NewRunner(plan, nil).initPKI(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, caKeyFile(plan)); !strings.Contains(got, "PRIVATE KEY") {
		t.Fatalf("without a master key the key should be PEM:\n%s", got)
	}
	certBefore := readFile(t, plan.BaseDir()+"/certs/norca-ca.pem")

	vault := testVault(t)
	if err := NewRunner(plan, vault).initPKI(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, caKeyFile(plan)); !strings.HasPrefix(got, crypto.EncryptedPrefix) {
		t.Fatalf("the key was not encrypted once a master key was available:\n%s", got)
	}
	if readFile(t, plan.BaseDir()+"/certs/norca-ca.pem") != certBefore {
		t.Fatal("the CA changed")
	}
	if err := NewRunner(plan, vault).initPKI(); err != nil {
		t.Fatalf("the encrypted key does not load: %v", err)
	}
}

// An encrypted key without the master key is an error that says so, and no
// new CA is generated over it.
func TestEncryptedCAKeyWithoutAMasterKeyFails(t *testing.T) {
	plan := loadPlan(t, spiffePlan)
	if err := NewRunner(plan, testVault(t)).initPKI(); err != nil {
		t.Fatal(err)
	}
	err := NewRunner(plan, nil).initPKI()
	if err == nil || !strings.Contains(err.Error(), "encrypted but no master key") {
		t.Fatalf("err = %v", err)
	}
}

// A key file named in the plan belongs to the user: it is read (and decrypted
// if it is encrypted), never rewritten.
func TestConfiguredCAKeyIsNotRewritten(t *testing.T) {
	plan := loadPlan(t, spiffePlan)
	dir := t.TempDir()
	td, _ := pki.TrustDomain("prod.example")
	ca, err := pki.NewCA("external", 1, td)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca-key.pem")
	if err := os.WriteFile(certPath, ca.CertPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, ca.KeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	plan.Security.PKI.CACertPath, plan.Security.PKI.CAKeyPath = certPath, keyPath

	if err := NewRunner(plan, testVault(t)).initPKI(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, keyPath); got != string(ca.KeyPEM) {
		t.Fatal("the key file named in the plan was rewritten")
	}
}

// pki issue-node-ca and issue-intermediate read the CA key through the vault.
func TestIssueFromAnEncryptedCAKey(t *testing.T) {
	plan := loadPlan(t, spiffePlan)
	vault := testVault(t)
	if err := NewRunner(plan, vault).initPKI(); err != nil {
		t.Fatal(err)
	}
	if _, err := IssueNodeCA(plan, NodeCAOptions{Vault: vault}); err != nil {
		t.Fatalf("with the master key: %v", err)
	}
	if _, err := IssueNodeCA(plan, NodeCAOptions{Force: true}); err == nil || !strings.Contains(err.Error(), "encrypted but no master key") {
		t.Fatalf("without the master key: %v", err)
	}
	if _, err := IssueIntermediate(plan, IntermediateOptions{Name: "mesh", Years: 1, MaxPathLen: 1, Vault: vault}); err != nil {
		t.Fatalf("intermediate with the master key: %v", err)
	}
}
