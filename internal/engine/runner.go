package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bacanoo-io/norca-deployer/internal/artifacts"
	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/crypto"
	"github.com/bacanoo-io/norca-deployer/internal/host"
	"github.com/bacanoo-io/norca-deployer/internal/platform"
	"github.com/bacanoo-io/norca-deployer/internal/platform/packages"
	"github.com/bacanoo-io/norca-deployer/internal/platform/scripts"
	"github.com/bacanoo-io/norca-deployer/internal/platform/step"
	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
	"github.com/bacanoo-io/norca-deployer/internal/state"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
	"github.com/bacanoo-io/norca-pki"
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
	// replicationToken is the acl.replication_token of the servers: the
	// management token of the authoritative region, in every region of a
	// federation (see resolveReplicationToken).
	replicationToken string
	// bootstrapSecret is the secret the ACL bootstrap is to use: the
	// replication token chosen before the first bootstrap of an
	// authoritative region. Empty: the bootstrap chooses the token.
	bootstrapSecret string
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

	// Abort ends at once what is in progress: the commands on the nodes, the
	// downloads and the waits (the second interrupt). nil: never. The context
	// of Apply is the gentle one: it is looked at between the steps, so that
	// the step in progress, a node being restarted, is finished.
	Abort context.Context
}

func (r *Runner) abortCtx() context.Context {
	if r.Abort == nil {
		return context.Background()
	}
	return r.Abort
}

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// interrupted is the error of an apply that stopped between two steps.
func interrupted(ctx context.Context, before string) error {
	return fmt.Errorf("interrupted before %s: %w", before, ctx.Err())
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
	// facts are what the probe found on the node (with the overrides of
	// the plan) and drivers the drivers they choose; set when the node is
	// connected.
	facts   *host.Facts
	drivers *platform.Drivers

	changed    bool // binary, certificates, configuration or service changed
	wasRunning bool
	err        error

	// hostDNSSources is the docker0 subnet whose containers ask the DNS
	// stub of the node (consul.dns_forwarding.docker), for the firewall.
	hostDNSSources []string
}

func (t *target) isWindows() bool { return strings.ToLower(t.node.Platform) == "windows" }

// scriptTarget is the node for the scripts of the plan.
func (t *target) scriptTarget() scripts.Target {
	st := scripts.Target{Node: t.node, IsMaster: t.isMaster}
	if t.facts != nil {
		st.Family = string(t.facts.Family)
	}
	return st
}

// needsClient reports whether the node runs the client (clients and masters
// with client settings).
func (t *target) needsClient() bool { return !t.isMaster || t.node.ClientSettings != nil }

func (t *target) cli(r *Runner, token string) *remoteCLI {
	return &remoteCLI{conn: t.conn, plan: r.plan, node: t.node, paths: t.paths, isMaster: t.isMaster, token: token}
}

