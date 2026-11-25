package gitops

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// maxAnswerBytes bounds the answer of the API that is read: a plan of a
// large stack fits many times.
const maxAnswerBytes = 64 << 20

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
// context: the log of an operation is waited for.
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
	// The address is the cluster the operator names (a flag, the plan).
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bytes.NewReader(body)) //nolint:gosec // G704: the operator's own address
	if err != nil {
		return 0, "", err
	}
	if c.Token != "" {
		req.Header.Set("X-Nomad-Token", c.Token)
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req) //nolint:gosec // G704: the operator's own address
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()

	out, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	if err != nil {
		return 0, "", fmt.Errorf("reading the answer of %s %s: %w", method, strings.SplitN(path, "?", 2)[0], err)
	}
	return resp.StatusCode, string(out), nil
}
