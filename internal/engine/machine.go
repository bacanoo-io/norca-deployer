package engine

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"text/template"
	"unicode/utf8"

	"github.com/bacanoo-io/norca-deployer/internal/artifacts"
	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/crypto"
	"github.com/bacanoo-io/norca-deployer/internal/platform/packages"
	"github.com/bacanoo-io/norca-deployer/internal/platform/service"
	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
	"github.com/bacanoo-io/norca-deployer/internal/platform/tuning"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// The install script of a scaled machine: one bash script, generated from
// the plan, that puts on a machine what the bootstrap of the node scaling
// operator expects there: the norca binary, the CNI plugins, norca-join, the
// directories and the systemd unit of the agent, and the host packages and
// the sysctl tuning of a client. It runs when the image of the provider is
// built (node_scaling.machine.install = image) or at the first boot, from
// the bootstrap (boot). It does what prepareNode does on a client of the
// inventory, without the configuration, the certificates and the firewall:
// the bootstrap writes the first two, the cloud does the third.

//go:embed machine_install.sh.tmpl
var machineScriptText string

var machineScriptTemplate = template.Must(template.New("install").Option("missingkey=error").Funcs(template.FuncMap{
	"q":    connector.ShellQuote,
	"file": shellFile,
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

type machineDir struct {
	Path string
	Mode string
}

type machineDownload struct {
	Source  string
	Package string
	URL     string
	SHA256  string
}

type machineCNI struct {
	Version string
	Dir     string
	Marker  string
	Local   bool
	URL     string
	SHA256  string
}

type machineJoin struct {
	Path   string
	URL    string
	SHA256 string
}

type machineSysctl struct {
	ModulesPath string
	Modules     string
	SysctlPath  string
	Sysctl      string
}

type machineScriptData struct {
	Cluster, Region, Mode, Arch, Version string
	StateDir                             string
	Dirs                                 []machineDir
	NorcaBinary                          string
	Release                              machineDownload
	CNI                                  *machineCNI
	Join                                 machineJoin
	Packages                             string
	Sysctl                               *machineSysctl
	UnitPath, Unit, Service              string
}

// validMachineArch are the architectures of a scaled machine.
var validMachineArch = map[string]bool{"amd64": true, "arm64": true, "arm": true, "386": true}

// GenerateInstallScript returns the install script of a scaled Linux
// machine. The downloads it names are resolved with res: the URL and the
// SHA-256 of the release archive and of the CNI plugins, as the native
// install resolves them (a SHA256SUMS file of the release is downloaded when
// the plan has no checksum), but the files themselves are downloaded by the
// machine. The script is deterministic: the same plan gives the same script.
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

	node := &config.NodeConfig{Platform: "linux", Arch: arch}
	paths := storage.ResolveNodePaths(node, plan)
	data := &machineScriptData{
		Cluster: plan.Metadata.Name, Region: plan.Metadata.Region, Mode: mode, Arch: arch,
		Version:  plan.Release.Version,
		StateDir: cliStateDir,
		// The directories of a client, with the modes of prepareNode.
		Dirs: []machineDir{
			{paths.BinDir, "0755"}, {paths.ConfigDir, "0750"}, {paths.DataDir, "0750"},
			{paths.LogDir, "0750"}, {paths.TLSDir, "0700"},
			{paths.CNIBinDir, "0755"}, {paths.CNIConfigDir, "0755"},
		},
		NorcaBinary: paths.BinDir + "/norca",
		Release:     machineDownload{Source: plan.Release.Source, Package: plan.Release.Package},
		Join:        machineJoin{Path: machine.JoinBinaryPath()},
		UnitPath:    service.UnitPath,
		Unit:        service.LinuxUnit(paths, plan.Tuning.FileDescriptorLimit),
		Service:     "norca",
	}

	switch plan.Release.Source {
	case config.ReleaseOfficial, config.ReleaseCustomURL:
		d, err := res.ReleaseDownload(ctx, "linux", arch)
		if err != nil {
			return "", fmt.Errorf("norca release: %w", err)
		}
		data.Release.URL, data.Release.SHA256 = d.URL, d.SHA256
	case config.ReleaseLocalPath:
		if boot {
			return "", fmt.Errorf("install: boot needs a release the machine can download, not release.source local-path")
		}
		data.Release.SHA256 = strings.ToLower(plan.Release.ChecksumSHA256)
	}

	if c := plan.CNI; c != nil && c.Install != nil && *c.Install {
		cni := &machineCNI{Version: c.Version, Dir: paths.CNIBinDir, Marker: paths.CNIBinDir + "/.norca-deployer-cni.sha256"}
		if c.Source == config.ReleaseLocalPath {
			if boot {
				return "", fmt.Errorf("install: boot needs CNI plugins the machine can download, not cni.source local-path")
			}
			cni.Local = true
			cni.SHA256 = strings.ToLower(c.ChecksumSHA256)
		} else {
			d, err := res.CNIDownload(ctx, arch)
			if err != nil {
				return "", fmt.Errorf("CNI plugins: %w", err)
			}
			cni.URL, cni.SHA256 = d.URL, d.SHA256
		}
		data.CNI = cni
	}

	if machine != nil && machine.JoinDownload != nil {
		data.Join.URL = machine.JoinDownload.URL
		data.Join.SHA256 = strings.ToLower(machine.JoinDownload.ChecksumSHA256)
	}

	sel := packages.Select(plan.HostPackages, node, false)
	pkgs, err := machinePackages(plan, sel, arch, boot, func(v string) (string, error) { return resolveSecretWith(opts.Vault, v) })
	if err != nil {
		return "", err
	}
	data.Packages = pkgs

	if plan.Tuning.ApplySysctl {
		data.Sysctl = &machineSysctl{
			ModulesPath: tuning.ModulesConfigPath, Modules: tuning.RecommendedModules,
			SysctlPath: tuning.SysctlConfigPath, Sysctl: tuning.RecommendedSysctl,
		}
	}

	var b strings.Builder
	if err := machineScriptTemplate.Execute(&b, data); err != nil {
		return "", err
	}
	return b.String(), nil
}

// machinePackages renders what packages.Apply does for the host packages of
// a client: the keyring, the source, the pin and the credentials of each
// repository, apt-get install when a package is missing (or not at its
// version), the holds and the check of the installed versions. A package of a
// local .deb file cannot be downloaded by the machine: the script checks that
// it is installed (in the image). The password of a repository would travel
// in the user data at boot, so a repository with credentials needs install:
// image.
func machinePackages(plan *config.DeploymentPlan, sel *packages.Selection, arch string, boot bool, resolve func(string) (string, error)) (string, error) {
	if sel.Empty() {
		return "", nil
	}
	hp := plan.HostPackages
	q := connector.ShellQuote
	var b strings.Builder
	b.WriteString("apt_changed=0\n")
	if len(sel.Repositories) > 0 {
		b.WriteString("ensure_dir " + q(packages.KeyringsDir) + " 0755\n")
	}
	for i := range sel.Repositories {
		repo := &sel.Repositories[i]
		key, err := plan.RepositoryKey(repo)
		if err != nil {
			return "", err
		}
		armored := config.KeyIsArmored(key)
		keyring := packages.KeyringPath(repo, armored)
		fmt.Fprintf(&b, "# APT repository %s (%s %s)\n", repo.Name, oneLine(repo.URI), oneLine(repo.Suite))
		b.WriteString(shellFile(keyring, "0644", string(key)) + "\n")
		b.WriteString("[ \"$wrote\" = 0 ] || apt_changed=1\n")
		b.WriteString("remove_file " + q(packages.KeyringPath(repo, !armored)) + "\n")
		b.WriteString(shellFile(packages.SourcePath(repo), "0644", packages.RenderSource(repo, keyring)) + "\n")
		b.WriteString("[ \"$wrote\" = 0 ] || apt_changed=1\n")
		if repo.PinPriority != 0 {
			b.WriteString("ensure_dir " + q(packages.PreferencesDir) + " 0755\n")
			b.WriteString(shellFile(packages.PreferencesPath(repo), "0644", packages.RenderPreferences(repo)) + "\n")
			b.WriteString("[ \"$wrote\" = 0 ] || apt_changed=1\n")
		} else {
			b.WriteString("remove_file " + q(packages.PreferencesPath(repo)) + "\n")
		}
		if repo.Auth != nil {
			if boot {
				return "", fmt.Errorf("host_packages: repository %q has credentials (auth): at boot its password would travel in the user data of the machine; use node_scaling.machine.install: image", repo.Name)
			}
			password, err := resolve(repo.Auth.Password)
			if err != nil {
				return "", fmt.Errorf("repository %q: auth.password: %w", repo.Name, err)
			}
			b.WriteString("ensure_dir " + q(packages.AuthDir) + " 0755\n")
			b.WriteString(shellFile(packages.AuthPath(repo), "0600", packages.RenderAuth(repo, password)) + "\n")
			b.WriteString("[ \"$wrote\" = 0 ] || apt_changed=1\n")
		} else {
			b.WriteString("remove_file " + q(packages.AuthPath(repo)) + "\n")
		}
	}
	if len(sel.Packages) == 0 {
		return strings.TrimSuffix(b.String(), "\n"), nil
	}

	lockTimeout := hp.LockTimeout
	if lockTimeout <= 0 {
		lockTimeout = packages.DefaultLockTimeout
	}
	b.WriteString("# Packages\n")
	b.WriteString("need_install=$apt_changed\n")
	var targets []string
	for _, p := range sel.Packages {
		if p.File != "" {
			fmt.Fprintf(&b, "require_package %s %s %s\n", q(p.Name), q(p.Version), q(filepath.Base(plan.DebFile(&p, arch))))
			continue
		}
		fmt.Fprintf(&b, "check_package %s %s\n", q(p.Name), q(p.Version))
		spec := p.Name
		if p.Version != "" {
			spec += "=" + p.Version
		}
		targets = append(targets, spec)
	}
	if len(targets) > 0 {
		b.WriteString("if [ \"$need_install\" = 1 ]; then\n")
		if sel.NeedsIndex() && (hp.UpdateCache == nil || *hp.UpdateCache) {
			fmt.Fprintf(&b, "  apt-get update -q -o DPkg::Lock::Timeout=%d\n", lockTimeout)
		}
		b.WriteString("  " + packages.InstallCommand(targets, lockTimeout) + "\n")
		b.WriteString("  changed=1\nfi\n")
	}
	for _, p := range sel.Packages {
		if p.Hold {
			fmt.Fprintf(&b, "if ! is_held %[1]s; then\n  apt-mark hold %[1]s >/dev/null\n  changed=1\n  log \"package %[2]s held\"\nfi\n", q(p.Name), p.Name)
		} else {
			// Release a hold that an earlier plan set.
			fmt.Fprintf(&b, "if is_held %[1]s; then\n  apt-mark unhold %[1]s >/dev/null\n  changed=1\n  log \"package %[2]s released\"\nfi\n", q(p.Name), p.Name)
		}
	}
	for _, p := range sel.Packages {
		fmt.Fprintf(&b, "expect_package %s %s\n", q(p.Name), q(p.Version))
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// shellFile renders a put_file call that writes content to path: a quoted
// here-document for text, base64 for anything else (a binary key, a file
// without a final newline), so that the file is written byte for byte.
func shellFile(path, mode, s string) string {
	content := []byte(s)
	const eof = "NORCA_EOF"
	text := utf8.Valid(content) && bytes.HasSuffix(content, []byte("\n")) &&
		!bytes.ContainsAny(content, "\x00\r") && !bytes.Contains(append([]byte("\n"), content...), []byte("\n"+eof+"\n"))
	if text {
		return "put_file " + connector.ShellQuote(path) + " " + mode + " <<'" + eof + "'\n" + string(content) + eof
	}
	enc := base64.StdEncoding.EncodeToString(content)
	var lines []string
	for len(enc) > 76 {
		lines = append(lines, enc[:76])
		enc = enc[76:]
	}
	lines = append(lines, enc)
	return "put_file " + connector.ShellQuote(path) + " " + mode + " < <(base64 -d <<'" + eof + "'\n" +
		strings.Join(lines, "\n") + "\n" + eof + "\n)"
}

// oneLine keeps a value of the plan that goes into a comment on one line.
func oneLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}
