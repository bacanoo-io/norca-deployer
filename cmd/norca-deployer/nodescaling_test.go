package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const machinePlanYAML = `
metadata: {name: prod, region: eu}
release:
  version: v0.3.0
  download_base_url: https://mirror.example/norca
  checksum_sha256: 1111111111111111111111111111111111111111111111111111111111111111
cni:
  download_base_url: https://mirror.example/cni
  checksum_sha256: 2222222222222222222222222222222222222222222222222222222222222222
security: {tls_enabled: true}
node_scaling:
  enabled: true
  machine: {install: image}
inventory:
  masters:
    - host: 10.0.0.1
      hostname: s1
`

// The script is the whole of stdout, so it can be redirected into a file.
func TestNodeScalingInstallScript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(path, []byte(machinePlanYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := execute(t, "node-scaling", "install-script", "-c", path, "--arch", "arm64")
	if code != exitOK || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if !strings.HasPrefix(stdout, "#!/usr/bin/env bash\n") || !strings.Contains(stdout, "norca_0.3.0_linux_arm64.tar.gz") ||
		!strings.HasSuffix(stdout, "fi\n") {
		t.Fatalf("stdout is not the script:\n%s", stdout)
	}

	if code, _, stderr := execute(t, "node-scaling", "install-script", "-c", path, "--arch", "mips"); code != exitError ||
		!strings.Contains(stderr, "arch") {
		t.Fatalf("a bad arch: code=%d stderr=%q", code, stderr)
	}
	if code, _, _ := execute(t, "node-scaling", "install-script", "extra"); code != exitUsage {
		t.Fatalf("an argument: code=%d", code)
	}
}
