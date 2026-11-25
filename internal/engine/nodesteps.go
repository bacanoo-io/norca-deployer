package engine

import (
	"fmt"

	"github.com/bacanoo-io/norca-deployer/internal/artifacts"
	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/host"
	"github.com/bacanoo-io/norca-deployer/internal/platform"
	"github.com/bacanoo-io/norca-deployer/internal/platform/packages"
	"github.com/bacanoo-io/norca-deployer/internal/platform/selinux"
	"github.com/bacanoo-io/norca-deployer/internal/platform/service"
	"github.com/bacanoo-io/norca-deployer/internal/platform/step"
	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
	"github.com/bacanoo-io/norca-deployer/internal/platform/tuning"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// The steps of a node that are no driver of their own: the directories,
// the norca binary and the CNI plugins. The SSH path (prepareNode) and the
// install script of a scaled machine (GenerateInstallScript) take them from
// here.

// nodeDirs are the directories of a node with the modes of the agent: those
// of a server, those of a client too, and the directories of the native
// application volumes when the client has them.
func nodeDirs(node *config.NodeConfig, paths *storage.ResolvedPaths, client bool) []step.Dir {
	dirs := []step.Dir{
		{Path: paths.BinDir, Mode: 0o755},
		{Path: paths.ConfigDir, Mode: 0o750},
		{Path: paths.DataDir, Mode: 0o750},
		{Path: paths.LogDir, Mode: 0o750},
		{Path: paths.TLSDir, Mode: 0o700},
	}
	if client {
		dirs = append(dirs, step.Dir{Path: paths.CNIBinDir, Mode: 0o755}, step.Dir{Path: paths.CNIConfigDir, Mode: 0o755})
		cs := node.ClientSettings
		if paths.AppsDir != "" && cs != nil && cs.NativeAppVolumes != nil && cs.NativeAppVolumes.EnableDefaults {
			dirs = append(dirs,
				step.Dir{Path: paths.AppsBinDir, Mode: 0o755}, step.Dir{Path: paths.AppsWorkDir, Mode: 0o770},
				step.Dir{Path: paths.AppsConfigDir, Mode: 0o750}, step.Dir{Path: paths.AppsDataDir, Mode: 0o750})
		}
	}
	res := make([]step.Dir, 0, len(dirs))
	for _, d := range dirs {
		if d.Path != "" {
			res = append(res, d)
		}
	}
	return res
}

// dirsStep creates the directories of a node.
func dirsStep(dirs []step.Dir) step.Step {
	return step.Step{Name: "directories", Dirs: dirs}
}

// hostPackageBinaryStep links the norca binary that a host package
// installed into the bin directory, when the package put it elsewhere
// (/usr/bin/norca), and fails when there is none (release.source
// host-package).
func hostPackageBinaryStep(bin, pkg string) step.Step {
	q := connector.ShellQuote
	return step.Step{
		Name:  "norca binary",
		Check: "[ -x " + q(bin) + " ]",
		Apply: "found=$(command -v norca || true)\n" +
			"if [ -n \"$found\" ] && [ \"$found\" != " + q(bin) + " ]; then\n" +
			"  ln -sf \"$found\" " + q(bin) + "\n" +
			"  log " + q(bin+" linked to ") + "\"$found\"\n" +
			"fi\n" +
			"[ -x " + q(bin) + " ] || die " + q("package "+pkg+" did not install an executable norca binary"),
		Restart: true,
	}
}

// binaryStep is the norca binary of a node over SSH: the file of the
// release (the musl build on Alpine), copied when the node has another one
// (on Windows staged as norca.exe.new for the restart).
func (r *Runner) binaryStep(t *target) (step.Step, error) {
	if r.plan.Release.Source == config.ReleaseHostPackage {
		return hostPackageBinaryStep(t.paths.BinDir+"/norca", r.plan.Release.Package), nil
	}
	goos, dest := "linux", t.paths.BinDir+"/norca"
	if t.isWindows() {
		goos, dest = "windows", t.paths.BinDir+`\norca.exe`
	}
	file, err := r.artifacts.NorcaBinary(r.abortCtx(), goos, t.node.Arch, t.libc())
	if err != nil {
		return step.Step{}, err
	}
	return step.Step{
		Name:    "norca binary",
		Files:   []step.File{{Path: dest, Mode: 0o755, Local: file.Path, SHA256: file.SHA256}},
		Restart: true,
	}, nil
}

// cniMarker holds the digest of the CNI archive that was unpacked.
func cniMarker(paths *storage.ResolvedPaths) string {
	return paths.CNIBinDir + "/.norca-deployer-cni.sha256"
}

// cniStep unpacks the CNI plugin archive into cni_bin_dir over SSH unless
// the same archive was unpacked before. The plugins run per allocation, so
// a new version needs no agent restart.
func (r *Runner) cniStep(t *target) (step.Step, error) {
	file, err := r.artifacts.CNIArchive(r.abortCtx(), t.node.Arch)
	if err != nil {
		return step.Step{}, err
	}
	q := connector.ShellQuote
	marker, archive := cniMarker(t.paths), cliStateDir+"/cni-plugins.tgz"
	return step.Step{
		Name:   "CNI plugins",
		Dirs:   []step.Dir{{Path: cliStateDir, Mode: 0o700}},
		Check:  "[ \"$(cat " + q(marker) + " 2>/dev/null)\" = " + q(file.SHA256) + " ]",
		Inputs: []step.File{{Path: archive, Mode: 0o600, Local: file.Path, SHA256: file.SHA256}},
		Apply: "tar -xzf " + q(archive) + " -C " + q(t.paths.CNIBinDir) + "\n" +
			"rm -f " + q(archive) + "\n" +
			"printf '%s\\n' " + q(file.SHA256) + " >" + q(marker) + "\n" +
			"chmod 0644 " + q(marker) + "\n" +
			"log " + q("CNI plugins "+r.plan.CNI.Version+" installed"),
	}, nil
}

// packageSelection is what the block of the node's package manager selects
// for it (apt for a node without facts); nothing for a manager without a
// block (checkFacts refused a plan that gives it packages in another one).
func packageSelection(plan *config.DeploymentPlan, t *target) *packages.Selection {
	manager := config.PackageManagerApt
	if t.facts != nil {
		manager = t.facts.PackageManager
	}
	sel, _ := packages.Block(plan.HostPackages, manager, t.node, t.isMaster)
	return sel
}

// defaultDrivers are the drivers of a node without facts: apt and systemd
// on Linux, the service manager of Windows.
func defaultDrivers(windows bool) *platform.Drivers {
	if windows {
		return &platform.Drivers{Packages: packages.None{}, Service: service.Windows{}, Tuning: tuning.Windows{}}
	}
	return &platform.Drivers{Packages: packages.Apt{}, Service: service.Systemd{}, Tuning: tuning.Linux{}}
}

// libc is the C library of the node, which the release binary must be
// built against: musl on Alpine, glibc elsewhere.
func (t *target) libc() string {
	if t.facts != nil && t.facts.Family == host.FamilyAlpine {
		return artifacts.LibcMusl
	}
	return artifacts.LibcGlibc
}

// serviceManager is the init system driver of the node: from its facts, or
// by its platform for a node the apply did not probe.
func (t *target) serviceManager() service.Manager {
	if t.drivers != nil {
		return t.drivers.Service
	}
	return service.For(t.isWindows())
}

// runSteps runs steps on a node and marks the node changed when a change
// needs the agent restarted; what names the steps in the error.
func (r *Runner) runSteps(t *target, what string, steps []step.Step) error {
	ex := &step.Executor{Conn: t.conn, Host: t.node.Host, Windows: t.isWindows()}
	changed, restart, err := ex.RunAll(r.abortCtx(), steps)
	t.wrote = t.wrote || changed
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	t.changed = t.changed || restart
	return nil
}

// restoreSELinuxLabels restores the SELinux labels of what the deployer
// wrote on a RHEL node whose SELinux labels files, when the apply changed
// something there.
func (r *Runner) restoreSELinuxLabels(t *target) error {
	f := t.facts
	if f == nil || f.Family != host.FamilyRHEL || !f.SELinuxActive() || !(t.wrote || t.changed) {
		return nil
	}
	return r.runSteps(t, "SELinux labels", []step.Step{selinux.RestoreStep(selinux.NodePaths(t.paths), false)})
}
