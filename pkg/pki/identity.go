package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Identity is the subject and the SANs of a certificate.
type Identity struct {
	CN  string
	DNS []string
	IPs []net.IP
	// SPIFFE is the SPIFFE ID (URI SAN); nil for none. A certificate carries
	// at most one, as SPIFFE and OpenZiti require.
	SPIFFE *url.URL
}

// ServerIdentity returns the identity of a server certificate.
//
// Norca keeps Nomad's TLS naming: with verify_server_hostname the agents
// expect "server.<region>.nomad" (helper/tlsutil/config.go, nomad/rpc.go) and
// "client.<region>.nomad" for clients (nomad/util.go).
func ServerIdentity(region string, ips []string, extraDNS []string) Identity {
	cn := fmt.Sprintf("server.%s.nomad", region)
	dnsSANs := []string{
		cn,
		fmt.Sprintf("server.%s.norca", region),
		fmt.Sprintf("*.server.%s.norca", region),
		fmt.Sprintf("*.%s.norca", region),
		"localhost",
		"*.norca.internal",
	}
	dnsSANs = append(dnsSANs, extraDNS...)
	return Identity{CN: cn, DNS: dnsSANs, IPs: parseIPs(append([]string{"127.0.0.1"}, ips...))}
}

// ClientIdentity returns the identity of a client certificate. It is
// IP-agnostic; the loopback names let the local CLI reach the client agent's
// HTTPS API with hostname verification.
func ClientIdentity(region string) Identity {
	cn := fmt.Sprintf("client.%s.nomad", region)
	dnsSANs := []string{
		cn,
		fmt.Sprintf("client.%s.norca", region),
		fmt.Sprintf("*.client.%s.norca", region),
		"localhost",
	}
	return Identity{CN: cn, DNS: dnsSANs, IPs: parseIPs([]string{"127.0.0.1"})}
}

// WithSPIFFE returns the identity with a SPIFFE ID.
func (id Identity) WithSPIFFE(u *url.URL) Identity {
	id.SPIFFE = u
	return id
}

var (
	spiffeSegmentRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	trustDomainRe   = regexp.MustCompile(`^[a-z0-9._-]+$`)
)

// TrustDomain returns spiffe://<domain> after checking the SPIFFE rules:
// lower case letters, digits, '.', '-' and '_'.
func TrustDomain(domain string) (*url.URL, error) {
	if !trustDomainRe.MatchString(domain) {
		return nil, fmt.Errorf("SPIFFE trust domain %q may only contain lower case letters, digits, '.', '-' and '_'", domain)
	}
	return &url.URL{Scheme: "spiffe", Host: domain}, nil
}

// SPIFFEID returns spiffe://<domain>/<segments...>.
func SPIFFEID(trustDomain *url.URL, segments ...string) (*url.URL, error) {
	for _, s := range segments {
		if !spiffeSegmentRe.MatchString(s) || s == "." || s == ".." {
			return nil, fmt.Errorf("SPIFFE path segment %q may only contain letters, digits, '.', '-' and '_'", s)
		}
	}
	return &url.URL{Scheme: "spiffe", Host: trustDomain.Host, Path: "/" + strings.Join(segments, "/")}, nil
}

// SPIFFEIDs returns the spiffe:// URI SANs of a certificate.
func SPIFFEIDs(cert *x509.Certificate) []string {
	var res []string
	for _, u := range cert.URIs {
		if u.Scheme == "spiffe" {
			res = append(res, u.String())
		}
	}
	return res
}

// Issue signs a leaf certificate (server and client auth) for the identity.
func (ca *CertificateAuthority) Issue(id Identity, validYears int) (*CertificateBundle, error) {
	template := &x509.Certificate{
		Subject:     pkix.Name{CommonName: id.CN},
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:    id.DNS,
		IPAddresses: id.IPs,
	}
	if id.SPIFFE != nil {
		template.URIs = []*url.URL{id.SPIFFE}
	}
	return ca.sign(template, validYears)
}

// IssueIntermediate signs an intermediate CA, e.g. for the PKI of OpenZiti:
// its controllers and routers then chain up to the cluster CA. maxPathLen is
// the number of CA levels allowed below it; the trust domain, if any, is its
// URI SAN, where OpenZiti looks for it.
func (ca *CertificateAuthority) IssueIntermediate(cn string, validYears, maxPathLen int, trustDomain *url.URL) (*CertificateBundle, error) {
	template := &x509.Certificate{
		Subject:        pkix.Name{CommonName: cn, Organization: ca.Certificate.Subject.Organization},
		IsCA:           true,
		KeyUsage:       x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		MaxPathLen:     maxPathLen,
		MaxPathLenZero: maxPathLen == 0,
	}
	if trustDomain != nil {
		template.URIs = []*url.URL{trustDomain}
	}
	return ca.sign(template, validYears)
}

func (ca *CertificateAuthority) sign(template *x509.Certificate, validYears int) (*CertificateBundle, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	if template.SerialNumber, err = rand.Int(rand.Reader, serialNumberLimit); err != nil {
		return nil, err
	}
	template.NotBefore = time.Now().Add(-1 * time.Hour)
	template.NotAfter = time.Now().AddDate(validYears, 0, 0)
	template.BasicConstraintsValid = true

	derBytes, err := x509.CreateCertificate(rand.Reader, template, ca.Certificate, &priv.PublicKey, ca.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("failed to sign certificate: %w", err)
	}
	keyBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	return &CertificateBundle{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}),
		CAPEM:   ca.CertPEM,
	}, nil
}

func parseIPs(raw []string) []net.IP {
	var res []net.IP
	seen := map[string]bool{}
	for _, r := range raw {
		if ip := net.ParseIP(r); ip != nil && !seen[ip.String()] {
			seen[ip.String()] = true
			res = append(res, ip)
		}
	}
	return res
}

// Reusable reports whether an existing certificate was issued by this CA,
// carries at least the wanted names and exactly the wanted SPIFFE ID, and
// stays valid for minValidity. Reusing it keeps apply idempotent: an
// unchanged certificate does not restart the agent.
func (ca *CertificateAuthority) Reusable(certPEM []byte, id Identity, minValidity time.Duration) bool {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   id.CN,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return false
	}
	if time.Until(cert.NotAfter) < minValidity {
		return false
	}
	have := map[string]bool{}
	for _, d := range cert.DNSNames {
		have[d] = true
	}
	for _, d := range id.DNS {
		if !have[d] {
			return false
		}
	}
	haveIP := map[string]bool{}
	for _, ip := range cert.IPAddresses {
		haveIP[ip.String()] = true
	}
	for _, ip := range id.IPs {
		if !haveIP[ip.String()] {
			return false
		}
	}
	want := ""
	if id.SPIFFE != nil {
		want = id.SPIFFE.String()
	}
	return strings.Join(SPIFFEIDs(cert), " ") == want
}
