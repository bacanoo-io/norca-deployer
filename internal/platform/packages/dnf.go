package packages

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/artifacts"
	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/platform/step"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// The files of the dnf driver.
const (
	RepoDir   = "/etc/yum.repos.d"
	RPMKeyDir = "/etc/pki/rpm-gpg"
	// RPMDir holds the .rpm files copied by the deployer.
	RPMDir = "/var/lib/norca-deployer/rpms"
	// VersionlockPlugin is the package of the versionlock command of dnf 4
	// (RHEL 9, RHEL 10); the versionlock of dnf5 is a command of dnf5.
	VersionlockPlugin = "python3-dnf-plugin-versionlock"
)

// DnfRepoID is the id of a repository in its file and on the command line
// of dnf.
func DnfRepoID(repo *config.DnfRepository) string { return "norca-" + repo.Name }

// RepoFilePath is the repository file of a repository.
func RepoFilePath(repo *config.DnfRepository) string {
	return fmt.Sprintf("%s/%s.repo", RepoDir, DnfRepoID(repo))
}

// RPMKeyPath is the key of a repository, named by gpgkey=.
func RPMKeyPath(repo *config.DnfRepository) string {
	return fmt.Sprintf("%s/%s.asc", RPMKeyDir, DnfRepoID(repo))
}

// RenderRepoFile renders the repository file: gpgcheck with the key of the
// repository, its priority and package filters, and its credentials
// (password is the resolved password; the file is then 0600).
func RenderRepoFile(repo *config.DnfRepository, password string) string {
	var sb strings.Builder
	sb.WriteString("# Managed by norca-deployer. Do not edit.\n")
	fmt.Fprintf(&sb, "[%s]\n", DnfRepoID(repo))
	fmt.Fprintf(&sb, "name=norca-deployer: %s\n", repo.Name)
	if repo.BaseURL != "" {
		fmt.Fprintf(&sb, "baseurl=%s\n", repo.BaseURL)
	} else {
		fmt.Fprintf(&sb, "metalink=%s\n", repo.Metalink)
	}
	sb.WriteString("enabled=1\n")
	sb.WriteString("gpgcheck=1\n")
	fmt.Fprintf(&sb, "gpgkey=file://%s\n", RPMKeyPath(repo))
	if repo.Priority != 0 {
		fmt.Fprintf(&sb, "priority=%d\n", repo.Priority)
	}
	if len(repo.Include) > 0 {
		fmt.Fprintf(&sb, "includepkgs=%s\n", strings.Join(repo.Include, " "))
	}
	if len(repo.Exclude) > 0 {
		fmt.Fprintf(&sb, "excludepkgs=%s\n", strings.Join(repo.Exclude, " "))
	}
	if repo.Auth != nil {
		fmt.Fprintf(&sb, "username=%s\npassword=%s\n", repo.Auth.Username, password)
	}
	return sb.String()
}

// SelectDnf filters the repositories and packages of the dnf block by the
// node's role and host.
func SelectDnf(dnf *config.DnfPackages, node *config.NodeConfig, isMaster bool) *Selection {
	sel := &Selection{}
	if dnf == nil {
		return sel
	}
	for _, r := range dnf.Repositories {
		if config.AppliesTo(r.Roles, nil, node, isMaster) {
			sel.DnfRepositories = append(sel.DnfRepositories, r)
		}
	}
	for _, p := range dnf.Install {
		if config.AppliesTo(p.Roles, p.Hosts, node, isMaster) {
			sel.Packages = append(sel.Packages, p)
		}
	}
	return sel
}

// DnfInstallCommand builds the dnf command line. targets are package specs
// (name or name-[epoch:]version-release) or absolute paths of local .rpm
// files; they are validated by the configuration loader and quoted here
// as well. Weak dependencies are not installed (as apt installs no
// recommended packages); refresh expires the metadata first.
func DnfInstallCommand(targets []string, refresh bool) string {
	args := []string{"dnf", "-y", "-q", "--setopt=install_weak_deps=False"}
	if refresh {
		args = append(args, "--refresh")
	}
	args = append(args, "install")
	for _, t := range targets {
		args = append(args, connector.ShellQuote(t))
	}
	return strings.Join(args, " ")
}

