package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca-deployer/internal/crypto"
	"github.com/bacanoo-io/norca-deployer/internal/gitops"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// clusterFlags are the flags that reach a Norca cluster. The environment
// variables are those of the norca CLI.
type clusterFlags struct {
	server   string
	tokenEnv string
	caCert   string
	insecure bool
}

func (f *clusterFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.server, "server", "", "Norca HTTP API URL (default: NORCA_ADDR, then http://127.0.0.1:4646)")
	cmd.Flags().StringVar(&f.tokenEnv, "norca-token-env", "NORCA_TOKEN", "Environment variable with the Norca ACL token")
	cmd.Flags().StringVar(&f.caCert, "norca-ca-cert", "", "CA certificate file of the Norca API (default: NORCA_CACERT)")
	cmd.Flags().BoolVar(&f.insecure, "norca-insecure-skip-tls-verify", false, "Do not verify the certificate of the Norca API")
}

func (f *clusterFlags) client() (*gitops.HTTPClient, error) {
	server := f.server
	if server == "" {
		server = os.Getenv("NORCA_ADDR")
	}
	if server == "" {
		server = "http://127.0.0.1:4646"
	}
	caFile := f.caCert
	if caFile == "" {
		caFile = os.Getenv("NORCA_CACERT")
	}
	caPEM := ""
	if caFile != "" {
		data, err := os.ReadFile(caFile) //nolint:gosec // G703: the CA file is named by the user (flag or NORCA_CACERT)
		if err != nil {
			return nil, err
		}
		caPEM = string(data)
	}
	return gitops.NewHTTPClient(server, os.Getenv(f.tokenEnv), caPEM, f.insecure)
}

// secretResolver decrypts !enc: values; one that can't be decrypted is an
// error, never a credential.
func secretResolver() func(string) (string, error) {
	vault := vaultIfAvailable()
	return func(v string) (string, error) {
		if !strings.HasPrefix(strings.TrimSpace(v), crypto.EncryptedPrefix) {
			return v, nil
		}
		if vault == nil {
			return "", errors.New("an encrypted secret is configured but no master key is available (use --secret-key or NORCA_DEPLOYER_SECRET_KEY)")
		}
		return vault.ResolveSecret(v)
	}
}

// runFlags are the flags of gitops run.
type runFlags struct {
	cluster    clusterFlags
	binding    string
	url        string
	branch     string
	tag        string
	ref        string
	commit     string
	provider   string
	subpath    string
	token      string
	tokenEnv   string
	username   string
	password   string
	sshKeyFile string
	knownHosts string
	apiURL     string
	caCert     string
	insecure   bool
	name       string
	namespace  string
	region     string
	atomic     bool
	vars       []string
	verifyKeys []string
	noVerify   bool
}

// binding returns the binding to run: the one of --binding, the first of the
// plan when the flags name no repository, or one made of the flags.
func (f *runFlags) resolveBinding(cmd *cobra.Command) (*config.GitOpsBinding, error) {
	b := &config.GitOpsBinding{}
	if f.url == "" {
		plan, err := config.LoadFromFile(configFile)
		if err != nil {
			return nil, fmt.Errorf("--url or a binding in the configuration is required: %w", err)
		}
		bindings, err := plan.ResolvedGitOpsBindings(secretResolver())
		if err != nil {
			return nil, err
		}
		for i := range bindings {
			if f.binding == "" || bindings[i].Name == f.binding {
				b = &bindings[i]
				break
			}
		}
		if b.Name == "" {
			return nil, errors.New("--url or a configured binding is required")
		}
	} else {
		b.Provider = f.provider
		repo := &config.RepoConfig{URL: f.url}
		switch b.EffectiveProvider() {
		case "gitea":
			b.Gitea = repo
		case "github":
			b.GitHub = &config.GitHubConfig{RepoConfig: *repo}
		case "gitlab":
			b.GitLab = &config.GitLabConfig{RepoConfig: *repo}
		default:
			b.Forgejo = repo
		}
	}

	if f.name != "" {
		b.ReleaseName = f.name
	}
	if b.ReleaseName == "" {
		return nil, errors.New("--name (release name) is required")
	}
	if f.namespace != "" {
		b.Namespace = f.namespace
	}
	if f.region != "" {
		b.Region = f.region
	}
	if cmd.Flags().Changed("atomic") || f.url != "" {
		b.Atomic = f.atomic
	}
	if len(f.vars) > 0 && b.Values == nil {
		b.Values = map[string]any{}
	}
	for _, v := range f.vars {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--var %q: expected name=value", v)
		}
		b.Values[k] = val
	}
	return b, nil
}

