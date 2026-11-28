// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package gitops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// A run of a binding has the servers apply a stack (the Rigger stacks API
// of Norca): it sends them the files of the stack directory, they plan it,
// and unless the stack is in sync or the binding only plans, they apply it
// as an operation, which the run follows to its end.

const (
	// deployTimeout is how long a run waits for another operation of its
	// stack to end.
	deployTimeout = 10 * time.Minute

	// conflictWait is the wait before the next try while another operation
	// of the stack runs.
	conflictWait = 5 * time.Second

	// requestTimeout bounds a plan and the start of an apply: the servers
	// bound theirs by ten minutes.
	requestTimeout = 11 * time.Minute

	// logWait is how long the servers hold a read of the log of an
	// operation for its next records, readTimeout what a read may take
	// beyond that.
	logWait     = 10 * time.Second
	readTimeout = time.Minute

	// pollInterval is the wait before the next read of an operation whose
	// last read failed or brought nothing.
	pollInterval = 2 * time.Second

	// maxApplies is how many operations a run starts: an operation that
	// ends Interrupted (the leader changed) is applied again.
	maxApplies = 3

	// maxReadFailures is how many reads in a row may fail while an
	// operation is followed; a change of leader fails some.
	maxReadFailures = 10

	// maxMessageBytes bounds a text of the servers in a log or an error.
	maxMessageBytes = 4096
)

// The phases of an operation of the servers.
const (
	opRunning     = "Running"
	opSucceeded   = "Succeeded"
	opInterrupted = "Interrupted"
)

// The phases of the events of a run.
const (
	PhaseFetch     = "FETCH"
	PhaseVerify    = "VERIFY"
	PhasePlan      = "PLAN"
	PhaseApply     = "APPLY"
	PhaseOperation = "OPERATION"
)

// How a run ended without an error.
const (
	// OutcomeInSync: the plan had nothing to do.
	OutcomeInSync = "in-sync"
	// OutcomePlanned: the binding only plans, and the plan has changes.
	OutcomePlanned = "planned"
	// OutcomeApplied: an operation applied the stack.
	OutcomeApplied = "applied"
)

// Event is a step of a run, for the log of the caller. Stack, Commit and
// Operation are set once the run knows them.
type Event struct {
	Phase     string
	Details   string
	Stack     string
	Commit    string
	Operation string
}

// Result is how a run ended.
type Result struct {
	Outcome   string
	Stack     string
	Commit    string
	Operation string
}

// String says how the run ended, for a log.
func (r *Result) String() string {
	at := ""
	if r.Commit != "" {
		at = " at commit " + shortCommit(r.Commit)
	}
	switch r.Outcome {
	case OutcomeInSync:
		return fmt.Sprintf("stack %s%s is in sync", r.Stack, at)
	case OutcomePlanned:
		return fmt.Sprintf("stack %s%s planned, nothing applied", r.Stack, at)
	}
	return fmt.Sprintf("stack %s%s applied by the operation %s", r.Stack, at, r.Operation)
}

// Origin is where the files of a stack came from, as the servers record it
// in the operation; they write the commit into the releases of the stack's
// own modules. They verify nothing of it.
type Origin struct {
	// Repository is the address of the repository, without credentials.
	Repository string `json:"repository,omitempty"`
	// Ref is the branch, the tag or the revision that was fetched.
	Ref string `json:"ref,omitempty"`
	// Commit is the commit, as the servers take one.
	Commit string `json:"commit,omitempty"`
	// Signer is who signed the commit or the tag, when it was verified.
	Signer string `json:"signer,omitempty"`
}

// originCommitRe is a commit as the servers take one in an origin.
var originCommitRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// maxSignerBytes is the longest signer the servers take.
const maxSignerBytes = 255

