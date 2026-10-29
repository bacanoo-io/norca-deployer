package engine

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/connector"
	"github.com/bacanoo-io/norca-deployer/pkg/pki"
)

// caPaths returns where the cluster CA is: the configured files, or
// certs/norca-ca.pem next to the plan.
func caPaths(plan *config.DeploymentPlan) (cert, key string, configured bool) {
	if p := plan.Security.PKI; p != nil && p.CACertPath != "" && p.CAKeyPath != "" {
		return plan.ResolveLocalPath(p.CACertPath), plan.ResolveLocalPath(p.CAKeyPath), true
	}
	dir := filepath.Join(plan.BaseDir(), "certs")
	return filepath.Join(dir, "norca-ca.pem"), filepath.Join(dir, "norca-ca-key.pem"), false
}

func (r *Runner) initPKI() error {
	if !r.plan.Security.TLSEnabled {
		return nil
	}
	td, err := r.plan.SPIFFETrustDomain()
	if err != nil {
		return err
	}
	r.trustDomain = td

	certsDir := filepath.Join(r.plan.BaseDir(), "certs")
	caCert, caKey, configured := caPaths(r.plan)

	// A configured CA that cannot be loaded is an error: silently
	// generating a new one would split the cluster.
	if _, statErr := os.Stat(caCert); configured || statErr == nil {
		ca, err := pki.LoadCA(caCert, caKey)
		if err != nil {
			return fmt.Errorf("failed to load the CA (%s): %w", caCert, err)
		}
		r.ca = ca
		fmt.Printf("    Existing CA reused (%s)\n", caCert)
		r.checkCATrustDomain()
	} else {
		ca, err := pki.NewCA(r.plan.Metadata.Name, 10, td)
		if err != nil {
			return fmt.Errorf("failed to generate new CA: %w", err)
		}
		r.ca = ca
		if err := os.MkdirAll(certsDir, 0700); err != nil {
			return fmt.Errorf("failed to create %s: %w", certsDir, err)
		}
		if err := os.WriteFile(caCert, ca.CertPEM, 0644); err != nil {
			return fmt.Errorf("failed to save CA certificate: %w", err)
		}
		if err := os.WriteFile(caKey, ca.KeyPEM, 0600); err != nil {
			return fmt.Errorf("failed to save CA key: %w", err)
		}
		fmt.Printf("    New internal mTLS CA generated (valid for 10 years)\n")
	}

	// Reusable client pool certificate for dynamic nodes, and an operator
	// certificate when the HTTP API requires client certificates.
	if p := r.plan.Security.PKI; p != nil && p.GenerateWorkerPoolCert {
		if err := r.issueLocalClientCert(certsDir, "client-pool", "client-pool"); err != nil {
			return err
		}
	}
	if r.plan.Security.VerifyHTTPSClient {
		if err := r.issueLocalClientCert(certsDir, "cli", "operator"); err != nil {
			return err
		}
	}
	return nil
}

// checkCATrustDomain reports a CA whose SPIFFE ID does not fit the plan. A
// CA without one is fine: the node and intermediate certificates carry the
// trust domain, and SPIFFE consumers (OpenZiti) search the chain from the
// leaf up.
func (r *Runner) checkCATrustDomain() {
	ids := pki.SPIFFEIDs(r.ca.Certificate)
	switch {
	case r.trustDomain == nil || len(ids) == 0:
	case ids[0] != r.trustDomain.String():
		fmt.Printf("    [WARN] The CA carries %s, the plan's trust domain is %s; SPIFFE consumers may pick the CA's\n",
			ids[0], r.trustDomain)
	}
}

// spiffeID returns the SPIFFE ID spiffe://<td>/norca/<region>/<parts...>, or
// nil without SPIFFE.
func (r *Runner) spiffeID(parts ...string) (*url.URL, error) {
	if r.trustDomain == nil {
		return nil, nil
	}
	return pki.SPIFFEID(r.trustDomain, append([]string{"norca", r.plan.Metadata.Region}, parts...)...)
}

// issueLocalClientCert issues a client certificate into certs/ once (again
// when it no longer fits, e.g. after enabling SPIFFE).
func (r *Runner) issueLocalClientCert(dir, name, spiffeName string) error {
	sid, err := r.spiffeID(spiffeName)
	if err != nil {
		return err
	}
	id := pki.ClientIdentity(r.plan.Metadata.Region).WithSPIFFE(sid)
	certPath := filepath.Join(dir, name+".pem")
	if data, err := os.ReadFile(certPath); err == nil && r.ca.Reusable(data, id, certMinValidity) {
		return nil
	}
	bundle, err := r.ca.Issue(id, 3)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name+"-key.pem"), bundle.KeyPEM, 0600); err != nil {
		return err
	}
	if err := os.WriteFile(certPath, bundle.CertPEM, 0644); err != nil {
		return err
	}
	fmt.Printf("    Client certificate generated (%s)\n", certPath)
	return nil
}

