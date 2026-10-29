package artifacts

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
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
	f, err := NewResolver(plan, cache).NorcaBinary("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(f.Path); string(data) != "binary" || f.SHA256 != hexSum([]byte("binary")) {
		t.Fatalf("extracted %q (%s)", data, f.SHA256)
	}

	// A second resolver reuses the cached archive (only the sums file is fetched).
	before := hits.Load()
	if _, err := NewResolver(plan, cache).NorcaBinary("linux", "amd64"); err != nil {
		t.Fatal(err)
	}
	if hits.Load()-before != 1 {
		t.Fatalf("cached archive downloaded again (%d requests)", hits.Load()-before)
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
	_, err := NewResolver(plan, t.TempDir()).CNIArchive("amd64")
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
	f, err := NewResolver(plan, t.TempDir()).NorcaBinary("linux", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(f.Path); string(data) != "arm binary" {
		t.Fatalf("extracted %q", data)
	}
}

func TestNATSBridgeFromTheSameArchive(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range map[string]string{"norca_1.2.3_linux_amd64/norca": "norca", "norca_1.2.3_linux_amd64/norca-natsbridge": "bridge"} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(content)), Typeflag: tar.TypeReg})
		tw.Write([]byte(content))
	}
	tw.Close()
	gz.Close()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "release.tar.gz"), buf.Bytes(), 0644)

	plan := &config.DeploymentPlan{Release: config.ReleaseConfig{Source: config.ReleaseLocalPath, LocalBinaryPath: filepath.Join(dir, "release.tar.gz")}}
	res := NewResolver(plan, t.TempDir())
	for name, want := range map[string]string{"norca": "norca", "norca-natsbridge": "bridge"} {
		f, err := res.ReleaseBinary(name, "linux", "amd64")
		if err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(f.Path); string(data) != want {
			t.Fatalf("%s: extracted %q", name, data)
		}
	}
}