// OriginOf returns the origin of a fetched stack. The servers refuse an
// address with credentials, and keep what they are sent for everyone who
// reads the operation.
func OriginOf(repoURL string, res *FetchResult) *Origin {
	o := &Origin{Repository: SanitizeURL(repoURL), Ref: res.Ref, Signer: originSigner(res.Signer)}
	if c := strings.ToLower(res.CommitSHA); originCommitRe.MatchString(c) {
		o.Commit = c
	}
	return o
}

// originSigner is the name of a key as the servers take a signer: without
// control characters, within their bound.
func originSigner(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, name)
	return clip(name, maxSignerBytes)
}

// clip cuts a text to at most n bytes, at a character.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// StackRun is a fetched stack on its way to the servers.
type StackRun struct {
	// Binding names the binding in the events and the errors.
	Binding string
	// Namespace and Region are where the request is sent: those of the
	// rigger block of the stack, or the servers refuse it.
	Namespace string
	Region    string
	// Files are the files of the stack directory, by their paths below it.
	Files map[string][]byte
	// Values are the values of the variables of the stack.
	Values map[string]any
	// Prune uninstalls the releases the stack owns and no longer has.
	Prune bool
	// PlanOnly stops after the plan.
	PlanOnly bool
	// Commit is the commit that was fetched, for the events and the errors.
	Commit string
	Origin *Origin
	// Label is what the errors name the run by; empty is "gitops binding
	// <Binding>". A run of a tree names its stack.
	Label string
	// Check, when set, looks at the plan before an apply starts and stops
	// the run with its error: the guards of a tree run on the plan the
	// apply is made of, not on an older one. A plan with nothing to do is
	// not checked.
	Check func(*StackPlan) error
}

// label is what the errors of the run name it by.
func (run *StackRun) label() string {
	if run.Label != "" {
		return run.Label
	}
	return "gitops binding " + run.Binding
}

// stackRequest is the body of a plan and of an apply.
type stackRequest struct {
	Files      []byte
	ValuesJSON json.RawMessage `json:",omitempty"`
	Prune      bool
	Origin     *Origin `json:",omitempty"`
}

// The answers of the servers, as far as a run reads them.
type (
	stackPlanResponse struct {
		Plan     *StackPlan
		Warnings []string
	}

	// StackPlan is the plan of a stack as the servers answer it: what an
	// apply would do to each release, to the secrets of the stack, and the
	// values it would generate.
	StackPlan struct {
		Stack    string        `json:"stack"`
		Region   string        `json:"region"`
		Releases []ReleasePlan `json:"releases"`
		Generate []string      `json:"generate"`
		Secrets  []SecretPlan  `json:"secrets"`
	}

	// ReleaseRef names a release.
	ReleaseRef struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
	}

	// ModuleRef names the module of a release and its version.
	ModuleRef struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}

	// ReleasePlan is what an apply would do to a release: install,
	// upgrade, unchanged, disabled or remove.
	ReleasePlan struct {
		Release ReleaseRef `json:"release"`
		Action  string     `json:"action"`
		Module  ModuleRef  `json:"module"`
		// Changes are the changes per resource of an upgrade, as the
		// servers' dry run answers them; an install has none.
		Changes []ResourceChange `json:"changes"`
		// TakesOver says whose the release is when an apply would take it
		// over.
		TakesOver string `json:"takes_over"`
	}

	// ResourceChange is the change of one resource of a release: create,
	// update or delete.
	ResourceChange struct {
		Kind   string `json:"kind"`
		Name   string `json:"name"`
		Action string `json:"action"`
		// Error is why the servers would refuse the change; Diff the
		// lines of a job's diff. Servers before Rigger's G27 send neither.
		Error string   `json:"error,omitempty"`
		Diff  []string `json:"diff,omitempty"`
	}

	// SecretPlan is what an apply would do to a secret of the stack:
	// create, keep or overwrite.
	SecretPlan struct {
		Path   string `json:"path"`
		Action string `json:"action"`
	}

	// operation is an operation: the answer of an apply, and the object
	// rigger.norca.io/Operation, whose spec and status have the same keys.
	operation struct {
		Name   string `json:"name"`
		Status struct {
			Phase    string             `json:"phase"`
			Error    string             `json:"error"`
			Plan     []operationRelease `json:"plan"`
			Outcomes []operationRelease `json:"outcomes"`
		} `json:"status"`
	}

	operationRelease struct {
		Namespace string `json:"namespace"`
		Release   string `json:"release"`
		Action    string `json:"action"`
		// Revision is a number with a fraction in the object: its status
		// is stored without a type, and the servers write 1 as 1.0.
		Revision float64 `json:"revision"`
		Skipped  bool    `json:"skipped"`
		Failed   bool    `json:"failed"`
	}

	operationLog struct {
		Lines []struct {
			Seq  uint64
			Line map[string]any
		}
		Phase    string
		Finished bool
		// LogUnavailable says why the page has no records.
		LogUnavailable string
	}
)

