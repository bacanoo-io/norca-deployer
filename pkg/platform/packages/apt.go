// Package packages installs Debian packages on Debian-based Linux nodes,
// from APT repositories or from local .deb files copied by the deployer.
package packages

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bacanoo-io/norca-deployer/pkg/artifacts"
	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/connector"
)

const (
	SourcesDir     = "/etc/apt/sources.list.d"
	KeyringsDir    = "/etc/apt/keyrings"
	PreferencesDir = "/etc/apt/preferences.d"
	AuthDir        = "/etc/apt/auth.conf.d"
	// DebDir holds the .deb files copied by the deployer.
	DebDir = "/var/lib/norca-deployer/debs"

	defaultLockTimeout = 300
)

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

// Selection is the part of the host package configuration that applies to
// one node.
type Selection struct {
	Repositories []config.AptRepository
	Packages     []config.HostPackage
}

// Empty reports whether nothing is to be done on the node.
func (s *Selection) Empty() bool {
	return len(s.Repositories) == 0 && len(s.Packages) == 0
}

// needsIndex reports whether apt has to read repository indexes: true when
// a repository is configured or a package is not a local file. Nodes without
// any reachable repository can install local .deb files only.
func (s *Selection) needsIndex() bool {
	if len(s.Repositories) > 0 {
		return true
	}
	for _, p := range s.Packages {
		if p.File == "" {
			return true
		}
	}
	return false
}

