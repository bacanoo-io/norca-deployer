package engine

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/artifacts"
	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/host"
	"github.com/bacanoo-io/norca-deployer/internal/platform/dns"
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
  apt:
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
		"#!/bin/sh\n", "set -eu\n", "# Mode image:", "# Architecture: amd64.",
		// The family of the machine at run time, and its steps.
		"os_family=$(family_of \"$os_id\" \"$(os_release ID_LIKE)\")", "packages_debian() {\n", "service_debian() {\n",
		"debian) packages_debian ;;", "debian) service_debian ;;",
		// The release and the CNI plugins, with the URLs and the digests of
		// the native install.
		"fetch 'https://mirror.example/norca/v0.3.0/norca_0.3.0_linux_amd64.tar.gz' \"$work/norca.tar.gz\"",
		"verify \"$work/norca.tar.gz\" '" + sumRelease + "'",
		"put_file '/usr/local/bin/norca' 0755 <\"$norca_src\"",
		"fetch 'https://mirror.example/cni/v1.9.1/cni-plugins-linux-amd64-v1.9.1.tgz' \"$work/cni.tgz\"",
		"tar -xzf \"$work/cni.tgz\" -C '/opt/cni/bin'",
		"put_file '/opt/cni/bin/.norca-deployer-cni.sha256' 0644",
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
		"expect_package 'docker-ce' '5:27.3.1-1~debian.12~bookworm'", "expect_package 'jq' ''",
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
		// No bash features: the script is POSIX sh.
		"pipefail", "[[", "<<<", "< <(", "\n  local ", "8#",
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
		"      - {name: jq}", "      - {name: jq}\n      - {name: norca}", 1))
	script = generate(t, hp, MachineScriptOptions{})
	for _, want := range []string{"expect_package 'norca' ''", "found=$(command -v norca || true)",
		"package norca did not install an executable norca binary"} {
		if !strings.Contains(script, want) {
			t.Errorf("host-package: the script has no %q", want)
		}
	}
	// The packages come before the binary they provide.
	if strings.Index(script, "debian) packages_debian ;;") > strings.Index(script, "found=$(command -v norca") ||
		!strings.Contains(script, "expect_package 'norca'") {
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
	yaml := strings.Replace(machinePlan(officialRelease, ""), "      - {name: jq}",
		"      - {name: jq}\n      - {name: tool, file: '"+filepath.ToSlash(debs)+"/tool_{arch}.deb', version: 1.0-1}", 1)
	script := generate(t, loadPlan(t, yaml), MachineScriptOptions{})
	if !strings.Contains(script, "require_package 'tool' '1.0-1' 'tool_amd64.deb'") || strings.Contains(script, debs) {
		t.Fatal("a local .deb is not checked")
	}

	auth := strings.Replace(machinePlan(officialRelease, "  machine: {install: boot}"), "        pin_priority: 600",
		"        pin_priority: 600\n        auth: {username: u, password: secret-pass}", 1)
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

// consulDNSMachinePlan is a machine plan with consul.dns_forwarding (and
// docker).
func consulDNSMachinePlan() string {
	return strings.Replace(machinePlan(officialRelease, ""), "node_scaling:",
		"consul:\n  dns_forwarding: {enabled: true, docker: true}\nnode_scaling:", 1)
}

// consul.dns_forwarding: the machine writes and runs the script the SSH
// path runs, with the parameters of the plan, after the sysctl tuning and
// before the directories (the order of prepareNode).
func TestInstallScriptConsulDNS(t *testing.T) {
	plain := generate(t, loadPlan(t, machinePlan(officialRelease, "")), MachineScriptOptions{})
	if strings.Contains(plain, dns.ScriptPath) {
		t.Fatal("a plan without consul.dns_forwarding has the DNS script")
	}

	script := generate(t, loadPlan(t, consulDNSMachinePlan()), MachineScriptOptions{})
	vs, err := dns.Vars(&config.ConsulDNSForwarding{Enabled: true, Address: "127.0.0.1:8600", Domain: "consul", Method: "auto", Docker: true})
	if err != nil {
		t.Fatal(err)
	}
	sysctl := strings.Index(script, "# Host tuning: kernel modules and sysctl")
	file := strings.Index(script, "put_file '"+dns.ScriptPath+"' 0700 <<'NORCA_EOF'\n"+dns.Script())
	run := strings.Index(script, dns.Assignments(vs, " ")+" sh "+dns.ScriptPath+"\n")
	lines := strings.Index(script, "step_lines 'consul.dns_forwarding' \"$step_rc\" \"$step_out\"")
	dirs := strings.Index(script, "# directories")
	if sysctl < 0 || file < 0 || run < 0 || lines < 0 || dirs < 0 ||
		!(sysctl < file && file < run && run < lines && lines < dirs) {
		t.Fatalf("sysctl %d, file %d, run %d, lines %d, directories %d:\n%s", sysctl, file, run, lines, dirs, script)
	}
	for _, want := range []string{
		`die "$1: this machine ${step_errors:-could not be configured}"`,
		`warn "$1: ${step_line#warn }"`,
		"the DNS forwarding of the consul domain",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the script has no %q", want)
		}
	}
}

