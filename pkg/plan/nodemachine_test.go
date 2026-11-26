package plan

import (
	"strings"
	"testing"
)

const machineBase = `
metadata: {name: prod, region: eu}
release: {version: v0.3.0}
security: {tls_enabled: true}
inventory:
  masters:
    - host: 10.0.0.1
      hostname: s1
`

func TestNodeMachineConfig(t *testing.T) {
	// Defaults: image, amd64; join_binary stays empty (the bootstrap
	// Variable is written as before) and the path is the operator's default.
	p, err := Parse([]byte(machineBase+"node_scaling:\n  enabled: true\n  machine: {}\n"), FormatYAML, "")
	if err != nil {
		t.Fatal(err)
	}
	m := p.NodeScaling.Machine
	if m.Install != MachineInstallImage || m.Arch != "amd64" || m.JoinBinary != "" || m.JoinBinaryPath() != DefaultJoinBinary || m.InstallAtBoot() {
		t.Fatalf("defaults: %+v", m)
	}
	var none *NodeMachineConfig
	if none.JoinBinaryPath() != DefaultJoinBinary || none.InstallAtBoot() || none.MachineArch() != "amd64" {
		t.Fatal("a plan without the block")
	}

	// The same block in HCL.
	hclPlan := `
metadata {
  name   = "prod"
  region = "eu"
}
release {
  version = "v0.3.0"
}
security {
  tls_enabled = true
}
node_scaling {
  enabled = true
  machine {
    install     = "boot"
    arch        = "arm64"
    join_binary = "/opt/norca/bin/norca-join"
    join_download {
      url             = "https://mirror.example/norca-join"
      checksum_sha256 = "` + strings.Repeat("a", 64) + `"
    }
  }
}
inventory {
  masters {
    host     = "10.0.0.1"
    hostname = "s1"
  }
}
`
	p, err = Parse([]byte(hclPlan), FormatHCL, "")
	if err != nil {
		t.Fatal(err)
	}
	m = p.NodeScaling.Machine
	if !m.InstallAtBoot() || m.MachineArch() != "arm64" || m.JoinBinaryPath() != "/opt/norca/bin/norca-join" ||
		m.JoinDownload == nil || m.JoinDownload.URL != "https://mirror.example/norca-join" || m.JoinDownload.Official() {
		t.Fatalf("HCL: %+v", m)
	}
}

// join_download names a release of norca-platform by its version (source
// official), in YAML and in HCL; the releases are on GitHub by default.
func TestJoinDownloadOfficial(t *testing.T) {
	p, err := Parse([]byte(machineBase+"node_scaling:\n  machine:\n    join_download: {source: official, version: v0.2.0}\n"), FormatYAML, "")
	if err != nil {
		t.Fatal(err)
	}
	d := p.NodeScaling.Machine.JoinDownload
	if !d.Official() || d.Version != "v0.2.0" || d.DownloadBaseURL != DefaultJoinDownloadBaseURL || d.URL != "" {
		t.Fatalf("YAML: %+v", d)
	}

	hclPlan := `
metadata {
  name   = "prod"
  region = "eu"
}
release {
  version = "v0.3.0"
}
security {
  tls_enabled = true
}
node_scaling {
  machine {
    arch = "arm64"
    join_download {
      source            = "official"
      version           = "v0.2.0"
      download_base_url = "https://mirror.example/norca-platform"
    }
  }
}
inventory {
  masters {
    host     = "10.0.0.1"
    hostname = "s1"
  }
}
`
	p, err = Parse([]byte(hclPlan), FormatHCL, "")
	if err != nil {
		t.Fatal(err)
	}
	d = p.NodeScaling.Machine.JoinDownload
	if !d.Official() || d.Version != "v0.2.0" || d.DownloadBaseURL != "https://mirror.example/norca-platform" {
		t.Fatalf("HCL: %+v", d)
	}
}

func TestNodeMachineValidation(t *testing.T) {
	sum := strings.Repeat("a", 64)
	for name, tc := range map[string]struct {
		extra, machine, want string
	}{
		"bad install":       {"", "{install: later}", "image or boot"},
		"bad arch":          {"", "{arch: mips}", "arch"},
		"relative join":     {"", "{join_binary: bin/norca-join}", "absolute path"},
		"join with a space": {"", "{join_binary: '/opt/my bin/norca-join'}", "absolute path"},
		"join url":          {"", "{join_download: {url: 'ftp://x/norca-join', checksum_sha256: " + sum + "}}", "http(s) URL"},
		"join checksum":     {"", "{join_download: {url: 'https://x/norca-join'}}", "64 hex"},
		"join url with a version": {"", "{join_download: {url: 'https://x/norca-join', checksum_sha256: " + sum + ", version: v0.2.0}}",
			"for source official"},
		"join source":          {"", "{join_download: {source: github, version: v0.2.0}}", "must be url or official"},
		"official without":     {"", "{join_download: {source: official}}", "must look like v0.1.0"},
		"official bad version": {"", "{join_download: {source: official, version: latest}}", "must look like v0.1.0"},
		"official with a url": {"", "{join_download: {source: official, version: v0.2.0, url: 'https://x/norca-join'}}",
			"not url or checksum_sha256"},
		"official with a checksum": {"", "{join_download: {source: official, version: v0.2.0, checksum_sha256: " + sum + "}}",
			"not url or checksum_sha256"},
		"official base url": {"", "{join_download: {source: official, version: v0.2.0, download_base_url: 'ftp://x'}}",
			"download_base_url"},
		"official for 386": {"", "{arch: '386', join_download: {source: official, version: v0.2.0}}", "amd64 and arm64"},
		"boot local release": {"release: {version: v0.3.0, source: local-path, local_binary_path: ./norca}\n",
			"{install: boot}", "not local-path"},
		"boot local cni": {"cni: {source: local-path, local_archive_path: ./cni.tgz}\n", "{install: boot}", "CNI plugins"},
	} {
		yaml := machineBase + tc.extra + "node_scaling:\n  machine: " + tc.machine + "\n"
		if tc.extra != "" && strings.HasPrefix(tc.extra, "release:") {
			yaml = strings.Replace(yaml, "release: {version: v0.3.0}\n", "", 1)
		}
		if _, err := Parse([]byte(yaml), FormatYAML, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want an error with %q, got %v", name, tc.want, err)
		}
	}
	// local-path is fine for an image: the image build has the file.
	yaml := strings.Replace(machineBase, "release: {version: v0.3.0}", "release: {version: v0.3.0, source: local-path, local_binary_path: ./norca}", 1) +
		"node_scaling:\n  machine: {install: image}\n"
	if _, err := Parse([]byte(yaml), FormatYAML, ""); err != nil {
		t.Fatal(err)
	}
}
