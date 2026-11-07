package engine

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/artifacts"
	"github.com/bacanoo-io/norca-deployer/internal/platform/service"
	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
	"github.com/bacanoo-io/norca-deployer/internal/platform/tuning"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

const (
	sumRelease = "1111111111111111111111111111111111111111111111111111111111111111"
	sumCNI     = "2222222222222222222222222222222222222222222222222222222222222222"
	sumJoin    = "3333333333333333333333333333333333333333333333333333333333333333"
)

// machinePlan is a plan for scaled machines; release is its release block.
func machinePlan(release, machine string) string {
	return `
metadata: {name: prod, region: eu}
` + release + `
cni:
  version: v1.9.1
  download_base_url: https://mirror.example/cni
  checksum_sha256: ` + sumCNI + `
security: {tls_enabled: true}
tuning: {apply_sysctl: true, file_descriptor_limit: 100000}
paths:
  linux: {config_dir: /etc/norca, tls_dir: /etc/norca/tls}
host_packages:
  repositories:
    - name: docker
      uri: https://download.docker.com/linux/debian
      suite: bookworm
      components: [stable]
      key: |
        -----BEGIN PGP PUBLIC KEY BLOCK-----
        mQINBFit2ioBEADhWpZ8/wvZ6hUTiXOwQHXMAlaFHcPH9hAtr4F1y2+OYdbtMuth
        -----END PGP PUBLIC KEY BLOCK-----
      roles: [client]
      pin_priority: 600
  install:
    - {name: docker-ce, version: "5:27.3.1-1~debian.12~bookworm", hold: true, roles: [client]}
    - {name: jq}
    - {name: etcd-only, roles: [master]}
node_scaling:
  enabled: true
` + machine + `
inventory:
  masters:
    - host: 10.0.0.1
      hostname: s1
`
}

const officialRelease = `release: {version: v0.3.0, download_base_url: https://mirror.example/norca, checksum_sha256: ` + sumRelease + `}`

const joinMachine = `  machine:
    join_binary: /opt/norca/bin/norca-join
    join_download:
      url: https://mirror.example/norca-join_linux_amd64
      checksum_sha256: ` + sumJoin

func generate(t *testing.T, plan *config.DeploymentPlan, opts MachineScriptOptions) string {
	t.Helper()
	script, err := GenerateInstallScript(context.Background(), plan, artifacts.NewResolver(plan, t.TempDir()), opts)
	if err != nil {
		t.Fatal(err)
	}
	return script
}