// dnfFunctions are the shell functions of the dnf steps. The versionlock
// commands are those of dnf 4 (its plugin) and dnf5 alike, with every
// repository disabled: they work on the installed packages, offline.
const dnfFunctions = `# dnf_busy prints another dnf, yum or rpm process, and is false when none
# runs.
dnf_busy() {
  for dnf_proc in /proc/[0-9]*; do
    { read -r dnf_comm <"$dnf_proc/comm"; } 2>/dev/null || continue
    case $dnf_comm in
    dnf | dnf-3 | dnf4 | dnf5 | yum | rpm | dnf-automatic)
      echo "$dnf_comm (process ${dnf_proc#/proc/})"
      return 0
      ;;
    esac
  done
  return 1
}

# dnf_wait SECONDS waits until no other dnf, yum or rpm process runs, at
# most SECONDS: dnf 4 waits for its own lock without an end, and dnf and
# rpm stop at once on the lock of the rpm database.
dnf_wait() {
  dnf_waited=0
  while dnf_holder=$(dnf_busy); do
    [ "$dnf_waited" -lt "$1" ] || die "another package manager process, $dnf_holder, still runs after $1 seconds (host_packages.lock_timeout)"
    [ "$dnf_waited" -gt 0 ] || log "waiting for $dnf_holder to finish"
    sleep 2
    dnf_waited=$((dnf_waited + 2))
  done
}

# rpm_versions NAME prints the installed versions of a package as
# EPOCH:VERSION-RELEASE, one line per installed architecture.
rpm_versions() {
  rpm -q --qf '%{EPOCHNUM}:%{VERSION}-%{RELEASE}\n' "$1" 2>/dev/null | sed -n '/^[0-9][0-9]*:[^ ]*$/p'
}

# rpm_match VERSIONS WANTED is true when one of the versions
# (EPOCH:VERSION-RELEASE lines) is WANTED; a WANTED without an epoch matches
# the version of any epoch.
rpm_match() {
  case $2 in
  *:*) printf '%s\n' "$1" | grep -qxF -- "$2" ;;
  *) printf '%s\n' "$1" | sed 's/^[0-9]*://' | grep -qxF -- "$2" ;;
  esac
}

# rpm_has NAME VERSION is true when the package is installed (at VERSION,
# when one is given).
rpm_has() {
  rpm_v=$(rpm_versions "$1")
  [ -n "$rpm_v" ] && { [ -z "$2" ] || rpm_match "$rpm_v" "$2"; }
}

# rpm_field FILE FORMAT prints fields of an .rpm file (a query format of rpm).
rpm_field() {
  rpm -qp --qf "$2" "$1" 2>/dev/null
}

# rpm_check FILE NAME VERSION stops unless an .rpm holds the package NAME
# (at VERSION, when one is given).
rpm_check() {
  rpm_name=$(rpm_field "$1" '%{NAME}') || die "package $2: ${1##*/} is not a valid .rpm"
  rpm_fv=$(rpm_field "$1" '%{EPOCHNUM}:%{VERSION}-%{RELEASE}')
  [ "$rpm_name" = "$2" ] || die "package $2: ${1##*/} contains package $rpm_name"
  [ -z "$3" ] || rpm_match "$rpm_fv" "$3" || die "package $2: ${1##*/} has version $rpm_fv, expected $3"
}

# dnf_expect NAME VERSION stops unless the package is installed (at
# VERSION, when one is given).
dnf_expect() {
  rpm_v=$(rpm_versions "$1")
  [ -n "$rpm_v" ] || die "package $1 is not installed after dnf install"
  [ -z "$2" ] || rpm_match "$rpm_v" "$2" || die "package $1: version $(printf '%s' "$rpm_v" | tr '\n' ' ') installed, $2 requested"
  log "package $1 $(printf '%s\n' "$rpm_v" | sed -n '1s/^0://p')"
}

# dnf_require NAME VERSION FILE stops unless a package that the plan
# installs from a local .rpm file is installed: the install script of a
# machine cannot reach a file on the deployer machine.
dnf_require() {
  rpm_has "$1" "$2" || die "package $1${2:+ $2} is installed from the local file $3 on the deployer machine: install it into the image first"
}

# dnf_versionlock_ok is true when dnf has its versionlock command (dnf5, or
# dnf 4 with its plugin).
dnf_versionlock_ok() {
  dnf -q --disablerepo='*' versionlock list >/dev/null 2>&1
}

# dnf_locks prints the names of the packages a versionlock entry holds:
# dnf 4 lists NAME-EPOCH:VERSION-RELEASE.*, dnf5 "Package name: NAME". It
# prints nothing without the versionlock command.
dnf_locks() {
  dnf -q --disablerepo='*' versionlock list 2>/dev/null | awk '
    /^Package name: / { print $3; next }
    /^[#!]/ || NF != 1 { next }
    {
      n = split($1, part, "-")
      if (n < 3) next
      name = part[1]
      for (i = 2; i <= n - 2; i++) name = name "-" part[i]
      print name
    }'
}

# dnf_held NAME is true when the package is in the list of dnf_locks that
# the caller put into dnf_held_list.
dnf_held() {
  printf '%s\n' "$dnf_held_list" | grep -qxF -- "$1"
}

# dnf_lock NAME holds a package at its installed version.
dnf_lock() {
  dnf -q --disablerepo='*' versionlock add "$1" >/dev/null
}

# dnf_unlock NAME removes the versionlock entries of a package.
dnf_unlock() {
  dnf -q --disablerepo='*' versionlock delete "$1" >/dev/null
}
`