// options applies the flags to the fetch options of a binding.
func (f *runFlags) options(b *config.GitOpsBinding) (*gitops.FetchOptions, error) {
	opts := gitops.FetchOptionsFor(b)
	if f.branch != "" {
		opts.Branch, opts.Ref = f.branch, ""
	}
	if f.tag != "" {
		opts.Tag, opts.Branch, opts.Ref = f.tag, "", ""
	}
	if f.ref != "" {
		opts.Ref = f.ref
	}
	opts.CommitSHA = f.commit
	if f.subpath != "" {
		opts.Subpath = f.subpath
	}
	if f.apiURL != "" {
		opts.APIURL = f.apiURL
	}

	token := f.token
	if token == "" && f.tokenEnv != "" {
		token = os.Getenv(f.tokenEnv)
	}
	token, err := secretResolver()(token)
	if err != nil {
		return nil, err
	}
	if token != "" {
		opts.Auth.Token = token
	}
	if opts.Auth.Username = f.username; f.password != "" {
		if opts.Auth.Password, err = secretResolver()(f.password); err != nil {
			return nil, err
		}
	}
	if f.sshKeyFile != "" {
		data, err := os.ReadFile(f.sshKeyFile)
		if err != nil {
			return nil, err
		}
		opts.Auth.SSHPrivateKey = string(data)
	}
	if f.knownHosts != "" {
		data, err := os.ReadFile(f.knownHosts)
		if err != nil {
			return nil, err
		}
		opts.Auth.KnownHosts = string(data)
	}
	if f.caCert != "" {
		data, err := os.ReadFile(f.caCert)
		if err != nil {
			return nil, err
		}
		opts.TLS.CACertPEM = string(data)
	}
	if f.insecure {
		opts.TLS.InsecureSkipVerify = true
	}
	if f.noVerify {
		opts.VerifyKeys = nil
	} else if len(f.verifyKeys) > 0 {
		opts.VerifyKeys = nil
		for _, file := range f.verifyKeys {
			data, err := os.ReadFile(file)
			if err != nil {
				return nil, err
			}
			opts.VerifyKeys = append(opts.VerifyKeys, string(data))
		}
	}
	return opts, nil
}

func (f *runFlags) run(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	client, err := f.cluster.client()
	if err != nil {
		return err
	}
	b, err := f.resolveBinding(cmd)
	if err != nil {
		return err
	}
	opts, err := f.options(b)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(out, "=== In-Memory GitOps Fetch & Deploy ===\n")
	fmt.Fprintf(out, "Provider:     %s\n", b.EffectiveProvider())
	fmt.Fprintf(out, "Repository:   %s\n", gitops.SanitizeURL(opts.URL))
	if opts.Subpath != "" {
		fmt.Fprintf(out, "Subpath:      %s\n", opts.Subpath)
	}
	fmt.Fprintf(out, "Target:       Release %q (namespace: %q, atomic: %t)\n", b.ReleaseName, b.Namespace, b.Atomic)
	fmt.Fprintln(out, "Fetching repository strictly into memory...")

	start := time.Now()
	res, err := gitops.FetchPackInMemory(ctx, opts)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "[OK] Pack %q (version: %s, commit: %s) fetched into RAM in %dms (%d files)\n",
		res.PackName, res.PackVersion, res.CommitSHA, time.Since(start).Milliseconds(), len(res.PackFiles))
	if res.Signer != "" {
		fmt.Fprintf(out, "[OK] Verified: %s\n", res.Signer)
	}

	fmt.Fprintln(out, "Uploading the pack and deploying it...")
	if err := gitops.Deploy(ctx, client, res, gitops.ReleaseOf(b, b.Values, res)); err != nil {
		return err
	}
	fmt.Fprintf(out, "[OK] Release %q successfully deployed in %dms!\n", b.ReleaseName, time.Since(start).Milliseconds())
	return nil
}

// daemonFlags are the flags of gitops daemon.
type daemonFlags struct {
	cluster     clusterFlags
	addr        string
	apiTokenEnv string
	tlsCert     string
	tlsKey      string
}

