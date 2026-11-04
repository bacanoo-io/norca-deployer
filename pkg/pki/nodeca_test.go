package pki

import (
	"crypto/x509"
	"encoding/pem"
	"net"
	"net/url"
	"testing"
	"time"
)

const day = 24 * time.Hour

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// nodeCAOf turns the bundle of IssueNodeCA into a signer, like the operator
// does with the files of `pki issue-node-ca`.
func nodeCAOf(t *testing.T, b *CertificateBundle) *CertificateAuthority {
	t.Helper()
	block, _ := pem.Decode(b.KeyPEM)
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return &CertificateAuthority{Certificate: parse(t, b.CertPEM), PrivateKey: key, CertPEM: b.CertPEM, KeyPEM: b.KeyPEM}
}

// The node CA signs what a node certificate carries today (ClientIdentity)
// and nothing else: the agents verify the chain the node sends (leaf and
// intermediate in one file) against the cluster CA.
func TestNodeCASignsOnlyNodeCertificates(t *testing.T) {
	td, _ := TrustDomain("prod.norca")
	root, err := NewCA("test", 1, td)
	if err != nil {
		t.Fatal(err)
	}
	b, err := root.IssueNodeCA("test eu Node CA", "eu", 180*day, td)
	if err != nil {
		t.Fatal(err)
	}
	inter := nodeCAOf(t, b)
	if !inter.Certificate.IsCA || inter.Certificate.MaxPathLen != 0 || !inter.Certificate.MaxPathLenZero {
		t.Fatalf("node CA must sign leaves only: IsCA=%t MaxPathLen=%d", inter.Certificate.IsCA, inter.Certificate.MaxPathLen)
	}

	roots := x509.NewCertPool()
	roots.AddCert(root.Certificate)
	ints := x509.NewCertPool()
	ints.AddCert(inter.Certificate)
	verify := func(c *x509.Certificate, withChain bool, eku x509.ExtKeyUsage) error {
		o := x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{eku}}
		if withChain {
			o.Intermediates = ints
		}
		_, err := c.Verify(o)
		return err
	}
	issue := func(id Identity) *x509.Certificate {
		t.Helper()
		l, err := inter.Issue(id, 1)
		if err != nil {
			t.Fatal(err)
		}
		return parse(t, l.CertPEM)
	}

	nodeID, _ := SPIFFEID(td, "eu", "node-7")
	good := issue(ClientIdentity("eu").WithSPIFFE(nodeID))
	for _, eku := range []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth} {
		if err := verify(good, true, eku); err != nil {
			t.Fatalf("node certificate with its chain (usage %v): %v", eku, err)
		}
	}
	if err := verify(issue(ClientIdentity("eu")), true, x509.ExtKeyUsageClientAuth); err != nil {
		t.Fatalf("node certificate without SPIFFE: %v", err)
	}
	if err := verify(good, false, x509.ExtKeyUsageClientAuth); err == nil {
		t.Fatal("a leaf without the intermediate must not verify: the cert file of a node holds both")
	}

	bad := map[string]Identity{
		"server certificate":      ServerIdentity("eu", []string{"10.0.0.1"}, nil),
		"other region":            ClientIdentity("us"),
		"foreign name":            {CN: "client.eu.norca", DNS: []string{"client.eu.norca", "evilclient.eu.norca"}},
		"foreign address":         {CN: "client.eu.norca", DNS: []string{"client.eu.norca"}, IPs: []net.IP{net.ParseIP("10.0.0.5")}},
		"foreign trust domain":    ClientIdentity("eu").WithSPIFFE(mustURL(t, "spiffe://other.domain/node")),
		"the server DNS wildcard": {CN: "client.eu.norca", DNS: []string{"*.eu.norca"}},
	}
	for name, id := range bad {
		if err := verify(issue(id), true, x509.ExtKeyUsageClientAuth); err == nil {
			t.Errorf("%s: a node CA must not be able to issue it", name)
		}
	}
}

func TestNodeCAWithoutSPIFFEAllowsNoURI(t *testing.T) {
	root, _ := NewCA("test", 1, nil)
	b, err := root.IssueNodeCA("test eu Node CA", "eu", 180*day, nil)
	if err != nil {
		t.Fatal(err)
	}
	inter := nodeCAOf(t, b)
	roots := x509.NewCertPool()
	roots.AddCert(root.Certificate)
	ints := x509.NewCertPool()
	ints.AddCert(inter.Certificate)
	opts := x509.VerifyOptions{Roots: roots, Intermediates: ints, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}

	l, _ := inter.Issue(ClientIdentity("eu"), 1)
	if _, err := parse(t, l.CertPEM).Verify(opts); err != nil {
		t.Fatalf("node certificate: %v", err)
	}
	l, _ = inter.Issue(ClientIdentity("eu").WithSPIFFE(mustURL(t, "spiffe://any.domain/node")), 1)
	if _, err := parse(t, l.CertPEM).Verify(opts); err == nil {
		t.Fatal("a cluster without SPIFFE must not accept a URI SAN from the node CA")
	}
}

func TestNodeCARegionIsChecked(t *testing.T) {
	root, _ := NewCA("test", 1, nil)
	for _, region := range []string{"", "eu west", "eu/../x", "-eu"} {
		if _, err := root.IssueNodeCA("x", region, 30*day, nil); err == nil {
			t.Errorf("region %q accepted", region)
		}
	}
}

// The validity is the only bound on a stolen node CA key (no revocation), so
// it is exact, in days, and never reaches beyond the cluster CA.
func TestNodeCAValidity(t *testing.T) {
	root, _ := NewCA("test", 1, nil) // valid for one year

	b, err := root.IssueNodeCA("x", "eu", 90*day, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := parse(t, b.CertPEM)
	if got := time.Until(c.NotAfter); got < 89*day || got > 91*day {
		t.Fatalf("node CA expires in %v, want about 90 days", got)
	}

	for name, validity := range map[string]time.Duration{
		"zero":                  0,
		"negative":              -day,
		"beyond the cluster CA": 400 * day,
	} {
		if _, err := root.IssueNodeCA("x", "eu", validity, nil); err == nil {
			t.Errorf("%s: a node CA with this validity was issued", name)
		}
	}
}