func TestInstallScriptOfficialRelease(t *testing.T) {
	plan := loadPlan(t, machinePlan(officialRelease, joinMachine))
	script := generate(t, plan, MachineScriptOptions{})
	paths := storage.ResolveNodePaths(&config.NodeConfig{Platform: "linux"}, plan)
	for _, want := range []string{
		"#!/usr/bin/env bash\n", "set -euo pipefail\n", "# Mode image:", "# Architecture: amd64.",
		// The release and the CNI plugins, with the URLs and the digests of
		// the native install.
		"fetch 'https://mirror.example/norca/v0.3.0/norca_0.3.0_linux_amd64.tar.gz' \"$work/norca.tar.gz\"",
		"verify \"$work/norca.tar.gz\" '" + sumRelease + "'",
		"norca_bin='/usr/local/bin/norca'",
		"fetch 'https://mirror.example/cni/v1.9.1/cni-plugins-linux-amd64-v1.9.1.tgz' \"$work/cni.tgz\"",
		"tar -xzf \"$work/cni.tgz\" -C '/opt/cni/bin'",
		"cni_marker='/opt/cni/bin/.norca-deployer-cni.sha256'",
		// norca-join from join_download.
		"join_bin='/opt/norca/bin/norca-join'",
		"fetch 'https://mirror.example/norca-join_linux_amd64' \"$work/norca-join\"",
		"verify \"$work/norca-join\" '" + sumJoin + "'",
		// The directories with the modes of prepareNode.
		"ensure_dir '/usr/local/bin' 0755\n", "ensure_dir '/etc/norca' 0750\n", "ensure_dir '/var/lib/norca' 0750\n",
		"ensure_dir '/var/log/norca' 0750\n", "ensure_dir '/etc/norca/tls' 0700\n",
		"ensure_dir '/opt/cni/bin' 0755\n", "ensure_dir '/opt/cni/config' 0755\n",
		// The unit of the native install, enabled and not started.
		"put_file '/etc/systemd/system/norca.service' 0644 <<'NORCA_EOF'\n" + service.LinuxUnit(paths, 100000) + "NORCA_EOF\n",
		"systemctl enable --quiet norca", "multi-user.target.wants",
		// The host packages of a client and the sysctl tuning.
		"put_file '/etc/apt/keyrings/norca-docker.asc' 0644 <<'NORCA_EOF'\n-----BEGIN PGP PUBLIC KEY BLOCK-----\n",
		"put_file '/etc/apt/sources.list.d/norca-docker.sources' 0644",
		"put_file '/etc/apt/preferences.d/norca-docker.pref' 0644", "remove_file '/etc/apt/auth.conf.d/norca-docker.conf'",
		"check_package 'docker-ce' '5:27.3.1-1~debian.12~bookworm'", "check_package 'jq' ''",
		"apt-get update -q -o DPkg::Lock::Timeout=300",
		"DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends",
		"'docker-ce=5:27.3.1-1~debian.12~bookworm' 'jq'",
		"apt-mark hold 'docker-ce'", "apt-mark unhold 'jq'", "expect_package 'docker-ce' '5:27.3.1-1~debian.12~bookworm'",
		"put_file '" + tuning.SysctlConfigPath + "' 0644 <<'NORCA_EOF'\n" + tuning.RecommendedSysctl + "NORCA_EOF\n",
		"sysctl --system",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script has no %q", want)
		}
	}
	for _, unwanted := range []string{
		"etcd-only",       // a package of the servers
		"systemctl start", // the bootstrap starts the agent
		"systemctl restart",
		"--now",
		"iptables -", "nft ", "ufw ", // the firewall is the cloud's
		"NORCA_BINARY",
	} {
		if strings.Contains(script, unwanted) {
			t.Errorf("the script has %q", unwanted)
		}
	}
	// The same plan, the same script: the Variable is not rewritten on
	// every apply.
	if again := generate(t, plan, MachineScriptOptions{}); again != script {
		t.Fatal("the script is not deterministic")
	}
	// Another architecture.
	arm := generate(t, plan, MachineScriptOptions{Arch: "arm64"})
	if !strings.Contains(arm, "norca_0.3.0_linux_arm64.tar.gz") || !strings.Contains(arm, "cni-plugins-linux-arm64-v1.9.1.tgz") {
		t.Fatal("the arm64 script downloads other files")
	}
	if _, err := GenerateInstallScript(context.Background(), plan, artifacts.NewResolver(plan, t.TempDir()),
		MachineScriptOptions{Arch: "mips"}); err == nil {
		t.Fatal("an unknown architecture is accepted")
	}
}

// Without checksums in the plan the digests come from the checksum files of
// the releases, as for the native install; the archives are not downloaded.
func TestInstallScriptChecksumFiles(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/norca/v0.3.0/norca_0.3.0_SHA256SUMS":
			fmt.Fprintf(w, "%s  norca_0.3.0_linux_amd64.tar.gz\n%s  norca_0.3.0_linux_arm64.tar.gz\n", sumRelease, sumJoin)
		case "/cni/v1.9.1/cni-plugins-linux-amd64-v1.9.1.tgz.sha256":
			fmt.Fprintf(w, "%s  cni-plugins-linux-amd64-v1.9.1.tgz\n", sumCNI)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	yaml := strings.Replace(machinePlan(`release: {version: v0.3.0, source: custom-url, download_base_url: `+srv.URL+`/norca}`, ""),
		"download_base_url: https://mirror.example/cni\n  checksum_sha256: "+sumCNI, "download_base_url: "+srv.URL+"/cni", 1)
	plan := loadPlan(t, yaml)
	script := generate(t, plan, MachineScriptOptions{})
	for _, want := range []string{
		"fetch '" + srv.URL + "/norca/v0.3.0/norca_0.3.0_linux_amd64.tar.gz'", "verify \"$work/norca.tar.gz\" '" + sumRelease + "'",
		"verify \"$work/cni.tgz\" '" + sumCNI + "'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script has no %q", want)
		}
	}
	if slices.ContainsFunc(paths, func(p string) bool { return strings.HasSuffix(p, ".tar.gz") || strings.HasSuffix(p, ".tgz") }) {
		t.Fatalf("an archive was downloaded: %v", paths)
	}
	// Without join_download the script checks for norca-join and says how
	// to provide it.
	if !strings.Contains(script, "join_bin='/usr/local/bin/norca-join'") || !strings.Contains(script, "node_scaling.machine.join_download") {
		t.Fatal("no check of norca-join")
	}
}