// The actions of a plan that change nothing.
const (
	actionUnchanged = "unchanged"
	actionDisabled  = "disabled"
	actionRemove    = "remove"
	secretKeep      = "keep"
)

// apiError is an answer of the servers that is not the one asked for.
type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.Status, clip(strings.TrimSpace(e.Body), maxMessageBytes))
}

// runningOperation is how the servers word the conflict of a stack that has
// an operation (riggerOperations.reserve in norca/norca/rigger_operation.go
// of Norca): "the stack <name> has a running operation, <operation>: ...".
const runningOperation = "has a running operation"

// isConflict reports the answer of the servers while another operation of
// the stack runs. Their other conflicts do not end by waiting, and are
// errors at once: a registry that was never synced, a release that became
// another's.
func isConflict(err error) bool {
	e, ok := errors.AsType[*apiError](err)
	return ok && e.Status == http.StatusConflict && strings.Contains(e.Body, runningOperation)
}

// answerError is an answer of the servers that this program can't read: a
// later try gets the same answer.
type answerError struct{ err error }

func (e *answerError) Error() string { return "the answer of the servers: " + e.err.Error() }
func (e *answerError) Unwrap() error { return e.err }

// isTransient reports a failure that a later try may not have: no answer,
// or an answer of a server in trouble (no leader). An answer that can't be
// read is none.
func isTransient(err error) bool {
	if _, unreadable := errors.AsType[*answerError](err); unreadable {
		return false
	}
	e, ok := errors.AsType[*apiError](err)
	return !ok || e.Status >= http.StatusInternalServerError || e.Status == http.StatusTooManyRequests
}

// Runner runs bindings against a cluster.
type Runner struct {
	Client APIClient
	// Report receives the steps of a run; nil drops them.
	Report func(Event)
}

// ErrApplyNeedsVerify refuses a run that would have the servers apply a
// stack nobody signed.
var ErrApplyNeedsVerify = errors.New(config.GitOpsApplyNeedsVerify)

// Run is one run of a binding: it fetches the stack directory as opts say,
// verifies the signature of its commit or tag, and has the servers plan and
// apply it (Sync). The secrets of the binding are resolved; opts are its
// fetch options, as an event or a flag changed them.
func (r *Runner) Run(ctx context.Context, b *config.GitOpsBinding, opts *FetchOptions, values map[string]any) (*Result, error) {
	// Asked again here: the options of a run may not be those of the plan.
	if !b.PlanOnly() && len(opts.VerifyKeys) == 0 {
		return nil, fmt.Errorf("gitops binding %s: %w", b.Name, ErrApplyNeedsVerify)
	}

	source := fmt.Sprintf("%s (%s)", SanitizeURL(opts.URL), resolveRef(opts))
	if opts.Subpath != "" {
		source += ", directory " + opts.Subpath
	}
	r.report(Event{Phase: PhaseFetch, Details: "fetching the stack from " + source + " into memory", Commit: wantedCommit(opts)})

	res, err := FetchStackInMemory(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("gitops binding %s: fetching the stack from %s: %w", b.Name, source, err)
	}
	if res.Signer != "" {
		r.report(Event{Phase: PhaseVerify, Details: res.Signature(), Commit: res.CommitSHA})
	}

	return r.Sync(ctx, &StackRun{
		Binding:   b.Name,
		Namespace: b.Namespace,
		Region:    b.Region,
		Files:     res.Files,
		Values:    values,
		Prune:     b.Prune(),
		PlanOnly:  b.PlanOnly(),
		Commit:    res.CommitSHA,
		Origin:    OriginOf(opts.URL, res),
	})
}