// Dnf is the driver of dnf (dnf 4 and dnf5).
type Dnf struct{}

var _ Manager = Dnf{}

// Name implements Manager.
func (Dnf) Name() string { return config.PackageManagerDnf }

// Arch implements Manager: the RPM names.
func (Dnf) Arch(goarch string) string { return config.RPMArch(goarch) }

// Steps implements Manager. The steps are: dnf is there; per repository its
// key and file, and the metadata of the repository when they changed; the
// files of the repositories the plan no longer gives the node are removed;
// the local .rpm files (copied and checked over SSH, required to be
// installed in the install script); dnf install of the packages that are
// missing or not at their version (dnf install does not upgrade an
// installed package, so a package without a version is installed once);
// the versionlock command when a package is held; the holds; the check of
// the installed versions. Every dnf call waits for the other package
// manager processes first, at most host_packages.lock_timeout.
func (Dnf) Steps(sel *Selection, c Context) ([]step.Step, error) {
	if sel.Empty() {
		return nil, nil
	}
	hp := c.Plan.HostPackages
	lockTimeout := hp.LockTimeout
	if lockTimeout <= 0 {
		lockTimeout = DefaultLockTimeout
	}
	wait := fmt.Sprintf("dnf_wait %d\n", lockTimeout)
	steps := []step.Step{{
		Name: "dnf",
		Apply: "if ! command -v dnf >/dev/null 2>&1 || ! command -v rpm >/dev/null 2>&1; then\n" +
			"  die \"host_packages.dnf need a node with dnf and rpm\"\nfi",
	}}
	for i := range sel.DnfRepositories {
		s, err := dnfRepositoryStep(c, &sel.DnfRepositories[i], wait)
		if err != nil {
			return nil, err
		}
		steps = append(steps, s)
	}
	steps = append(steps, droppedRepositoriesStep(sel.DnfRepositories))
	if len(sel.Packages) == 0 {
		return steps, nil
	}
	q := connector.ShellQuote

	// installs are the packages of dnf install: the quoted name and the
	// shell word of the version to find installed.
	type install struct{ name, version string }
	var targets, has, expect []string
	var installs []install
	for i := range sel.Packages {
		p := &sel.Packages[i]
		if p.File == "" {
			spec := p.Name
			if p.Version != "" {
				spec += "-" + p.Version
			}
			targets = append(targets, spec)
			has = append(has, "rpm_has "+q(p.Name)+" "+q(p.Version))
			expect = append(expect, "dnf_expect "+q(p.Name)+" "+q(p.Version))
			installs = append(installs, install{q(p.Name), q(p.Version)})
			continue
		}
		local := c.Plan.RPMFile(p, c.Arch)
		if c.Script {
			// The machine cannot reach the file: it must be in the image.
			steps = append(steps, step.Step{
				Name: "dnf package " + p.Name, Functions: dnfFunctions,
				Apply: "dnf_require " + q(p.Name) + " " + q(p.Version) + " " + q(filepath.Base(local)),
			})
			expect = append(expect, "dnf_expect "+q(p.Name)+" "+q(p.Version))
			continue
		}
		sum, err := artifacts.FileSHA256(local)
		if err != nil {
			return nil, fmt.Errorf("package %q: %w", p.Name, err)
		}
		if p.SHA256 != "" && !strings.EqualFold(sum, p.SHA256) {
			return nil, fmt.Errorf("package %q: %s has sha256 %s, expected %s", p.Name, local, sum, p.SHA256)
		}
		remote := path.Join(RPMDir, filepath.Base(local))
		steps = append(steps, step.Step{
			Name:      "dnf package file " + filepath.Base(local),
			Dirs:      []step.Dir{{Path: RPMDir, Mode: 0o755}},
			Files:     []step.File{{Path: remote, Mode: 0o644, Local: local, SHA256: sum}},
			Functions: dnfFunctions,
			Apply:     "rpm_check " + q(remote) + " " + q(p.Name) + " " + q(p.Version),
		})
		targets = append(targets, remote)
		// The version of the file is the one to find installed.
		version := `"$(rpm_field ` + q(remote) + ` '%{EPOCHNUM}:%{VERSION}-%{RELEASE}')"`
		has = append(has, "rpm_has "+q(p.Name)+" "+version)
		expect = append(expect, "dnf_expect "+q(p.Name)+" "+version)
		installs = append(installs, install{q(p.Name), version})
	}

	if len(targets) > 0 {
		var apply strings.Builder
		apply.WriteString(wait)
		// A versionlock entry hides the other versions of a package (dnf5
		// even the one of a local file): the entries of the packages that
		// change are removed before, and the holds step sets them again.
		apply.WriteString("dnf_held_list=$(dnf_locks)\n")
		for _, in := range installs {
			fmt.Fprintf(&apply, "if ! rpm_has %[1]s %[2]s && dnf_held %[1]s; then\n  dnf_unlock %[1]s\nfi\n", in.name, in.version)
		}
		refresh := sel.NeedsIndex() && (hp.UpdateCache == nil || *hp.UpdateCache)
		apply.WriteString(DnfInstallCommand(targets, refresh))
		steps = append(steps, step.Step{
			Name: "dnf packages", Functions: dnfFunctions,
			// dnf install does not upgrade: install what is missing or not
			// at its version.
			Check: strings.Join(has, " &&\n  "),
			Apply: apply.String(),
		})
	}

	var holds strings.Builder
	anyHold := false
	holds.WriteString("dnf_held_list=$(dnf_locks)\n")
	for _, p := range sel.Packages {
		if p.Hold {
			anyHold = true
			fmt.Fprintf(&holds, "if ! dnf_held %[1]s; then\n  dnf_lock %[1]s\n  log \"package %[2]s held\"\nfi\n", q(p.Name), p.Name)
		} else {
			// Release a hold that an earlier plan set.
			fmt.Fprintf(&holds, "if dnf_held %[1]s; then\n  dnf_unlock %[1]s\n  log \"package %[2]s released\"\nfi\n", q(p.Name), p.Name)
		}
	}
	if anyHold {
		steps = append(steps, step.Step{
			Name:      "dnf versionlock",
			Functions: dnfFunctions,
			Check:     "dnf_versionlock_ok",
			Apply: wait +
				"if dnf --version 2>/dev/null | head -n 1 | grep -q '^dnf5'; then\n" +
				"  dnf -y -q install 'dnf5-command(versionlock)'\nelse\n" +
				"  dnf -y -q install " + VersionlockPlugin + "\nfi\n" +
				"dnf_versionlock_ok || die \"dnf has no versionlock command after the install of its plugin\"\n" +
				"log \"versionlock of dnf installed\"",
		})
	}
	steps = append(steps,
		step.Step{Name: "dnf holds", Functions: dnfFunctions, Apply: wait + holds.String()},
		step.Step{Name: "dnf versions", Functions: dnfFunctions, Apply: strings.Join(expect, "\n")},
	)
	return steps, nil
}

