package gitops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

const (
	// maxUploadBytes is the largest pack upload of the servers.
	maxUploadBytes = 8 << 20

	// deployTimeout is how long the servers wait for the deployments.
	deployTimeout = 10 * time.Minute
)

// APIClient calls the HTTP API of a Norca cluster.
type APIClient interface {
	API(ctx context.Context, method, path string, body []byte) (int, string, error)
}

// HTTPClient is an APIClient over HTTP.
type HTTPClient struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// NewHTTPClient returns a client of a cluster. A request is bound by its
// context: deployments are waited for.
func NewHTTPClient(baseURL, token, caPEM string, insecure bool) (*HTTPClient, error) {
	c := &HTTPClient{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{}}
	if caPEM != "" || insecure {
		cfg := &tls.Config{InsecureSkipVerify: insecure} //nolint:gosec // G402: --norca-insecure-skip-tls-verify is the user's explicit choice
		if caPEM != "" {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM([]byte(caPEM)) {
				return nil, errors.New("the CA certificate of the cluster has no certificates")
			}
			cfg.RootCAs = pool
		}
		c.HTTP.Transport = &http.Transport{Proxy: http.ProxyFromEnvironment, TLSClientConfig: cfg}
	}
	return c, nil
}

// API sends a request to the cluster.
func (c *HTTPClient) API(ctx context.Context, method, path string, body []byte) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, "", err
	}
	if c.Token != "" {
		req.Header.Set("X-Nomad-Token", c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String(), nil
}

// Release says where a fetched pack goes.
type Release struct {
	Name        string
	Namespace   string
	Region      string
	Description string
	Values      map[string]any
	Atomic      bool
	MaxHistory  int
}

// ReleaseOf returns the release of a binding, for a fetched pack.
func ReleaseOf(b *config.GitOpsBinding, values map[string]any, res *FetchResult) *Release {
	return &Release{
		Name:        b.ReleaseName,
		Namespace:   b.Namespace,
		Region:      b.Region,
		Values:      values,
		Atomic:      b.Atomic,
		MaxHistory:  b.MaxHistory,
		Description: Provenance(b.EffectiveProvider(), b.EffectiveRepo().URL, res),
	}
}

// Provenance describes the source of a release. A release has no metadata of
// its own for it, so it is kept in the description of the revision.
func Provenance(provider, repoURL string, res *FetchResult) string {
	d := fmt.Sprintf("GitOps %s %s (%s)", provider, SanitizeURL(repoURL), res.Branch)
	if res.CommitSHA != "" && res.CommitSHA != res.Branch {
		d += " commit " + res.CommitSHA
	}
	return d
}

// UploadRegistry returns the cluster registry of the uploads of a release: each
// has its own, so that the old uploads of a pack are known and two repositories
// can have packs of the same name and version.
func UploadRegistry(namespace, release string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + release))
	return "gitops-" + hex.EncodeToString(sum[:8])
}

