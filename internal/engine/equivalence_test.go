package engine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/artifacts"
	"github.com/bacanoo-io/norca-deployer/internal/connector"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// The equivalence check of the host platform work: what prepareNode does on
// a node (the commands it runs and the files it writes, in order) is
// recorded for a set of plans, so that two versions of the deployer can be
// compared. NORCA_EQUIV_PLANS names the plans (separated by the path list
// separator of the OS), NORCA_EQUIV_OUT the directory of the transcripts:
// <plan>.txt with every node of the plan, provisioned twice on the same fake
// host (a fresh node, then the same node again).
//
// The fake host keeps the files written to it, answers sha256sum, mv and
// rm on them, answers the package queries with the versions of the plan and
// fails the checks of the steps (commands that start with "# check:") on
// the first run, so that the changes they guard are recorded too.

// equivHost is a fake Linux node that records what is done to it.
type equivHost struct {
	mu     sync.Mutex
	events []string
	files  map[string][]byte
	modes  map[string]os.FileMode
	// debs maps the base name of a local package file to its fields.
	debs map[string][2]string
	// versions are the installed versions dpkg-query reports.
	versions map[string]string
	// fresh fails the checks of the steps.
	fresh bool
	probe string
}

func equivDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (h *equivHost) log(format string, args ...any) {
	h.events = append(h.events, fmt.Sprintf(format, args...))
}

// unquote splits the shell words of a simple command (single quotes only).
func unquote(s string) []string {
	var words []string
	var cur strings.Builder
	in, started := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			in = !in
			started = true
		case !in && (c == ' ' || c == '\t'):
			if started {
				words = append(words, cur.String())
				cur.Reset()
				started = false
			}
		case !in && c == '\\' && i+1 < len(s):
			i++
			cur.WriteByte(s[i])
			started = true
		default:
			cur.WriteByte(c)
			started = true
		}
	}
	if started {
		words = append(words, cur.String())
	}
	return words
}

func (h *equivHost) ExecuteCommand(cmd string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log("$ %s", strings.ReplaceAll(cmd, "\n", "\n  "))
	switch {
	case strings.HasPrefix(cmd, "# check:"):
		if h.fresh {
			return "", fmt.Errorf("exit status 1")
		}
		return "", nil
	case h.probe != "" && strings.Contains(cmd, "/etc/os-release"):
		return h.probe, nil
	case strings.HasPrefix(cmd, "sha256sum "):
		w := unquote(strings.TrimSuffix(cmd, " 2>/dev/null"))
		if len(w) == 2 {
			if data, ok := h.files[w[1]]; ok {
				return equivDigest(data) + "  " + w[1] + "\n", nil
			}
		}
		return "", nil
	case strings.HasPrefix(cmd, "mv -f "):
		w := unquote(cmd)
		if len(w) == 4 {
			if data, ok := h.files[w[2]]; ok {
				h.files[w[3]], h.modes[w[3]] = data, h.modes[w[2]]
				delete(h.files, w[2])
			}
		}
		return "", nil
	case strings.HasPrefix(cmd, "dpkg-deb -f "):
		w := unquote(cmd)
		if f, ok := h.debs[path.Base(w[2])]; ok {
			return "Package: " + f[0] + "\nVersion: " + f[1] + "\n", nil
		}
		return "", fmt.Errorf("not a .deb")
	case strings.HasPrefix(cmd, "dpkg-query -W "):
		var b strings.Builder
		for _, name := range unquote(cmd)[3:] {
			if v, ok := h.versions[name]; ok {
				fmt.Fprintf(&b, "%s %s\n", name, v)
			}
		}
		return b.String(), nil
	}
	return "", nil
}

func (h *equivHost) CopyFile(src io.Reader, dest string, mode os.FileMode) error {
	data, err := io.ReadAll(src)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.files[dest], h.modes[dest] = data, mode
	h.log("> write %s %04o %d bytes sha256 %s", dest, mode.Perm(), len(data), equivDigest(data)[:16])
	return nil
}

func (h *equivHost) ReadFile(p string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	data, ok := h.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return data, nil
}

func (h *equivHost) FileExists(p string) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.files[p]
	return ok, nil
}

func (h *equivHost) MkdirAll(p string, mode os.FileMode) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.log("> mkdir %s %04o", p, mode.Perm())
	return nil
}

func (h *equivHost) Close() error { return nil }