const machineSystemctlStub = `#!/bin/sh
case $1 in
--version) echo 'systemd 252 (252.30-1~deb12u2)' ;;
is-active)
  case " $ACTIVE " in
  *" $3 "*) exit 0 ;;
  esac
  exit 3
  ;;
restart) exit 0 ;;
*) exit 1 ;;
esac
`

// machineShells are the POSIX shells of this machine for running parts of
// the install script: dash, bash and busybox sh (NORCA_TEST_BUSYBOX).
func machineShells(t *testing.T) map[string][]string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the script runs on Linux")
	}
	res := map[string][]string{}
	for _, name := range []string{"dash", "bash"} {
		if p, err := exec.LookPath(name); err == nil {
			res[name] = []string{p}
		}
	}
	if bb := os.Getenv("NORCA_TEST_BUSYBOX"); bb != "" {
		res["busybox"] = []string{bb, "sh"}
	}
	if len(res) == 0 {
		t.Skip("no POSIX shell")
	}
	return res
}

// scriptHelpers returns the helpers of a generated install script: its
// part before the family of the machine, without the check for root.
func scriptHelpers(t *testing.T, script string) string {
	t.Helper()
	helpers, _, found := strings.Cut(script, "\n# The family of this machine")
	if !found {
		t.Fatal("no family part in the script")
	}
	return strings.Replace(helpers, `[ "$(id -u)" -eq 0 ] || die "run this script as root"`, "", 1)
}

// The DNS part of the machine script, run as the script runs it in each
// shell: the result lines of the script become log lines and changed, an
// error line the message of die.
func TestInstallScriptConsulDNSRun(t *testing.T) {
	script := generate(t, loadPlan(t, strings.Replace(consulDNSMachinePlan(), "docker: true", "docker: false", 1)), MachineScriptOptions{})
	start, end := strings.Index(script, "# DNS of the host:"), strings.Index(script, "\n# directories")
	if start < 0 || end < start {
		t.Fatal("no DNS part in the script")
	}
	block := script[start:end]
	helpers := scriptHelpers(t, script)

	for name, argv := range machineShells(t) {
		t.Run(name, func(t *testing.T) {
			run := func(t *testing.T, root, active string) (string, error) {
				t.Helper()
				bin := t.TempDir()
				if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(machineSystemctlStub), 0o755); err != nil {
					t.Fatal(err)
				}
				// The script and its directory, which the step makes, go
				// below the root of the test too.
				scriptDir := filepath.ToSlash(filepath.Dir(dns.ScriptPath))
				b := strings.ReplaceAll(block, scriptDir, root+scriptDir)
				b = strings.Replace(b, "NCD_ROOT=''", "NCD_ROOT="+connector.ShellQuote(root), 1)
				prog := helpers + "\n" + b + "\necho \"changed=$changed\"\n"
				c := exec.Command(argv[0], append(argv[1:], "-c", prog)...)
				c.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "ACTIVE=" + active, "TMPDIR=" + t.TempDir(), "LC_ALL=C"}
				out, err := c.CombinedOutput()
				return string(out), err
			}
			host := func(t *testing.T, resolvConf string) string {
				t.Helper()
				root := t.TempDir()
				for _, d := range []string{"run/systemd/system", "etc", "usr/lib/systemd"} {
					if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(root, "etc/resolv.conf"), []byte(resolvConf), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "usr/lib/systemd/systemd-resolved"), nil, 0o755); err != nil {
					t.Fatal(err)
				}
				return root
			}

			t.Run("no resolver", func(t *testing.T) {
				out, err := run(t, host(t, "nameserver 10.0.0.2\n"), "")
				want := "ERROR: consul.dns_forwarding: this machine has no local resolver the deployer can forward the consul domain from: " +
					"systemd-resolved inactive, dnsmasq not installed, /etc/resolv.conf names 10.0.0.2."
				if err == nil || !strings.Contains(out, want) {
					t.Fatalf("err %v, output:\n%s", err, out)
				}
			})
			t.Run("systemd-resolved", func(t *testing.T) {
				root := host(t, "nameserver 127.0.0.53\n")
				out, err := run(t, root, "systemd-resolved")
				if err != nil {
					t.Fatalf("%v:\n%s", err, out)
				}
				for _, want := range []string{
					"consul DNS forwarding\n", "consul.dns_forwarding: method systemd-resolved\n",
					"consul.dns_forwarding: changed " + dns.ResolvedDropIn + "\n", "consul.dns_forwarding: restarted systemd-resolved\n", "changed=1\n",
				} {
					if !strings.Contains(out, want) {
						t.Errorf("no %q in:\n%s", want, out)
					}
				}
				if _, err := os.Stat(filepath.Join(root, dns.ResolvedDropIn)); err != nil {
					t.Fatal(err)
				}
				// A second run changes nothing.
				out, err = run(t, root, "systemd-resolved")
				if err != nil || !strings.HasSuffix(out, "changed=0\n") || strings.Contains(out, "restarted") {
					t.Fatalf("second run: %v:\n%s", err, out)
				}
			})
		})
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