func (f *daemonFlags) run(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	plan, err := config.LoadFromFile(configFile)
	if err != nil {
		return fmt.Errorf("failed to load configuration (%s): %w", configFile, err)
	}
	bindings, err := plan.ResolvedGitOpsBindings(secretResolver())
	if err != nil {
		return err
	}
	if len(bindings) == 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "[WARN] No GitOps bindings configured in %s\n", configFile)
	}
	for i := range bindings {
		if err := gitops.CheckBinding(&bindings[i]); err != nil {
			return err
		}
	}
	client, err := f.cluster.client()
	if err != nil {
		return err
	}

	d := gitops.NewDaemon(gitops.DaemonConfig{
		ListenAddr:  f.addr,
		Bindings:    bindings,
		Logger:      gitops.NewRingLogger(plan.GitOpsLogBufferSize()),
		Client:      client,
		Debounce:    plan.GitOpsDebounce(),
		APIToken:    os.Getenv(f.apiTokenEnv),
		TLSCertFile: f.tlsCert,
		TLSKeyFile:  f.tlsKey,
	})
	fmt.Fprintf(out, "=== Norca Deployer GitOps Daemon ===\n")
	fmt.Fprintf(out, "Listening for webhooks on: %s\n", f.addr)
	fmt.Fprintf(out, "Endpoints:  POST /v1/gitops/webhook, GET /v1/gitops/logs, GET /v1/gitops/status\n")
	fmt.Fprintf(out, "Bindings registered: %d\n", len(bindings))
	for _, b := range bindings {
		fmt.Fprintf(out, "  - %s: %s (%s, %s)\n", b.Name, b.ReleaseName, b.EffectiveProvider(), gitops.SanitizeURL(b.EffectiveRepo().URL))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- d.Start() }()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("daemon: %w", err)
		}
		return nil
	case <-ctx.Done():
		fmt.Fprintln(out, "Shutting down...")
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return d.Shutdown(shutdown)
	}
}

// logsFlags are the flags of gitops logs.
type logsFlags struct {
	addr        string
	tail        int
	follow      bool
	apiTokenEnv string
}

// url returns the logs URL of the daemon: addr is host:port, :port or a URL.
func (f *logsFlags) url(binding string) string {
	base := f.addr
	switch {
	case strings.HasPrefix(base, "http://"), strings.HasPrefix(base, "https://"):
	case strings.HasPrefix(base, ":"):
		base = "http://127.0.0.1" + base
	default:
		base = "http://" + base
	}
	q := url.Values{"tail": {fmt.Sprint(f.tail)}}
	if binding != "" {
		q.Set("binding", binding)
	}
	return strings.TrimRight(base, "/") + "/v1/gitops/logs?" + q.Encode()
}

func (f *logsFlags) fetch(ctx context.Context, target string) ([]gitops.LogEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	if token := os.Getenv(f.apiTokenEnv); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the daemon answered HTTP %d", resp.StatusCode)
	}
	var entries []gitops.LogEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("failed to parse logs from daemon: %w", err)
	}
	return entries, nil
}

func printLogEntry(out io.Writer, e gitops.LogEntry) {
	fmt.Fprintf(out, "[%s] [%-7s] [%-12s] %s (trigger: %s, duration: %dms)\n",
		e.Timestamp.Format("15:04:05"), e.Phase, e.BindingName, e.Details, e.Trigger, e.DurationMs)
	if e.Error != "" {
		fmt.Fprintf(out, "    -> Error: %s\n", e.Error)
	}
}

func (f *logsFlags) run(cmd *cobra.Command, binding string) error {
	out := cmd.OutOrStdout()
	target := f.url(binding)
	entries, err := f.fetch(cmd.Context(), target)
	if err != nil {
		return fmt.Errorf("could not read the logs of the GitOps daemon at %s: %w (start it with: norca-deployer gitops daemon)", target, err)
	}
	fmt.Fprintf(out, "=== GitOps In-Memory Execution Logs (%d entries) ===\n", len(entries))
	seen := map[string]bool{}
	for _, e := range entries {
		seen[e.ID] = true
		printLogEntry(out, e)
	}
	if !f.follow {
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			entries, err := f.fetch(cmd.Context(), target)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if !seen[e.ID] {
					seen[e.ID] = true
					printLogEntry(out, e)
				}
			}
		}
	}
}

func newGitOpsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gitops",
		Short: "In-memory GitOps deployments, webhook daemon and log viewer",
	}

	// 1. norca-deployer gitops run
	run := &runFlags{}
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Execute an immediate in-memory Git fetch and deployment to the cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			return run.run(cmd)
		},
	}
	f := runCmd.Flags()
	run.cluster.register(runCmd)
	f.StringVar(&run.binding, "binding", "", "Binding of the configuration to run (default: the first)")
	f.StringVar(&run.url, "url", "", "Git repository URL")
	f.StringVar(&run.branch, "branch", "", "Branch to fetch (default: main)")
	f.StringVar(&run.tag, "tag", "", "Tag to fetch")
	f.StringVar(&run.ref, "ref", "", "Any revision to fetch")
	f.StringVar(&run.commit, "commit", "", "Commit to fetch")
	f.StringVar(&run.provider, "provider", "", "Git provider (forgejo, gitea, github, gitlab)")
	f.StringVar(&run.subpath, "subpath", "", "Pack subpath within the repository")
	f.StringVar(&run.token, "token", "", "Git token, or !enc:v1:... (prefer --token-env: arguments are visible to other processes)")
	f.StringVar(&run.tokenEnv, "token-env", "", "Environment variable containing the Git token")
	f.StringVar(&run.username, "username", "", "Git username")
	f.StringVar(&run.password, "password", "", "Git password, or !enc:v1:...")
	f.StringVar(&run.sshKeyFile, "ssh-key-file", "", "SSH private key file of a deploy key")
	f.StringVar(&run.knownHosts, "known-hosts-file", "", "known_hosts file of the Git host")
	f.StringVar(&run.apiURL, "api-url", "", "API URL of a GitHub Enterprise Server or a GitLab instance")
	f.StringVar(&run.caCert, "ca-cert", "", "CA bundle file of the Git host")
	f.BoolVar(&run.insecure, "insecure-skip-tls-verify", false, "Do not verify the certificate of the Git host")
	f.StringVar(&run.name, "name", "", "Release name in the Norca cluster")
	f.StringVar(&run.namespace, "namespace", "", "Release namespace")
	f.StringVar(&run.region, "region", "", "Region of the release")
	f.BoolVar(&run.atomic, "atomic", true, "Automatically rollback on health check failure")
	f.StringArrayVar(&run.vars, "var", nil, "Pack variable as name=value (can be repeated)")
	f.StringArrayVar(&run.verifyKeys, "verify-key-file", nil, "Armored PGP public key file: require a commit or tag signed with it (can be repeated)")
	f.BoolVar(&run.noVerify, "no-verify", false, "Do not verify signatures, even if the binding requires it")
	runCmd.MarkFlagsMutuallyExclusive("verify-key-file", "no-verify")

	// 2. norca-deployer gitops daemon
	daemon := &daemonFlags{}
	daemonCmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run the continuous webhook listener daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return daemon.run(cmd)
		},
	}
	daemon.cluster.register(daemonCmd)
	daemonCmd.Flags().StringVar(&daemon.addr, "addr", ":9095", "HTTP listen address for incoming webhooks")
	daemonCmd.Flags().StringVar(&daemon.apiTokenEnv, "api-token-env", "NORCA_GITOPS_API_TOKEN", "Environment variable with the token of the logs and status API; without one they answer on loopback only")
	daemonCmd.Flags().StringVar(&daemon.tlsCert, "tls-cert", "", "TLS certificate file of the daemon")
	daemonCmd.Flags().StringVar(&daemon.tlsKey, "tls-key", "", "TLS key file of the daemon")
	daemonCmd.MarkFlagsRequiredTogether("tls-cert", "tls-key")

	// 3. norca-deployer gitops status
	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Display status of configured GitOps bindings",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			plan, err := config.LoadFromFile(configFile)
			if err != nil {
				return err
			}

			bindings := plan.AllGitOpsBindings()
			fmt.Fprintf(out, "=== Configured GitOps Bindings (%d) ===\n", len(bindings))
			if len(bindings) == 0 {
				fmt.Fprintln(out, "No bindings configured.")
				return nil
			}
			for _, b := range bindings {
				repo := b.EffectiveRepo()
				fmt.Fprintf(out, "Binding:   %s\n", b.Name)
				fmt.Fprintf(out, "  Release: %s (Namespace: %s, Atomic: %t)\n", b.ReleaseName, b.Namespace, b.Atomic)
				fmt.Fprintf(out, "  Repo:    %s (%s, branch: %s)\n", gitops.SanitizeURL(repo.URL), b.EffectiveProvider(), repo.Branch)
				if b.Webhook != nil && len(b.Webhook.Events) > 0 {
					fmt.Fprintf(out, "  Webhook: events: %v, branches: %v\n", b.Webhook.Events, b.Webhook.FilterBranches)
				}
				fmt.Fprintln(out)
			}
			return nil
		},
	}

	// 4. norca-deployer gitops logs
	logs := &logsFlags{}
	logsCmd := &cobra.Command{
		Use:   "logs [binding-name]",
		Short: "Display in-memory ring-buffer execution and audit logs",
		RunE: func(cmd *cobra.Command, args []string) error {
			binding := ""
			if len(args) > 0 {
				binding = args[0]
			}
			return logs.run(cmd, binding)
		},
	}
	logsCmd.Flags().IntVar(&logs.tail, "tail", 50, "Number of log lines to show")
	logsCmd.Flags().BoolVarP(&logs.follow, "follow", "f", false, "Follow the logs")
	logsCmd.Flags().StringVar(&logs.addr, "addr", ":9095", "Address of the running GitOps daemon")
	logsCmd.Flags().StringVar(&logs.apiTokenEnv, "api-token-env", "NORCA_GITOPS_API_TOKEN", "Environment variable with the token of the logs API")

	cmd.AddCommand(runCmd, daemonCmd, statusCmd, logsCmd)
	return cmd
}
