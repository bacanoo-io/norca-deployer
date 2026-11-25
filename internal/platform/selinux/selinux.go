// Package selinux restores the SELinux labels of the files the deployer
// writes on a node. The deployer uploads a file to a temporary file and
// moves it into place, and a move keeps the label of the temporary file:
// systemd would then refuse the unit and the binary of the agent under
// SELinux. restorecon gives each path the label of the policy.
package selinux

import (
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/platform/dns"
	"github.com/bacanoo-io/norca-deployer/internal/platform/packages"
	"github.com/bacanoo-io/norca-deployer/internal/platform/service"
	"github.com/bacanoo-io/norca-deployer/internal/platform/step"
	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
	"github.com/bacanoo-io/norca-deployer/internal/platform/tuning"
)

// StepName is the name of the step in the log.
const StepName = "SELinux labels"

// Paths are what the deployer writes on a Linux node, for restorecon.
type Paths struct {
	// Recursive are restored with what is below them: the binary, the
	// unit, the configuration and the certificates, the CNI plugins and
	// their configuration, the files of the tuning and of the DNS
	// forwarding, the package files of the deployer.
	Recursive []string
	// Self are directories the deployer only creates: the data, the log
	// and the application volume directories hold the files of the agent
	// and of the allocations, whose labels the container runtimes set
	// (restorecon -R would take them back).
	Self []string
}

// NodePaths returns the paths of a node.
func NodePaths(p *storage.ResolvedPaths) Paths {
	var bin string
	if p.BinDir != "" {
		bin = p.BinDir + "/norca"
	}
	rec := []string{
		bin, service.UnitPath, p.ConfigDir, p.TLSDir, p.CNIBinDir, p.CNIConfigDir,
		tuning.SysctlConfigPath, tuning.ModulesConfigPath,
		dns.ScriptPath, dns.ResolvedDropIn, dns.TrustAnchor, dns.DnsmasqConf,
		packages.RPMDir,
	}
	self := []string{p.DataDir, p.LogDir, p.AppsDir, p.AppsBinDir, p.AppsWorkDir, p.AppsConfigDir, p.AppsDataDir}
	return Paths{Recursive: nonEmpty(rec), Self: nonEmpty(self)}
}

// nonEmpty drops the paths a node does not have.
func nonEmpty(list []string) []string {
	res := make([]string, 0, len(list))
	for _, p := range list {
		if p != "" {
			res = append(res, p)
		}
	}
	return res
}

// globs are the files of the dnf repositories of the deployer.
const globs = packages.RepoDir + "/norca-*.repo " + packages.RPMKeyDir + "/norca-*.asc"

// RestoreStep restores the labels of the paths that exist. Without
// restorecon (policycoreutils) it warns. In the install script of a
// machine (script) it runs only when the script changed something and
// SELinux is not disabled; over SSH the caller decides from the facts of
// the node.
func RestoreStep(paths Paths, script bool) step.Step {
	q := func(list []string) string {
		res := make([]string, 0, len(list))
		for _, p := range list {
			res = append(res, connector.ShellQuote(p))
		}
		return strings.Join(res, " ")
	}
	var b strings.Builder
	b.WriteString("if ! command -v restorecon >/dev/null 2>&1; then\n")
	b.WriteString("  warn \"SELinux is on, but restorecon (policycoreutils) is missing: the SELinux labels of the files of Norca are not restored\"\n")
	b.WriteString("else\n")
	b.WriteString("  for p in " + q(paths.Recursive) + " " + globs + "; do\n")
	b.WriteString("    if [ -e \"$p\" ]; then restorecon -R \"$p\"; fi\n")
	b.WriteString("  done\n")
	if len(paths.Self) > 0 {
		b.WriteString("  for p in " + q(paths.Self) + "; do\n")
		b.WriteString("    if [ -e \"$p\" ]; then restorecon \"$p\"; fi\n")
		b.WriteString("  done\n")
	}
	b.WriteString("  log \"SELinux labels restored\"\n")
	b.WriteString("fi")
	s := step.Step{Name: StepName, Apply: b.String()}
	if script {
		s.When = "[ \"$changed\" = 1 ] && command -v getenforce >/dev/null 2>&1 && [ \"$(getenforce)\" != Disabled ]"
	}
	return s
}