// machineScriptPlans are the plans of the install scripts of the tests
// above.
func machineScriptPlans() map[string]string {
	return map[string]string{
		"official": machinePlan(officialRelease, joinMachine),
		"boot":     machinePlan(officialRelease, "  machine: {install: boot}"),
		"local":    machinePlan(`release: {version: v0.3.0, source: local-path, local_binary_path: ./norca}`, ""),
		"host-package": strings.Replace(machinePlan(`release: {version: v0.3.0, source: host-package}`, ""),
			"      - {name: jq}", "      - {name: jq}\n      - {name: norca}", 1),
		"auth": strings.Replace(machinePlan(officialRelease, ""), "        pin_priority: 600",
			"        pin_priority: 600\n        auth: {username: u, password: p}", 1),
		"consul-dns": consulDNSMachinePlan(),
	}
}

// The generated scripts of the tests above, for shellcheck and the end to
// end run: NORCA_MACHINE_SCRIPTS=<dir> writes them there.
func TestWriteInstallScripts(t *testing.T) {
	dir := os.Getenv("NORCA_MACHINE_SCRIPTS")
	if dir == "" {
		t.Skip("NORCA_MACHINE_SCRIPTS is not set")
	}
	for name, yaml := range machineScriptPlans() {
		script := generate(t, loadPlan(t, yaml), MachineScriptOptions{})
		if err := os.WriteFile(filepath.Join(dir, name+".sh"), []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The install script finds out the family of its machine as the deployer
// does over SSH: the same os-release files give the same family, in every
// shell.
func TestInstallScriptFamilyOf(t *testing.T) {
	helpers := scriptHelpers(t, generate(t, loadPlan(t, machinePlan(officialRelease, "")), MachineScriptOptions{}))
	samples, err := filepath.Glob(filepath.Join("..", "host", "testdata", "os-release", "*"))
	if err != nil || len(samples) < 5 {
		t.Fatalf("os-release samples: %v %v", samples, err)
	}
	for name, argv := range machineShells(t) {
		t.Run(name, func(t *testing.T) {
			for _, sample := range samples {
				data, err := os.ReadFile(sample)
				if err != nil {
					t.Fatal(err)
				}
				facts, err := host.Parse(string(data) + "--\nx86_64\n--\nsystemd\n--\n--\n--\n--\n0\n")
				if err != nil {
					t.Fatal(err)
				}
				want := string(facts.Family)
				if want == "" {
					want = "unknown"
				}
				prog := helpers + "\nfamily_of \"$(os_release ID)\" \"$(os_release ID_LIKE)\"\n"
				c := exec.Command(argv[0], append(argv[1:], "-c", prog)...)
				c.Env = []string{"PATH=/usr/bin:/bin", "NORCA_OS_RELEASE=" + sample, "TMPDIR=" + t.TempDir()}
				out, err := c.CombinedOutput()
				if err != nil || strings.TrimSpace(string(out)) != want {
					t.Errorf("%s: family %q (%v), the deployer says %q", filepath.Base(sample), out, err, want)
				}
			}
		})
	}
}
