package plan

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
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

	// RPM package names: letters, digits and . _ + -, not starting with
	// a punctuation character.
	rpmPackageNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	// [epoch:]version-release: neither the version nor the release has a
	// '-'.
	rpmVersionRe = regexp.MustCompile(`^([0-9]+:)?[A-Za-z0-9._+~^]+-[A-Za-z0-9._+~^]+$`)
	// includepkgs and excludepkgs entries: names and globs.
	rpmPatternRe = regexp.MustCompile(`^[A-Za-z0-9._+*?\[\]-]+$`)

	// apk package names: letters, digits and . _ + -, not starting with a
	// punctuation character (no @, =, < or >: they are the syntax of a
	// world entry).
	apkPackageNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)
	// The versions of apk: numbers, an optional letter, suffixes
	// (_alpha1, _rc2, _git20240101, _p3), and the release -rN.
	apkVersionRe = regexp.MustCompile(`^[0-9]+(\.[0-9]+)*[a-z]?(_(alpha|beta|pre|rc|cvs|svn|git|hg|p)[0-9]*)*-r[0-9]+$`)
	// The file name of an apk key in /etc/apk/keys, by which apk finds the
	// key of a signature (e.g. alpine-devel@lists.alpinelinux.org-6165ee59.rsa.pub).
	apkKeyNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._+-]*\.pub$`)
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
		found := slices.Contains(roles, role)
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

// DnfRepositoryKey returns the key of a dnf repository (ASCII-armored).
func (p *DeploymentPlan) DnfRepositoryKey(r *DnfRepository) ([]byte, error) {
	if r.Key != "" {
		return []byte(r.Key), nil
	}
	data, err := os.ReadFile(p.ResolveLocalPath(r.KeyFile))
	if err != nil {
		return nil, fmt.Errorf("repository %q: failed to read key file: %w", r.Name, err)
	}
	return data, nil
}

// ApkRepositoryKey returns the RSA key of an apk repository and the name it
// is installed under in /etc/apk/keys (the base name of key_file, or
// key_name); no key and "" for a repository signed by a key apk has.
func (p *DeploymentPlan) ApkRepositoryKey(r *ApkRepository) ([]byte, string, error) {
	switch {
	case r.Key != "":
		return []byte(r.Key), r.KeyName, nil
	case r.KeyFile == "":
		return nil, "", nil
	}
	data, err := os.ReadFile(p.ResolveLocalPath(r.KeyFile))
	if err != nil {
		return nil, "", fmt.Errorf("repository %q: failed to read key file: %w", r.Name, err)
	}
	return data, filepath.Base(r.KeyFile), nil
}

// isRSAPublicKey reports whether a key is an RSA public key in PEM, the
// form of the keys of apk.
func isRSAPublicKey(key []byte) bool {
	block, _ := pem.Decode(key)
	if block == nil || block.Type != "PUBLIC KEY" {
		return false
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return false
	}
	_, ok := pub.(*rsa.PublicKey)
	return ok
}

// ApkFile resolves the local .apk of a package for a node architecture.
func (p *DeploymentPlan) ApkFile(pkg *HostPackage, arch string) string {
	return p.ResolveLocalPath(strings.ReplaceAll(pkg.File, "{arch}", ApkArch(arch)))
}

// ApkArch maps a Go architecture to the architecture name of Alpine.
func ApkArch(goArch string) string {
	switch goArch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "arm":
		return "armv7"
	case "386":
		return "x86"
	default:
		return goArch
	}
}

// DebFile resolves the local .deb of a package for a node architecture.
func (p *DeploymentPlan) DebFile(pkg *HostPackage, arch string) string {
	return p.ResolveLocalPath(strings.ReplaceAll(pkg.File, "{arch}", DebArch(arch)))
}

// RPMFile resolves the local .rpm of a package for a node architecture.
func (p *DeploymentPlan) RPMFile(pkg *HostPackage, arch string) string {
	return p.ResolveLocalPath(strings.ReplaceAll(pkg.File, "{arch}", RPMArch(arch)))
}

// RPMArch maps a Go architecture to the RPM architecture name.
func RPMArch(goArch string) string {
	switch goArch {
	case "amd64":
		return "x86_64"
	case "arm64":
		return "aarch64"
	case "arm":
		return "armv7hl"
	case "386":
		return "i686"
	default:
		return goArch
	}
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

// The interpreters of a script.
const (
	InterpreterBash = "bash"
	InterpreterSh   = "sh"
)

// FamilyWindows is the family of a Windows node, for the families of a
// script.
const FamilyWindows = "windows"

// ScriptInterpreter returns the interpreter of a script on Linux: bash
// when the plan names none.
func ScriptInterpreter(s *ScriptConfig) string {
	if s.Interpreter == "" {
		return InterpreterBash
	}
	return s.Interpreter
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

// AptPackages returns the apt block of host_packages, or nil.
func (p *DeploymentPlan) AptPackages() *AptPackages {
	if p.HostPackages == nil {
		return nil
	}
	return p.HostPackages.Apt
}

// DnfPackages returns the dnf block of host_packages, or nil.
func (p *DeploymentPlan) DnfPackages() *DnfPackages {
	if p.HostPackages == nil {
		return nil
	}
	return p.HostPackages.Dnf
}

// ApkPackages returns the apk block of host_packages, or nil.
func (p *DeploymentPlan) ApkPackages() *ApkPackages {
	if p.HostPackages == nil {
		return nil
	}
	return p.HostPackages.Apk
}

// repositoryKeys are the YAML keys of the repositories of a block, for the
// error of a key that belongs to another package manager (suite in dnf,
// baseurl in apt, tagged in apt).
type repositoryKeys struct {
	manager string
	keys    map[string]bool
}

var (
	aptRepositoryKeys = repositoryKeys{"apt", yamlKeys(reflect.TypeFor[AptRepository]())}
	dnfRepositoryKeys = repositoryKeys{"dnf", yamlKeys(reflect.TypeFor[DnfRepository]())}
	apkRepositoryKeys = repositoryKeys{"apk", yamlKeys(reflect.TypeFor[ApkRepository]())}
)

// yamlKeys returns the YAML keys of the fields of a struct.
func yamlKeys(t reflect.Type) map[string]bool {
	keys := map[string]bool{}
	for f := range t.Fields() {
		if name, _, _ := strings.Cut(f.Tag.Get("yaml"), ","); name != "" && name != "-" {
			keys[name] = true
		}
	}
	return keys
}

// checkKeys refuses the keys of a mapping that the type of what does not
// have; a key of the repositories of another manager is named as such.
func checkKeys(node *yaml.Node, what string, own repositoryKeys, others ...repositoryKeys) error {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		k := node.Content[i]
		if own.keys[k.Value] {
			continue
		}
		for _, o := range others {
			if o.keys[k.Value] {
				return fmt.Errorf("line %d: %s is a key of the repositories of %s, not of %s", k.Line, k.Value, o.manager, what)
			}
		}
		return fmt.Errorf("line %d: unknown key %q in %s", k.Line, k.Value, what)
	}
	return nil
}

// UnmarshalYAML reads an apt repository and refuses a key it does not
// have: a dnf key in the apt block is an error of the plan, not a key that
// is dropped.
func (r *AptRepository) UnmarshalYAML(node *yaml.Node) error {
	if err := checkKeys(node, "an apt repository (host_packages.apt)", aptRepositoryKeys, dnfRepositoryKeys, apkRepositoryKeys); err != nil {
		return err
	}
	type plain AptRepository
	return node.Decode((*plain)(r))
}

// UnmarshalYAML reads a dnf repository and refuses a key it does not have.
func (r *DnfRepository) UnmarshalYAML(node *yaml.Node) error {
	if err := checkKeys(node, "a dnf repository (host_packages.dnf)", dnfRepositoryKeys, aptRepositoryKeys, apkRepositoryKeys); err != nil {
		return err
	}
	type plain DnfRepository
	return node.Decode((*plain)(r))
}

// UnmarshalYAML reads an apk repository and refuses a key it does not have.
func (r *ApkRepository) UnmarshalYAML(node *yaml.Node) error {
	if err := checkKeys(node, "an apk repository (host_packages.apk)", apkRepositoryKeys, aptRepositoryKeys, dnfRepositoryKeys); err != nil {
		return err
	}
	type plain ApkRepository
	return node.Decode((*plain)(r))
}

func (p *DeploymentPlan) validateHostPackages() error {
	hp := p.HostPackages
	if hp == nil {
		return nil
	}
	if hp.MovedRepositories != nil || hp.MovedInstall != nil {
		return fmt.Errorf("host_packages.repositories and host_packages.install moved into host_packages.apt: " +
			"the packages of the nodes whose package manager is apt (Debian, Ubuntu) are written as " +
			"host_packages: {apt: {repositories: [...], install: [...]}}")
	}
	if hp.LockTimeout < 0 {
		return fmt.Errorf("host_packages.lock_timeout must not be negative")
	}
	if hp.Apt != nil {
		if err := p.validateAptPackages(hp.Apt); err != nil {
			return err
		}
	}
	if hp.Dnf != nil {
		if err := p.validateDnfPackages(hp.Dnf); err != nil {
			return err
		}
	}
	if hp.Apk != nil {
		return p.validateApkPackages(hp.Apk)
	}
	return nil
}

// validateAptPackages checks the apt block with the rules of Debian: the
// names and versions of Debian policy, deb822 sources, OpenPGP keys and
// .deb files.
func (p *DeploymentPlan) validateAptPackages(apt *AptPackages) error {
	repoNames := map[string]bool{}
	for i := range apt.Repositories {
		r := &apt.Repositories[i]
		what := fmt.Sprintf("host_packages.apt.repositories[%d]", i)
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

	return p.validatePackageList(apt.Install, packageRules{
		block: "apt", kind: "Debian", nameRe: debPackageNameRe, versionRe: debVersionRe,
		ext: ".deb", file: p.DebFile,
	})
}

// packageRules are the rules of the packages of one block.
type packageRules struct {
	block     string // apt, dnf, apk
	kind      string // Debian, RPM, apk: in the errors
	nameRe    *regexp.Regexp
	versionRe *regexp.Regexp
	version   string // the form of a version, for the error
	ext       string // the extension of a local package file
	file      func(pkg *HostPackage, arch string) string
}

// validatePackageList checks the packages of a block with the rules of its
// format.
func (p *DeploymentPlan) validatePackageList(list []HostPackage, rules packageRules) error {
	pkgNames := map[string]bool{}
	for i := range list {
		pkg := &list[i]
		what := fmt.Sprintf("host_packages.%s.install[%d]", rules.block, i)
		if !rules.nameRe.MatchString(pkg.Name) {
			return fmt.Errorf("%s: invalid %s package name %q", what, rules.kind, pkg.Name)
		}
		if pkgNames[pkg.Name] {
			return fmt.Errorf("%s: package %q is listed more than once", what, pkg.Name)
		}
		pkgNames[pkg.Name] = true
		if pkg.Repository != "" && rules.block != "apk" {
			return fmt.Errorf("%s: repository names a tagged repository of apk; the packages of %s are taken from its repositories by name", what, rules.block)
		}
		if pkg.Version != "" && !rules.versionRe.MatchString(pkg.Version) {
			return fmt.Errorf("%s: invalid %s version %q%s", what, rules.kind, pkg.Version, rules.version)
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
			if !strings.HasSuffix(pkg.File, rules.ext) {
				return fmt.Errorf("%s: file %q must be a %s", what, pkg.File, rules.ext)
			}
			// Every architecture of the selected Linux nodes needs its file.
			for _, arch := range p.archesFor(pkg.Roles, pkg.Hosts) {
				if _, err := os.Stat(rules.file(pkg, arch)); err != nil {
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

// validateDnfPackages checks the dnf block with the rules of RPM: package
// names, [epoch:]version-release, repository files with an OpenPGP key and
// gpgcheck, and .rpm files.
func (p *DeploymentPlan) validateDnfPackages(dnf *DnfPackages) error {
	repoNames := map[string]bool{}
	for i := range dnf.Repositories {
		r := &dnf.Repositories[i]
		what := fmt.Sprintf("host_packages.dnf.repositories[%d]", i)
		if !repoNameRe.MatchString(r.Name) {
			return fmt.Errorf("%s: name %q must be lower case letters, digits and '-'", what, r.Name)
		}
		if repoNames[r.Name] {
			return fmt.Errorf("%s: duplicate repository name %q", what, r.Name)
		}
		repoNames[r.Name] = true

		if (r.BaseURL == "") == (r.Metalink == "") {
			return fmt.Errorf("%s: exactly one of baseurl or metalink is required", what)
		}
		for key, v := range map[string]string{"baseurl": r.BaseURL, "metalink": r.Metalink} {
			if v == "" {
				continue
			}
			u, err := url.Parse(v)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return fmt.Errorf("%s: %s %q must be an http(s) URL", what, key, v)
			}
			if u.User != nil {
				return fmt.Errorf("%s: put credentials into auth, not into the %s", what, key)
			}
			if strings.ContainsAny(v, " \t\r\n") {
				return fmt.Errorf("%s: %s must not contain whitespace", what, key)
			}
		}
		// Unsigned repositories are not supported: every repository gets
		// gpgcheck=1 with its own key.
		if (r.Key == "") == (r.KeyFile == "") {
			return fmt.Errorf("%s: exactly one of key or key_file is required", what)
		}
		key, err := p.DnfRepositoryKey(r)
		if err != nil {
			return err
		}
		if !KeyIsArmored(key) {
			return fmt.Errorf("%s: the key must be an ASCII-armored OpenPGP public key (rpm imports no binary keys)", what)
		}
		if r.Priority < 0 || r.Priority > 99 {
			return fmt.Errorf("%s: priority must be between 1 and 99 (0: not set)", what)
		}
		for _, pat := range slices.Concat(r.Include, r.Exclude) {
			if !rpmPatternRe.MatchString(pat) {
				return fmt.Errorf("%s: invalid include or exclude entry %q (package names and globs)", what, pat)
			}
		}
		if a := r.Auth; a != nil {
			if a.Username == "" || a.Password == "" {
				return fmt.Errorf("%s: auth needs username and password", what)
			}
			if strings.ContainsAny(a.Username, " \t\r\n") {
				return fmt.Errorf("%s: auth.username must not contain whitespace", what)
			}
			if strings.ContainsAny(a.Password, "\r\n") {
				return fmt.Errorf("%s: auth.password must be one line", what)
			}
		}
		if err := validateRoles(what, r.Roles); err != nil {
			return err
		}
	}
	return p.validatePackageList(dnf.Install, packageRules{
		block: "dnf", kind: "RPM", nameRe: rpmPackageNameRe, versionRe: rpmVersionRe,
		version: " (want [epoch:]version-release, e.g. 1.7.27-3.1.el9)", ext: ".rpm", file: p.RPMFile,
	})
}

// validateApkPackages checks the apk block with the rules of apk: package
// names, version-rN, repositories with an RSA key installed under its own
// name (or none: a key apk has) and without credentials, tagged
// repositories for the packages that name them, and .apk files.
func (p *DeploymentPlan) validateApkPackages(apk *ApkPackages) error {
	repoNames := map[string]bool{}
	tagged := map[string]bool{}
	keys := map[string][]byte{}
	for i := range apk.Repositories {
		r := &apk.Repositories[i]
		what := fmt.Sprintf("host_packages.apk.repositories[%d]", i)
		if !repoNameRe.MatchString(r.Name) {
			return fmt.Errorf("%s: name %q must be lower case letters, digits and '-'", what, r.Name)
		}
		if repoNames[r.Name] {
			return fmt.Errorf("%s: duplicate repository name %q", what, r.Name)
		}
		repoNames[r.Name] = true
		tagged[r.Name] = r.Tagged

		// Credentials are refused (decision Q5 of the host platform plan).
		const private = "a private repository is reached through local .apk files (file) or a proxy that adds the credentials"
		if r.Auth != nil {
			return fmt.Errorf("%s: apk repositories take no credentials: apk keeps them in /etc/apk/repositories, which every user can read; %s", what, private)
		}
		u, err := url.Parse(r.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("%s: url %q must be an http(s) URL", what, r.URL)
		}
		if u.User != nil {
			return fmt.Errorf("%s: the url takes no credentials: apk keeps them in /etc/apk/repositories, which every user can read; %s", what, private)
		}
		if strings.ContainsAny(r.URL, " \t\r\n") {
			return fmt.Errorf("%s: url must not contain whitespace", what)
		}
		if r.Key != "" && r.KeyFile != "" {
			return fmt.Errorf("%s: at most one of key or key_file (none: a repository signed by a key apk has)", what)
		}
		if r.Key != "" && r.KeyName == "" {
			return fmt.Errorf("%s: key needs key_name, the file name apk finds it by (the name of the key that signs the index, e.g. packager-6165ee59.rsa.pub)", what)
		}
		if r.Key == "" && r.KeyName != "" {
			return fmt.Errorf("%s: key_name needs key (a key_file is installed under its own file name)", what)
		}
		key, name, err := p.ApkRepositoryKey(r)
		if err != nil {
			return err
		}
		if name != "" {
			if !apkKeyNameRe.MatchString(name) {
				return fmt.Errorf("%s: invalid key name %q: it is the file name in /etc/apk/keys (letters, digits, @ . _ + -, ending in .pub)", what, name)
			}
			if !isRSAPublicKey(key) {
				return fmt.Errorf("%s: the key must be an RSA public key in PEM (BEGIN PUBLIC KEY), as apk keys are", what)
			}
			if other, ok := keys[name]; ok && !bytes.Equal(other, key) {
				return fmt.Errorf("%s: two different keys named %s: apk finds a key by its name", what, name)
			}
			keys[name] = key
		}
		if err := validateRoles(what, r.Roles); err != nil {
			return err
		}
	}
	for i := range apk.Install {
		pkg := &apk.Install[i]
		what := fmt.Sprintf("host_packages.apk.install[%d]", i)
		switch {
		case pkg.Repository == "":
		case pkg.File != "":
			return fmt.Errorf("%s: a package of a local file has no repository", what)
		case !repoNames[pkg.Repository]:
			return fmt.Errorf("%s: repository %q is no repository of host_packages.apk", what, pkg.Repository)
		case !tagged[pkg.Repository]:
			return fmt.Errorf("%s: repository %s is not tagged: apk takes the packages of an untagged repository by their name, without repository", what, pkg.Repository)
		}
	}
	return p.validatePackageList(apk.Install, packageRules{
		block: "apk", kind: "apk", nameRe: apkPackageNameRe, versionRe: apkVersionRe,
		version: " (want version-rN, e.g. 1.7.27-r0)", ext: ".apk", file: p.ApkFile,
	})
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
		switch s.Interpreter {
		case "", InterpreterBash, InterpreterSh:
		case "powershell":
			return fmt.Errorf("%s: interpreter powershell is not supported yet (scripts of Windows nodes follow in a later phase)", what)
		default:
			return fmt.Errorf("%s: interpreter %q must be bash or sh", what, s.Interpreter)
		}
		for _, f := range s.Families {
			switch f {
			case FamilyDebian, FamilyRHEL, FamilyAlpine, FamilyWindows:
			default:
				return fmt.Errorf("%s: family %q must be debian, rhel, alpine or windows", what, f)
			}
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
