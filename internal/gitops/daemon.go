package gitops

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// maxWebhookBytes is the largest payload of the providers.
const maxWebhookBytes = 25 << 20

// DaemonConfig holds parameters for running the GitOps background daemon.
type DaemonConfig struct {
	ListenAddr string

	// Bindings have their secrets resolved.
	Bindings []config.GitOpsBinding
	Logger   *RingLogger
	Client   APIClient
	Debounce time.Duration

	// APIToken protects the logs and the status. Without it they answer
	// only on the loopback interface.
	APIToken string

	// TLSCertFile and TLSKeyFile serve HTTPS.
	TLSCertFile string
	TLSKeyFile  string
}

// Daemon represents the active GitOps HTTP service.
type Daemon struct {
	server      *http.Server
	reconciler  *Reconciler
	logger      *RingLogger
	bindings    []config.GitOpsBinding
	apiToken    string
	tlsCertFile string
	tlsKeyFile  string
}

// NewDaemon creates a new GitOps Daemon.
func NewDaemon(cfg DaemonConfig) *Daemon {
	if cfg.Logger == nil {
		cfg.Logger = NewRingLogger(500)
	}

	reconciler := NewReconciler(cfg.Logger, cfg.Client, cfg.Debounce)
	for _, b := range cfg.Bindings {
		reconciler.RegisterBinding(b)
	}

	d := &Daemon{
		reconciler:  reconciler,
		logger:      cfg.Logger,
		bindings:    cfg.Bindings,
		apiToken:    cfg.APIToken,
		tlsCertFile: cfg.TLSCertFile,
		tlsKeyFile:  cfg.TLSKeyFile,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/gitops/webhook", d.handleWebhook)
	mux.HandleFunc("/webhook", d.handleWebhook) // Convenience route
	mux.HandleFunc("/v1/gitops/logs", d.protected(d.handleLogs))
	mux.HandleFunc("/v1/gitops/status", d.protected(d.handleStatus))

	addr := cfg.ListenAddr
	if addr == "" {
		addr = ":9095"
	}

	d.server = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
	}

	return d
}

// Start begins listening for webhook events.
func (d *Daemon) Start() error {
	if d.tlsCertFile != "" {
		return d.server.ListenAndServeTLS(d.tlsCertFile, d.tlsKeyFile)
	}
	return d.server.ListenAndServe()
}

// Shutdown gracefully stops the daemon.
func (d *Daemon) Shutdown(ctx context.Context) error {
	err := d.server.Shutdown(ctx)
	d.reconciler.Close()
	return err
}

// protected requires the API token, or a loopback client without one.
func (d *Daemon) protected(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.apiToken != "" {
			token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(token), []byte(d.apiToken)) != 1 {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		} else if host, _, err := net.SplitHostPort(r.RemoteAddr); err != nil || !net.ParseIP(host).IsLoopback() {
			http.Error(w, "Forbidden: set an API token to read this remotely", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// providerMatches tells whether a signature fits the provider of a binding.
// Forgejo also sends the Gitea headers.
func providerMatches(binding, signature string) bool {
	return binding == signature || (binding == "forgejo" && signature == "gitea")
}

func (d *Daemon) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBytes))
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "Payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "Failed to read request body", http.StatusBadRequest)
		return
	}

	// 1. The bindings whose secret signed the request. Without ?binding= a
	// shared webhook URL works for all of them.
	bindingName := r.URL.Query().Get("binding")
	var signed []config.GitOpsBinding
	provider := ""
	known := false
	var lastErr error
	for _, b := range d.bindings {
		if bindingName != "" && b.Name != bindingName {
			continue
		}
		known = true
		if b.Webhook == nil {
			continue
		}
		p, err := VerifyWebhookSignature(r, payload, b.Webhook.SecretKey)
		if err != nil {
			lastErr = err
			continue
		}
		if !providerMatches(b.EffectiveProvider(), p) {
			lastErr = fmt.Errorf("a %s signature for the %s binding %s", p, b.EffectiveProvider(), b.Name)
			continue
		}
		provider = p
		signed = append(signed, b)
	}
	if !known {
		http.Error(w, "Unknown binding", http.StatusNotFound)
		return
	}
	if len(signed) == 0 {
		if lastErr == nil {
			lastErr = errors.New("no binding has a webhook")
		}
		d.logger.Log(LogEntry{
			Trigger: "webhook",
			Phase:   "FAILED",
			Details: fmt.Sprintf("Webhook verification failed from %s: %v", r.RemoteAddr, lastErr),
			Error:   lastErr.Error(),
		})
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// 2. Parse event
	event, err := ParseWebhookPayload(provider, WebhookEventType(provider, r), payload)
	if err != nil {
		http.Error(w, "Bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// 3. Trigger reconciliation after the debounce window
	matched := 0
	for i := range signed {
		b := &signed[i]
		repo := b.EffectiveRepo()
		if event.RepoURL != "" && repoPath(event.RepoURL) != repoPath(repo.URL) {
			continue
		}
		if MatchEvent(event, b.Webhook.Events, b.Webhook.FilterBranches, b.Webhook.FilterTags, repo.Branch) {
			matched++
			d.reconciler.Schedule(b.Name, event)
		}
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(fmt.Sprintf(`{"status":"accepted","matched_bindings":%d,"provider":%q}`, matched, provider)))
}

func (d *Daemon) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	tailStr := r.URL.Query().Get("tail")
	bindingFilter := r.URL.Query().Get("binding")

	n := 50
	if tailStr != "" {
		if parsed, err := strconv.Atoi(tailStr); err == nil && parsed > 0 {
			n = parsed
		}
	}

	entries := d.logger.Tail(n, bindingFilter)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(entries)
}

func (d *Daemon) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status := make([]map[string]any, 0, len(d.bindings))
	for _, b := range d.bindings {
		repo := b.EffectiveRepo()
		recent := d.logger.Tail(1, b.Name)
		lastStatus := "Idle"
		lastTrigger := ""
		lastCommit := ""
		if len(recent) > 0 {
			lastStatus = recent[0].Phase
			lastTrigger = recent[0].Timestamp.Format(time.RFC3339)
			lastCommit = recent[0].CommitSHA
		}

		status = append(status, map[string]any{
			"name":         b.Name,
			"release_name": b.ReleaseName,
			"provider":     b.EffectiveProvider(),
			"url":          SanitizeURL(repo.URL),
			"branch":       repo.Branch,
			"status":       lastStatus,
			"last_sync":    lastTrigger,
			"last_commit":  lastCommit,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}
