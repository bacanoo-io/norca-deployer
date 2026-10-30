package engine

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bacanoo-io/norca-deployer/pkg/artifacts"
	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/connector"
	"github.com/bacanoo-io/norca-deployer/pkg/crypto"
	"github.com/bacanoo-io/norca-deployer/pkg/pki"
	"github.com/bacanoo-io/norca-deployer/pkg/platform/packages"
	"github.com/bacanoo-io/norca-deployer/pkg/platform/scripts"
	"github.com/bacanoo-io/norca-deployer/pkg/platform/service"
	"github.com/bacanoo-io/norca-deployer/pkg/platform/storage"
	"github.com/bacanoo-io/norca-deployer/pkg/platform/tuning"
	"github.com/bacanoo-io/norca-deployer/pkg/state"
)

// certMinValidity is the remaining validity below which a node certificate
// is renewed.
const certMinValidity = 30 * 24 * time.Hour

// healthPollInterval is the wait between health checks.
var healthPollInterval = 3 * time.Second

// Runner manages the idempotent execution of deployment tasks across the inventory.
type Runner struct {
	plan      *config.DeploymentPlan
	vault     *crypto.Vault
	ca        *pki.CertificateAuthority
	store     *state.Store
	artifacts *artifacts.Resolver
	gossipKey string
	// consulToken is the Consul ACL token of the agents (plan.consul).
	consulToken string
	// replicationToken is the ACL token of the authoritative region (replica
	// regions only).
	replicationToken string
	// trustDomain is the SPIFFE trust domain (nil without SPIFFE).
	trustDomain *url.URL

	// installed is the list of nodes installed by earlier applies, and
	// decom the resolved inventory.decommission entries.
	installed *state.Installed
	decom     []*config.DecommissionNode
	decomDone []string // decommission entries removed by an earlier apply

	// connect opens the connection to a node (replaced in tests).
	connect func(*config.NodeConfig) (connector.Connector, error)

	// ForceRestart restarts every agent, even when nothing changed.
	ForceRestart bool
}

// NewRunner creates a new Runner instance.
func NewRunner(plan *config.DeploymentPlan, vault *crypto.Vault) *Runner {
	r := &Runner{
		plan:  plan,
		vault: vault,
	}
	r.connect = r.createConnector
	return r
}

// target is one node during an apply.
type target struct {
	node     *config.NodeConfig
	isMaster bool
	paths    *storage.ResolvedPaths
	conn     connector.Connector

	changed    bool // binary, certificates, configuration or service changed
	wasRunning bool
	err        error
}

func (t *target) isWindows() bool { return strings.ToLower(t.node.Platform) == "windows" }

// needsClient reports whether the node runs the client (clients and masters
// with client settings).
func (t *target) needsClient() bool { return !t.isMaster || t.node.ClientSettings != nil }

func (t *target) cli(r *Runner, token string) *remoteCLI {
	return &remoteCLI{conn: t.conn, plan: r.plan, node: t.node, paths: t.paths, isMaster: t.isMaster, token: token}
}