func TestInstallScriptLocalPathAndHostPackage(t *testing.T) {
	local := loadPlan(t, machinePlan(`release: {version: v0.3.0, source: local-path, local_binary_path: ./norca, checksum_sha256: `+sumRelease+`}`, ""))
	script := generate(t, local, MachineScriptOptions{})
	for _, want := range []string{"NORCA_BINARY", "verify \"$NORCA_BINARY\" '" + sumRelease + "'", "extract_binary \"$NORCA_BINARY\" norca"} {
		if !strings.Contains(script, want) {
			t.Errorf("local-path: the script has no %q", want)
		}
	}
	if strings.Contains(script, "norca.tar.gz") {
		t.Error("local-path: the script downloads a release")
	}

	hp := loadPlan(t, strings.Replace(machinePlan(`release: {version: v0.3.0, source: host-package}`, ""),
		"    - {name: jq}", "    - {name: jq}\n    - {name: norca}", 1))
	script = generate(t, hp, MachineScriptOptions{})
	for _, want := range []string{"check_package 'norca' ''", "found=$(command -v norca || true)",
		"package norca did not install an executable norca binary"} {
		if !strings.Contains(script, want) {
			t.Errorf("host-package: the script has no %q", want)
		}
	}
	// The packages come before the binary they provide.
	if strings.Index(script, "check_package 'norca'") > strings.Index(script, "command -v norca") {
		t.Error("host-package: the binary is checked before the packages are installed")
	}

	// CNI from a local archive: the image build names it.
	cni := loadPlan(t, strings.Replace(machinePlan(officialRelease, ""), "  version: v1.9.1\n", "  version: v1.9.1\n  source: local-path\n  local_archive_path: ./cni.tgz\n", 1))
	if script := generate(t, cni, MachineScriptOptions{}); !strings.Contains(script, "NORCA_CNI_ARCHIVE") || strings.Contains(script, "cni.tgz\"") {
		t.Error("cni local-path: the script does not take the archive from NORCA_CNI_ARCHIVE")
	}
}

// A local .deb cannot be downloaded by the machine: the script checks that
// the package is there. A repository with credentials is refused at boot.
func TestInstallScriptPackagesOutOfReach(t *testing.T) {
	debs := t.TempDir()
	if err := os.WriteFile(filepath.Join(debs, "tool_amd64.deb"), []byte("deb"), 0o600); err != nil {
		t.Fatal(err)
	}
	yaml := strings.Replace(machinePlan(officialRelease, ""), "    - {name: jq}",
		"    - {name: jq}\n    - {name: tool, file: '"+filepath.ToSlash(debs)+"/tool_{arch}.deb', version: 1.0-1}", 1)
	script := generate(t, loadPlan(t, yaml), MachineScriptOptions{})
	if !strings.Contains(script, "require_package 'tool' '1.0-1' 'tool_amd64.deb'") || strings.Contains(script, debs) {
		t.Fatal("a local .deb is not checked")
	}

	auth := strings.Replace(machinePlan(officialRelease, "  machine: {install: boot}"), "      pin_priority: 600",
		"      pin_priority: 600\n      auth: {username: u, password: secret-pass}", 1)
	plan := loadPlan(t, auth)
	if _, err := GenerateInstallScript(context.Background(), plan, artifacts.NewResolver(plan, t.TempDir()), MachineScriptOptions{}); err == nil ||
		!strings.Contains(err.Error(), "install: image") {
		t.Fatalf("boot with repository credentials: %v", err)
	}
	plan.NodeScaling.Machine.Install = config.MachineInstallImage
	if script := generate(t, plan, MachineScriptOptions{}); !strings.Contains(script, "password secret-pass") ||
		!strings.Contains(script, "put_file '/etc/apt/auth.conf.d/norca-docker.conf' 0600") {
		t.Fatal("image: the credentials of the repository are not written")
	}
}

