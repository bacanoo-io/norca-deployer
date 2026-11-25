// Package packages installs the host packages of a node with the package
// manager of the node: apt (Debian, Ubuntu), dnf (RHEL, Rocky Linux,
// AlmaLinux); none on a system without one.
// A driver returns steps (internal/platform/step), so that the SSH path and
// the install script of a scaled machine do the same.
package packages

import (
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/artifacts"
	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/platform/step"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

const (
	SourcesDir     = "/etc/apt/sources.list.d"
	KeyringsDir    = "/etc/apt/keyrings"
	PreferencesDir = "/etc/apt/preferences.d"
	AuthDir        = "/etc/apt/auth.conf.d"
	// DebDir holds the .deb files copied by the deployer.
	DebDir = "/var/lib/norca-deployer/debs"

	// DefaultLockTimeout is the dpkg lock wait in seconds without host_packages.lock_timeout.
	DefaultLockTimeout = 300
)

// Manager is the driver of a package manager.
type Manager interface {
	// Name is the package manager: apt, dnf, none.
	Name() string
	// Arch maps a GOARCH name to the {arch} of a package file of this
	// manager.
	Arch(goarch string) string
	// Steps installs a selection: the repositories (key, source, pin,
	// credentials), the cache refresh, the packages, the holds and the
	// check of the installed versions.
	Steps(sel *Selection, c Context) ([]step.Step, error)
}

// Resolver decrypts inline secrets.
type Resolver func(string) (string, error)

// Context is what the steps of a selection depend on.
type Context struct {
	Plan *config.DeploymentPlan
	// Arch is the GOARCH of the node, for the {arch} of package files.
	Arch    string
	Resolve Resolver
	// Script makes the steps of the install script of a scaled machine: a
	// package of a local file cannot be copied there and is only checked.
	Script bool
}

// SourcePath is the deb822 source file of a repository.
func SourcePath(repo *config.AptRepository) string {
	return fmt.Sprintf("%s/norca-%s.sources", SourcesDir, repo.Name)
}

// KeyringPath is the keyring of a repository, referenced by Signed-By. APT
// reads armored keys from .asc files and binary keys from .gpg files.
func KeyringPath(repo *config.AptRepository, armored bool) string {
	ext := ".gpg"
	if armored {
		ext = ".asc"
	}
	return fmt.Sprintf("%s/norca-%s%s", KeyringsDir, repo.Name, ext)
}

// PreferencesPath is the APT pin of a repository.
func PreferencesPath(repo *config.AptRepository) string {
	return fmt.Sprintf("%s/norca-%s.pref", PreferencesDir, repo.Name)
}

// AuthPath is the credentials file of a repository.
func AuthPath(repo *config.AptRepository) string {
	return fmt.Sprintf("%s/norca-%s.conf", AuthDir, repo.Name)
}

// RenderSource renders the deb822 source of a repository.
func RenderSource(repo *config.AptRepository, keyring string) string {
	var sb strings.Builder
	sb.WriteString("# Managed by norca-deployer. Do not edit.\n")
	sb.WriteString("Types: deb\n")
	sb.WriteString(fmt.Sprintf("URIs: %s\n", repo.URI))
	sb.WriteString(fmt.Sprintf("Suites: %s\n", repo.Suite))
	if len(repo.Components) > 0 {
		sb.WriteString(fmt.Sprintf("Components: %s\n", strings.Join(repo.Components, " ")))
	}
	if len(repo.Architectures) > 0 {
		sb.WriteString(fmt.Sprintf("Architectures: %s\n", strings.Join(repo.Architectures, " ")))
	}
	sb.WriteString(fmt.Sprintf("Signed-By: %s\n", keyring))
	return sb.String()
}

// RenderPreferences pins the packages of a repository by its release fields
// (pin_release), or else by its origin host. An origin pin applies to every
// repository on the same host.
func RenderPreferences(repo *config.AptRepository) string {
	host := repo.URI
	if u, err := url.Parse(repo.URI); err == nil {
		host = u.Hostname()
	}
	pkgs := "*"
	if len(repo.PinPackages) > 0 {
		pkgs = strings.Join(repo.PinPackages, " ")
	}
	pin := fmt.Sprintf("origin %q", host)
	if repo.PinRelease != "" {
		pin = "release " + repo.PinRelease
	}
	return fmt.Sprintf("# Managed by norca-deployer. Do not edit.\nPackage: %s\nPin: %s\nPin-Priority: %d\n",
		pkgs, pin, repo.PinPriority)
}

// RenderAuth renders an apt_auth.conf entry. The machine is the URI without
// its scheme, so that the credentials only apply below it.
func RenderAuth(repo *config.AptRepository, password string) string {
	machine := repo.URI
	if u, err := url.Parse(repo.URI); err == nil {
		machine = u.Host + strings.TrimRight(u.Path, "/")
	}
	return fmt.Sprintf("# Managed by norca-deployer. Do not edit.\nmachine %s\nlogin %s\npassword %s\n",
		machine, repo.Auth.Username, password)
}

// Selection is the part of the block of one package manager that applies
// to one node: its repositories (apt or dnf) and its packages.
type Selection struct {
	Repositories    []config.AptRepository
	DnfRepositories []config.DnfRepository
	Packages        []config.HostPackage
}

// Empty reports whether nothing is to be done on the node.
func (s *Selection) Empty() bool {
	return len(s.Repositories) == 0 && len(s.DnfRepositories) == 0 && len(s.Packages) == 0
}

// BlockManagers are the package managers that have a block in
// host_packages.
var BlockManagers = []string{config.PackageManagerApt, config.PackageManagerDnf}

// Block returns what the block of a package manager selects for a node, and
// whether the plan has that block at all.
func Block(hp *config.HostPackages, manager string, node *config.NodeConfig, isMaster bool) (*Selection, bool) {
	if hp == nil {
		return &Selection{}, false
	}
	switch manager {
	case config.PackageManagerApt:
		return Select(hp.Apt, node, isMaster), hp.Apt != nil
	case config.PackageManagerDnf:
		return SelectDnf(hp.Dnf, node, isMaster), hp.Dnf != nil
	}
	return &Selection{}, false
}

// Installs reports whether the selection installs a package.
func (s *Selection) Installs(name string) bool {
	for _, p := range s.Packages {
		if p.Name == name {
			return true
		}
	}
	return false
}

// NeedsIndex reports whether the package manager has to read repository
// indexes: true when a repository is configured or a package is not a
// local file. Nodes without any reachable repository can install local
// package files only.
func (s *Selection) NeedsIndex() bool {
	if len(s.Repositories) > 0 || len(s.DnfRepositories) > 0 {
		return true
	}
	for _, p := range s.Packages {
		if p.File == "" {
			return true
		}
	}
	return false
}

// Select filters the repositories and packages of the apt block by the
// node's role and host.
func Select(apt *config.AptPackages, node *config.NodeConfig, isMaster bool) *Selection {
	sel := &Selection{}
	if apt == nil {
		return sel
	}
	for _, r := range apt.Repositories {
		if config.AppliesTo(r.Roles, nil, node, isMaster) {
			sel.Repositories = append(sel.Repositories, r)
		}
	}
	for _, p := range apt.Install {
		if config.AppliesTo(p.Roles, p.Hosts, node, isMaster) {
			sel.Packages = append(sel.Packages, p)
		}
	}
	return sel
}

// InstallCommand builds the apt-get command line. targets are package specs
// (name or name=version) or absolute paths of local .deb files; they are
// validated by the configuration loader and quoted here as well.
func InstallCommand(targets []string, lockTimeout int) string {
	if lockTimeout <= 0 {
		lockTimeout = DefaultLockTimeout
	}
	args := []string{
		"DEBIAN_FRONTEND=noninteractive",
		"apt-get", "install", "-y", "-q",
		"--no-install-recommends",
		// Packages held by an earlier apply are upgraded on purpose when
		// their pinned version changes.
		"--allow-change-held-packages",
		"-o", fmt.Sprintf("DPkg::Lock::Timeout=%d", lockTimeout),
		"-o", "Dpkg::Options::=--force-confdef",
		"-o", "Dpkg::Options::=--force-confold",
	}
	for _, t := range targets {
		args = append(args, connector.ShellQuote(t))
	}
	return strings.Join(args, " ")
}

// aptFunctions are the shell functions of the apt steps.
const aptFunctions = `# installed_version NAME prints the version of an installed Debian package.
installed_version() {
  # shellcheck disable=SC2016 # dpkg-query fields, not shell variables
  dpkg-query -W -f='${Status} ${Version}\n' "$1" 2>/dev/null | awk '$3 == "installed" { print $4 }'
}

# apt_has NAME VERSION is true when the package is installed (at VERSION,
# when one is given).
apt_has() {
  apt_v=$(installed_version "$1")
  [ -n "$apt_v" ] && { [ -z "$2" ] || [ "$apt_v" = "$2" ]; }
}

# deb_field FILE FIELD prints a field of a .deb file.
deb_field() {
  dpkg-deb -f "$1" "$2" 2>/dev/null
}

# deb_check FILE NAME VERSION stops unless a .deb holds the package NAME
# (at VERSION, when one is given).
deb_check() {
  deb_name=$(deb_field "$1" Package) || die "package $2: ${1##*/} is not a valid .deb"
  deb_version=$(deb_field "$1" Version)
  [ "$deb_name" = "$2" ] || die "package $2: ${1##*/} contains package $deb_name"
  [ -z "$3" ] || [ "$deb_version" = "$3" ] || die "package $2: ${1##*/} has version $deb_version, expected $3"
}

# expect_package NAME VERSION stops unless the package is installed (at
# VERSION, when one is given).
expect_package() {
  apt_v=$(installed_version "$1")
  [ -n "$apt_v" ] || die "package $1 is not installed after apt-get install"
  [ -z "$2" ] || [ "$apt_v" = "$2" ] || die "package $1: version $apt_v installed, $2 requested"
  log "package $1 $apt_v"
}

# require_package NAME VERSION FILE stops unless a package that the plan
# installs from a local .deb file is installed: the install script of a
# machine cannot reach a file on the deployer machine.
require_package() {
  apt_has "$1" "$2" || die "package $1${2:+ $2} is installed from the local file $3 on the deployer machine: install it into the image first"
}

# is_held NAME reports whether apt holds a package.
is_held() {
  apt-mark showhold | grep -qxF -- "$1"
}
`

// Apt is the driver of apt.
type Apt struct{}

var _ Manager = Apt{}

// Name implements Manager.
func (Apt) Name() string { return config.PackageManagerApt }

// Arch implements Manager: the Debian names.
func (Apt) Arch(goarch string) string { return config.DebArch(goarch) }

// Steps implements Manager. The steps are: apt is there; per repository its
// files (keyring, source, pin, credentials; the files of a setting the plan
// dropped are removed); the local .deb files (copied and checked over SSH,
// required to be installed in the install script); apt-get update and
// install (every time while update_cache is on, so that a package without a
// version follows its candidate; else only when a package is missing or not
// at its version); the holds; the check of the installed versions.
func (Apt) Steps(sel *Selection, c Context) ([]step.Step, error) {
	if sel.Empty() {
		return nil, nil
	}
	hp := c.Plan.HostPackages
	steps := []step.Step{{
		Name: "apt",
		Apply: "if ! command -v apt-get >/dev/null 2>&1 || ! command -v dpkg-query >/dev/null 2>&1; then\n" +
			"  die \"host_packages need a Debian-based node with apt-get and dpkg\"\nfi",
	}}
	for i := range sel.Repositories {
		s, err := repositoryStep(c, &sel.Repositories[i])
		if err != nil {
			return nil, err
		}
		steps = append(steps, s)
	}
	if len(sel.Packages) == 0 {
		return steps, nil
	}
	lockTimeout := hp.LockTimeout
	if lockTimeout <= 0 {
		lockTimeout = DefaultLockTimeout
	}
	q := connector.ShellQuote

	var targets, has, expect []string
	for i := range sel.Packages {
		p := &sel.Packages[i]
		if p.File == "" {
			spec := p.Name
			if p.Version != "" {
				spec += "=" + p.Version
			}
			targets = append(targets, spec)
			has = append(has, "apt_has "+q(p.Name)+" "+q(p.Version))
			expect = append(expect, "expect_package "+q(p.Name)+" "+q(p.Version))
			continue
		}
		local := c.Plan.DebFile(p, c.Arch)
		if c.Script {
			// The machine cannot reach the file: it must be in the image.
			steps = append(steps, step.Step{
				Name: "apt package " + p.Name, Functions: aptFunctions,
				Apply: "require_package " + q(p.Name) + " " + q(p.Version) + " " + q(filepath.Base(local)),
			})
			expect = append(expect, "expect_package "+q(p.Name)+" "+q(p.Version))
			continue
		}
		sum, err := artifacts.FileSHA256(local)
		if err != nil {
			return nil, fmt.Errorf("package %q: %w", p.Name, err)
		}
		if p.SHA256 != "" && !strings.EqualFold(sum, p.SHA256) {
			return nil, fmt.Errorf("package %q: %s has sha256 %s, expected %s", p.Name, local, sum, p.SHA256)
		}
		remote := path.Join(DebDir, filepath.Base(local))
		steps = append(steps, step.Step{
			Name:      "apt package file " + filepath.Base(local),
			Dirs:      []step.Dir{{Path: DebDir, Mode: 0o755}},
			Files:     []step.File{{Path: remote, Mode: 0o644, Local: local, SHA256: sum}},
			Functions: aptFunctions,
			Apply:     "deb_check " + q(remote) + " " + q(p.Name) + " " + q(p.Version),
		})
		targets = append(targets, remote)
		// The version of the file is the one to find installed.
		has = append(has, "apt_has "+q(p.Name)+` "$(deb_field `+q(remote)+` Version)"`)
		expect = append(expect, "expect_package "+q(p.Name)+` "$(deb_field `+q(remote)+` Version)"`)
	}

	if len(targets) > 0 {
		var apply strings.Builder
		if sel.NeedsIndex() && (hp.UpdateCache == nil || *hp.UpdateCache) {
			fmt.Fprintf(&apply, "apt-get update -q -o DPkg::Lock::Timeout=%d\n", lockTimeout)
		}
		apply.WriteString(InstallCommand(targets, lockTimeout))
		s := step.Step{Name: "apt packages", Functions: aptFunctions, Apply: apply.String()}
		if hp.UpdateCache != nil && !*hp.UpdateCache {
			// Without a cache refresh the candidates do not change: install
			// only what is missing or not at its version.
			s.Check = strings.Join(has, " &&\n  ")
		}
		steps = append(steps, s)
	}

	var holds strings.Builder
	for _, p := range sel.Packages {
		if p.Hold {
			fmt.Fprintf(&holds, "if ! is_held %[1]s; then\n  apt-mark hold %[1]s >/dev/null\n  log \"package %[2]s held\"\nfi\n", q(p.Name), p.Name)
		} else {
			// Release a hold that an earlier plan set.
			fmt.Fprintf(&holds, "if is_held %[1]s; then\n  apt-mark unhold %[1]s >/dev/null\n  log \"package %[2]s released\"\nfi\n", q(p.Name), p.Name)
		}
	}
	steps = append(steps,
		step.Step{Name: "apt holds", Functions: aptFunctions, Apply: holds.String()},
		step.Step{Name: "apt versions", Functions: aptFunctions, Apply: strings.Join(expect, "\n")},
	)
	return steps, nil
}

// repositoryStep writes the files of a repository and removes those of the
// settings the plan dropped.
func repositoryStep(c Context, repo *config.AptRepository) (step.Step, error) {
	key, err := c.Plan.RepositoryKey(repo)
	if err != nil {
		return step.Step{}, err
	}
	armored := config.KeyIsArmored(key)
	keyring := KeyringPath(repo, armored)
	s := step.Step{
		Name: "apt repository " + repo.Name,
		Dirs: []step.Dir{{Path: KeyringsDir, Mode: 0o755}},
		Files: []step.File{
			{Path: keyring, Mode: 0o644, Content: key},
			{Path: SourcePath(repo), Mode: 0o644, Content: []byte(RenderSource(repo, keyring))},
		},
		// The keyring of the other format an earlier key left.
		Remove: []string{KeyringPath(repo, !armored)},
	}
	if repo.PinPriority != 0 {
		s.Dirs = append(s.Dirs, step.Dir{Path: PreferencesDir, Mode: 0o755})
		s.Files = append(s.Files, step.File{Path: PreferencesPath(repo), Mode: 0o644, Content: []byte(RenderPreferences(repo))})
	} else {
		s.Remove = append(s.Remove, PreferencesPath(repo))
	}
	if repo.Auth == nil {
		s.Remove = append(s.Remove, AuthPath(repo))
		return s, nil
	}
	password := repo.Auth.Password
	if c.Resolve != nil {
		if password, err = c.Resolve(password); err != nil {
			return step.Step{}, fmt.Errorf("repository %q: auth.password: %w", repo.Name, err)
		}
	}
	s.Secret = true
	s.Name = "apt repository " + repo.Name + " (with credentials)"
	s.Dirs = append(s.Dirs, step.Dir{Path: AuthDir, Mode: 0o755})
	s.Files = append(s.Files, step.File{Path: AuthPath(repo), Mode: 0o600, Content: []byte(RenderAuth(repo, password))})
	return s, nil
}

// None is the driver of a node without a package manager (an immutable
// system, or host_facts.package_manager none): it installs nothing.
type None struct{}

var _ Manager = None{}

// Name implements Manager.
func (None) Name() string { return config.PackageManagerNone }

// Arch implements Manager.
func (None) Arch(goarch string) string { return goarch }

// Steps implements Manager: a selection that is not empty is an error.
func (None) Steps(sel *Selection, _ Context) ([]step.Step, error) {
	if sel.Empty() {
		return nil, nil
	}
	return nil, fmt.Errorf("the node has no package manager, but host_packages selects packages for it")
}

// Unsupported is the driver of a package manager the deployer does not drive
// yet (apk): a selection that is not empty is an error.
type Unsupported struct{ Manager string }

// Name implements Manager.
func (u Unsupported) Name() string { return u.Manager }

// Arch implements Manager.
func (Unsupported) Arch(goarch string) string { return goarch }

// Steps implements Manager.
func (u Unsupported) Steps(sel *Selection, _ Context) ([]step.Step, error) {
	if sel.Empty() {
		return nil, nil
	}
	return nil, fmt.Errorf("the package manager %s is not supported yet", u.Manager)
}