// Deploy sends a fetched pack to the cluster: the servers render packs from
// registries, so it is uploaded into a registry of the type "uploads", which
// the upload creates, and deployed from there. The leader keeps the archive in
// Raft, and the token needs the rights of a pack upload and of a deployment.
func Deploy(ctx context.Context, c APIClient, res *FetchResult, rel *Release) error {
	if res.PackName == "" || res.PackVersion == "" {
		return errors.New("metadata.hcl of the pack needs a name and a version in its pack block")
	}
	archive, err := packArchive(res.PackFiles)
	if err != nil {
		return err
	}
	if len(archive) > maxUploadBytes {
		return fmt.Errorf("the pack archive has %d bytes, the servers take %d", len(archive), maxUploadBytes)
	}

	registry := UploadRegistry(rel.Namespace, rel.Name)
	regional := url.Values{}
	if rel.Region != "" {
		regional.Set("region", rel.Region)
	}

	q := url.Values{"registry": {registry}, "filename": {res.PackName + ".tar.gz"}}
	if rel.Region != "" {
		q.Set("region", rel.Region)
	}
	status, out, err := c.API(ctx, http.MethodPut, "/v1/pack-upload?"+q.Encode(), archive)
	if err != nil {
		return fmt.Errorf("pack upload: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("pack upload: HTTP %d: %s", status, out)
	}
	var uploaded struct {
		Digest   string
		Registry struct {
			Status struct {
				Uploads []struct {
					Digest string `json:"digest"`
				} `json:"uploads"`
			}
		}
	}
	if err := json.Unmarshal([]byte(out), &uploaded); err != nil {
		return fmt.Errorf("pack upload: %w", err)
	}

	// Only the last upload stays: a later upload of a version replaces an
	// earlier one in the order of their times, to the second.
	for _, u := range uploaded.Registry.Status.Uploads {
		if u.Digest == "" || u.Digest == uploaded.Digest {
			continue
		}
		p := "/v1/pack-upload/" + url.PathEscape(registry) + "/" + url.PathEscape(u.Digest)
		if len(regional) > 0 {
			p += "?" + regional.Encode()
		}
		status, out, err := c.API(ctx, http.MethodDelete, p, nil)
		if err != nil {
			return fmt.Errorf("removing the old upload: %w", err)
		}
		if status != http.StatusOK && status != http.StatusNotFound {
			return fmt.Errorf("removing the old upload: HTTP %d: %s", status, out)
		}
	}

	req := map[string]any{
		"Registry":    registry,
		"Pack":        res.PackName,
		"Version":     res.PackVersion,
		"Values":      rel.Values,
		"Wait":        true,
		"Timeout":     int64(deployTimeout),
		"Description": rel.Description,
	}
	// Without Atomic the namespace decides.
	if rel.Atomic {
		req["Atomic"] = true
	}
	if rel.MaxHistory > 0 {
		req["MaxHistory"] = rel.MaxHistory
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}

	if rel.Namespace != "" {
		regional.Set("namespace", rel.Namespace)
	}
	p := "/v1/release/" + url.PathEscape(rel.Name) + "/deploy-pack"
	if len(regional) > 0 {
		p += "?" + regional.Encode()
	}
	// The servers wait for the deployments.
	ctx, cancel := context.WithTimeout(ctx, deployTimeout+time.Minute)
	defer cancel()
	status, out, err = c.API(ctx, http.MethodPost, p, body)
	if err != nil {
		return fmt.Errorf("deploy: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("deploy: HTTP %d: %s", status, out)
	}
	return nil
}

// packArchive builds a tar.gz of a pack with the pack at its root. The same
// files give the same archive, which the servers store once.
func packArchive(files map[string][]byte) ([]byte, error) {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(files[name]))}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(files[name]); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// FetchOptionsFor returns the fetch options of a binding whose secrets are
// resolved.
func FetchOptionsFor(b *config.GitOpsBinding) *FetchOptions {
	repo := b.EffectiveRepo()
	opts := &FetchOptions{
		Provider: b.EffectiveProvider(),
		URL:      repo.URL,
		Branch:   repo.Branch,
		Ref:      repo.Ref,
		Subpath:  repo.Subpath,
		APIURL:   b.EffectiveAPIURL(),
		Auth: &AuthCredentials{
			Token:         repo.AuthSecret,
			SSHPrivateKey: repo.SSHPrivateKey,
			KnownHosts:    repo.KnownHosts,
		},
		TLS: &TLSConfig{
			InsecureSkipVerify: repo.InsecureSkipVerify,
			CACertPEM:          repo.CACertPEM,
		},
	}
	if b.GitLab != nil {
		opts.ProjectPath = b.GitLab.ProjectPath
	}
	if b.Verify.Active() {
		opts.VerifyKeys = b.Verify.PublicKeys
	}
	return opts
}