func TestInstallScriptBootMode(t *testing.T) {
	plan := loadPlan(t, machinePlan(officialRelease, "  machine: {install: boot, arch: arm64}"))
	script := generate(t, plan, MachineScriptOptions{})
	if !strings.Contains(script, "# Mode boot:") || !strings.Contains(script, "# Architecture: arm64.") ||
		!strings.Contains(script, "norca_0.3.0_linux_arm64.tar.gz") {
		t.Fatalf("boot script:\n%s", script[:400])
	}
}

// A file that is not text with a final newline is written from base64, byte
// for byte.
func TestShellFile(t *testing.T) {
	if got := shellFile("/a", "0644", "x\ny\n"); got != "put_file '/a' 0644 <<'NORCA_EOF'\nx\ny\nNORCA_EOF" {
		t.Fatalf("text: %q", got)
	}
	for _, content := range [][]byte{[]byte("no newline"), {0x99, 0x01, 0x0d, 0x04}, []byte("a\nNORCA_EOF\nb\n"), []byte("dos\r\n")} {
		got := shellFile("/a", "0644", string(content))
		if !strings.HasPrefix(got, "put_file '/a' 0644 < <(base64 -d <<'NORCA_EOF'\n") {
			t.Errorf("%q: %q", content, got)
		}
	}
}

// The bootstrap Variable: join_binary only when the plan sets it, the
// install script only with install: boot; a plan without the machine block
// keeps the items it had.
func TestNodeScalingBootstrapMachineItems(t *testing.T) {
	for _, tc := range []struct {
		name    string
		machine string
		items   []string
	}{
		{"no machine block", "", nil},
		{"image", joinMachine, []string{JoinBinaryItem}},
		{"boot", "  machine: {install: boot}", []string{InstallScriptItem}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := loadPlan(t, machinePlan(officialRelease, tc.machine))
			r := NewRunner(plan, nil)
			if err := r.initPKI(); err != nil {
				t.Fatal(err)
			}
			if _, err := IssueNodeCA(plan, NodeCAOptions{}); err != nil {
				t.Fatal(err)
			}
			r.artifacts = artifacts.NewResolver(plan, t.TempDir())
			items, err := r.nodeScalingBootstrapItems()
			if err != nil {
				t.Fatal(err)
			}
			var extra []string
			for k := range items {
				switch k {
				case "ca", "node_ca", "node_ca_key", "agent_config", "tls_dir", "config_file", "service", "validity", "trust_domain":
				default:
					extra = append(extra, k)
				}
			}
			slices.Sort(extra)
			if !slices.Equal(extra, tc.items) {
				t.Fatalf("items beyond the bootstrap of before: %v, want %v (all: %v)", extra, tc.items, slices.Sorted(maps.Keys(items)))
			}
			if tc.name == "image" && items[JoinBinaryItem] != "/opt/norca/bin/norca-join" {
				t.Fatalf("join_binary %q", items[JoinBinaryItem])
			}
			if tc.name == "boot" {
				want := generate(t, plan, MachineScriptOptions{})
				if items[InstallScriptItem] != want {
					t.Fatal("the install script of the Variable is not the one of the command")
				}
			}
		})
	}
}

// The generated scripts of the tests above, for shellcheck and the end to
// end run: NORCA_MACHINE_SCRIPTS=<dir> writes them there.
func TestWriteInstallScripts(t *testing.T) {
	dir := os.Getenv("NORCA_MACHINE_SCRIPTS")
	if dir == "" {
		t.Skip("NORCA_MACHINE_SCRIPTS is not set")
	}
	plans := map[string]string{
		"official": machinePlan(officialRelease, joinMachine),
		"boot":     machinePlan(officialRelease, "  machine: {install: boot}"),
		"local":    machinePlan(`release: {version: v0.3.0, source: local-path, local_binary_path: ./norca}`, ""),
		"host-package": strings.Replace(machinePlan(`release: {version: v0.3.0, source: host-package}`, ""),
			"    - {name: jq}", "    - {name: jq}\n    - {name: norca}", 1),
		"auth": strings.Replace(machinePlan(officialRelease, ""), "      pin_priority: 600",
			"      pin_priority: 600\n      auth: {username: u, password: p}", 1),
	}
	for name, yaml := range plans {
		script := generate(t, loadPlan(t, yaml), MachineScriptOptions{})
		if err := os.WriteFile(filepath.Join(dir, name+".sh"), []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}
