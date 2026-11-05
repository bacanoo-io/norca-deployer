package gitops

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bacanoo-io/norca-deployer/internal/config"
)

// DefaultDebounce coalesces rapid sequential pushes.
const DefaultDebounce = 5 * time.Second

// Reconciler manages GitOps bindings, webhook events, and debounced reconciliations.
type Reconciler struct {
	logger   *RingLogger
	client   APIClient
	debounce time.Duration

	mu         sync.Mutex
	bindings   map[string]config.GitOpsBinding
	debouncers map[string]*debouncer
	locks      map[string]*sync.Mutex
	closed     bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// debouncer is the pending event of a binding. A push during a run waits for
// it, and only the latest one runs.
type debouncer struct {
	timer   *time.Timer
	pending *WebhookEvent
	waiting bool // the timer is set
	running bool
}

// NewReconciler creates a new GitOps reconciler. A debounce of 0 uses
// DefaultDebounce.
func NewReconciler(logger *RingLogger, client APIClient, debounce time.Duration) *Reconciler {
	if logger == nil {
		logger = NewRingLogger(200)
	}
	if debounce <= 0 {
		debounce = DefaultDebounce
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Reconciler{
		logger:     logger,
		client:     client,
		debounce:   debounce,
		bindings:   make(map[string]config.GitOpsBinding),
		debouncers: make(map[string]*debouncer),
		locks:      make(map[string]*sync.Mutex),
		ctx:        ctx,
		cancel:     cancel,
	}
}

// RegisterBinding adds a binding whose secrets are resolved to the reconciler.
func (r *Reconciler) RegisterBinding(b config.GitOpsBinding) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bindings[b.Name] = b
}

// Close stops the pending events, cancels the running reconciliations and
// waits for them. It can be called more than once.
func (r *Reconciler) Close() {
	r.mu.Lock()
	r.closed = true
	for _, d := range r.debouncers {
		if d.timer != nil {
			d.timer.Stop()
		}
		d.waiting, d.pending = false, nil
	}
	r.mu.Unlock()
	r.cancel()
	r.wg.Wait()
}

// Schedule runs a binding for an event after the debounce window: rapid
// sequential events become one run against the latest of them.
func (r *Reconciler) Schedule(name string, event *WebhookEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	d, ok := r.debouncers[name]
	if !ok {
		d = &debouncer{}
		r.debouncers[name] = d
	}
	d.pending = event
	if d.timer != nil {
		d.timer.Stop()
	}
	d.waiting = true
	d.timer = time.AfterFunc(r.debounce, func() { r.fire(name, d) })
}

func (r *Reconciler) fire(name string, d *debouncer) {
	r.mu.Lock()
	d.waiting = false
	if r.closed || d.running || d.pending == nil {
		r.mu.Unlock()
		return
	}
	event := d.pending
	d.pending, d.running = nil, true
	r.wg.Add(1)
	r.mu.Unlock()

	go func() {
		defer r.wg.Done()
		// The outcome is in the log.
		_ = r.ReconcileBinding(r.ctx, name, "webhook", event)

		r.mu.Lock()
		d.running = false
		again := d.pending != nil && !d.waiting && !r.closed
		r.mu.Unlock()
		if again {
			r.fire(name, d)
		}
	}()
}

// ReconcileBinding fetches a binding and deploys it. The event selects what
// to fetch: the pushed commit or the tag.
func (r *Reconciler) ReconcileBinding(ctx context.Context, bindingName string, trigger string, event *WebhookEvent) error {
	r.mu.Lock()
	binding, ok := r.bindings[bindingName]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("binding %q not found", bindingName)
	}

	start := time.Now()
	entry := LogEntry{
		BindingName: binding.Name,
		Provider:    binding.EffectiveProvider(),
		Trigger:     trigger,
		CommitSHA:   getEventCommit(event),
		Author:      getEventAuthor(event),
	}
	log := func(phase, details string, err error) {
		e := entry
		e.Phase, e.Details = phase, details
		if err != nil {
			e.Error = err.Error()
		}
		if phase == "SUCCESS" || phase == "FAILED" {
			e.DurationMs = time.Since(start).Milliseconds()
		}
		r.logger.Log(e)
	}
	log("TRIGGER", fmt.Sprintf("Reconciliation triggered via %s", trigger), nil)

	// Release mutex: ensures only one deployment runs at a time for this release
	lockKey := binding.Namespace + "/" + binding.ReleaseName
	r.mu.Lock()
	lock, ok := r.locks[lockKey]
	if !ok {
		lock = &sync.Mutex{}
		r.locks[lockKey] = lock
	}
	r.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()

	// 1. In-Memory Fetch Phase
	opts := FetchOptionsFor(&binding)
	applyEvent(opts, event)
	log("FETCH", fmt.Sprintf("Fetching %s (%s) purely into RAM", SanitizeURL(opts.URL), resolveRef(opts)), nil)

	res, err := FetchPackInMemory(ctx, opts)
	if err != nil {
		log("FAILED", fmt.Sprintf("In-memory fetch failed: %v", err), err)
		return fmt.Errorf("fetch failed: %w", err)
	}
	entry.CommitSHA = res.CommitSHA

	// 2. Deploy Phase
	if res.Signer != "" {
		log("VERIFY", res.Signer, nil)
	}
	log("APPLY", fmt.Sprintf("Deploying pack %s %s to release %s (namespace: %s)", res.PackName, res.PackVersion, binding.ReleaseName, binding.Namespace), nil)
	if err := Deploy(ctx, r.client, res, ReleaseOf(&binding, binding.Values, res)); err != nil {
		log("FAILED", fmt.Sprintf("Deploy failed: %v", err), err)
		return err
	}

	log("SUCCESS", fmt.Sprintf("Release %s successfully deployed in %dms", binding.ReleaseName, time.Since(start).Milliseconds()), nil)
	return nil
}

// applyEvent points the options at what an event carries.
func applyEvent(opts *FetchOptions, event *WebhookEvent) {
	switch {
	case event == nil:
	case event.Tag != "":
		opts.Tag, opts.Branch, opts.Ref = event.Tag, "", ""
	case event.Branch != "":
		opts.Branch, opts.Ref = event.Branch, ""
		opts.CommitSHA = event.CommitSHA
	}
}

func getEventCommit(e *WebhookEvent) string {
	if e != nil {
		return e.CommitSHA
	}
	return ""
}

func getEventAuthor(e *WebhookEvent) string {
	if e != nil {
		return e.Author
	}
	return ""
}
