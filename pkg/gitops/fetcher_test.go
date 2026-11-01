package gitops

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"testing"
)

func TestExtractTarGzStream(t *testing.T) {
	// Create an in-memory tar.gz buffer
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	// File 1: metadata.hcl
	metaContent := []byte("pack {\n  name    = \"test-pack\"\n  version = \"1.0.0\"\n}\n")
	_ = tw.WriteHeader(&tar.Header{
		Name:     "repo-main/metadata.hcl",
		Mode:     0644,
		Size:     int64(len(metaContent)),
		Typeflag: tar.TypeReg,
	})
	_, _ = tw.Write(metaContent)

	// File 2: variables.hcl in subpath
	varContent := []byte(`variable "count" { default = 1 }`)
	_ = tw.WriteHeader(&tar.Header{
		Name:     "repo-main/variables.hcl",
		Mode:     0644,
		Size:     int64(len(varContent)),
		Typeflag: tar.TypeReg,
	})
	_, _ = tw.Write(varContent)

	_ = tw.Close()
	_ = gw.Close()

	// Extract in memory
	files, _, err := extractTarGzStream(&buf, "")
	if err != nil {
		t.Fatalf("extract failed: %v", err)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}
	if string(files["metadata.hcl"]) != string(metaContent) {
		t.Errorf("metadata.hcl content mismatch: %s", string(files["metadata.hcl"]))
	}
	if string(files["variables.hcl"]) != string(varContent) {
		t.Errorf("variables.hcl content mismatch: %s", string(files["variables.hcl"]))
	}

	res, err := buildFetchResult("forgejo", "main", "", "", files)
	if err != nil {
		t.Fatalf("buildFetchResult failed: %v", err)
	}
	if res.PackName != "test-pack" {
		t.Errorf("expected pack name 'test-pack', got %s", res.PackName)
	}
	if res.PackVersion != "1.0.0" {
		t.Errorf("expected pack version '1.0.0', got %s", res.PackVersion)
	}
}

func TestBuildArchiveURL(t *testing.T) {
	opts := &FetchOptions{
		URL: "https://forgejo.internal/org/my-pack.git",
		Auth: &AuthCredentials{
			Token: "my-pat-token",
		},
	}

	url, headers, err := buildArchiveURLAndHeaders("forgejo", opts, "v1.2.0")
	if err != nil {
		t.Fatalf("failed to build archive URL: %v", err)
	}

	expectedURL := "https://forgejo.internal/api/v1/repos/org/my-pack/archive/v1.2.0.tar.gz"
	if url != expectedURL {
		t.Errorf("expected %s, got %s", expectedURL, url)
	}
	if headers["Authorization"] != "token my-pat-token" {
		t.Errorf("expected token header, got %s", headers["Authorization"])
	}
}