// Apply executes the entire cluster deployment plan. When ctx is done it
// stops between the steps: the node being restarted is finished (and its
// health checked), the next one is not started.
func (r *Runner) Apply(ctx context.Context) error {
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
	if err := r.resolveReplicationToken(); err != nil {
		return err
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

	// Every node is connected and probed before the first change: the
	// checks of the plan that need the facts of the nodes run here.
	fmt.Printf("    Connecting to the nodes and detecting their hosts (%d nodes)...\n", len(masters)+len(clients))
	all := append(append([]*target{}, masters...), clients...)
	r.probeAll(ctx, all)
	if err := collect(masters); err != nil {
		return fmt.Errorf("master node provisioning failed (nothing was changed): %w", err)
	}
	if err := r.checkFacts(all); err != nil {
		return err
	}

	fmt.Printf("==> [2/5] Provisioning Master (Server) nodes (%d nodes)...\n", len(masters))
	r.prepareAll(ctx, masters)
	if err := collect(masters); err != nil {
		return fmt.Errorf("master node provisioning failed (no agent was restarted): %w", err)
	}

	if ctx.Err() != nil {
		return interrupted(ctx, "the restart of the servers (nothing was restarted)")
	}
	fmt.Printf("==> [3/5] Rolling restart of Master nodes...\n")
	if err := r.restartMasters(ctx, masters); err != nil {
		return err
	}
	if err := r.recordInstalled(masters, config.RoleMaster); err != nil {
		return err
	}

	var clientErr error
	if len(clients) > 0 {
		fmt.Printf("==> [4/5] Provisioning Client (Worker) nodes (%d nodes)...\n", len(clients))
		r.prepareAll(ctx, clients)
		clientErr = r.restartClients(ctx, clients)
		if err := r.recordInstalled(clients, config.RoleClient); err != nil {
			return err
		}
	} else {
		fmt.Printf("==> [4/5] No dedicated client nodes configured (skipped).\n")
	}

	if ctx.Err() != nil {
		return interrupted(ctx, "the post-cluster phase")
	}
	fmt.Printf("==> [5/5] Post-Cluster phase (ACL, decommission, secrets, Rigger stack & GitOps)...\n")
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
			_ = t.conn.Close()
		}
	}
}

// nodeErrors is the error of several nodes. It keeps the causes, so that
// errors.Is still finds an interrupt (context.Canceled) in it.
type nodeErrors struct {
	msg  string
	errs []error
}

func (e *nodeErrors) Error() string   { return e.msg }
func (e *nodeErrors) Unwrap() []error { return e.errs }

func collect(ts []*target) error {
	var lines []string
	var errs []error
	for _, t := range ts {
		if t.err != nil {
			lines = append(lines, fmt.Sprintf("node [%s]: %v", t.node.Host, t.err))
			errs = append(errs, t.err)
		}
	}
	if len(lines) > 0 {
		return &nodeErrors{msg: fmt.Sprintf("\n - %s", strings.Join(lines, "\n - ")), errs: errs}
	}
	return nil
}

// prepareAll provisions the nodes in parallel without restarting agents.
func (r *Runner) prepareAll(ctx context.Context, ts []*target) {
	var wg sync.WaitGroup
	for _, t := range ts {
		wg.Add(1)
		go func(t *target) {
			defer wg.Done()
			if t.err != nil {
				return // the connection or the probe failed
			}
			if ctx.Err() != nil {
				t.err = interrupted(ctx, "the provisioning of the node")
				return
			}
			t.err = r.prepareNode(t)
		}(t)
	}
	wg.Wait()
}