func (r *Runner) report(e Event) {
	if r.Report != nil {
		r.Report(e)
	}
}

// Sync has the servers bring the cluster to a fetched stack. It plans the
// stack; when the plan has nothing to do the stack is in sync, and a run
// that only plans stops there too. Otherwise it starts the apply and
// follows its operation to the end, passing on the records of its log. An
// operation that ends Interrupted is applied again, maxApplies times at
// most. While another operation of the stack runs (the HTTP 409 that names
// it) it waits and plans again, for deployTimeout at most. Every wait ends
// with ctx.
func (r *Runner) Sync(ctx context.Context, run *StackRun) (*Result, error) {
	s, err := r.newSync(run)
	if err != nil {
		return nil, err
	}

	return s.do(ctx)
}

// Plan has the servers plan a stack and returns their plan and warnings,
// applying nothing. While another operation of the stack runs it waits, as
// Sync does. A tree plans every stack this way before it applies one.
func (r *Runner) Plan(ctx context.Context, run *StackRun) (*StackPlan, []string, error) {
	s, err := r.newSync(run)
	if err != nil {
		return nil, nil, err
	}

	for {
		resp, err := s.plan(ctx)
		if isConflict(err) {
			if err := s.waitForOther(ctx, PhasePlan, err); err != nil {
				return nil, nil, err
			}
			continue
		}
		if err != nil {
			return nil, nil, s.errorf("plan: %w", err)
		}

		s.reportPlan(resp)
		return resp.Plan, resp.Warnings, nil
	}
}

// newSync builds the request body of a run: the files of the stack as an
// archive, its values and its options.
func (r *Runner) newSync(run *StackRun) (*stackSync, error) {
	s := &stackSync{runner: r, run: run}

	archive, err := StackArchive(run.Files)
	if err != nil {
		return nil, s.errorf("%w", err)
	}
	req := stackRequest{Files: archive, Prune: run.Prune, Origin: run.Origin}
	if len(run.Values) > 0 {
		if req.ValuesJSON, err = json.Marshal(run.Values); err != nil {
			return nil, s.errorf("values: %w", err)
		}
	}
	if s.body, err = json.Marshal(req); err != nil {
		return nil, s.errorf("%w", err)
	}

	return s, nil
}

// stackSync is the state of one Sync.
type stackSync struct {
	runner *Runner
	run    *StackRun
	body   []byte

	// stack is the name of the stack, once a plan answered it.
	stack string
	// operation is the operation that runs or ran last.
	operation string
	// conflictSince is when the servers first answered that another
	// operation of the stack runs; zero when they did not.
	conflictSince time.Time
	// lastPlan is the plan as it was reported last.
	lastPlan string
}

// errorf is an error of the run: it names the binding, the stack, the
// commit and the operation, as far as they are known.
func (s *stackSync) errorf(format string, args ...any) error {
	what := s.run.label()
	if s.stack != "" {
		what += ": stack " + s.stack
	}
	if s.run.Commit != "" {
		what += " at commit " + shortCommit(s.run.Commit)
	}
	if s.operation != "" {
		what += ": operation " + s.operation
	}
	return fmt.Errorf("%s: %w", what, fmt.Errorf(format, args...))
}

// shortCommit is a commit as a text names it.
func shortCommit(commit string) string {
	return clip(commit, 12)
}

func (s *stackSync) report(phase, format string, args ...any) {
	s.runner.report(Event{
		Phase: phase, Details: fmt.Sprintf(format, args...),
		Stack: s.stack, Commit: s.run.Commit, Operation: s.operation,
	})
}

