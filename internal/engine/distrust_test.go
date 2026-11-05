package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
	"github.com/bacanoo-io/norca-pki"
)

const distrustPlan = `
metadata: {name: prod, region: eu}
release: {version: v0.2.0}
security:
  tls_enabled: true
  pki:
    distrusted_ca_files: [certs/lost-a.pem, certs/lost-b.pem]
inventory:
  masters:
    - host: 10.0.0.1
      hostname: s1
  clients:
    - host: 10.0.0.2
      hostname: w1
`

// writeCA writes a new CA certificate to the file.
func writeCA(t *testing.T, path, name string) []byte {
	t.Helper()
	ca, err := pki.NewCA(name, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, ca.CertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return ca.CertPEM
}

func TestDistrustedCAs(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, "plan.yaml")
	if err := os.WriteFile(planPath, []byte(distrustPlan), 0o600); err != nil {
		t.Fatal(err)
	}
	a := writeCA(t, filepath.Join(dir, "certs", "lost-a.pem"), "a")
	b := writeCA(t, filepath.Join(dir, "certs", "lost-b.pem"), "b")
	plan := loadPlanFile(t, planPath)

	// The servers get the file; a client does not.
	server := &plan.Inventory.Masters[0]
	cfg := GenerateNorcaConfig(server, plan, storage.ResolveNodePaths(server, plan), true, GenOptions{})
	if !strings.Contains(cfg, `distrusted_ca_file = "`) || !strings.Contains(cfg, "distrusted-ca.pem") {
		t.Fatalf("the server has no distrusted CA file:\n%s", cfg)
	}
	client := &plan.Inventory.Clients[0]
	if cfg := GenerateNorcaConfig(client, plan, storage.ResolveNodePaths(client, plan), false, GenOptions{}); strings.Contains(cfg, "distrusted") {
		t.Fatalf("a client has a distrusted CA file:\n%s", cfg)
	}

	// The files are joined.
	bundle, err := distrustedBundle(plan)
	if err != nil {
		t.Fatal(err)
	}
	if string(bundle) != string(a)+string(b) {
		t.Fatalf("bundle:\n%s", bundle)
	}

	// A file without a certificate, or a missing one, is an error.
	if err := os.WriteFile(filepath.Join(dir, "certs", "lost-b.pem"), []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := distrustedBundle(plan); err == nil || !strings.Contains(err.Error(), "no certificate") {
		t.Fatalf("want no certificate, got %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "certs", "lost-b.pem")); err != nil {
		t.Fatal(err)
	}
	if _, err := distrustedBundle(plan); err == nil {
		t.Fatal("a missing file is accepted")
	}

	// Without TLS there is nothing to distrust.
	plan.Security.TLSEnabled = false
	if plan.HasDistrustedCAs() {
		t.Fatal("distrusted CAs without TLS")
	}
}