// nodeIdentity is the certificate identity of a node.
func (r *Runner) nodeIdentity(t *target) (pki.Identity, error) {
	role := config.RoleClient
	var id pki.Identity
	if t.isMaster {
		role = config.RoleMaster
		ips := []string{t.node.Host}
		if t.node.ClusterAddress != "" {
			ips = append(ips, t.node.ClusterAddress)
		}
		var extraDNS []string
		if p := r.plan.Security.PKI; p != nil {
			ips = append(ips, p.ServerAdditionalIPSANs...)
			extraDNS = p.ServerAdditionalDNSSANs
		}
		id = pki.ServerIdentity(r.plan.Metadata.Region, ips, extraDNS)
	} else {
		id = pki.ClientIdentity(r.plan.Metadata.Region)
	}
	kind := "client"
	if role == config.RoleMaster {
		kind = "server"
	}
	sid, err := r.spiffeID(kind, config.NodeKey(t.node))
	if err != nil {
		return id, err
	}
	return id.WithSPIFFE(sid), nil
}

// provisionCertificates copies the CA and the node certificate. An existing
// certificate is kept while it fits the node's identity (names, SPIFFE ID,
// validity), so that an unchanged node is not restarted.
func (r *Runner) provisionCertificates(t *target) (bool, error) {
	conn, paths := t.conn, t.paths
	changed, err := connector.SyncFile(conn, joinPath(paths, paths.TLSDir, "ca.pem"), r.ca.CertPEM, 0644)
	if err != nil {
		return false, err
	}
	id, err := r.nodeIdentity(t)
	if err != nil {
		return false, err
	}
	certName, keyName := "client.pem", "client-key.pem"
	if t.isMaster {
		certName, keyName = "server.pem", "server-key.pem"
	}
	certPath := joinPath(paths, paths.TLSDir, certName)
	keyPath := joinPath(paths, paths.TLSDir, keyName)

	if current, err := conn.ReadFile(certPath); err == nil && r.ca.Reusable(current, id, certMinValidity) {
		if exists, _ := conn.FileExists(keyPath); exists {
			return changed, nil
		}
	}
	bundle, err := r.ca.Issue(id, 3)
	if err != nil {
		return false, err
	}
	// Key first: the agent must never see a certificate without its key.
	if err := conn.CopyFile(bytes.NewReader(bundle.KeyPEM), keyPath, 0600); err != nil {
		return false, err
	}
	if err := conn.CopyFile(bytes.NewReader(bundle.CertPEM), certPath, 0644); err != nil {
		return false, err
	}
	fmt.Printf("    -> [%s] Certificate %s issued\n", t.node.Host, certName)
	return true, nil
}

var intermediateNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// IntermediateOptions are the settings of `pki issue-intermediate`.
type IntermediateOptions struct {
	Name       string // file prefix and part of the subject, e.g. "ziti"
	Years      int
	MaxPathLen int // CA levels allowed below the intermediate
	Force      bool
}

// IssueIntermediate signs an intermediate CA with the cluster CA, for a PKI
// that must chain up to it (OpenZiti controllers and routers). It carries
// the plan's SPIFFE trust domain. Written to certs/<name>-ca.pem,
// <name>-ca-key.pem (0600) and <name>-ca-chain.pem (intermediate + root).
func IssueIntermediate(plan *config.DeploymentPlan, o IntermediateOptions) ([]string, error) {
	if !intermediateNameRe.MatchString(o.Name) {
		return nil, fmt.Errorf("name %q must be lower case letters, digits and '-'", o.Name)
	}
	if o.Years <= 0 || o.MaxPathLen < 0 {
		return nil, fmt.Errorf("years must be positive and the path length not negative")
	}
	caCert, caKey, _ := caPaths(plan)
	ca, err := pki.LoadCA(caCert, caKey)
	if err != nil {
		return nil, fmt.Errorf("cluster CA (%s): %w (run apply first)", caCert, err)
	}
	td, err := plan.SPIFFETrustDomain()
	if err != nil {
		return nil, err
	}

	dir := filepath.Join(plan.BaseDir(), "certs")
	files := []string{
		filepath.Join(dir, o.Name+"-ca.pem"),
		filepath.Join(dir, o.Name+"-ca-key.pem"),
		filepath.Join(dir, o.Name+"-ca-chain.pem"),
	}
	if !o.Force {
		for _, f := range files {
			if _, err := os.Stat(f); err == nil {
				return nil, fmt.Errorf("%s already exists (use --force to replace the intermediate)", f)
			}
		}
	}
	bundle, err := ca.IssueIntermediate(fmt.Sprintf("%s %s Intermediate CA", plan.Metadata.Name, o.Name), o.Years, o.MaxPathLen, td)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	chain := append(append([]byte{}, bundle.CertPEM...), ca.CertPEM...)
	for i, data := range [][]byte{bundle.CertPEM, bundle.KeyPEM, chain} {
		mode := os.FileMode(0644)
		if i == 1 {
			mode = 0600
		}
		if err := os.WriteFile(files[i], data, mode); err != nil {
			return nil, err
		}
	}
	return files, nil
}