func (s *stackSync) result(outcome string) *Result {
	return &Result{Outcome: outcome, Stack: s.stack, Commit: s.run.Commit, Operation: s.operation}
}

func (s *stackSync) do(ctx context.Context) (*Result, error) {
	for applies := 1; ; {
		plan, err := s.plan(ctx)
		if isConflict(err) {
			if err := s.waitForOther(ctx, PhasePlan, err); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, s.errorf("plan: %w", err)
		}

		if !s.reportPlan(plan) {
			s.report(PhasePlan, "the stack %s is in sync: nothing to do", s.stack)
			return s.result(OutcomeInSync), nil
		}
		if s.run.PlanOnly {
			s.report(PhasePlan, "the binding only plans: nothing is applied")
			return s.result(OutcomePlanned), nil
		}
		// The servers refuse such an apply; the plan says it without a try.
		for _, rp := range plan.Plan.Releases {
			if rp.TakesOver != "" {
				return nil, s.errorf("the release %s/%s is of %s: a binding takes over no release (apply the stack once with rigger stack apply -adopt)",
					rp.Release.Namespace, rp.Release.Name, rp.TakesOver)
			}
		}
		if s.run.Check != nil {
			if err := s.run.Check(plan.Plan); err != nil {
				return nil, s.errorf("%w", err)
			}
		}

		op, err := s.apply(ctx)
		if isConflict(err) {
			if err := s.waitForOther(ctx, PhaseApply, err); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, s.errorf("apply: %w", err)
		}
		s.conflictSince = time.Time{}
		s.operation = op.Name
		s.report(PhaseApply, "operation %s of the stack %s started: %d releases", op.Name, s.stack, len(op.Status.Plan))

		end, err := s.follow(ctx, op.Name)
		if err != nil {
			return nil, s.errorf("%w", err)
		}
		for _, o := range end.Status.Outcomes {
			if line, ok := outcomeLine(o); ok {
				s.report(PhaseApply, "%s", line)
			}
		}

		switch end.Status.Phase {
		case opSucceeded:
			s.report(PhaseApply, "operation %s succeeded", op.Name)
			return s.result(OutcomeApplied), nil
		case opInterrupted:
			// The releases already done come back unchanged.
			if applies >= maxApplies {
				return nil, s.errorf("%s, as the %d before it", ending(end), applies-1)
			}
			applies++
			s.report(PhaseApply, "operation %s was %s: applying again, try %d of %d", op.Name, ending(end), applies, maxApplies)
			// What follows is of the next operation.
			s.operation = ""
		default:
			return nil, s.errorf("%s", ending(end))
		}
	}
}

// ending is how an operation ended: its phase and its error.
func ending(op *operation) string {
	phase := strings.ToLower(op.Status.Phase)
	if op.Status.Error == "" {
		return phase
	}
	return phase + " (" + clip(op.Status.Error, maxMessageBytes) + ")"
}

// query is the namespace and the region of the stack as a query.
func (s *stackSync) query() url.Values {
	q := url.Values{}
	if s.run.Namespace != "" {
		q.Set("namespace", s.run.Namespace)
	}
	if s.run.Region != "" {
		q.Set("region", s.run.Region)
	}
	return q
}

// call sends a request and reads its answer into out.
func (s *stackSync) call(ctx context.Context, timeout time.Duration, method, path string, q url.Values, body []byte, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	status, answer, err := s.runner.Client.API(ctx, method, path, body)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return &apiError{Status: status, Body: answer}
	}
	if err := json.Unmarshal([]byte(answer), out); err != nil {
		return &answerError{err: err}
	}
	return nil
}

func (s *stackSync) plan(ctx context.Context) (*stackPlanResponse, error) {
	var out stackPlanResponse
	if err := s.call(ctx, requestTimeout, http.MethodPut, "/v1/rigger/stack/plan", s.query(), s.body, &out); err != nil {
		return nil, err
	}
	if out.Plan == nil {
		return nil, errors.New("the servers answered without a plan")
	}
	s.stack = out.Plan.Stack
	return &out, nil
}

