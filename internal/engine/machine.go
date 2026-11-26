package engine

import (
	"context"
	_ "embed"
	"fmt"
	"strings"
	"text/template"

	"github.com/bacanoo-io/norca-deployer/internal/artifacts"
	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/crypto"
	"github.com/bacanoo-io/norca-deployer/internal/host"
	"github.com/bacanoo-io/norca-deployer/internal/platform/dns"
	"github.com/bacanoo-io/norca-deployer/internal/platform/packages"
	"github.com/bacanoo-io/norca-deployer/internal/platform/selinux"
	"github.com/bacanoo-io/norca-deployer/internal/platform/service"
	"github.com/bacanoo-io/norca-deployer/internal/platform/step"
	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
	"github.com/bacanoo-io/norca-deployer/internal/platform/tuning"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// The install script of a scaled machine: one POSIX sh script, generated
// from the plan, that puts on a machine what the bootstrap of the node
// scaling operator expects there: the norca binary, the CNI plugins,
// norca-join, the directories and the service of the agent, and the host
// packages, the sysctl tuning and the DNS forwarding of the consul domain
// of a client. It runs when the image of the provider is built
// (node_scaling.machine.install = image) or at the first boot, from the
// bootstrap (boot). It renders the steps prepareNode runs on a client of
// the inventory (the drivers of packages, service and tuning, the
// directories, the DNS forwarding), without the configuration, the
// certificates and the firewall: the bootstrap writes the first two, the
// cloud does the third. The machine is a machine the deployer never sees,
// so the script finds out its family at run time and runs the steps of
// that family: the host packages and the service of Debian (apt, systemd),
// RHEL (dnf, systemd, with the SELinux labels restored) and Alpine (apk,
// OpenRC, its tuning and the musl build of norca).

//go:embed machine_install.sh.tmpl
var machineScriptText string

var machineScriptTemplate = template.Must(template.New("install").Option("missingkey=error").Funcs(template.FuncMap{
	"q": connector.ShellQuote,
}).Parse(machineScriptText))

// InstallScriptItem is the item of the bootstrap Variable with the install
// script (install: boot); JoinBinaryItem is where norca-join is.
const (
	InstallScriptItem = "install_script"
	JoinBinaryItem    = "join_binary"
)

// MachineScriptOptions are the choices of GenerateInstallScript.
type MachineScriptOptions struct {
	// Arch is the architecture of the machines; empty for
	// node_scaling.machine.arch (amd64 by default).
	Arch string
	// Vault decrypts the inline secrets of the plan (the password of an APT
	// repository); nil without a master key.
	Vault *crypto.Vault
}

type machineJoin struct {
	Path   string
	URL    string
	SHA256 string
}

type machineScriptData struct {
	Cluster, Region, Mode, Arch, Version string
	// Families are the families the script has steps of: their names for
	// the messages ("debian, rhel") and as a case pattern ("debian |
	// rhel"), and their parts.
	Families, FamilyPattern string
	Parts                   []machineFamily
	// HasPackages: a family has host packages; HasRHEL: the RHEL family is
	// one of them (the SELinux warning).
	HasPackages, HasRHEL bool
	// TuningByFamily and BinaryByFamily: the families have a tuning or a
	// norca binary of their own (in their parts), not the common one.
	TuningByFamily, BinaryByFamily bool
	// Functions are the functions of the steps.
	Functions string
	// The rendered steps common to the families.
	Tuning, DNS, Dirs, Binary, CNI string
	// DNSInits are the init systems of the families for the DNS step
	// (NCD_INIT), with the families of each as a case pattern.
	DNSInits []machineInit
	Join     machineJoin
	// Minimums are the first versions of the distributions, those of the
	// check of a node over SSH (host.Minimums).
	Minimums []host.Minimum
}

// machineFamily is the part of the install script of one family: its host
// packages and the service of the agent, and its tuning and norca binary
// when they differ between the families.
type machineFamily struct {
	Name, Manager, Packages, Service, Tuning, Binary string
}

// machineInit is an init system and the families of the script that have
// it ("debian | rhel").
type machineInit struct {
	Init, Families string
}

// machineFamilies are the families of the install script: the manager of
// their packages, their init system (its service driver, and its name for
// the DNS step), their tuning and the C library of their norca binary.
var machineFamilies = []struct {
	family  string
	manager string
	driver  packages.Manager
	service service.Manager
	init    string
	tuner   tuning.Tuner
	libc    string
}{
	{config.FamilyDebian, config.PackageManagerApt, packages.Apt{}, service.Systemd{}, config.InitSystemd, tuning.Linux{}, artifacts.LibcGlibc},
	{config.FamilyRHEL, config.PackageManagerDnf, packages.Dnf{}, service.Systemd{}, config.InitSystemd, tuning.Linux{}, artifacts.LibcGlibc},
	{config.FamilyAlpine, config.PackageManagerApk, packages.Apk{}, service.OpenRC{}, config.InitOpenRC, tuning.OpenRC{}, artifacts.LibcMusl},
}

// scriptFamilies returns the families the install script installs on, as
// apply does for a client: a family whose manager has no block in
// host_packages while another block selects packages for a client is left
// out (the script stops on such a machine, as apply stops on such a node),
// and so is an OpenRC family when the forwarding of the consul domain needs
// systemd-resolved (method systemd-resolved, or docker), as apply refuses
// such a node.
func scriptFamilies(plan *config.DeploymentPlan, node *config.NodeConfig) []int {
	var res []int
	for i, f := range machineFamilies {
		if _, has := packages.Block(plan.HostPackages, f.manager, node, false); !has && otherBlockSelects(plan, node, f.manager) {
			continue
		}
		if f.init == config.InitOpenRC && plan.Consul.DNSForwardingNeedsResolved() {
			continue
		}
		res = append(res, i)
	}
	return res
}

// otherBlockSelects reports whether a block of host_packages other than the
// one of manager selects packages for a client.
func otherBlockSelects(plan *config.DeploymentPlan, node *config.NodeConfig, manager string) bool {
	for _, m := range packages.BlockManagers {
		if sel, _ := packages.Block(plan.HostPackages, m, node, false); m != manager && !sel.Empty() {
			return true
		}
	}
	return false
}

// validMachineArch are the architectures of a scaled machine.
var validMachineArch = map[string]bool{"amd64": true, "arm64": true, "arm": true, "386": true}

// GenerateInstallScript returns the install script of a scaled Linux
// machine. The downloads it names are resolved with res: the URL and the
// SHA-256 of the release archive, of the CNI plugins and of norca-join, as
// the native install resolves them (a SHA256SUMS file of the release is
// downloaded when the plan has no checksum), but the files themselves are
// downloaded by the machine. The script is deterministic: the same plan
// gives the same script.
func GenerateInstallScript(ctx context.Context, plan *config.DeploymentPlan, res *artifacts.Resolver, opts MachineScriptOptions) (string, error) {
	var machine *config.NodeMachineConfig
	if plan.NodeScaling != nil {
		machine = plan.NodeScaling.Machine
	}
	arch := opts.Arch
	if arch == "" {
		arch = machine.MachineArch()
	}
	if !validMachineArch[arch] {
		return "", fmt.Errorf("arch %q must be amd64, arm64, arm or 386", arch)
	}
	boot := machine.InstallAtBoot()
	mode := config.MachineInstallImage
	if boot {
		mode = config.MachineInstallBoot
	}
	renderOpts := step.RenderOptions{Boot: boot, BootHint: "use node_scaling.machine.install: image"}

	node := &config.NodeConfig{Platform: "linux", Arch: arch}
	paths := storage.ResolveNodePaths(node, plan)
	data := &machineScriptData{
		Cluster: plan.Metadata.Name, Region: plan.Metadata.Region, Mode: mode, Arch: arch,
		Version:  plan.Release.Version,
		Join:     machineJoin{Path: machine.JoinBinaryPath()},
		Minimums: host.Minimums(),
	}
	var all []step.Step
	render := func(steps []step.Step) (string, error) {
		all = append(all, steps...)
		return step.Render(steps, renderOpts)
	}

	// Per family: the host packages of a client with the manager of the
	// family, the service (on RHEL with the SELinux labels), the tuning and
	// the norca binary.
	var names []string
	tunings, binaries := map[string]bool{}, map[string]bool{}
	inits := map[string][]string{}
	for _, i := range scriptFamilies(plan, node) {
		f := machineFamilies[i]
		if _, seen := inits[f.init]; !seen {
			data.DNSInits = append(data.DNSInits, machineInit{Init: f.init})
		}
		inits[f.init] = append(inits[f.init], f.family)
		sel, _ := packages.Block(plan.HostPackages, f.manager, node, false)
		pkgSteps, err := f.driver.Steps(sel, packages.Context{Plan: plan, Arch: arch, Script: true,
			Resolve: func(v string) (string, error) { return resolveSecretWith(opts.Vault, v) }})
		if err != nil {
			return "", fmt.Errorf("host_packages.%s: %w", f.manager, err)
		}
		part := machineFamily{Name: f.family, Manager: f.manager}
		if part.Packages, err = render(pkgSteps); err != nil {
			return "", fmt.Errorf("host_packages.%s: %w", f.manager, err)
		}
		svc := f.service.InstallSteps(paths, plan.Tuning.FileDescriptorLimit)
		if f.family == config.FamilyRHEL {
			svc = append(svc, selinux.RestoreStep(selinux.NodePaths(paths), true))
			data.HasRHEL = true
		}
		if part.Service, err = render(svc); err != nil {
			return "", err
		}
		if plan.Tuning.ApplySysctl {
			if part.Tuning, err = render(f.tuner.Steps()); err != nil {
				return "", err
			}
		}
		bin, err := machineBinaryStep(ctx, plan, res, paths, arch, boot, f.libc)
		if err != nil {
			return "", err
		}
		if part.Binary, err = render([]step.Step{bin}); err != nil {
			return "", err
		}
		tunings[part.Tuning], binaries[part.Binary] = true, true
		data.HasPackages = data.HasPackages || part.Packages != ""
		data.Parts = append(data.Parts, part)
		names = append(names, f.family)
	}
	if len(names) == 0 {
		return "", fmt.Errorf("the install script has no family to install on: every family's package manager lacks its block " +
			"of host_packages while another block selects packages for a client, or (Alpine) consul.dns_forwarding needs systemd-resolved " +
			"(method systemd-resolved, or docker)")
	}
	data.Families, data.FamilyPattern = strings.Join(names, ", "), strings.Join(names, " | ")
	for i := range data.DNSInits {
		data.DNSInits[i].Families = strings.Join(inits[data.DNSInits[i].Init], " | ")
	}
	// A tuning or a binary that every family shares is rendered once.
	data.TuningByFamily, data.BinaryByFamily = len(tunings) > 1, len(binaries) > 1
	for i := range data.Parts {
		if !data.TuningByFamily {
			data.Tuning, data.Parts[i].Tuning = data.Parts[i].Tuning, ""
		}
		if !data.BinaryByFamily {
			data.Binary, data.Parts[i].Binary = data.Parts[i].Binary, ""
		}
	}

	var err error
	if plan.Consul.DNSForwardingEnabled() {
		// NCD_INIT comes from the family of the machine (DNSInits).
		dnsSteps, err := dns.Steps(plan.Consul.DNSForwarding, "")
		if err != nil {
			return "", err
		}
		if data.DNS, err = render(dnsSteps); err != nil {
			return "", err
		}
	}

	// The directories of a client, with the modes of prepareNode, and the
	// state directory of the script.
	dirs := append(nodeDirs(node, paths, true), step.Dir{Path: cliStateDir, Mode: 0o700})
	if data.Dirs, err = render([]step.Step{dirsStep(dirs)}); err != nil {
		return "", err
	}

	if c := plan.CNI; c != nil && c.Install != nil && *c.Install {
		cni, err := machineCNIStep(ctx, plan, res, paths, arch, boot)
		if err != nil {
			return "", err
		}
		if data.CNI, err = render([]step.Step{cni}); err != nil {
			return "", err
		}
	}

	if machine != nil && machine.JoinDownload != nil {
		d, err := res.JoinDownload(ctx, machine.JoinDownload, arch)
		if err != nil {
			return "", err
		}
		data.Join.URL, data.Join.SHA256 = d.URL, d.SHA256
	}
	data.Functions = strings.TrimSuffix(step.Functions(all), "\n")

	var b strings.Builder
	if err := machineScriptTemplate.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// machineBinaryStep installs the norca binary on a machine: from the package
// of release.source host-package (the step of the SSH path), from the file
// the image build names (local-path), or from the release archive of the C
// library of the family (libc) that the machine downloads and checks.
func machineBinaryStep(ctx context.Context, plan *config.DeploymentPlan, res *artifacts.Resolver, paths *storage.ResolvedPaths,
	arch string, boot bool, libc string) (step.Step, error) {
	q := connector.ShellQuote
	bin := paths.BinDir + "/norca"
	switch plan.Release.Source {
	case config.ReleaseHostPackage:
		return hostPackageBinaryStep(bin, plan.Release.Package), nil
	case config.ReleaseLocalPath:
		if boot {
			return step.Step{}, fmt.Errorf("install: boot needs a release the machine can download, not release.source local-path")
		}
		var verify string
		if sum := strings.ToLower(plan.Release.ChecksumSHA256); sum != "" {
			verify = "verify \"$NORCA_BINARY\" " + q(sum) + "\n"
		}
		return step.Step{
			Name: "norca binary",
			Apply: `[ -n "${NORCA_BINARY:-}" ] || die "release.source is local-path: set NORCA_BINARY to the norca binary or release archive (.tar.gz) for linux/` + arch + `"` + "\n" +
				`[ -f "$NORCA_BINARY" ] || die "NORCA_BINARY: $NORCA_BINARY is not a file"` + "\n" +
				verify +
				"case \"$NORCA_BINARY\" in\n" +
				"*.tar.gz | *.tgz) norca_src=$(extract_binary \"$NORCA_BINARY\" norca) ;;\n" +
				"*.zip) die \"NORCA_BINARY: a .zip release is for Windows\" ;;\n" +
				"*) norca_src=$NORCA_BINARY ;;\n" +
				"esac\n" +
				"put_file " + q(bin) + " 0755 <\"$norca_src\"",
		}, nil
	}
	d, err := res.ReleaseDownload(ctx, "linux", arch, libc)
	if err != nil {
		return step.Step{}, fmt.Errorf("norca release: %w", err)
	}
	marker := cliStateDir + "/norca-release.sha256"
	return step.Step{
		Name: "norca binary",
		// The marker names the release and the binary it gave.
		Check: "[ -f " + q(marker) + " ] && [ -f " + q(bin) + " ] &&\n  [ \"$(cat " + q(marker) + ")\" = \"" + d.SHA256 + " $(sha256_of " + q(bin) + ")\" ]",
		Apply: "fetch " + q(d.URL) + " \"$work/norca.tar.gz\"\n" +
			"verify \"$work/norca.tar.gz\" " + q(d.SHA256) + "\n" +
			"norca_src=$(extract_binary \"$work/norca.tar.gz\" norca)\n" +
			"put_file " + q(bin) + " 0755 <\"$norca_src\"\n" +
			"printf '%s %s\\n' " + q(d.SHA256) + " \"$(sha256_of " + q(bin) + ")\" >\"$work/norca-release\"\n" +
			"put_file " + q(marker) + " 0600 <\"$work/norca-release\"",
	}, nil
}

// machineCNIStep unpacks the CNI plugins on a machine: the archive the
// image build names (local-path) or the one the machine downloads and
// checks, unless the marker names the same archive.
func machineCNIStep(ctx context.Context, plan *config.DeploymentPlan, res *artifacts.Resolver, paths *storage.ResolvedPaths,
	arch string, boot bool) (step.Step, error) {
	q := connector.ShellQuote
	c := plan.CNI
	marker, dir := cniMarker(paths), paths.CNIBinDir
	name := "CNI plugins " + c.Version
	if c.Source == config.ReleaseLocalPath {
		if boot {
			return step.Step{}, fmt.Errorf("install: boot needs CNI plugins the machine can download, not cni.source local-path")
		}
		var verify string
		if sum := strings.ToLower(c.ChecksumSHA256); sum != "" {
			verify = "verify \"$NORCA_CNI_ARCHIVE\" " + q(sum) + "\n"
		}
		return step.Step{
			Name: name,
			Apply: `[ -n "${NORCA_CNI_ARCHIVE:-}" ] || die "cni.source is local-path: set NORCA_CNI_ARCHIVE to the CNI plugin archive (.tgz) for linux/` + arch + `"` + "\n" +
				`[ -f "$NORCA_CNI_ARCHIVE" ] || die "NORCA_CNI_ARCHIVE: $NORCA_CNI_ARCHIVE is not a file"` + "\n" +
				verify +
				"cni_sha=$(sha256_of \"$NORCA_CNI_ARCHIVE\")\n" +
				"if [ -f " + q(marker) + " ] && [ \"$(cat " + q(marker) + ")\" = \"$cni_sha\" ]; then\n" +
				"  log \"CNI plugins unchanged\"\n" +
				"else\n" +
				"  tar -xzf \"$NORCA_CNI_ARCHIVE\" -C " + q(dir) + "\n" +
				"  changed=1\n" +
				"  log " + q("CNI plugins installed into "+dir) + "\n" +
				"  printf '%s\\n' \"$cni_sha\" >\"$work/cni-marker\"\n" +
				"  put_file " + q(marker) + " 0644 <\"$work/cni-marker\"\n" +
				"fi",
		}, nil
	}
	d, err := res.CNIDownload(ctx, arch)
	if err != nil {
		return step.Step{}, fmt.Errorf("CNI plugins: %w", err)
	}
	return step.Step{
		Name:  name,
		Check: "[ -f " + q(marker) + " ] && [ \"$(cat " + q(marker) + ")\" = " + q(d.SHA256) + " ]",
		Apply: "fetch " + q(d.URL) + " \"$work/cni.tgz\"\n" +
			"verify \"$work/cni.tgz\" " + q(d.SHA256) + "\n" +
			"tar -xzf \"$work/cni.tgz\" -C " + q(dir) + "\n" +
			"log " + q("CNI plugins installed into "+dir) + "\n" +
			"printf '%s\\n' " + q(d.SHA256) + " >\"$work/cni-marker\"\n" +
			"put_file " + q(marker) + " 0644 <\"$work/cni-marker\"",
	}, nil
}