// Select filters the repositories and packages by the node's role and host.
func Select(hp *config.HostPackages, node *config.NodeConfig, isMaster bool) *Selection {
	sel := &Selection{}
	if hp == nil {
		return sel
	}
	for _, r := range hp.Repositories {
		if config.AppliesTo(r.Roles, nil, node, isMaster) {
			sel.Repositories = append(sel.Repositories, r)
		}
	}
	for _, p := range hp.Install {
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
		lockTimeout = defaultLockTimeout
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

// Resolver decrypts inline secrets.
type Resolver func(string) (string, error)

// Apply configures the repositories and installs the packages selected for
// the node. It is idempotent: files are rewritten with the same content,
// copied .deb files are only uploaded when their digest changed, and apt-get
// skips packages already at the requested version.
func Apply(conn connector.Connector, plan *config.DeploymentPlan, sel *Selection, node *config.NodeConfig, resolve Resolver) error {
	if sel.Empty() {
		return nil
	}
	hp := plan.HostPackages
	host := node.Host

	if out, err := conn.ExecuteCommand("command -v apt-get >/dev/null && command -v dpkg-query >/dev/null"); err != nil {
		return fmt.Errorf("host_packages need a Debian-based node with apt-get and dpkg (%w, output: %s)", err, out)
	}

	// 1. Repositories: keyring, source, pin, credentials
	if len(sel.Repositories) > 0 {
		if err := conn.MkdirAll(KeyringsDir, 0755); err != nil {
			return fmt.Errorf("failed to create %s: %w", KeyringsDir, err)
		}
	}
	for i := range sel.Repositories {
		repo := &sel.Repositories[i]
		if err := applyRepository(conn, plan, repo, resolve); err != nil {
			return err
		}
		fmt.Printf("    -> [%s] APT repository %q configured (%s %s)\n", host, repo.Name, repo.URI, repo.Suite)
	}

	if len(sel.Packages) == 0 {
		return nil
	}

	lockTimeout := hp.LockTimeout
	if lockTimeout <= 0 {
		lockTimeout = defaultLockTimeout
	}

	// 2. Local .deb files
	targets := make([]string, 0, len(sel.Packages))
	fileVersions := map[string]string{}
	for i := range sel.Packages {
		p := &sel.Packages[i]
		if p.File == "" {
			spec := p.Name
			if p.Version != "" {
				spec += "=" + p.Version
			}
			targets = append(targets, spec)
			continue
		}
		remote, version, err := copyDeb(conn, plan, p, node)
		if err != nil {
			return err
		}
		fileVersions[p.Name] = version
		targets = append(targets, remote)
	}

	// 3. Package index
	if sel.needsIndex() && (hp.UpdateCache == nil || *hp.UpdateCache) {
		cmd := fmt.Sprintf("apt-get update -q -o DPkg::Lock::Timeout=%d", lockTimeout)
		if out, err := conn.ExecuteCommand(cmd); err != nil {
			return fmt.Errorf("apt-get update failed: %w (output: %s)", err, lastLines(out, 20))
		}
	}

	// 4. Install in one transaction
	if out, err := conn.ExecuteCommand(InstallCommand(targets, lockTimeout)); err != nil {
		return fmt.Errorf("apt-get install failed: %w (output: %s)", err, lastLines(out, 30))
	}

	// 5. Holds
	var hold, unhold []string
	for _, p := range sel.Packages {
		if p.Hold {
			hold = append(hold, connector.ShellQuote(p.Name))
		} else {
			unhold = append(unhold, connector.ShellQuote(p.Name))
		}
	}
	if len(hold) > 0 {
		if out, err := conn.ExecuteCommand("apt-mark hold " + strings.Join(hold, " ")); err != nil {
			return fmt.Errorf("apt-mark hold failed: %w (output: %s)", err, out)
		}
	}
	if len(unhold) > 0 {
		// Release holds that an earlier apply set and the plan no longer asks for.
		_, _ = conn.ExecuteCommand("apt-mark unhold " + strings.Join(unhold, " ") + " >/dev/null")
	}

	// 6. Verify installed versions
	names := make([]string, 0, len(sel.Packages))
	for _, p := range sel.Packages {
		names = append(names, connector.ShellQuote(p.Name))
	}
	out, err := conn.ExecuteCommand("dpkg-query -W -f='${Package} ${Version}\\n' " + strings.Join(names, " "))
	if err != nil {
		return fmt.Errorf("dpkg-query failed: %w (output: %s)", err, out)
	}
	installed := ParseDpkgQuery(out)
	for _, p := range sel.Packages {
		v, ok := installed[p.Name]
		if !ok {
			return fmt.Errorf("package %q is not installed after apt-get install", p.Name)
		}
		want := p.Version
		if fv, ok := fileVersions[p.Name]; ok {
			want = fv
		}
		if want != "" && v != want {
			return fmt.Errorf("package %q: version %s installed, %s requested", p.Name, v, want)
		}
		fmt.Printf("    -> [%s] Package %s %s installed\n", host, p.Name, v)
	}
	return nil
}

func applyRepository(conn connector.Connector, plan *config.DeploymentPlan, repo *config.AptRepository, resolve Resolver) error {
	key, err := plan.RepositoryKey(repo)
	if err != nil {
		return err
	}
	armored := config.KeyIsArmored(key)
	keyring := KeyringPath(repo, armored)
	if err := conn.CopyFile(bytes.NewReader(key), keyring, 0644); err != nil {
		return fmt.Errorf("repository %q: failed to write keyring: %w", repo.Name, err)
	}
	// Remove the keyring of the other format left by an earlier apply.
	_, _ = conn.ExecuteCommand("rm -f " + connector.ShellQuote(KeyringPath(repo, !armored)))

	if err := conn.CopyFile(bytes.NewReader([]byte(RenderSource(repo, keyring))), SourcePath(repo), 0644); err != nil {
		return fmt.Errorf("repository %q: failed to write source: %w", repo.Name, err)
	}

	if repo.PinPriority != 0 {
		if err := conn.MkdirAll(PreferencesDir, 0755); err != nil {
			return err
		}
		if err := conn.CopyFile(bytes.NewReader([]byte(RenderPreferences(repo))), PreferencesPath(repo), 0644); err != nil {
			return fmt.Errorf("repository %q: failed to write the pin: %w", repo.Name, err)
		}
	} else {
		_, _ = conn.ExecuteCommand("rm -f " + connector.ShellQuote(PreferencesPath(repo)))
	}

	if repo.Auth != nil {
		password := repo.Auth.Password
		if resolve != nil {
			if password, err = resolve(password); err != nil {
				return fmt.Errorf("repository %q: auth.password: %w", repo.Name, err)
			}
		}
		if err := conn.MkdirAll(AuthDir, 0755); err != nil {
			return err
		}
		if err := conn.CopyFile(bytes.NewReader([]byte(RenderAuth(repo, password))), AuthPath(repo), 0600); err != nil {
			return fmt.Errorf("repository %q: failed to write credentials: %w", repo.Name, err)
		}
	} else {
		_, _ = conn.ExecuteCommand("rm -f " + connector.ShellQuote(AuthPath(repo)))
	}
	return nil
}

// copyDeb copies a local .deb to the node unless the same file is already
// there, and checks that it contains the declared package. It returns the
// remote path and the version of the file.
func copyDeb(conn connector.Connector, plan *config.DeploymentPlan, p *config.HostPackage, node *config.NodeConfig) (string, string, error) {
	local := plan.DebFile(p, node.Arch)
	sum, err := artifacts.FileSHA256(local)
	if err != nil {
		return "", "", fmt.Errorf("package %q: %w", p.Name, err)
	}
	if p.SHA256 != "" && !strings.EqualFold(sum, p.SHA256) {
		return "", "", fmt.Errorf("package %q: %s has sha256 %s, expected %s", p.Name, local, sum, p.SHA256)
	}

	remote := path.Join(DebDir, filepath.Base(local))
	if err := conn.MkdirAll(DebDir, 0755); err != nil {
		return "", "", err
	}
	if RemoteSHA256(conn, remote) != sum {
		f, err := os.Open(local)
		if err != nil {
			return "", "", err
		}
		err = conn.CopyFile(f, remote, 0644)
		f.Close()
		if err != nil {
			return "", "", fmt.Errorf("package %q: upload failed: %w", p.Name, err)
		}
		fmt.Printf("    -> [%s] Copied %s\n", node.Host, filepath.Base(local))
	}

	out, err := conn.ExecuteCommand("dpkg-deb -f " + connector.ShellQuote(remote) + " Package Version")
	if err != nil {
		return "", "", fmt.Errorf("package %q: %s is not a valid .deb: %w", p.Name, filepath.Base(local), err)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if fields["Package"] != p.Name {
		return "", "", fmt.Errorf("package %q: %s contains package %q", p.Name, filepath.Base(local), fields["Package"])
	}
	if p.Version != "" && fields["Version"] != p.Version {
		return "", "", fmt.Errorf("package %q: %s has version %s, expected %s", p.Name, filepath.Base(local), fields["Version"], p.Version)
	}
	return remote, fields["Version"], nil
}

// RemoteSHA256 returns the digest of a file on a Linux node, or "" if it
// does not exist.
func RemoteSHA256(conn connector.Connector, p string) string {
	out, err := conn.ExecuteCommand("sha256sum " + connector.ShellQuote(p) + " 2>/dev/null")
	if err != nil {
		return ""
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// ParseDpkgQuery parses "name version" lines.
func ParseDpkgQuery(out string) map[string]string {
	res := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			res[fields[0]] = fields[1]
		}
	}
	return res
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
