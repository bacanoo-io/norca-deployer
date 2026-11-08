package artifacts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

func tarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(content)), Typeflag: tar.TypeReg})
	tw.Write(content)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func hexSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestOfficialReleaseVerifiedAndCached(t *testing.T) {
	archive := tarGz(t, "norca_1.2.3_linux_amd64/norca", []byte("binary"))
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/v1.2.3/norca_1.2.3_SHA256SUMS":
			fmt.Fprintf(w, "%s  norca_1.2.3_linux_amd64.tar.gz\n", hexSum(archive))
		case "/v1.2.3/norca_1.2.3_linux_amd64.tar.gz":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	plan := &config.DeploymentPlan{Release: config.ReleaseConfig{Version: "v1.2.3", Source: config.ReleaseCustomURL, DownloadBaseURL: srv.URL}}
	cache := t.TempDir()
	f, err := NewResolver(plan, cache).NorcaBinary(t.Context(), "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(f.Path); string(data) != "binary" || f.SHA256 != hexSum([]byte("binary")) {
		t.Fatalf("extracted %q (%s)", data, f.SHA256)
	}

	// A second resolver reuses the cached archive and the cached sums file:
	// it asks the server nothing.
	before := hits.Load()
	if _, err := NewResolver(plan, cache).NorcaBinary(t.Context(), "linux", "amd64"); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != before {
		t.Fatalf("cached files downloaded again (%d requests)", hits.Load()-before)
	}
}

// cniServer serves the CNI archive v1.9.1 of amd64 and its .sha256 file;
// fail, when set, answers a request instead (true: handled).
func cniServer(t *testing.T, archive []byte, hits *atomic.Int32, fail func(w http.ResponseWriter, r *http.Request) bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if fail != nil && fail(w, r) {
			return
		}
		switch r.URL.Path {
		case "/v1.9.1/cni-plugins-linux-amd64-v1.9.1.tgz.sha256":
			fmt.Fprintf(w, "%s  cni-plugins-linux-amd64-v1.9.1.tgz\n", hexSum(archive))
		case "/v1.9.1/cni-plugins-linux-amd64-v1.9.1.tgz":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func cniPlan(url string) *config.DeploymentPlan {
	return &config.DeploymentPlan{CNI: &config.CNIConfig{Version: "v1.9.1", Source: config.ReleaseOfficial, DownloadBaseURL: url}}
}

func quickResolver(plan *config.DeploymentPlan, cache string) *Resolver {
	r := NewResolver(plan, cache)
	r.retryDelay = time.Millisecond
	r.SetLog(io.Discard)
	return r
}

// A 504 of the release server (GitHub had one) is tried again.
func TestDownloadRetriesPassingFailures(t *testing.T) {
	archive := []byte("cni")
	var hits, failed atomic.Int32
	srv := cniServer(t, archive, &hits, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, ".sha256") && failed.Add(1) <= 2 {
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			return true
		}
		return false
	})
	f, err := quickResolver(cniPlan(srv.URL), t.TempDir()).CNIArchive(t.Context(), "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if f.SHA256 != hexSum(archive) || failed.Load() != 3 {
		t.Fatalf("digest %s after %d requests of the sums", f.SHA256, failed.Load())
	}
}

// A failure that stays is reported after the attempts, and a 404 at once.
func TestDownloadGivesUp(t *testing.T) {
	var hits atomic.Int32
	srv := cniServer(t, []byte("cni"), &hits, func(w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
		} else {
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
		}
		return true
	})
	_, err := quickResolver(cniPlan(srv.URL), t.TempDir()).CNIArchive(t.Context(), "amd64")
	if err == nil || !strings.Contains(err.Error(), "504") || hits.Load() != downloadAttempts {
		t.Fatalf("after %d requests: %v", hits.Load(), err)
	}

	hits.Store(0)
	r := quickResolver(cniPlan(srv.URL), t.TempDir())
	if err := r.download(t.Context(), srv.URL+"/missing", filepath.Join(t.TempDir(), "f"), ""); err == nil || hits.Load() != 1 {
		t.Fatalf("a 404 after %d requests: %v", hits.Load(), err)
	}
}

// With the archive and its sums in the cache, an apply works while the
// release server is down.
func TestCachedSumsNeedNoServer(t *testing.T) {
	archive := []byte("cni")
	var hits atomic.Int32
	var down atomic.Bool
	srv := cniServer(t, archive, &hits, func(w http.ResponseWriter, r *http.Request) bool {
		if down.Load() {
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
			return true
		}
		return false
	})
	cache := t.TempDir()
	if _, err := quickResolver(cniPlan(srv.URL), cache).CNIArchive(t.Context(), "amd64"); err != nil {
		t.Fatal(err)
	}
	down.Store(true)
	hits.Store(0)
	f, err := quickResolver(cniPlan(srv.URL), cache).CNIArchive(t.Context(), "amd64")
	if err != nil || f.SHA256 != hexSum(archive) || hits.Load() != 0 {
		t.Fatalf("with the server down: %v, %d requests", err, hits.Load())
	}
}

// A cached sums file that no longer matches the archive (a release
// republished under its version) is downloaded once more.
func TestStaleCachedSumsDownloadedAgain(t *testing.T) {
	archive := []byte("new archive")
	var hits atomic.Int32
	srv := cniServer(t, archive, &hits, nil)
	cache := t.TempDir()
	dir := filepath.Join(cache, "cni", "v1.9.1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := fmt.Sprintf("%s  cni-plugins-linux-amd64-v1.9.1.tgz\n", hexSum([]byte("old archive")))
	if err := os.WriteFile(filepath.Join(dir, "cni-plugins-linux-amd64-v1.9.1.tgz.sha256"), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := quickResolver(cniPlan(srv.URL), cache).CNIArchive(t.Context(), "amd64")
	if err != nil || f.SHA256 != hexSum(archive) {
		t.Fatalf("%v (%v)", f, err)
	}
}

func TestChecksumMismatchRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			fmt.Fprintf(w, "%s  cni-plugins-linux-amd64-v1.9.1.tgz\n", strings.Repeat("0", 64))
			return
		}
		w.Write([]byte("tampered"))
	}))
	defer srv.Close()

	plan := &config.DeploymentPlan{CNI: &config.CNIConfig{Version: "v1.9.1", Source: config.ReleaseOfficial, DownloadBaseURL: srv.URL}}
	_, err := NewResolver(plan, t.TempDir()).CNIArchive(t.Context(), "amd64")
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected a checksum error, got %v", err)
	}
}

func TestLocalArchiveWithPlaceholders(t *testing.T) {
	dir := t.TempDir()
	archive := tarGz(t, "norca", []byte("arm binary"))
	os.WriteFile(filepath.Join(dir, "norca_linux_arm64.tar.gz"), archive, 0644)

	plan := &config.DeploymentPlan{Release: config.ReleaseConfig{
		Source: config.ReleaseLocalPath, LocalBinaryPath: filepath.Join(dir, "norca_{os}_{arch}.tar.gz"),
	}}
	f, err := NewResolver(plan, t.TempDir()).NorcaBinary(t.Context(), "linux", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(f.Path); string(data) != "arm binary" {
		t.Fatalf("extracted %q", data)
	}
}
