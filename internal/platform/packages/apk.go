package packages

import (
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/artifacts"
	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/platform/step"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// The files of the apk driver.
const (
	// ApkKeyDir holds the keys apk trusts; apk finds the key of a
	// signature by its file name.
	ApkKeyDir = "/etc/apk/keys"
	// ApkRepositoriesFile lists the repositories of the deployer, written
	// whole; apk 2.14 (Alpine 3.20 and newer) and apk 3 read the .list
	// files of /etc/apk/repositories.d next to /etc/apk/repositories.
	ApkRepositoriesFile = apkRepositoriesDir + "/norca.list"
	apkRepositoriesDir  = "/etc/apk/repositories.d"
	// ApkKeysRecord lists the keys the deployer installed, so that the
	// key of a repository the plan dropped can be removed.
	ApkKeysRecord = "/var/lib/norca-deployer/apk-keys"
	// ApkDir holds the .apk files copied by the deployer.
	ApkDir = "/var/lib/norca-deployer/apks"
	// apkWorld is the list of the packages apk keeps installed, with their
	// version constraints.
	apkWorld = "/etc/apk/world"
)

// ApkTag is the tag of a tagged repository: a package is taken from it only
// when it is named name@<tag>.
func ApkTag(repo *config.ApkRepository) string { return "norca-" + repo.Name }

// RenderApkRepositories renders the repositories file of the deployer: one
// line per repository, @norca-<name> before the URL of a tagged one.
func RenderApkRepositories(repos []config.ApkRepository) string {
	var sb strings.Builder
	sb.WriteString("# Managed by norca-deployer. Do not edit: the deployer writes this file whole.\n")
	for i := range repos {
		if repos[i].Tagged {
			sb.WriteString("@" + ApkTag(&repos[i]) + " ")
		}
		sb.WriteString(repos[i].URL + "\n")
	}
	return sb.String()
}

// SelectApk filters the repositories and packages of the apk block by the
// node's role and host.
func SelectApk(apk *config.ApkPackages, node *config.NodeConfig, isMaster bool) *Selection {
	sel := &Selection{}
	if apk == nil {
		return sel
	}
	for _, r := range apk.Repositories {
		if config.AppliesTo(r.Roles, nil, node, isMaster) {
			sel.ApkRepositories = append(sel.ApkRepositories, r)
		}
	}
	for _, p := range apk.Install {
		if config.AppliesTo(p.Roles, p.Hosts, node, isMaster) {
			sel.Packages = append(sel.Packages, p)
		}
	}
	return sel
}

// apkFunctions are the shell functions of the apk steps. They read the
// database of apk (/lib/apk/db/installed, the same in apk 2 and 3) and
// /etc/apk/world directly: no lock is needed for that.
const apkFunctions = `# apk_version NAME prints the installed version of a package.
apk_version() {
  awk -v p="P:$1" '$0 == p { f = 1; next } f && /^V:/ { print substr($0, 3); exit } /^$/ { f = 0 }' /lib/apk/db/installed 2>/dev/null
}

# apk_has NAME VERSION is true when the package is installed (at VERSION,
# when one is given).
apk_has() {
  apk_v=$(apk_version "$1")
  [ -n "$apk_v" ] && { [ -z "$2" ] || [ "$apk_v" = "$2" ]; }
}

# apk_world ENTRY is true when /etc/apk/world has the entry (name@tag=version).
apk_world() {
  grep -qxF -- "$1" ` + apkWorld + ` 2>/dev/null
}

# apk_field FILE FIELD prints a field of the .PKGINFO of an .apk file
# (pkgname, pkgver).
apk_field() {
  tar -xzOf "$1" .PKGINFO 2>/dev/null | sed -n "s/^$2 = //p" | head -n 1
}

# apk_check FILE NAME VERSION stops unless an .apk holds the package NAME
# (at VERSION, when one is given).
apk_check() {
  apk_name=$(apk_field "$1" pkgname)
  [ -n "$apk_name" ] || die "package $2: ${1##*/} is not an .apk file apk 2 or 3 builds (no .PKGINFO)"
  apk_fv=$(apk_field "$1" pkgver)
  [ "$apk_name" = "$2" ] || die "package $2: ${1##*/} contains package $apk_name"
  [ -z "$3" ] || [ "$apk_fv" = "$3" ] || die "package $2: ${1##*/} has version $apk_fv, expected $3"
}

# apk_expect NAME VERSION stops unless the package is installed (at
# VERSION, when one is given).
apk_expect() {
  apk_v=$(apk_version "$1")
  [ -n "$apk_v" ] || die "package $1 is not installed after apk add"
  [ -z "$2" ] || [ "$apk_v" = "$2" ] || die "package $1: version $apk_v installed, $2 requested"
  if apk_held "$1"; then
    log "package $1 $apk_v (held)"
  else
    log "package $1 $apk_v"
  fi
}

# apk_held NAME is true when the world entry of a package has a version
# constraint (name=1.2-r0, name@tag=1.2-r0, or name><checksum of a file).
apk_held() {
  awk -v n="$1" '{ e = $0; sub(/[=<>~].*/, "", e); sub(/@.*/, "", e) } e == n && /[=<>~]/ { h = 1 } END { exit !h }' ` + apkWorld + ` 2>/dev/null
}

# apk_require NAME VERSION FILE stops unless a package that the plan
# installs from a local .apk file is installed: the install script of a
# machine cannot reach a file on the deployer machine.
apk_require() {
  apk_has "$1" "$2" || die "package $1${2:+ $2} is installed from the local file $3 on the deployer machine: install it into the image first"
}

# apk_key_drop NAME removes a key of ` + ApkKeyDir + ` that an earlier plan
# installed, unless a package owns it (the keys of Alpine).
apk_key_drop() {
  apk_kf=` + ApkKeyDir + `/$1
  [ -e "$apk_kf" ] || return 0
  if apk info -q --who-owns "$apk_kf" >/dev/null 2>&1; then
    return 0
  fi
  rm -f "$apk_kf"
  echo "removed $apk_kf"
}
`

// Apk is the driver of apk (apk 2.14 and apk 3).
type Apk struct{}

var _ Manager = Apk{}

// Name implements Manager.
func (Apk) Name() string { return config.PackageManagerApk }

// Arch implements Manager: the names of Alpine.
func (Apk) Arch(goarch string) string { return config.ApkArch(goarch) }

// apkPackage is a package of the repositories in a step: its world entry
// as apk add takes it, and the entry /etc/apk/world must have.
type apkPackage struct {
	name, version string
	// spec is what the install asks for: name[@tag][=version].
	spec string
	// world is the entry of /etc/apk/world: the spec of a held package
	// (with the installed version when the plan names none), the name
	// without a version otherwise. A shell word.
	world string
	// fixup is the apk add argument that sets the world entry after the
	// install, "" when the install set it.
	fixup string
}

// apkPackageOf returns the world entries of a package of the repositories.
func apkPackageOf(p *config.HostPackage) apkPackage {
	q := connector.ShellQuote
	name := p.Name
	if p.Repository != "" {
		name += "@norca-" + p.Repository
	}
	ap := apkPackage{name: p.Name, version: p.Version, spec: name, world: q(name)}
	if p.Version != "" {
		ap.spec = name + "=" + p.Version
	}
	switch {
	case p.Hold && p.Version != "":
		ap.world = q(ap.spec)
	case p.Hold:
		// Held at the version apk installed.
		ap.world = "\"" + name + "=$(apk_version " + q(p.Name) + ")\""
		ap.fixup = ap.world
	case p.Version != "":
		// Installed at its version, then released: apk upgrade may move
		// it, as apt upgrade may move a package that is not held.
		ap.fixup = q(name)
	}
	return ap
}

// Steps implements Manager. The steps are: apk is there; the repositories
// file of the deployer and the keys of its repositories, and apk update
// when they changed; the keys of the repositories the plan dropped are
// removed (not a key a package owns); the local .apk files (copied and
// checked over SSH, required to be installed in the install script); apk
// add of the repository packages that are missing, not at their version or
// without their world entry (the version constraint in /etc/apk/world is
// the hold of apk); apk add of the local files; the check of the installed
// versions. Every apk call waits for the lock of apk at most
// host_packages.lock_timeout (apk --wait).
func (Apk) Steps(sel *Selection, c Context) ([]step.Step, error) {
	if sel.Empty() {
		return nil, nil
	}
	hp := c.Plan.HostPackages
	lockTimeout := hp.LockTimeout
	if lockTimeout <= 0 {
		lockTimeout = DefaultLockTimeout
	}
	apk := fmt.Sprintf("apk --wait %d", lockTimeout)
	q := connector.ShellQuote
	// A world entry of a tagged repository the node no longer has would
	// stop every apk add ("missing repository tags"): say so first.
	var tags []string
	for i := range sel.ApkRepositories {
		if sel.ApkRepositories[i].Tagged {
			tags = append(tags, ApkTag(&sel.ApkRepositories[i]))
		}
	}
	steps := []step.Step{{
		Name: "apk",
		Apply: "if ! command -v apk >/dev/null 2>&1; then\n  die \"host_packages.apk need a node with apk\"\nfi\n" +
			"for apk_tag in $(sed -n 's/^[^@=<>~]*@\\(norca-[a-z0-9-]*\\).*/\\1/p' " + apkWorld + " 2>/dev/null | sort -u); do\n" +
			"  case ' " + strings.Join(tags, " ") + " ' in\n" +
			"  *\" $apk_tag \"*) ;;\n" +
			"  *) die \"/etc/apk/world takes packages from the tagged repository @$apk_tag, which host_packages.apk no longer gives the node: keep the repository, or remove its packages (apk del) first\" ;;\n" +
			"  esac\ndone",
	}}
	repos, keyNames, err := apkRepositoriesStep(c, sel.ApkRepositories, apk)
	if err != nil {
		return nil, err
	}
	steps = append(steps, repos, droppedKeysStep(keyNames))
	if len(sel.Packages) == 0 {
		return steps, nil
	}

	var pkgs []apkPackage
	var files, fileHas, expect []string
	for i := range sel.Packages {
		p := &sel.Packages[i]
		if p.File == "" {
			pkgs = append(pkgs, apkPackageOf(p))
			expect = append(expect, "apk_expect "+q(p.Name)+" "+q(p.Version))
			continue
		}
		local := c.Plan.ApkFile(p, c.Arch)
		if c.Script {
			// The machine cannot reach the file: it must be in the image.
			steps = append(steps, step.Step{
				Name: "apk package " + p.Name, Functions: apkFunctions,
				Apply: "apk_require " + q(p.Name) + " " + q(p.Version) + " " + q(filepath.Base(local)),
			})
			expect = append(expect, "apk_expect "+q(p.Name)+" "+q(p.Version))
			continue
		}
		sum, err := artifacts.FileSHA256(local)
		if err != nil {
			return nil, fmt.Errorf("package %q: %w", p.Name, err)
		}
		if p.SHA256 != "" && !strings.EqualFold(sum, p.SHA256) {
			return nil, fmt.Errorf("package %q: %s has sha256 %s, expected %s", p.Name, local, sum, p.SHA256)
		}
		remote := path.Join(ApkDir, filepath.Base(local))
		steps = append(steps, step.Step{
			Name:      "apk package file " + filepath.Base(local),
			Dirs:      []step.Dir{{Path: ApkDir, Mode: 0o755}},
			Files:     []step.File{{Path: remote, Mode: 0o644, Local: local, SHA256: sum}},
			Functions: apkFunctions,
			Apply:     "apk_check " + q(remote) + " " + q(p.Name) + " " + q(p.Version),
		})
		files = append(files, q(remote))
		// The version of the file is the one to find installed.
		version := `"$(apk_field ` + q(remote) + ` pkgver)"`
		fileHas = append(fileHas, "apk_has "+q(p.Name)+" "+version)
		expect = append(expect, "apk_expect "+q(p.Name)+" "+version)
	}

	if len(pkgs) > 0 {
		var check, specs, fixups []string
		for _, p := range pkgs {
			check = append(check, "apk_has "+q(p.name)+" "+q(p.version), "apk_world "+p.world)
			specs = append(specs, q(p.spec))
			if p.fixup != "" {
				fixups = append(fixups, p.fixup)
			}
		}
		update := ""
		if sel.NeedsIndex() && (hp.UpdateCache == nil || *hp.UpdateCache) {
			update = " -U"
		}
		apply := apk + " add -q" + update + " " + strings.Join(specs, " ")
		if len(fixups) > 0 {
			// The world entries of the packages whose hold is not the
			// version of the install: held at the installed version, or
			// released after the install at a version.
			apply += "\n" + apk + " add -q " + strings.Join(fixups, " ")
		}
		steps = append(steps, step.Step{
			Name: "apk packages", Functions: apkFunctions,
			// apk add does not upgrade an installed package: install what is
			// missing, not at its version, or without its world entry.
			Check: strings.Join(check, " &&\n  "),
			Apply: apply,
		})
	}
	if len(files) > 0 {
		steps = append(steps, step.Step{
			Name: "apk package files", Functions: apkFunctions,
			Check: strings.Join(fileHas, " &&\n  "),
			// --allow-untrusted: the files are the plan's own, copied with
			// their digest (apk refuses a file that no key of
			// /etc/apk/keys signed). It holds for every package of the
			// call, so the call takes nothing from the network: the
			// dependencies of the files come from the repositories in the
			// step before.
			Apply: apk + " add -q --allow-untrusted --no-network " + strings.Join(files, " "),
		})
	}
	steps = append(steps, step.Step{Name: "apk versions", Functions: apkFunctions, Apply: strings.Join(expect, "\n")})
	return steps, nil
}

// apkRepositoriesStep writes the repositories file of the deployer and the
// keys of its repositories, and reads the indexes when they changed;
// without repositories it removes the file. It returns the names of the
// keys, for the step that removes the dropped ones.
func apkRepositoriesStep(c Context, repos []config.ApkRepository, apk string) (step.Step, []string, error) {
	if len(repos) == 0 {
		return step.Step{Name: "apk repositories", Remove: []string{ApkRepositoriesFile}}, nil, nil
	}
	s := step.Step{
		Name: "apk repositories",
		Dirs: []step.Dir{{Path: ApkKeyDir, Mode: 0o755}, {Path: apkRepositoriesDir, Mode: 0o755}},
		// Nothing to do unless a file changed.
		Check: "true",
		Apply: apk + " update >/dev/null\n" +
			"log " + connector.ShellQuote("repositories: indexes read"),
	}
	var names []string
	for i := range repos {
		key, name, err := c.Plan.ApkRepositoryKey(&repos[i])
		if err != nil {
			return step.Step{}, nil, err
		}
		if name == "" || slices.Contains(names, name) {
			continue
		}
		names = append(names, name)
		s.Files = append(s.Files, step.File{Path: ApkKeyDir + "/" + name, Mode: 0o644, Content: key})
	}
	s.Files = append(s.Files, step.File{Path: ApkRepositoriesFile, Mode: 0o644, Content: []byte(RenderApkRepositories(repos))})
	return s, names, nil
}

// droppedKeysStep removes the keys that an earlier plan installed and this
// one does not, unless a package owns them, and records the keys of this
// plan. A key is not named after the deployer (apk finds it by its own
// name), so the record says which keys are the deployer's.
func droppedKeysStep(keep []string) step.Step {
	var b strings.Builder
	b.WriteString("apk_keys_keep=" + connector.ShellQuote(strings.Join(keep, "\n")) + "\n")
	b.WriteString("if [ -f " + ApkKeysRecord + " ]; then\n")
	b.WriteString("  while IFS= read -r apk_key; do\n")
	b.WriteString("    [ -n \"$apk_key\" ] || continue\n")
	b.WriteString("    if printf '%s\\n' \"$apk_keys_keep\" | grep -qxF -- \"$apk_key\"; then\n      continue\n    fi\n")
	b.WriteString("    apk_key_drop \"$apk_key\"\n")
	b.WriteString("  done <" + ApkKeysRecord + "\nfi\n")
	if len(keep) == 0 {
		b.WriteString("rm -f " + ApkKeysRecord)
	} else {
		b.WriteString("mkdir -p " + path.Dir(ApkKeysRecord) + "\n")
		b.WriteString("printf '%s\\n' \"$apk_keys_keep\" >" + ApkKeysRecord)
	}
	return step.Step{Name: "apk keys the plan dropped", Lines: true, Functions: apkFunctions, Apply: b.String()}
}
