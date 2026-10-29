package engine

import (
	"fmt"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/connector"
)

// installNATSBridge copies norca-natsbridge from the release into bin_dir,
// where the bridge job (raw_exec or exec) of the nats pack expects it. With
// source host-package the package must provide it; it is only checked.
func (r *Runner) installNATSBridge(t *target) error {
	goos, name := "linux", "norca-natsbridge"
	dest := t.paths.BinDir + "/" + name
	if t.isWindows() {
		goos, dest = "windows", t.paths.BinDir+`\`+name+".exe"
	}
	if r.plan.Release.Source == config.ReleaseHostPackage {
		if out, err := t.conn.ExecuteCommand("test -x " + connector.ShellQuote(dest) + " || command -v " + name); err != nil {
			return fmt.Errorf("not installed by the host packages (%s): %w %s", dest, err, out)
		}
		return nil
	}
	file, err := r.artifacts.ReleaseBinary(name, goos, t.node.Arch)
	if err != nil {
		return err
	}
	changed, err := connector.SyncLocalFile(t.conn, file.Path, file.SHA256, dest, 0755, t.isWindows())
	if err != nil {
		return err
	}
	if changed && t.isWindows() {
		// Windows binaries are staged as .new (the norca service swaps its
		// own on restart); the bridge has no service, so swap it now. A
		// running bridge locks the file; the next apply tries again.
		staged := dest + connector.PendingSuffix
		if out, err := t.conn.ExecuteCommand("Move-Item -Force -LiteralPath " + connector.PSQuote(staged) + " -Destination " + connector.PSQuote(dest)); err != nil {
			fmt.Printf("    [WARN] [%s] %s is in use, not replaced: %v %s\n", t.node.Host, dest, err, out)
			return nil
		}
	}
	if changed {
		fmt.Printf("    -> [%s] %s %s installed\n", t.node.Host, name, r.plan.Release.Version)
	}
	return nil
}