// dnfRepositoryStep writes the key and the file of a repository and reads
// its metadata when they changed.
func dnfRepositoryStep(c Context, repo *config.DnfRepository, wait string) (step.Step, error) {
	key, err := c.Plan.DnfRepositoryKey(repo)
	if err != nil {
		return step.Step{}, err
	}
	s := step.Step{
		Name:      "dnf repository " + repo.Name,
		Dirs:      []step.Dir{{Path: RPMKeyDir, Mode: 0o755}, {Path: RepoDir, Mode: 0o755}},
		Functions: dnfFunctions,
		// Nothing to do unless a file changed.
		Check: "true",
		Apply: wait + "dnf -q makecache --repo=" + connector.ShellQuote(DnfRepoID(repo)) + " >/dev/null\n" +
			"log " + connector.ShellQuote("repository "+repo.Name+": metadata read"),
	}
	mode, password := os.FileMode(0o644), ""
	if repo.Auth != nil {
		password = repo.Auth.Password
		if c.Resolve != nil {
			if password, err = c.Resolve(password); err != nil {
				return step.Step{}, fmt.Errorf("repository %q: auth.password: %w", repo.Name, err)
			}
		}
		if strings.ContainsAny(password, "\r\n") {
			return step.Step{}, fmt.Errorf("repository %q: auth.password must be one line", repo.Name)
		}
		// The credentials are in the repository file.
		mode = 0o600
		s.Secret = true
		s.Name += " (with credentials)"
	}
	s.Files = []step.File{
		{Path: RPMKeyPath(repo), Mode: 0o644, Content: key},
		{Path: RepoFilePath(repo), Mode: mode, Content: []byte(RenderRepoFile(repo, password))},
	}
	return s, nil
}

// droppedRepositoriesStep removes the repository files (and keys) of the
// deployer that the plan no longer gives the node, so that a dropped
// repository does not stay enabled.
func droppedRepositoriesStep(keep []config.DnfRepository) step.Step {
	var b strings.Builder
	b.WriteString("for f in " + RepoDir + "/norca-*.repo " + RPMKeyDir + "/norca-*.asc; do\n")
	b.WriteString("  [ -e \"$f\" ] || continue\n")
	if len(keep) > 0 {
		names := make([]string, 0, 2*len(keep))
		for i := range keep {
			names = append(names, path.Base(RepoFilePath(&keep[i])), path.Base(RPMKeyPath(&keep[i])))
		}
		b.WriteString("  case ${f##*/} in\n  " + strings.Join(names, " | ") + ") continue ;;\n  esac\n")
	}
	b.WriteString("  rm -f \"$f\"\n  echo \"removed $f\"\ndone")
	return step.Step{Name: "dnf repositories the plan dropped", Lines: true, Apply: b.String()}
}