// equivArtifacts makes a fake norca binary and CNI archive and points the
// release and the CNI plugins of a plan that would download them there.
func equivArtifacts(t *testing.T, plan *config.DeploymentPlan) {
	t.Helper()
	dir := t.TempDir()
	switch plan.Release.Source {
	case config.ReleaseOfficial, config.ReleaseCustomURL:
		bin := filepath.Join(dir, "norca")
		if err := os.WriteFile(bin, []byte("fake norca binary\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		plan.Release.Source, plan.Release.LocalBinaryPath, plan.Release.ChecksumSHA256 = config.ReleaseLocalPath, bin, ""
	}
	if c := plan.CNI; c != nil && c.Install != nil && *c.Install && c.Source != config.ReleaseLocalPath {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		_ = tw.WriteHeader(&tar.Header{Name: "bridge", Mode: 0o755, Size: 4})
		_, _ = tw.Write([]byte("fake"))
		_ = tw.Close()
		_ = gz.Close()
		tgz := filepath.Join(dir, "cni.tgz")
		if err := os.WriteFile(tgz, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		c.Source, c.LocalArchivePath, c.ChecksumSHA256 = config.ReleaseLocalPath, tgz, ""
	}
}

// equivPackages returns the local package files (base name -> name,
// version) and the versions dpkg-query reports for the host packages of a
// plan.
func equivPackages(plan *config.DeploymentPlan, arch string) (map[string][2]string, map[string]string) {
	debs, versions := map[string][2]string{}, map[string]string{}
	for _, p := range equivHostPackages(plan) {
		v := p.Version
		if v == "" {
			v = "1.0-1"
		}
		versions[p.Name] = v
		if p.File != "" {
			debs[filepath.Base(plan.DebFile(&p, arch))] = [2]string{p.Name, v}
		}
	}
	return debs, versions
}

// equivProbe is what the facts probe reads on a Debian 12 node; empty
// before the probe exists.
var equivProbe = ""

func TestRecordPrepareNode(t *testing.T) {
	out := os.Getenv("NORCA_EQUIV_OUT")
	if out == "" {
		t.Skip("NORCA_EQUIV_OUT is not set")
	}
	for _, file := range filepath.SplitList(os.Getenv("NORCA_EQUIV_PLANS")) {
		name := strings.TrimSuffix(filepath.Base(filepath.Dir(file)), string(filepath.Separator)) + "-" +
			strings.TrimSuffix(filepath.Base(file), filepath.Ext(file))
		t.Run(name, func(t *testing.T) {
			plan, err := config.LoadFromFile(file)
			if err != nil {
				t.Fatal(err)
			}
			equivArtifacts(t, plan)
			r := NewRunner(plan, nil)
			r.artifacts = artifacts.NewResolver(plan, t.TempDir())
			r.gossipKey = "Z2t0ZXN0a2V5Z2t0ZXN0a2V5Z2t0ZXN0a2V5Z2t0ZXM="
			var b strings.Builder
			nodes := append(r.targets(plan.Inventory.Masters, true), r.targets(plan.Inventory.Clients, false)...)
			for _, tg := range nodes {
				debs, versions := equivPackages(plan, tg.node.Arch)
				h := &equivHost{files: map[string][]byte{}, modes: map[string]os.FileMode{}, debs: debs, versions: versions,
					probe: equivProbe}
				r.connect = func(*config.NodeConfig) (connector.Connector, error) { return h, nil }
				for run := 1; run <= 2; run++ {
					h.fresh = run == 1
					h.events = nil
					t2 := &target{node: tg.node, isMaster: tg.isMaster, paths: tg.paths}
					err := r.prepareNode(t2)
					fmt.Fprintf(&b, "== node %s (%s) run %d: changed %t, error %v\n", tg.node.Host, roleOf(tg.isMaster), run, t2.changed, err)
					for _, e := range h.events {
						b.WriteString(e + "\n")
					}
				}
				var paths []string
				for p := range h.files {
					paths = append(paths, p)
				}
				sort.Strings(paths)
				fmt.Fprintf(&b, "== node %s files\n", tg.node.Host)
				for _, p := range paths {
					fmt.Fprintf(&b, "%s %04o sha256 %s\n", p, h.modes[p].Perm(), equivDigest(h.files[p])[:16])
				}
			}
			if err := os.WriteFile(filepath.Join(out, name+".txt"), []byte(b.String()), 0o600); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func roleOf(isMaster bool) string {
	if isMaster {
		return config.RoleMaster
	}
	return config.RoleClient
}

// equivHostPackages are the packages of the plan, of every package manager.
func equivHostPackages(plan *config.DeploymentPlan) []config.HostPackage {
	if plan.HostPackages == nil {
		return nil
	}
	return plan.HostPackages.Install
}
