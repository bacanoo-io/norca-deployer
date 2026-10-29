package pki

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

func parse(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// Norca agents verify "server.<region>.nomad" and "client.<region>.nomad"
// when verify_server_hostname is on.
func TestCertificateNamesMatchNorcaVerification(t *testing.T) {
	ca, err := NewCA("test", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate)

	for name, id := range map[string]Identity{
		"server.global.nomad": ServerIdentity("global", []string{"10.0.0.1"}, nil),
		"client.global.nomad": ClientIdentity("global"),
	} {
		b, err := ca.Issue(id, 3)
		if err != nil {
			t.Fatal(err)
		}
		cert := parse(t, b.CertPEM)
		if _, err := cert.Verify(x509.VerifyOptions{DNSName: name, Roots: pool}); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if len(cert.URIs) != 0 {
			t.Errorf("%s: URI SANs without SPIFFE: %v", name, cert.URIs)
		}
	}
}

func TestSPIFFEIdentities(t *testing.T) {
	td, err := TrustDomain("prod.norca")
	if err != nil {
		t.Fatal(err)
	}
	ca, err := NewCA("test", 1, td)
	if err != nil {
		t.Fatal(err)
	}
	if got := SPIFFEIDs(ca.Certificate); len(got) != 1 || got[0] != "spiffe://prod.norca" {
		t.Fatalf("CA SPIFFE IDs = %v", got)
	}

	sid, err := SPIFFEID(td, "norca", "eu", "server", "norca-server-01")
	if err != nil {
		t.Fatal(err)
	}
	id := ServerIdentity("eu", []string{"10.0.0.1"}, nil).WithSPIFFE(sid)
	b, err := ca.Issue(id, 3)
	if err != nil {
		t.Fatal(err)
	}
	cert := parse(t, b.CertPEM)
	if got := SPIFFEIDs(cert); len(got) != 1 || got[0] != "spiffe://prod.norca/norca/eu/server/norca-server-01" {
		t.Fatalf("leaf SPIFFE IDs = %v", got)
	}

	// Reuse needs exactly the wanted SPIFFE ID: adding, changing or
	// removing it re-issues the certificate.
	if !ca.Reusable(b.CertPEM, id, time.Hour) {
		t.Fatal("unchanged certificate not reusable")
	}
	other, _ := SPIFFEID(td, "norca", "eu", "server", "other")
	if ca.Reusable(b.CertPEM, id.WithSPIFFE(other), time.Hour) || ca.Reusable(b.CertPEM, id.WithSPIFFE(nil), time.Hour) {
		t.Fatal("certificate with another SPIFFE ID reused")
	}

	for _, bad := range []string{"Prod.norca", "a b", "x/y", ""} {
		if _, err := TrustDomain(bad); err == nil {
			t.Errorf("trust domain %q accepted", bad)
		}
	}
	if _, err := SPIFFEID(td, "norca", ".."); err == nil {
		t.Error("path segment '..' accepted")
	}
}

// OpenZiti walks the chain from the leaf up and takes the trust domain from
// the first spiffe:// URI SAN; an intermediate it uses as its signing CA must
// verify against the cluster CA and allow its own sub-CAs.
func TestIntermediateForOpenZiti(t *testing.T) {
	td, _ := TrustDomain("prod.norca")
	root, err := NewCA("test", 1, nil) // a CA from before SPIFFE was enabled
	if err != nil {
		t.Fatal(err)
	}
	b, err := root.IssueIntermediate("test ziti Intermediate CA", 5, 1, td)
	if err != nil {
		t.Fatal(err)
	}
	inter := parse(t, b.CertPEM)
	if !inter.IsCA || inter.MaxPathLen != 1 || inter.MaxPathLenZero {
		t.Fatalf("intermediate constraints: IsCA=%t MaxPathLen=%d", inter.IsCA, inter.MaxPathLen)
	}
	if got := SPIFFEIDs(inter); len(got) != 1 || got[0] != "spiffe://prod.norca" {
		t.Fatalf("intermediate SPIFFE IDs = %v", got)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root.Certificate)
	if _, err := inter.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		t.Fatalf("intermediate does not chain to the root: %v", err)
	}

	zero, err := root.IssueIntermediate("leaf-only", 1, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c := parse(t, zero.CertPEM); c.MaxPathLen != 0 || !c.MaxPathLenZero {
		t.Fatal("path length 0 not encoded")
	}
}
