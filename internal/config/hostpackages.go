package config

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	// Debian policy: package names are lower case alphanumerics and + - .
	debPackageNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
	// Debian policy: [epoch:]upstream[-revision]
	debVersionRe = regexp.MustCompile(`^([0-9]+:)?[0-9][A-Za-z0-9.+~-]*$`)
	repoNameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	aptWordRe    = regexp.MustCompile(`^[A-Za-z0-9._/~+-]+$`)
	scriptNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	envNameRe    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// APT preference package patterns: names, globs and /regexps/.
	aptPinPatternRe = regexp.MustCompile(`^[A-Za-z0-9*?+.:/^$\[\]-]+$`)
	aptPinReleaseRe = regexp.MustCompile(`^[a-zA-Z]=[A-Za-z0-9._ ~+-]+(,[a-zA-Z]=[A-Za-z0-9._ ~+-]+)*$`)
)

// BaseDir is the directory of the configuration file the plan was loaded from.
func (p *DeploymentPlan) BaseDir() string {
	if p.baseDir == "" {
		return "."
	}
	return p.baseDir
}

// ResolveLocalPath resolves a path on the deployer machine relative to the
// configuration file.
func (p *DeploymentPlan) ResolveLocalPath(path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(p.BaseDir(), path)
}

// AppliesTo reports whether a role/host filter selects the node.
func AppliesTo(roles, hosts []string, node *NodeConfig, isMaster bool) bool {
	if len(roles) > 0 {
		role := RoleClient
		if isMaster {
			role = RoleMaster
		}
		found := false
		for _, r := range roles {
			if r == role {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if len(hosts) > 0 {
		for _, h := range hosts {
			if h == node.Host || (node.Hostname != "" && h == node.Hostname) {
				return true
			}
		}
		return false
	}
	return true
}

// RepositoryKey returns the key of a repository, ASCII-armored or binary.
func (p *DeploymentPlan) RepositoryKey(r *AptRepository) ([]byte, error) {
	if r.Key != "" {
		return []byte(r.Key), nil
	}
	data, err := os.ReadFile(p.ResolveLocalPath(r.KeyFile))
	if err != nil {
		return nil, fmt.Errorf("repository %q: failed to read key file: %w", r.Name, err)
	}
	return data, nil
}

// KeyIsArmored reports whether a key is ASCII-armored. APT reads armored
// keyrings with the .asc extension and binary ones with .gpg.
func KeyIsArmored(key []byte) bool {
	return bytes.Contains(key, []byte("-----BEGIN PGP PUBLIC KEY BLOCK-----"))
}

// keyIsBinaryOpenPGP checks the first packet header of a binary key: the
// high bit is always set, and the tag must be a public key (6).
func keyIsBinaryOpenPGP(key []byte) bool {
	if len(key) < 2 || key[0]&0x80 == 0 {
		return false
	}
	var tag byte
	if key[0]&0x40 != 0 {
		tag = key[0] & 0x3f // new format
	} else {
		tag = (key[0] & 0x3c) >> 2 // old format
	}
	return tag == 6
}

// DebFile resolves the local .deb of a package for a node architecture.
func (p *DeploymentPlan) DebFile(pkg *HostPackage, arch string) string {
	return p.ResolveLocalPath(strings.ReplaceAll(pkg.File, "{arch}", DebArch(arch)))
}

// DebArch maps a Go architecture to the Debian architecture name.
func DebArch(goArch string) string {
	switch goArch {
	case "arm":
		return "armhf"
	case "386":
		return "i386"
	default:
		return goArch
	}
}

// ScriptBody returns the content of a script.
func (p *DeploymentPlan) ScriptBody(s *ScriptConfig) (string, error) {
	if s.Inline != "" {
		return s.Inline, nil
	}
	data, err := os.ReadFile(p.ResolveLocalPath(s.Path))
	if err != nil {
		return "", fmt.Errorf("script %q: failed to read %s: %w", s.Name, s.Path, err)
	}
	return string(data), nil
}

func validateRoles(what string, roles []string) error {
	for _, r := range roles {
		if r != RoleMaster && r != RoleClient {
			return fmt.Errorf("%s: unknown role %q (allowed: %s, %s)", what, r, RoleMaster, RoleClient)
		}
	}
	return nil
}

func (p *DeploymentPlan) validateHostPackages() error {
	hp := p.HostPackages
	if hp == nil {
		return nil
	}
	if hp.LockTimeout < 0 {
		return fmt.Errorf("host_packages.lock_timeout must not be negative")
	}

	repoNames := map[string]bool{}
	for i := range hp.Repositories {
		r := &hp.Repositories[i]
		what := fmt.Sprintf("host_packages.repositories[%d]", i)
		if !repoNameRe.MatchString(r.Name) {
			return fmt.Errorf("%s: name %q must be lower case letters, digits and '-'", what, r.Name)
		}
		if repoNames[r.Name] {
			return fmt.Errorf("%s: duplicate repository name %q", what, r.Name)
		}
		repoNames[r.Name] = true

		u, err := url.Parse(r.URI)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("%s: uri %q must be an http(s) URL", what, r.URI)
		}
		if u.User != nil {
			return fmt.Errorf("%s: put credentials into auth, not into the uri (the source file is world-readable)", what)
		}
		if strings.ContainsAny(r.URI, " \t\n") {
			return fmt.Errorf("%s: uri must not contain whitespace", what)
		}
		if !aptWordRe.MatchString(r.Suite) {
			return fmt.Errorf("%s: suite %q is missing or invalid", what, r.Suite)
		}
		flat := strings.HasSuffix(r.Suite, "/")
		if flat && len(r.Components) > 0 {
			return fmt.Errorf("%s: a flat repository (suite ending in '/') takes no components", what)
		}
		if !flat && len(r.Components) == 0 {
			return fmt.Errorf("%s: components are required (e.g. [main])", what)
		}
		for _, c := range append(append([]string{}, r.Components...), r.Architectures...) {
			if !aptWordRe.MatchString(c) {
				return fmt.Errorf("%s: invalid component or architecture %q", what, c)
			}
		}
		// Unsigned repositories are not supported: every source gets its own
		// Signed-By keyring.
		if (r.Key == "") == (r.KeyFile == "") {
			return fmt.Errorf("%s: exactly one of key or key_file is required", what)
		}
		key, err := p.RepositoryKey(r)
		if err != nil {
			return err
		}
		if !KeyIsArmored(key) && !keyIsBinaryOpenPGP(key) {
			return fmt.Errorf("%s: the key must be an OpenPGP public key (ASCII-armored or binary)", what)
		}
		if r.PinPriority < -32768 || r.PinPriority > 32767 {
			return fmt.Errorf("%s: pin_priority must be between -32768 and 32767", what)
		}
		if len(r.PinPackages) > 0 && r.PinPriority == 0 {
			return fmt.Errorf("%s: pin_packages needs pin_priority", what)
		}
		if r.PinRelease != "" && !aptPinReleaseRe.MatchString(r.PinRelease) {
			return fmt.Errorf("%s: pin_release %q must be key=value pairs like n=bookworm-backports", what, r.PinRelease)
		}
		for _, pp := range r.PinPackages {
			if !aptPinPatternRe.MatchString(pp) {
				return fmt.Errorf("%s: invalid pin_packages entry %q", what, pp)
			}
		}
		if a := r.Auth; a != nil {
			if a.Username == "" || a.Password == "" {
				return fmt.Errorf("%s: auth needs username and password", what)
			}
			if strings.ContainsAny(a.Username, " \t\n") {
				return fmt.Errorf("%s: auth.username must not contain whitespace", what)
			}
		}
		if err := validateRoles(what, r.Roles); err != nil {
			return err
		}
	}

	pkgNames := map[string]bool{}
	for i := range hp.Install {
		pkg := &hp.Install[i]
		what := fmt.Sprintf("host_packages.install[%d]", i)
		if !debPackageNameRe.MatchString(pkg.Name) {
			return fmt.Errorf("%s: invalid Debian package name %q", what, pkg.Name)
		}
		if pkgNames[pkg.Name] {
			return fmt.Errorf("%s: package %q is listed more than once", what, pkg.Name)
		}
		pkgNames[pkg.Name] = true
		if pkg.Version != "" && !debVersionRe.MatchString(pkg.Version) {
			return fmt.Errorf("%s: invalid Debian version %q", what, pkg.Version)
		}
		if pkg.SHA256 != "" {
			if pkg.File == "" {
				return fmt.Errorf("%s: sha256 needs file", what)
			}
			if strings.Contains(pkg.File, "{arch}") {
				return fmt.Errorf("%s: sha256 cannot be used with an {arch} file pattern", what)
			}
			if !sha256Re.MatchString(strings.ToLower(pkg.SHA256)) {
				return fmt.Errorf("%s: sha256 must be 64 hex characters", what)
			}
		}
		if pkg.File != "" {
			if !strings.HasSuffix(pkg.File, ".deb") {
				return fmt.Errorf("%s: file %q must be a .deb", what, pkg.File)
			}
			// Every architecture of the selected Linux nodes needs its file.
			for _, arch := range p.archesFor(pkg.Roles, pkg.Hosts) {
				if _, err := os.Stat(p.DebFile(pkg, arch)); err != nil {
					return fmt.Errorf("%s: %w", what, err)
				}
			}
		}
		if err := validateRoles(what, pkg.Roles); err != nil {
			return err
		}
	}
	return nil
}

// archesFor returns the architectures of the Linux nodes a filter selects.
func (p *DeploymentPlan) archesFor(roles, hosts []string) []string {
	set := map[string]bool{}
	add := func(nodes []NodeConfig, isMaster bool) {
		for i := range nodes {
			n := &nodes[i]
			if n.Platform == "linux" && AppliesTo(roles, hosts, n, isMaster) {
				set[n.Arch] = true
			}
		}
	}
	add(p.Inventory.Masters, true)
	add(p.Inventory.Clients, false)
	var res []string
	for a := range set {
		res = append(res, a)
	}
	return res
}

func (p *DeploymentPlan) validateScripts() error {
	names := map[string]bool{}
	for i := range p.Scripts {
		s := &p.Scripts[i]
		what := fmt.Sprintf("scripts[%d]", i)
		if !scriptNameRe.MatchString(s.Name) {
			return fmt.Errorf("%s: name %q must be letters, digits, '_', '.' or '-'", what, s.Name)
		}
		if names[s.Name] {
			return fmt.Errorf("%s: duplicate script name %q", what, s.Name)
		}
		names[s.Name] = true

		switch s.Phase {
		case PhasePreInstall, PhasePostPackages, PhasePostInstall:
		default:
			return fmt.Errorf("%s: phase %q must be one of %s, %s, %s", what, s.Phase,
				PhasePreInstall, PhasePostPackages, PhasePostInstall)
		}
		if (s.Path == "") == (s.Inline == "") {
			return fmt.Errorf("%s: exactly one of path or inline is required", what)
		}
		if _, err := p.ScriptBody(s); err != nil {
			return err
		}
		for k := range s.Env {
			if !envNameRe.MatchString(k) {
				return fmt.Errorf("%s: invalid environment variable name %q", what, k)
			}
		}
		if s.Timeout < 0 {
			return fmt.Errorf("%s: timeout must not be negative", what)
		}
		if err := validateRoles(what, s.Roles); err != nil {
			return err
		}
	}
	return nil
}