// Apply executes the entire cluster deployment plan.
func (r *Runner) Apply() error {
	fmt.Printf("==> [1/5] Preparing Security, PKI and artifacts...\n")
	store, err := state.Open(r.plan, r.vault)
	if err != nil {
		return err
	}
	r.store = store
	if err := r.loadInventory(); err != nil {
		return err
	}
	r.artifacts = artifacts.NewResolver(r.plan, store.CacheDir())
	if err := r.initPKI(); err != nil {
		return fmt.Errorf("PKI initialization error: %w", err)
	}
	if r.gossipKey, err = store.GossipKey(r.plan.Security.GossipEncryptionKey); err != nil {
		return fmt.Errorf("gossip key: %w", err)
	}
	if r.plan.IsReplicaRegion() && r.plan.Federation.ReplicationTokenFile != "" {
		path := r.plan.ResolveLocalPath(r.plan.Federation.ReplicationTokenFile)
		token, ok, err := store.ReadSecret(path)
		if err != nil {
			return fmt.Errorf("replication token: %w", err)
		}
		if !ok {
			return fmt.Errorf("replication token %s not found: apply the authoritative region %q first",
				path, r.plan.Federation.AuthoritativeRegion)
		}
		r.replicationToken = token
	}
	if c := r.plan.Consul; c != nil && c.TokenFile != "" {
		path := r.plan.ResolveLocalPath(c.TokenFile)
		token, ok, err := store.ReadSecret(path)
		if err != nil {
			return fmt.Errorf("consul token: %w", err)
		}
		if !ok {
			return fmt.Errorf("consul token file %s not found", path)
		}
		r.consulToken = token
	}

	masters := r.targets(r.plan.Inventory.Masters, true)
	clients := r.targets(r.plan.Inventory.Clients, false)
	defer closeAll(masters)
	defer closeAll(clients)

	fmt.Printf("==> [2/5] Provisioning Master (Server) nodes (%d nodes)...\n", len(masters))
	r.prepareAll(masters)
	if err := collect(masters); err != nil {
		return fmt.Errorf("master node provisioning failed (no agent was restarted): %w", err)
	}

	fmt.Printf("==> [3/5] Rolling restart of Master nodes...\n")
	if err := r.restartMasters(masters); err != nil {
		return err
	}
	if err := r.recordInstalled(masters, config.RoleMaster); err != nil {
		return err
	}

	var clientErr error
	if len(clients) > 0 {
		fmt.Printf("==> [4/5] Provisioning Client (Worker) nodes (%d nodes)...\n", len(clients))
		r.prepareAll(clients)
		clientErr = r.restartClients(clients)
		if err := r.recordInstalled(clients, config.RoleClient); err != nil {
			return err
		}
	} else {
		fmt.Printf("==> [4/5] No dedicated client nodes configured (skipped).\n")
	}

	fmt.Printf("==> [5/5] Post-Cluster phase (ACL, decommission, secrets & packs)...\n")
	if err := r.postClusterTasks(masters[0]); err != nil {
		return fmt.Errorf("post-cluster tasks failed: %w", err)
	}

	if clientErr != nil {
		return fmt.Errorf("client node provisioning failed: %w", clientErr)
	}
	fmt.Printf("\n[SUCCESS] Norca cluster deployment completed successfully!\n")
	return nil
}

func (r *Runner) targets(nodes []config.NodeConfig, isMaster bool) []*target {
	res := make([]*target, 0, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		res = append(res, &target{node: n, isMaster: isMaster, paths: storage.ResolveNodePaths(n, r.plan)})
	}
	return res
}

func closeAll(ts []*target) {
	for _, t := range ts {
		if t.conn != nil {
			t.conn.Close()
		}
	}
}