func (s *stackSync) apply(ctx context.Context) (*operation, error) {
	var out operation
	if err := s.call(ctx, requestTimeout, http.MethodPut, "/v1/rigger/stack/apply", s.query(), s.body, &out); err != nil {
		return nil, err
	}
	if out.Name == "" {
		return nil, errors.New("the servers answered without an operation")
	}
	return &out, nil
}

// waitForOther waits before the next try while another operation of the
// stack runs, as cause says.
func (s *stackSync) waitForOther(ctx context.Context, phase string, cause error) error {
	now := time.Now()
	if s.conflictSince.IsZero() {
		s.conflictSince = now
		s.report(phase, "another operation of the stack runs (%v): waiting for its end, %s at most", cause, deployTimeout)
	}
	if now.Sub(s.conflictSince) >= deployTimeout {
		return s.errorf("another operation of the stack still runs after %s: %w", deployTimeout, cause)
	}
	if err := sleep(ctx, conflictWait); err != nil {
		return s.errorf("waiting for another operation of the stack: %w", err)
	}
	return nil
}

// follow passes on the log of an operation until it ends, and returns the
// operation as it ended. A read that fails is tried again: the leader may
// have changed, which ends the operation as Interrupted.
func (s *stackSync) follow(ctx context.Context, name string) (*operation, error) {
	path := "/v1/rigger/operation/" + url.PathEscape(name) + "/log"
	var after uint64
	noLog := false

	for failures := 0; ; {
		q := s.query()
		q.Set("wait", logWait.String())
		if after > 0 {
			q.Set("after", strconv.FormatUint(after, 10))
		}

		start := time.Now()
		var page operationLog
		if err := s.call(ctx, logWait+readTimeout, http.MethodGet, path, q, nil, &page); err != nil {
			if failures++; ctx.Err() != nil || !isTransient(err) || failures >= maxReadFailures {
				return nil, fmt.Errorf("reading its log: %w", err)
			}
			if err := sleep(ctx, pollInterval); err != nil {
				return nil, fmt.Errorf("reading its log: %w", err)
			}
			continue
		}
		failures = 0

		if page.LogUnavailable != "" && !noLog {
			noLog = true
			s.report(PhaseOperation, "the log of the operation can't be read (%s): following its phase", clip(page.LogUnavailable, maxMessageBytes))
		}
		for _, l := range page.Lines {
			after = max(after, l.Seq)
			if line, ok := logLine(l.Line); ok {
				s.report(PhaseOperation, "%s", line)
			}
		}
		if page.Finished {
			break
		}
		// The servers hold a read for its records; one that came back at
		// once with none is not asked again at once.
		if len(page.Lines) == 0 {
			if rest := pollInterval - time.Since(start); rest > 0 {
				if err := sleep(ctx, rest); err != nil {
					return nil, fmt.Errorf("reading its log: %w", err)
				}
			}
		}
	}

	return s.ended(ctx, name)
}

// ended reads an operation whose log is finished, until it is no longer
// Running.
func (s *stackSync) ended(ctx context.Context, name string) (*operation, error) {
	path := "/v1/object/rigger.norca.io/Operation/" + url.PathEscape(name)

	for failures := 0; ; {
		var op operation
		err := s.call(ctx, readTimeout, http.MethodGet, path, s.query(), nil, &op)
		switch {
		case err == nil && op.Status.Phase != opRunning && op.Status.Phase != "":
			op.Name = name
			return &op, nil
		case err == nil:
			failures = 0
		default:
			if failures++; ctx.Err() != nil || !isTransient(err) || failures >= maxReadFailures {
				return nil, fmt.Errorf("reading how it ended: %w", err)
			}
		}
		if err := sleep(ctx, pollInterval); err != nil {
			return nil, fmt.Errorf("reading how it ended: %w", err)
		}
	}
}