// prepareNode provisions one node. It does not restart the agent; it records
// whether anything that needs a restart changed.
func (r *Runner) prepareNode(t *target) error {
	node := t.node
	if err := r.openTarget(t); err != nil {
		return err
	}
	if t.drivers == nil {
		// A node connected without its facts (a test): the drivers of
		// its platform.
		t.drivers = defaultDrivers(t.isWindows())
	}
	conn := t.conn
	isWin := t.isWindows()
	paths := t.paths

	binaryBefore := r.binaryDigest(t)

	// 0. Scripts of phase pre_install
	if err := scripts.Run(conn, r.plan, config.PhasePreInstall, t.scriptTarget(), r.resolveSecret); err != nil {
		return err
	}

	// 0b. Host packages: the block of the node's package manager
	if sel := packageSelection(r.plan, t); !sel.Empty() {
		if isWin {
			fmt.Printf("    [WARN] [%s] host_packages skipped on a Windows node\n", node.Host)
		} else {
			steps, err := t.drivers.Packages.Steps(sel, packages.Context{Plan: r.plan, Arch: node.Arch, Resolve: r.resolveSecret})
			if err != nil {
				return fmt.Errorf("host packages: %w", err)
			}
			if err := r.runSteps(t, "host packages", steps); err != nil {
				return err
			}
		}
	}
	if err := scripts.Run(conn, r.plan, config.PhasePostPackages, t.scriptTarget(), r.resolveSecret); err != nil {
		return err
	}

	// 1. Host tuning (kernel modules and sysctl / Windows Registry)
	if r.plan.Tuning.ApplySysctl && !isWin {
		if err := r.runSteps(t, "Linux sysctl tuning error", t.drivers.Tuning.Steps()); err != nil {
			return err
		}
	}
	if r.plan.Tuning.WindowsLongPaths && isWin {
		if err := r.runSteps(t, "Windows environment tuning error", t.drivers.Tuning.Steps()); err != nil {
			return err
		}
	}

	// 1b. DNS of the host: the Consul domain to the local Consul agent
	// (consul.dns_forwarding; Linux only)
	if err := r.configureHostDNS(t); err != nil {
		return err
	}

	// 2. Directory structure preparation
	if err := r.runSteps(t, "directories", []step.Step{dirsStep(nodeDirs(node, paths, t.needsClient()))}); err != nil {
		return err
	}
	if err := r.prepareHostVolumes(t); err != nil {
		return err
	}

	// 3. Norca binary
	bin, err := r.binaryStep(t)
	if err != nil {
		return fmt.Errorf("norca binary: %w", err)
	}
	ex := &step.Executor{Conn: conn, Host: node.Host, Windows: isWin}
	res, err := ex.Run(r.abortCtx(), &bin)
	if err != nil {
		return fmt.Errorf("norca binary: %w", err)
	}
	if res.Changed && len(bin.Files) > 0 {
		fmt.Printf("    -> [%s] norca binary %s installed\n", node.Host, r.plan.Release.Version)
	}
	if after := r.binaryDigest(t); after != binaryBefore {
		t.changed = true
	}
	// 4. CNI plugins
	if t.needsClient() && !isWin && r.plan.CNI != nil && r.plan.CNI.Install != nil && *r.plan.CNI.Install {
		cni, err := r.cniStep(t)
		if err != nil {
			return fmt.Errorf("CNI plugins: %w", err)
		}
		if err := r.runSteps(t, "CNI plugins", []step.Step{cni}); err != nil {
			return err
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
	svc := t.serviceManager()
	if err := r.runSteps(t, "service installation error", svc.InstallSteps(paths, r.plan.Tuning.FileDescriptorLimit)); err != nil {
		return err
	}
	t.wasRunning = svc.IsRunning(conn)

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
func (r *Runner) restartMasters(ctx context.Context, masters []*target) error {
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
			if err := t.serviceManager().Restart(t.conn, t.paths); err != nil {
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
			if ctx.Err() != nil {
				return fmt.Errorf("%w (rolling restart stopped; the remaining servers were not restarted)", interrupted(ctx, "the restart of "+t.node.Host))
			}
			if !r.needsRestart(t) {
				fmt.Printf("    [%s] unchanged, not restarted\n", t.node.Host)
				continue
			}
			fmt.Printf("    [%s] restarting...\n", t.node.Host)
			if err := t.serviceManager().Restart(t.conn, t.paths); err != nil {
				return fmt.Errorf("node [%s]: %w", t.node.Host, err)
			}
			if err := r.waitHealthy(t, expectPeers); err != nil {
				return fmt.Errorf("%w (rolling restart stopped; the remaining servers were not restarted)", err)
			}
		}
	}

	for _, t := range masters {
		if err := scripts.Run(t.conn, r.plan, config.PhasePostInstall, t.scriptTarget(), r.resolveSecret); err != nil {
			return fmt.Errorf("node [%s]: %w", t.node.Host, err)
		}
	}
	return nil
}

// restartClients restarts changed clients in batches of max_parallel_clients.
// Allocations keep running while the client agent restarts. A failed node
// is reported and the others continue.
func (r *Runner) restartClients(ctx context.Context, clients []*target) error {
	var ready []*target
	for _, t := range clients {
		if t.err == nil {
			ready = append(ready, t)
		}
	}

	batch := r.plan.Upgrade.MaxParallelClients
	for start := 0; start < len(ready); start += batch {
		if ctx.Err() != nil {
			return fmt.Errorf("%w (rolling restart stopped; the remaining clients were not restarted)", interrupted(ctx, "the next batch of clients"))
		}
		end := min(start+batch, len(ready))
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
					if err := t.serviceManager().Restart(t.conn, t.paths); err != nil {
						t.err = err
						return
					}
					if err := r.waitHealthy(t, 0); err != nil {
						t.err = err
						return
					}
				}
				t.err = scripts.Run(t.conn, r.plan, config.PhasePostInstall, t.scriptTarget(), r.resolveSecret)
			}(t)
		}
		wg.Wait()
		for _, t := range group {
			if t.err != nil {
				// Stop the rollout: a failing batch may mean a bad release.
				return fmt.Errorf("%w (rolling restart stopped)", collect(clients))
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
		if err := sleep(r.abortCtx(), healthPollInterval); err != nil {
			return fmt.Errorf("node [%s]: waiting for it to be healthy aborted: %w", t.node.Host, err)
		}
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
		return connector.NewLocalConnector(r.abortCtx()), nil
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
		Context:         r.abortCtx(),
		Host:            node.Host,
		Port:            c.SSHPort,
		User:            c.SSHUser,
		Password:        c.SSHPassword,
		KeyPath:         c.SSHKeyPath,
		KeyPassphrase:   c.SSHKeyPassphrase,
		PrivateKeyData:  []byte(c.SSHPrivateKey),
		Timeout:         15 * time.Second,
		Become:          c.Become && !isWin,
		BecomeMethod:    connector.Escalation(c.BecomeMethod),
		BecomePassword:  c.BecomePassword,
		HostKeyCallback: hostKeyCallback,
		Windows:         isWin,
	}

	return connector.NewSSHConnector(opts)
}

// resolveSecret decrypts an inline !enc:v1 secret. A secret that cannot be
// decrypted is an error instead of being used verbatim as a credential.
func (r *Runner) resolveSecret(val string) (string, error) {
	return resolveSecretWith(r.vault, val)
}

// resolveSecretWith decrypts an inline !enc:v1 secret with the vault (nil:
// no master key).
func resolveSecretWith(vault *crypto.Vault, val string) (string, error) {
	if !strings.HasPrefix(strings.TrimSpace(val), crypto.EncryptedPrefix) {
		return val, nil
	}
	if vault == nil {
		return "", fmt.Errorf("an encrypted secret is configured but no master key is available (use --secret-key, --secret-key-file or NORCA_DEPLOYER_SECRET_KEY)")
	}
	return vault.ResolveSecret(val)
}

// resolveReplicationToken finds the acl.replication_token of the servers of
// a federation with ACLs. A replica region reads the management token of the
// authoritative region from federation.replication_token_file. The
// authoritative region uses its own management token: its leader asks the
// other regions with it whether a namespace still has jobs there, when it
// deletes a tenant (Norca does not send its leader ACL to another region).
// Before the first bootstrap that token does not exist yet: a secret is
// chosen, written to the servers' configuration and given to the bootstrap
// later, so the servers start with it and need no second restart.
func (r *Runner) resolveReplicationToken() error {
	acl := r.plan.Security.ACL
	if r.plan.Federation == nil || acl == nil || !acl.Enabled {
		return nil
	}
	if r.plan.IsReplicaRegion() {
		if r.plan.Federation.ReplicationTokenFile == "" {
			return nil
		}
		path := r.plan.ResolveLocalPath(r.plan.Federation.ReplicationTokenFile)
		token, ok, err := r.store.ReadSecret(path)
		if err != nil {
			return fmt.Errorf("replication token: %w", err)
		}
		if !ok {
			return fmt.Errorf("replication token %s not found: apply the authoritative region %q first",
				path, r.plan.Federation.AuthoritativeRegion)
		}
		r.replicationToken = token
		return nil
	}

	tokenPath := r.plan.ResolveLocalPath(acl.ManagementTokenOutput)
	token, ok, err := r.store.ReadSecret(tokenPath)
	if err != nil {
		return fmt.Errorf("management token: %w", err)
	}
	if ok {
		r.replicationToken = token
		return nil
	}
	if !acl.AutoBootstrap {
		fmt.Printf("    [WARN] No management token in %s: the servers get no acl.replication_token, so deleting a tenant fails while its namespace is checked in the other regions\n", tokenPath)
		return nil
	}
	secret, err := r.store.PendingBootstrapSecret(tokenPath)
	if err != nil {
		return err
	}
	r.replicationToken = secret
	r.bootstrapSecret = secret
	return nil
}

// postClusterTasks bootstraps ACLs, writes what the plan has for the running
// cluster (policies, node pools, namespaces, secrets, the node scaling
// Variables) and applies the Rigger stack and the GitOps bindings, all
// through the first server.
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
	if err := r.applyNamespaces(first, token); err != nil {
		return err
	}
	if err := r.bootstrapSecrets(first, token); err != nil {
		return err
	}
	if err := r.publishNodeScaling(first, token); err != nil {
		return err
	}
	if err := r.deployRigger(first, token); err != nil {
		return err
	}
	return r.deployGitOps(first, token)
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

	args := []string{"acl", "bootstrap", "-json"}
	var out string
	var err error
	if r.bootstrapSecret != "" {
		// The servers carry this secret as acl.replication_token already
		// (resolveReplicationToken): it becomes the management token. It is
		// passed as a file, not on stdin, so it arrives without a CR.
		out, err = first.cli(r, "").runFileArg(args, []byte(r.bootstrapSecret))
	} else {
		out, err = first.cli(r, "").run(args, nil)
	}
	if err != nil {
		if strings.Contains(out, "bootstrap already done") || strings.Contains(out, "ACL bootstrap already done") {
			// An earlier apply may have bootstrapped with the chosen
			// secret and stopped before it stored the token.
			if r.bootstrapSecret != "" && r.tokenAccepted(first, r.bootstrapSecret) {
				return r.storeManagementToken(tokenPath, r.bootstrapSecret)
			}
			if r.bootstrapSecret != "" {
				fmt.Printf("    [WARN] ACLs were bootstrapped before with another token: the servers' acl.replication_token is not valid; put the management token into %s and apply again\n", tokenPath)
			}
			fmt.Printf("    [WARN] ACLs were bootstrapped before, but %s has no token; the steps that need it are skipped (ACL policies, node pools, namespaces, secrets, node scaling Variables, the Rigger stack, GitOps bindings)\n", tokenPath)
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
	if r.bootstrapSecret != "" && resp.SecretID != r.bootstrapSecret {
		return "", fmt.Errorf("acl bootstrap returned another token than the secret it was given")
	}
	return r.storeManagementToken(tokenPath, resp.SecretID)
}

// tokenAccepted reports whether the cluster accepts token.
func (r *Runner) tokenAccepted(first *target, token string) bool {
	_, err := first.cli(r, token).run([]string{"acl", "token", "self", "-json"}, nil)
	return err == nil
}

// storeManagementToken stores the management token of a bootstrap (and
// removes a pending bootstrap secret).
func (r *Runner) storeManagementToken(tokenPath, token string) (string, error) {
	var plain bool
	var err error
	if r.bootstrapSecret != "" {
		plain, err = r.store.ConfirmBootstrapSecret(tokenPath, token)
	} else {
		plain, err = r.store.WriteSecret(tokenPath, token)
	}
	if err != nil {
		return "", fmt.Errorf("failed to save the management token: %w", err)
	}
	if plain {
		fmt.Printf("    [WARN] ACL management token stored UNENCRYPTED in %s (create a master key with 'secret create-key')\n", tokenPath)
	} else {
		fmt.Printf("    ACL bootstrapped; management token stored encrypted in %s\n", tokenPath)
	}
	return token, nil
}