func collect(ts []*target) error {
	var errs []string
	for _, t := range ts {
		if t.err != nil {
			errs = append(errs, fmt.Sprintf("node [%s]: %v", t.node.Host, t.err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("\n - %s", strings.Join(errs, "\n - "))
	}
	return nil
}

// prepareAll provisions the nodes in parallel without restarting agents.
func (r *Runner) prepareAll(ts []*target) {
	var wg sync.WaitGroup
	for _, t := range ts {
		wg.Add(1)
		go func(t *target) {
			defer wg.Done()
			t.err = r.prepareNode(t)
		}(t)
	}
	wg.Wait()
}

// prepareNode provisions one node. It does not restart the agent; it records
// whether anything that needs a restart changed.
func (r *Runner) prepareNode(t *target) error {
	node := t.node
	fmt.Printf("    -> [%s] Connecting (%s mode)...\n", node.Host, node.Connection.Mode)
	conn, err := r.connect(node)
	if err != nil {
		return fmt.Errorf("connection error: %w", err)
	}
	t.conn = conn
	isWin := t.isWindows()
	paths := t.paths

	binaryBefore := r.binaryDigest(t)

	// 0. Scripts of phase pre_install
	if err := scripts.Run(conn, r.plan, config.PhasePreInstall, node, t.isMaster, r.resolveSecret); err != nil {
		return err
	}

	// 0b. Debian packages from APT repositories or local .deb files
	if sel := packages.Select(r.plan.HostPackages, node, t.isMaster); !sel.Empty() {
		if isWin {
			fmt.Printf("    [WARN] [%s] host_packages skipped on a Windows node\n", node.Host)
		} else if err := packages.Apply(conn, r.plan, sel, node, r.resolveSecret); err != nil {
			return fmt.Errorf("host packages: %w", err)
		}
	}
	if err := scripts.Run(conn, r.plan, config.PhasePostPackages, node, t.isMaster, r.resolveSecret); err != nil {
		return err
	}

	// 1. Host tuning (sysctl / Windows Registry)
	if r.plan.Tuning.ApplySysctl && !isWin {
		if err := tuning.ApplyLinuxTuning(conn); err != nil {
			return fmt.Errorf("Linux sysctl tuning error: %w", err)
		}
	}
	if r.plan.Tuning.WindowsLongPaths && isWin {
		if err := tuning.ApplyWindowsTuning(conn); err != nil {
			return fmt.Errorf("Windows environment tuning error: %w", err)
		}
	}

	// 2. Directory structure preparation
	dirs := []struct {
		path string
		mode os.FileMode
	}{
		{paths.BinDir, 0755},
		{paths.ConfigDir, 0750},
		{paths.DataDir, 0750},
		{paths.LogDir, 0750},
		{paths.TLSDir, 0700},
	}
	if t.needsClient() {
		dirs = append(dirs, struct {
			path string
			mode os.FileMode
		}{paths.CNIBinDir, 0755}, struct {
			path string
			mode os.FileMode
		}{paths.CNIConfigDir, 0755})
		if paths.AppsDir != "" && node.ClientSettings != nil && node.ClientSettings.NativeAppVolumes != nil &&
			node.ClientSettings.NativeAppVolumes.EnableDefaults {
			for _, d := range []struct {
				path string
				mode os.FileMode
			}{{paths.AppsBinDir, 0755}, {paths.AppsWorkDir, 0770}, {paths.AppsConfigDir, 0750}, {paths.AppsDataDir, 0750}} {
				dirs = append(dirs, d)
			}
		}
	}
	for _, d := range dirs {
		if d.path == "" {
			continue
		}
		if err := conn.MkdirAll(d.path, d.mode); err != nil {
			return fmt.Errorf("failed to create %s: %w", d.path, err)
		}
	}
	if err := r.prepareHostVolumes(t); err != nil {
		return err
	}

	// 3. Norca binary
	if err := r.installBinary(t); err != nil {
		return fmt.Errorf("norca binary: %w", err)
	}
	if after := r.binaryDigest(t); after != binaryBefore {
		t.changed = true
	}
	// The NATS bridge runs as a job on the clients; installing it does not
	// restart the agent.
	if r.plan.Release.NATSBridge && t.needsClient() {
		if err := r.installNATSBridge(t); err != nil {
			return fmt.Errorf("norca-natsbridge: %w", err)
		}
	}

	// 4. CNI plugins
	if t.needsClient() && !isWin && r.plan.CNI != nil && r.plan.CNI.Install != nil && *r.plan.CNI.Install {
		if err := r.installCNI(t); err != nil {
			return fmt.Errorf("CNI plugins: %w", err)
		}
	}

	// 5. Provision mTLS certificates
	if r.plan.Security.TLSEnabled && r.ca != nil {
		changed, err := r.provisionCertificates(t)
		if err != nil {
			return fmt.Errorf("certificate provisioning error: %w", err)
		}
		t.changed = t.changed || changed
	}

	// 6. Generate and write configuration file (norca.hcl)
	hclContent := GenerateNorcaConfig(node, r.plan, paths, t.isMaster, GenOptions{GossipKey: r.gossipKey, ReplicationToken: r.replicationToken, ConsulToken: r.consulToken})
	configPath := joinPath(paths, paths.ConfigDir, "norca.hcl")
	changed, err := connector.SyncFile(conn, configPath, []byte(hclContent), 0640)
	if err != nil {
		return err
	}
	t.changed = t.changed || changed

	// 7. Apply firewall rules (Linux only; Windows firewalls are left to
	// the administrator)
	if r.plan.Tuning.ApplyFirewallRules && !isWin {
		if err := r.configureFirewall(t); err != nil {
			return err
		}
	}

	// 8. Register the service (started by the rolling restart)
	changed, err = service.InstallService(conn, paths, isWin, r.plan.Tuning.FileDescriptorLimit)
	if err != nil {
		return fmt.Errorf("service installation error: %w", err)
	}
	t.changed = t.changed || changed
	t.wasRunning = service.IsServiceRunning(conn, isWin)

	status := "unchanged"
	if t.changed {
		status = "changed"
	}
	fmt.Printf("    [OK] [%s] Provisioned (%s)\n", node.Host, status)
	return nil
}

// needsRestart reports whether the agent has to be (re)started.
func (r *Runner) needsRestart(t *target) bool {
	return r.ForceRestart || t.changed || !t.wasRunning
}

// restartMasters restarts the servers that need it. A new cluster (no server
// running yet) starts all servers at once, because a single server cannot
// elect a leader before bootstrap_expect servers joined. An existing cluster
// restarts one server at a time and waits until it is healthy, has a leader
// and sees all peers, so that quorum is kept.
func (r *Runner) restartMasters(masters []*target) error {
	bootstrap := true
	for _, t := range masters {
		if t.wasRunning {
			bootstrap = false
		}
	}
	expectPeers := len(masters)

	if bootstrap {
		fmt.Printf("    New cluster: starting all %d servers\n", len(masters))
		for _, t := range masters {
			if err := service.RestartService(t.conn, t.paths, t.isWindows()); err != nil {
				return fmt.Errorf("node [%s]: %w", t.node.Host, err)
			}
		}
		for _, t := range masters {
			if err := r.waitHealthy(t, expectPeers); err != nil {
				return err
			}
		}
	} else {
		for _, t := range masters {
			if !r.needsRestart(t) {
				fmt.Printf("    [%s] unchanged, not restarted\n", t.node.Host)
				continue
			}
			fmt.Printf("    [%s] restarting...\n", t.node.Host)
			if err := service.RestartService(t.conn, t.paths, t.isWindows()); err != nil {
				return fmt.Errorf("node [%s]: %w", t.node.Host, err)
			}
			if err := r.waitHealthy(t, expectPeers); err != nil {
				return fmt.Errorf("%w (rolling restart stopped; the remaining servers were not restarted)", err)
			}
		}
	}

	for _, t := range masters {
		if err := scripts.Run(t.conn, r.plan, config.PhasePostInstall, t.node, true, r.resolveSecret); err != nil {
			return fmt.Errorf("node [%s]: %w", t.node.Host, err)
		}
	}
	return nil
}

// restartClients restarts changed clients in batches of max_parallel_clients.
// Allocations keep running while the client agent restarts. A failed node
// is reported and the others continue.
func (r *Runner) restartClients(clients []*target) error {
	var ready []*target
	for _, t := range clients {
		if t.err == nil {
			ready = append(ready, t)
		}
	}

	batch := r.plan.Upgrade.MaxParallelClients
	for start := 0; start < len(ready); start += batch {
		end := start + batch
		if end > len(ready) {
			end = len(ready)
		}
		group := ready[start:end]
		var wg sync.WaitGroup
		for _, t := range group {
			wg.Add(1)
			go func(t *target) {
				defer wg.Done()
				if !r.needsRestart(t) {
					fmt.Printf("    [%s] unchanged, not restarted\n", t.node.Host)
				} else {
					fmt.Printf("    [%s] restarting...\n", t.node.Host)
					if err := service.RestartService(t.conn, t.paths, t.isWindows()); err != nil {
						t.err = err
						return
					}
					if err := r.waitHealthy(t, 0); err != nil {
						t.err = err
						return
					}
				}
				t.err = scripts.Run(t.conn, r.plan, config.PhasePostInstall, t.node, false, r.resolveSecret)
			}(t)
		}
		wg.Wait()
		for _, t := range group {
			if t.err != nil {
				// Stop the rollout: a failing batch may mean a bad release.
				return fmt.Errorf("%v (rolling restart stopped)", collect(clients))
			}
		}
	}
	return collect(clients)
}

// waitHealthy polls the agent until it is healthy or the timeout expires.
func (r *Runner) waitHealthy(t *target, expectPeers int) error {
	deadline := time.Now().Add(time.Duration(r.plan.Upgrade.HealthTimeout) * time.Second)
	cli := t.cli(r, "")
	var last string
	for {
		ok, reason := cli.healthy(expectPeers)
		if ok {
			fmt.Printf("    [OK] [%s] healthy\n", t.node.Host)
			return nil
		}
		last = reason
		if time.Now().After(deadline) {
			return fmt.Errorf("node [%s] not healthy after %ds: %s", t.node.Host, r.plan.Upgrade.HealthTimeout, last)
		}
		time.Sleep(healthPollInterval)
	}
}

// binaryDigest returns the digest of the installed norca binary (following a
// symlink), or "".
func (r *Runner) binaryDigest(t *target) string {
	if t.isWindows() {
		return connector.RemoteSHA256(t.conn, t.paths.BinDir+`\norca.exe`, true)
	}
	out, err := t.conn.ExecuteCommand(`sha256sum "$(readlink -f ` + connector.ShellQuote(t.paths.BinDir+"/norca") + `)" 2>/dev/null`)
	if err != nil {
		return ""
	}
	if f := strings.Fields(out); len(f) > 0 {
		return f[0]
	}
	return ""
}

func (r *Runner) installBinary(t *target) error {
	if r.plan.Release.Source == config.ReleaseHostPackage {
		// The package provides the binary; link it into bin_dir when it is
		// installed elsewhere (e.g. /usr/bin/norca).
		target := t.paths.BinDir + "/norca"
		cmd := "if [ ! -x " + connector.ShellQuote(target) + " ]; then p=\"$(command -v norca)\" && [ -n \"$p\" ] && ln -sf \"$p\" " +
			connector.ShellQuote(target) + "; fi; test -x " + connector.ShellQuote(target)
		if out, err := t.conn.ExecuteCommand(cmd); err != nil {
			return fmt.Errorf("package %q did not install an executable norca binary: %w (output: %s)", r.plan.Release.Package, err, out)
		}
		return nil
	}

	goos := "linux"
	dest := t.paths.BinDir + "/norca"
	if t.isWindows() {
		goos = "windows"
		dest = t.paths.BinDir + `\norca.exe`
	}
	file, err := r.artifacts.NorcaBinary(goos, t.node.Arch)
	if err != nil {
		return err
	}
	changed, err := connector.SyncLocalFile(t.conn, file.Path, file.SHA256, dest, 0755, t.isWindows())
	if err != nil {
		return err
	}
	if changed {
		fmt.Printf("    -> [%s] norca binary %s installed\n", t.node.Host, r.plan.Release.Version)
	}
	return nil
}

// installCNI unpacks the CNI plugin archive into cni_bin_dir unless the same
// archive was installed before. CNI plugins are run per allocation, so a new
// version needs no agent restart.
func (r *Runner) installCNI(t *target) error {
	file, err := r.artifacts.CNIArchive(t.node.Arch)
	if err != nil {
		return err
	}
	marker := t.paths.CNIBinDir + "/.norca-deployer-cni.sha256"
	if current, err := t.conn.ReadFile(marker); err == nil && strings.TrimSpace(string(current)) == file.SHA256 {
		return nil
	}
	remote := cliStateDir + "/cni-plugins.tgz"
	if err := t.conn.MkdirAll(cliStateDir, 0700); err != nil {
		return err
	}
	f, err := os.Open(file.Path)
	if err != nil {
		return err
	}
	err = t.conn.CopyFile(f, remote, 0600)
	f.Close()
	if err != nil {
		return err
	}
	cmd := "tar -xzf " + connector.ShellQuote(remote) + " -C " + connector.ShellQuote(t.paths.CNIBinDir) +
		" && rm -f " + connector.ShellQuote(remote)
	if out, err := t.conn.ExecuteCommand(cmd); err != nil {
		return fmt.Errorf("failed to unpack: %w (output: %s)", err, out)
	}
	if err := t.conn.CopyFile(strings.NewReader(file.SHA256+"\n"), marker, 0644); err != nil {
		return err
	}
	fmt.Printf("    -> [%s] CNI plugins %s installed\n", t.node.Host, r.plan.CNI.Version)
	return nil
}

// prepareHostVolumes creates the directories of custom host volumes with
// their mode and owner; the agent refuses to start with a missing one.
func (r *Runner) prepareHostVolumes(t *target) error {
	cs := t.node.ClientSettings
	if cs == nil || !t.needsClient() {
		return nil
	}
	for _, hv := range cs.CustomHostVolumes {
		mode := os.FileMode(0755)
		if hv.Mode != "" {
			var m uint32
			if _, err := fmt.Sscanf(hv.Mode, "%o", &m); err != nil {
				return fmt.Errorf("host volume %q: invalid mode %q", hv.Name, hv.Mode)
			}
			mode = os.FileMode(m)
		}
		if err := t.conn.MkdirAll(hv.Path, mode); err != nil {
			return fmt.Errorf("host volume %q: %w", hv.Name, err)
		}
		if hv.Owner != "" && !t.isWindows() {
			cmd := "chown " + connector.ShellQuote(hv.Owner) + " " + connector.ShellQuote(hv.Path)
			if out, err := t.conn.ExecuteCommand(cmd); err != nil {
				return fmt.Errorf("host volume %q: chown failed: %w (output: %s)", hv.Name, err, out)
			}
		}
	}
	return nil
}

func (r *Runner) createConnector(node *config.NodeConfig) (connector.Connector, error) {
	if strings.ToLower(node.Connection.Mode) == "local" {
		return connector.NewLocalConnector(), nil
	}

	// Decrypt credentials stored as inline encrypted secrets
	c := node.Connection
	for _, sp := range []*string{&c.SSHPassword, &c.SSHKeyPassphrase, &c.SSHPrivateKey, &c.BecomePassword} {
		v, err := r.resolveSecret(*sp)
		if err != nil {
			return nil, err
		}
		*sp = v
	}

	tofuFile := ""
	if r.store != nil {
		tofuFile = r.store.KnownHostsPath()
	}
	hostKeyCallback, err := connector.HostKeyCallback(c.HostKeyChecking, connector.ExpandHome(c.KnownHostsFile), tofuFile)
	if err != nil {
		return nil, err
	}

	isWin := strings.ToLower(node.Platform) == "windows"
	opts := connector.SSHOptions{
		Host:            node.Host,
		Port:            c.SSHPort,
		User:            c.SSHUser,
		Password:        c.SSHPassword,
		KeyPath:         c.SSHKeyPath,
		KeyPassphrase:   c.SSHKeyPassphrase,
		PrivateKeyData:  []byte(c.SSHPrivateKey),
		Timeout:         15 * time.Second,
		Become:          c.Become && !isWin,
		BecomePassword:  c.BecomePassword,
		HostKeyCallback: hostKeyCallback,
		Windows:         isWin,
	}

	return connector.NewSSHConnector(opts)
}

// resolveSecret decrypts an inline !enc:v1 secret. A secret that cannot be
// decrypted is an error instead of being used verbatim as a credential.
func (r *Runner) resolveSecret(val string) (string, error) {
	if !strings.HasPrefix(strings.TrimSpace(val), crypto.EncryptedPrefix) {
		return val, nil
	}
	if r.vault == nil {
		return "", fmt.Errorf("an encrypted secret is configured but no master key is available (use --secret-key, --secret-key-file or NORCA_DEPLOYER_SECRET_KEY)")
	}
	return r.vault.ResolveSecret(val)
}

// postClusterTasks bootstraps ACLs and installs the packs through the first
// server.
func (r *Runner) postClusterTasks(first *target) error {
	token, err := r.aclBootstrap(first)
	if err != nil {
		return err
	}
	if err := r.decommission(first, token); err != nil {
		return err
	}
	if err := r.applyACLPolicies(first, token); err != nil {
		return err
	}
	if err := r.applyNodePools(first, token); err != nil {
		return err
	}
	if err := r.bootstrapSecrets(first, token); err != nil {
		return err
	}
	return r.deployPacks(first, token)
}

// aclBootstrap bootstraps the ACL system once and stores the management
// token (encrypted when a master key is available). On later applies the
// stored token is reused.
func (r *Runner) aclBootstrap(first *target) (string, error) {
	acl := r.plan.Security.ACL
	if acl == nil || !acl.Enabled {
		return "", nil
	}
	if r.plan.IsReplicaRegion() {
		// ACLs replicate from the authoritative region; its management
		// token is valid here too.
		fmt.Printf("    Region %s replicates ACLs from %s (no bootstrap)\n", r.plan.Metadata.Region, r.plan.Federation.AuthoritativeRegion)
		return r.replicationToken, nil
	}
	tokenPath := r.plan.ResolveLocalPath(acl.ManagementTokenOutput)
	if token, ok, err := r.store.ReadSecret(tokenPath); err != nil {
		return "", err
	} else if ok {
		fmt.Printf("    ACL management token loaded from %s\n", tokenPath)
		return token, nil
	}
	if !acl.AutoBootstrap {
		fmt.Printf("    ACL auto bootstrap disabled; no management token in %s\n", tokenPath)
		return "", nil
	}

	out, err := first.cli(r, "").run([]string{"acl", "bootstrap", "-json"}, nil)
	if err != nil {
		if strings.Contains(out, "bootstrap already done") || strings.Contains(out, "ACL bootstrap already done") {
			fmt.Printf("    [WARN] ACLs were bootstrapped before, but %s has no token; packs are skipped\n", tokenPath)
			return "", nil
		}
		return "", fmt.Errorf("acl bootstrap failed: %w", err)
	}
	var resp struct {
		SecretID string `json:"SecretID"`
	}
	start := strings.Index(out, "{")
	if start < 0 || json.Unmarshal([]byte(out[start:]), &resp) != nil || resp.SecretID == "" {
		return "", fmt.Errorf("unexpected acl bootstrap output: %s", out)
	}
	plain, err := r.store.WriteSecret(tokenPath, resp.SecretID)
	if err != nil {
		return "", fmt.Errorf("failed to save the management token: %w", err)
	}
	if plain {
		fmt.Printf("    [WARN] ACL management token stored UNENCRYPTED in %s (create a master key with 'secret create-key')\n", tokenPath)
	} else {
		fmt.Printf("    ACL bootstrapped; management token stored encrypted in %s\n", tokenPath)
	}
	return resp.SecretID, nil
}