// reportPlan reports a plan, once while it stays the same, and returns
// whether an apply would change something: a release to install, upgrade
// or, with prune, remove; a secret to write; a value to generate.
func (s *stackSync) reportPlan(resp *stackPlanResponse) bool {
	plan := resp.Plan
	counts := map[string]int{}
	var lines []string
	changes := false

	for _, rp := range plan.Releases {
		counts[rp.Action]++
		line := fmt.Sprintf("%s %s/%s", rp.Action, rp.Release.Namespace, rp.Release.Name)
		switch {
		case rp.TakesOver != "":
			// An apply would make the release the stack's.
			line += fmt.Sprintf(": module %s %s (takes it over from %s)", rp.Module.Name, rp.Module.Version, rp.TakesOver)
		case rp.Action == actionUnchanged || rp.Action == actionDisabled:
			continue
		case rp.Action == actionRemove && !s.run.Prune:
			lines = append(lines, line+": the stack no longer has it; it stays, as prune is off")
			continue
		case rp.Action == actionRemove:
			line += ": the stack no longer has it"
		default:
			line += fmt.Sprintf(": module %s %s", rp.Module.Name, rp.Module.Version)
		}
		const shown = 10
		for i, c := range rp.Changes {
			if i == shown {
				line += fmt.Sprintf("; and %d more", len(rp.Changes)-shown)
				break
			}
			line += fmt.Sprintf("; %s %s: %s", c.Kind, c.Name, c.Action)
		}
		lines = append(lines, line)
		changes = true
	}
	for _, sp := range plan.Secrets {
		if sp.Action != secretKeep {
			lines = append(lines, fmt.Sprintf("secret %q: %s", sp.Path, sp.Action))
			changes = true
		}
	}
	if len(plan.Generate) > 0 {
		lines = append(lines, "generates "+strings.Join(plan.Generate, ", "))
		changes = true
	}
	for _, w := range resp.Warnings {
		lines = append(lines, "warning: "+clip(w, maxMessageBytes))
	}

	var summary []string
	for _, action := range slices.Sorted(maps.Keys(counts)) {
		summary = append(summary, fmt.Sprintf("%d %s", counts[action], action))
	}
	head := fmt.Sprintf("plan of the stack %s (region %s): %s", plan.Stack, plan.Region, strings.Join(summary, ", "))
	if len(summary) == 0 {
		head = fmt.Sprintf("plan of the stack %s (region %s): no releases", plan.Stack, plan.Region)
	}

	if all := head + "\n" + strings.Join(lines, "\n"); all != s.lastPlan {
		s.lastPlan = all
		s.report(PhasePlan, "%s", head)
		for _, line := range lines {
			s.report(PhasePlan, "%s", line)
		}
	}
	return changes
}

// outcomeLine is what an operation did to a release; nothing for a release
// it had nothing to do with.
func outcomeLine(o operationRelease) (string, bool) {
	what := fmt.Sprintf("%s %s/%s", o.Action, o.Namespace, o.Release)
	switch {
	case o.Failed:
		return what + ": failed", true
	case o.Skipped:
		return what + ": skipped", true
	case o.Action == actionUnchanged || o.Action == actionDisabled:
		return "", false
	}
	return fmt.Sprintf("%s: done (revision %d)", what, uint64(o.Revision)), true
}

// logLine is a record of the log of an operation as one line: its level,
// its message and its attributes by name. The servers keep every record;
// those below INFO are not passed on.
func logLine(record map[string]any) (string, bool) {
	level, _ := record["level"].(string)
	if strings.HasPrefix(strings.ToUpper(level), "DEBUG") {
		return "", false
	}
	msg, _ := record["msg"].(string)

	var b strings.Builder
	b.WriteString(strings.TrimSpace(level + " " + msg))
	for _, key := range slices.Sorted(maps.Keys(record)) {
		if key == "level" || key == "msg" || key == "time" {
			continue
		}
		fmt.Fprintf(&b, " %s=%v", key, record[key])
	}
	return clip(b.String(), maxMessageBytes), true
}

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
