// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package gitops

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// An answer of the servers that can't be read is the same at the next try:
// the run ends at once, without the retries of a server in trouble.
func TestIsTransient_UnreadableAnswer(t *testing.T) {
	t.Parallel()

	unreadable := &answerError{err: errors.New("cannot unmarshal")}
	if isTransient(unreadable) || isTransient(errors.Join(errors.New("reading"), unreadable)) {
		t.Error("an unreadable answer counts as transient")
	}

	if !isTransient(errors.New("connection refused")) || !isTransient(&apiError{Status: http.StatusInternalServerError}) {
		t.Error("no answer and a server error are transient")
	}

	if isTransient(&apiError{Status: http.StatusBadRequest}) {
		t.Error("a refused request counts as transient")
	}
}

// stackRun is a fetched stack on its way to the fake cluster.
func stackRun() *StackRun {
	return &StackRun{
		Binding:   "b",
		Namespace: "production",
		Region:    "eu",
		Files: map[string][]byte{
			"stack.hcl":              []byte(stackHCL),
			"modules/web/module.hcl": []byte("module"),
			".env":                   []byte("TOKEN=secret"),
		},
		Values: map[string]any{"replicas": 3},
		Commit: sha,
		Origin: &Origin{Repository: "https://git.example/o/r.git", Ref: "main", Commit: sha, Signer: "release <release@example.com>"},
	}
}

// syncStack runs a Sync against the fake cluster and returns the events.
func syncStack(ctx context.Context, f *fakeNorca, run *StackRun) (*Result, []Event, error) {
	var events []Event
	r := &Runner{Client: f, Report: func(e Event) { events = append(events, e) }}
	res, err := r.Sync(ctx, run)
	return res, events, err
}

// details are the details of the events of a phase.
func details(events []Event, phase string) []string {
	var out []string
	for _, e := range events {
		if e.Phase == phase {
			out = append(out, e.Details)
		}
	}
	return out
}

func hasLine(lines []string, part string) bool {
	return slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, part) })
}

func TestSync_InSyncStopsAfterThePlan(t *testing.T) {
	f := &fakeNorca{plans: []fakeAnswer{planOf(`{"Plan":{"secrets":[{"path":"rigger/stacks/demo/generated","action":"keep"}]}}`,
		fakeRelease{name: "cache", action: "unchanged"}, fakeRelease{name: "old", action: "disabled"})}}
	res, events, err := syncStack(t.Context(), f, stackRun())
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeInSync || res.Stack != "demo" || res.Commit != sha || res.Operation != "" {
		t.Errorf("result %+v", res)
	}
	if calls := f.called(); len(calls) != 1 || !strings.HasPrefix(calls[0], planCall) {
		t.Errorf("calls %v", calls)
	}
	if !hasLine(details(events, PhasePlan), "the stack demo is in sync") {
		t.Errorf("events %+v", events)
	}
}

// A release the stack no longer has is removed only with prune: without
// it an apply would do nothing, so the stack is in sync.
func TestSync_RemovalCountsWithPruneOnly(t *testing.T) {
	plan := planOf("", fakeRelease{name: "cache", action: "unchanged"}, fakeRelease{name: "old", action: "remove"})

	f := &fakeNorca{plans: []fakeAnswer{plan}}
	res, events, err := syncStack(t.Context(), f, stackRun())
	if err != nil || res.Outcome != OutcomeInSync || f.count(applyCall) != 0 {
		t.Fatalf("result %+v err %v calls %v", res, err, f.called())
	}
	if !hasLine(details(events, PhasePlan), "remove production/old: the stack no longer has it; it stays, as prune is off") {
		t.Errorf("events %+v", events)
	}

	f = &fakeNorca{plans: []fakeAnswer{plan}}
	run := stackRun()
	run.Prune = true
	res, events, err = syncStack(t.Context(), f, run)
	if err != nil || res.Outcome != OutcomeApplied || f.count(applyCall) != 1 {
		t.Fatalf("result %+v err %v calls %v", res, err, f.called())
	}
	if !hasLine(details(events, PhasePlan), "remove production/old: the stack no longer has it") {
		t.Errorf("events %+v", events)
	}
	for _, req := range f.asked() {
		if !req.Prune {
			t.Errorf("%s was sent without prune", req.Path)
		}
	}
}

func TestSync_SecretsAndGeneratedValuesAreChanges(t *testing.T) {
	for name, more := range map[string]string{
		"a secret to create":  `{"Plan":{"secrets":[{"path":"rigger/stacks/demo/generated","action":"create"}]}}`,
		"a value to generate": `{"Plan":{"generate":["db_password"]}}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeNorca{plans: []fakeAnswer{planOf(more, fakeRelease{name: "cache", action: "unchanged"})}}
			res, _, err := syncStack(t.Context(), f, stackRun())
			if err != nil || res.Outcome != OutcomeApplied || f.count(applyCall) != 1 {
				t.Fatalf("result %+v err %v calls %v", res, err, f.called())
			}
		})
	}
}

func TestSync_PlanOnlyLogsThePlanAndStops(t *testing.T) {
	f := &fakeNorca{plans: []fakeAnswer{planOf(
		`{"Plan":{"secrets":[{"path":"rigger/stacks/demo/generated","action":"create"}],"generate":["db_password"]},"Answer":{"Warnings":["the module cache is deprecated"]}}`,
		fakeRelease{name: "cache", action: "install"}, fakeRelease{name: "web", action: "upgrade"},
		fakeRelease{name: "db", action: "unchanged"}, fakeRelease{name: "taken", action: "upgrade", takesOver: "the stack production/other"})}}
	run := stackRun()
	run.PlanOnly = true
	res, events, err := syncStack(t.Context(), f, run)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomePlanned || res.Stack != "demo" {
		t.Errorf("result %+v", res)
	}
	if calls := f.called(); len(calls) != 1 || !strings.HasPrefix(calls[0], planCall) {
		t.Errorf("calls %v", calls)
	}
	plan := details(events, PhasePlan)
	for _, want := range []string{
		"plan of the stack demo (region eu): 1 install, 1 unchanged, 2 upgrade",
		"install production/cache: module redis 0.1.0",
		"upgrade production/web: module redis 0.1.0; Job web: update",
		"takes it over from the stack production/other",
		`secret "rigger/stacks/demo/generated": create`,
		"generates db_password",
		"warning: the module cache is deprecated",
		"the binding only plans: nothing is applied",
	} {
		if !hasLine(plan, want) {
			t.Errorf("%q is not in the plan:\n%s", want, strings.Join(plan, "\n"))
		}
	}
	if hasLine(plan, "production/db") {
		t.Errorf("an unchanged release is no line of a plan:\n%s", strings.Join(plan, "\n"))
	}
}

func TestSync_ApplyFollowsTheOperationToItsEnd(t *testing.T) {
	f := &fakeNorca{}
	res, events, err := syncStack(t.Context(), f, stackRun())
	if err != nil {
		t.Fatal(err)
	}
	op := operationName(0)
	if res.Outcome != OutcomeApplied || res.Stack != "demo" || res.Operation != op || res.Commit != sha {
		t.Errorf("result %+v", res)
	}

	// Every request goes to the namespace and the region of the stack.
	want := []string{
		planCall + "?namespace=production&region=eu",
		applyCall + "?namespace=production&region=eu",
		"GET /v1/rigger/operation/" + op + "/log?namespace=production&region=eu&wait=10s",
		"GET /v1/rigger/operation/" + op + "/log?after=2&namespace=production&region=eu&wait=10s",
		"GET /v1/object/rigger.norca.io/Operation/" + op + "?namespace=production&region=eu",
	}
	if calls := f.called(); !slices.Equal(calls, want) {
		t.Errorf("calls\n%s\nwant\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}

	// The records of the log of the operation are passed on, those below
	// INFO not.
	log := details(events, PhaseOperation)
	if !slices.Equal(log, []string{"INFO started op=release.apply release=cache", "INFO done release=cache revision=1"}) {
		t.Errorf("log %q", log)
	}
	apply := details(events, PhaseApply)
	for _, line := range []string{"operation " + op + " of the stack demo started: 1 releases", "install production/cache: done (revision 1)", "operation " + op + " succeeded"} {
		if !hasLine(apply, line) {
			t.Errorf("%q is not in %q", line, apply)
		}
	}
	if hasLine(apply, "production/db") {
		t.Errorf("an unchanged release is no line of the outcome: %q", apply)
	}

	// The events name the stack, the commit and the operation, once known.
	for _, e := range events {
		if e.Stack != "demo" || e.Commit != sha {
			t.Errorf("event %+v", e)
		}
		if (e.Phase == PhaseOperation) != (e.Operation == op) && e.Phase != PhaseApply {
			t.Errorf("event %+v", e)
		}
	}
}

func TestSync_SendsTheStackAsFiles(t *testing.T) {
	f := &fakeNorca{}
	run := stackRun()
	run.Prune = true
	if _, _, err := syncStack(t.Context(), f, run); err != nil {
		t.Fatal(err)
	}
	reqs := f.asked()
	if len(reqs) != 2 {
		t.Fatalf("requests %d", len(reqs))
	}
	for _, req := range reqs {
		// Nothing but what the servers read: no source, no adopt.
		if keys := slices.Sorted(maps.Keys(req.Keys)); !slices.Equal(keys, []string{"Files", "Origin", "Prune", "ValuesJSON"}) {
			t.Errorf("%s: members %v", req.Path, keys)
		}
		_, files, dir, err := readStackArchive(req.Files)
		if err != nil {
			t.Fatalf("%s: %v", req.Path, err)
		}
		if dir != "." || len(files) != 2 || files["stack.hcl"] != stackHCL || files["modules/web/module.hcl"] != "module" {
			t.Errorf("%s: directory %q files %v", req.Path, dir, files)
		}
		if string(req.ValuesJSON) != `{"replicas":3}` || !req.Prune {
			t.Errorf("%s: values %s prune %t", req.Path, req.ValuesJSON, req.Prune)
		}
		want := map[string]string{"repository": "https://git.example/o/r.git", "ref": "main", "commit": sha, "signer": "release <release@example.com>"}
		if !maps.Equal(req.Origin, want) {
			t.Errorf("%s: origin %v", req.Path, req.Origin)
		}
	}
	// The plan and the apply are of the same files.
	if string(reqs[0].Keys["Files"]) != string(reqs[1].Keys["Files"]) {
		t.Error("the apply sent other files than the plan")
	}

	// Without values, a namespace and a region nothing is said of them:
	// the servers take their defaults.
	f = &fakeNorca{}
	run = stackRun()
	run.Values, run.Namespace, run.Region, run.Origin = nil, "", "", nil
	if _, _, err := syncStack(t.Context(), f, run); err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(f.asked()[0].Keys)); !slices.Equal(keys, []string{"Files", "Prune"}) {
		t.Errorf("members %v", keys)
	}
	if calls := f.called(); calls[0] != planCall || calls[1] != applyCall {
		t.Errorf("calls %v", calls)
	}
}

func TestSync_OperationThatDoesNotSucceedFailsTheRun(t *testing.T) {
	for phase, want := range map[string]string{
		"Failed":    "failed (release cache: the deployment did not become healthy)",
		"Cancelled": "cancelled (release cache: the deployment did not become healthy)",
	} {
		t.Run(phase, func(t *testing.T) {
			f := &fakeNorca{ends: []string{phase}, opError: "release cache: the deployment did not become healthy"}
			res, events, err := syncStack(t.Context(), f, stackRun())
			if err == nil {
				t.Fatalf("result %+v", res)
			}
			// The error names the binding, the stack, the commit and the
			// operation, and carries the error of the operation.
			for _, part := range []string{"gitops binding b", "stack demo", "commit 7f8b91a24d8c", "operation " + operationName(0), want} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("%q is not in: %v", part, err)
				}
			}
			if f.count(applyCall) != 1 {
				t.Errorf("calls %v", f.called())
			}
			if !hasLine(details(events, PhaseApply), "install production/cache: failed") {
				t.Errorf("events %+v", events)
			}
		})
	}
}

func TestSync_InterruptedOperationIsAppliedAgainAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeNorca{ends: []string{opInterrupted, opSucceeded}, opError: "the server that ran it is no longer the leader"}
		start := time.Now()
		res, events, err := syncStack(t.Context(), f, stackRun())
		if err != nil {
			t.Fatal(err)
		}
		if res.Outcome != OutcomeApplied || res.Operation != operationName(1) {
			t.Errorf("result %+v", res)
		}
		if f.count(applyCall) != 2 {
			t.Errorf("calls %v", f.called())
		}
		if waited := time.Since(start); waited != 0 {
			t.Errorf("waited %s before the second apply", waited)
		}
		if !hasLine(details(events, PhaseApply), "operation "+operationName(0)+" was interrupted (the server that ran it is no longer the leader): applying again, try 2 of 3") {
			t.Errorf("events %q", details(events, PhaseApply))
		}
	})
}

func TestSync_InterruptedEveryTimeFailsAfterAFewTries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeNorca{ends: []string{opInterrupted}, opError: "the server that ran it is no longer the leader"}
		_, _, err := syncStack(t.Context(), f, stackRun())
		if err == nil || !strings.Contains(err.Error(), "interrupted") || !strings.Contains(err.Error(), "operation "+operationName(maxApplies-1)) {
			t.Fatalf("got %v", err)
		}
		if n := f.count(applyCall); n != maxApplies {
			t.Errorf("%d applies, want %d", n, maxApplies)
		}
	})
}

// What fails after an interrupted operation is not of that operation.
func TestSync_ErrorAfterAnInterruptedOperationDoesNotNameIt(t *testing.T) {
	f := &fakeNorca{
		ends:  []string{opInterrupted},
		plans: []fakeAnswer{{}, {status: http.StatusBadRequest, body: "the stack demo is invalid now"}},
	}
	_, _, err := syncStack(t.Context(), f, stackRun())
	if err == nil || !strings.Contains(err.Error(), "gitops binding b: stack demo at commit 7f8b91a24d8c: plan: HTTP 400") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), operationName(0)) {
		t.Errorf("the error names the operation that was interrupted: %v", err)
	}
}

// The servers answer 409 for more than a stack that has an operation.
// Waiting ends none of the others: they fail the run at once, with what the
// servers said.
func TestSync_ConflictOfAnotherKindFailsAtOnce(t *testing.T) {
	unsynced := fakeAnswer{status: http.StatusConflict, body: `the registry "global" has not been synced: sync it first (rigger registry sync global)`}
	taken := fakeAnswer{status: http.StatusConflict, body: "release production/cache belongs to pack: it is taken over only when asked to (adopt; the flag -adopt)"}
	for name, tc := range map[string]struct {
		f    *fakeNorca
		want string
	}{
		"a registry that was never synced": {&fakeNorca{plans: []fakeAnswer{unsynced}}, "plan: HTTP 409: " + unsynced.body},
		"a release that became another's":  {&fakeNorca{applies: []fakeAnswer{taken}}, "apply: HTTP 409: " + taken.body},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				_, events, err := syncStack(t.Context(), tc.f, stackRun())
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("got %v", err)
				}
				if waited := time.Since(start); waited != 0 {
					t.Errorf("waited %s", waited)
				}
				if tc.f.count(planCall) != 1 || tc.f.started != 0 {
					t.Errorf("calls %v", tc.f.called())
				}
				for _, e := range events {
					if strings.Contains(e.Details, "another operation") {
						t.Errorf("event %+v", e)
					}
				}
			})
		})
	}
}

func TestSync_ConflictWaitsAndPlansAgain(t *testing.T) {
	for name, f := range map[string]*fakeNorca{
		"on the apply": {applies: []fakeAnswer{conflict, conflict, {}}},
		"on the plan":  {plans: []fakeAnswer{conflict, conflict, {}}},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				res, events, err := syncStack(t.Context(), f, stackRun())
				if err != nil {
					t.Fatal(err)
				}
				if res.Outcome != OutcomeApplied || f.count(applyCall+"?") < 1 || f.count(planCall) != 3 {
					t.Errorf("result %+v calls %v", res, f.called())
				}
				if waited := time.Since(start); waited != 2*conflictWait {
					t.Errorf("waited %s, want %s", waited, 2*conflictWait)
				}
				// The wait is said once, with what the servers answered, and
				// a plan that stays the same is not said again.
				said, plans := 0, 0
				for _, e := range events {
					if strings.Contains(e.Details, "another operation of the stack runs") && strings.Contains(e.Details, conflictText) {
						said++
					}
					if strings.HasPrefix(e.Details, "plan of the stack demo") {
						plans++
					}
				}
				if said != 1 || plans != 1 {
					t.Errorf("the wait was said %d times, the plan %d times: %+v", said, plans, events)
				}
			})
		})
	}
}

func TestSync_ConflictUntilTheTimeoutFailsTheRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeNorca{applies: []fakeAnswer{conflict}}
		start := time.Now()
		_, _, err := syncStack(t.Context(), f, stackRun())
		if err == nil {
			t.Fatal("must fail")
		}
		for _, part := range []string{"gitops binding b", "stack demo", "another operation of the stack still runs after 10m0s", "HTTP 409", conflictText} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("%q is not in: %v", part, err)
			}
		}
		if waited := time.Since(start); waited != deployTimeout {
			t.Errorf("waited %s, want %s", waited, deployTimeout)
		}
		if f.started != 0 {
			t.Errorf("%d operations started", f.started)
		}
	})
}

func TestSync_ContextCancelled(t *testing.T) {
	t.Run("while the operation is followed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := &fakeNorca{logs: func(ctx context.Context, _ int) *fakeAnswer {
				<-ctx.Done() // the servers hold the read
				return &fakeAnswer{err: ctx.Err()}
			}}
			ctx, cancel := context.WithCancel(t.Context())
			go func() {
				time.Sleep(30 * time.Second)
				cancel()
			}()
			start := time.Now()
			_, _, err := syncStack(ctx, f, stackRun())
			if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "operation "+operationName(0)) {
				t.Fatalf("got %v", err)
			}
			if waited := time.Since(start); waited != 30*time.Second {
				t.Errorf("returned after %s", waited)
			}
			if f.logReads != 1 || f.objectReads != 0 {
				t.Errorf("calls %v", f.called())
			}
		})
	})

	t.Run("while another operation is waited for", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := &fakeNorca{applies: []fakeAnswer{conflict}}
			ctx, cancel := context.WithCancel(t.Context())
			go func() {
				time.Sleep(7 * time.Second)
				cancel()
			}()
			start := time.Now()
			_, _, err := syncStack(ctx, f, stackRun())
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("got %v", err)
			}
			if waited := time.Since(start); waited != 7*time.Second {
				t.Errorf("returned after %s", waited)
			}
		})
	})

	t.Run("before the run", func(t *testing.T) {
		f := &fakeNorca{}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, _, err := syncStack(ctx, f, stackRun())
		if !errors.Is(err, context.Canceled) || f.started != 0 {
			t.Fatalf("got %v, %d operations", err, f.started)
		}
	})
}

// A change of leader fails some reads before it ends the operation as
// Interrupted: the run reads again instead of giving up.
func TestSync_FollowReadsAgainAfterAReadThatFailed(t *testing.T) {
	noLeader := &fakeAnswer{status: http.StatusInternalServerError, body: "No cluster leader"}
	noAnswer := &fakeAnswer{err: errors.New("connection refused")}

	t.Run("the log", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := &fakeNorca{logs: func(_ context.Context, n int) *fakeAnswer {
				return []*fakeAnswer{noLeader, noAnswer, noLeader, nil}[min(n, 3)]
			}}
			start := time.Now()
			res, _, err := syncStack(t.Context(), f, stackRun())
			if err != nil || res.Outcome != OutcomeApplied {
				t.Fatalf("result %+v err %v", res, err)
			}
			if waited := time.Since(start); waited != 3*pollInterval {
				t.Errorf("waited %s, want %s", waited, 3*pollInterval)
			}
		})
	})

	t.Run("the operation", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := &fakeNorca{objects: func(_ context.Context, n int) *fakeAnswer {
				return []*fakeAnswer{noAnswer, noLeader, nil}[min(n, 2)]
			}}
			res, _, err := syncStack(t.Context(), f, stackRun())
			if err != nil || res.Outcome != OutcomeApplied {
				t.Fatalf("result %+v err %v", res, err)
			}
		})
	})

	t.Run("no read works", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := &fakeNorca{logs: func(context.Context, int) *fakeAnswer { return noLeader }}
			_, _, err := syncStack(t.Context(), f, stackRun())
			if err == nil || !strings.Contains(err.Error(), "operation "+operationName(0)+": reading its log: HTTP 500: No cluster leader") {
				t.Fatalf("got %v", err)
			}
			if f.logReads != maxReadFailures {
				t.Errorf("%d reads, want %d", f.logReads, maxReadFailures)
			}
		})
	})

	// An answer that a later read would get again is not asked for again.
	t.Run("a read that is refused", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := &fakeNorca{logs: func(context.Context, int) *fakeAnswer {
				return &fakeAnswer{status: http.StatusForbidden, body: "Permission denied"}
			}}
			start := time.Now()
			_, _, err := syncStack(t.Context(), f, stackRun())
			if err == nil || !strings.Contains(err.Error(), "HTTP 403: Permission denied") {
				t.Fatalf("got %v", err)
			}
			if f.logReads != 1 || time.Since(start) != 0 {
				t.Errorf("%d reads in %s", f.logReads, time.Since(start))
			}
		})
	})
}

func TestSync_FollowWaitsForTheEnd(t *testing.T) {
	// The servers hold a read for its records; a page that comes back at
	// once without any is not asked for again at once.
	t.Run("pages without records", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			empty := &fakeAnswer{status: http.StatusOK, body: `{"Lines":[],"Phase":"Running","Finished":false}`}
			f := &fakeNorca{logs: func(_ context.Context, n int) *fakeAnswer {
				if n < 3 {
					return empty
				}
				return nil
			}}
			start := time.Now()
			if _, _, err := syncStack(t.Context(), f, stackRun()); err != nil {
				t.Fatal(err)
			}
			if waited := time.Since(start); waited != 3*pollInterval {
				t.Errorf("waited %s, want %s", waited, 3*pollInterval)
			}
		})
	})

	// A server without the bus has no log to read: the phase is followed.
	t.Run("a log that can't be read", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := &fakeNorca{logs: func(_ context.Context, n int) *fakeAnswer {
				if n == 0 {
					return &fakeAnswer{status: http.StatusOK, body: `{"Lines":null,"Phase":"Running","Finished":false,"LogUnavailable":"the system bus is not enabled"}`}
				}
				return &fakeAnswer{status: http.StatusOK, body: `{"Lines":null,"Phase":"Succeeded","Finished":true,"LogUnavailable":"the system bus is not enabled"}`}
			}}
			res, events, err := syncStack(t.Context(), f, stackRun())
			if err != nil || res.Outcome != OutcomeApplied {
				t.Fatalf("result %+v err %v", res, err)
			}
			log := details(events, PhaseOperation)
			if len(log) != 1 || !strings.Contains(log[0], "the log of the operation can't be read (the system bus is not enabled)") {
				t.Errorf("log %q", log)
			}
		})
	})

	// The log ends before the end of the operation is written.
	t.Run("an operation that still runs", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			running := &fakeAnswer{status: http.StatusOK, body: `{"Name":"x","Status":{"phase":"Running"}}`}
			f := &fakeNorca{objects: func(_ context.Context, n int) *fakeAnswer {
				if n < 2 {
					return running
				}
				return nil
			}}
			start := time.Now()
			res, _, err := syncStack(t.Context(), f, stackRun())
			if err != nil || res.Outcome != OutcomeApplied {
				t.Fatalf("result %+v err %v", res, err)
			}
			if waited := time.Since(start); waited != 2*pollInterval {
				t.Errorf("waited %s, want %s", waited, 2*pollInterval)
			}
		})
	})
}

// The servers refuse an apply that takes over a release of another owner;
// the plan says so, and no apply is tried.
func TestSync_ReleaseOfAnotherOwnerFailsTheRun(t *testing.T) {
	f := &fakeNorca{plans: []fakeAnswer{planOf("", fakeRelease{name: "cache", action: "upgrade", takesOver: "the stack production/other"})}}
	_, _, err := syncStack(t.Context(), f, stackRun())
	if err == nil || !strings.Contains(err.Error(), "the release production/cache is of the stack production/other") {
		t.Fatalf("got %v", err)
	}
	if f.count(applyCall) != 0 {
		t.Errorf("calls %v", f.called())
	}
}

// Check looks at the plan an apply is made of: an error stops the run
// before the apply, and the error names the run by its label.
func TestSync_CheckStopsTheRunBeforeTheApply(t *testing.T) {
	f := &fakeNorca{plans: []fakeAnswer{planOf("", fakeRelease{name: "cache", action: "upgrade"})}}
	run := stackRun()
	run.Label = "tree acme: stack demo"

	var seen *StackPlan
	run.Check = func(p *StackPlan) error {
		seen = p
		return errors.New("a job delete needs --prune-jobs")
	}

	_, _, err := syncStack(t.Context(), f, run)
	if err == nil || !strings.HasPrefix(err.Error(), "tree acme: stack demo: stack demo") || !strings.Contains(err.Error(), "--prune-jobs") {
		t.Fatalf("got %v", err)
	}
	if f.count(applyCall) != 0 {
		t.Errorf("calls %v", f.called())
	}
	if seen == nil || len(seen.Releases) != 1 || len(seen.Releases[0].Changes) != 1 || seen.Releases[0].Changes[0].Action != "update" {
		t.Errorf("Check saw %+v", seen)
	}
}

// A Check that passes lets the apply run; a plan with nothing to do is
// not checked.
func TestSync_CheckThatPassesAndAPlanInSync(t *testing.T) {
	checks := 0
	check := func(*StackPlan) error { checks++; return nil }

	f := &fakeNorca{plans: []fakeAnswer{planOf("", fakeRelease{name: "cache", action: "install"})}, ends: []string{opSucceeded}}
	run := stackRun()
	run.Check = check
	if res, _, err := syncStack(t.Context(), f, run); err != nil || res.Outcome != OutcomeApplied {
		t.Fatalf("got %+v, %v", res, err)
	}

	inSync := &fakeNorca{plans: []fakeAnswer{planOf("", fakeRelease{name: "cache", action: "unchanged"})}}
	run = stackRun()
	run.Check = check
	if res, _, err := syncStack(t.Context(), inSync, run); err != nil || res.Outcome != OutcomeInSync {
		t.Fatalf("got %+v, %v", res, err)
	}

	if checks != 1 {
		t.Errorf("checked %d times, want once", checks)
	}
}

// Plan answers the servers' plan and its warnings, and applies nothing.
func TestRunner_PlanAppliesNothing(t *testing.T) {
	f := &fakeNorca{plans: []fakeAnswer{planOf(`{"Answer":{"Warnings":["a warning"]},"Plan":{"generate":["db"]}}`,
		fakeRelease{name: "cache", action: "upgrade"}, fakeRelease{name: "new", action: "install"})}}

	r := &Runner{Client: f}
	plan, warnings, err := r.Plan(t.Context(), stackRun())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Stack != "demo" || len(plan.Releases) != 2 || plan.Releases[0].Module.Name != "redis" || len(plan.Generate) != 1 {
		t.Errorf("plan %+v", plan)
	}
	if plan.Releases[1].Action != "install" || len(plan.Releases[1].Changes) != 0 {
		t.Errorf("an install has no changes: %+v", plan.Releases[1])
	}
	if len(warnings) != 1 || warnings[0] != "a warning" {
		t.Errorf("warnings %v", warnings)
	}
	if calls := f.called(); len(calls) != 1 || !strings.HasPrefix(calls[0], planCall) {
		t.Errorf("calls %v", calls)
	}
}

func TestSync_PlanThatIsRefusedFailsTheRun(t *testing.T) {
	for name, answer := range map[string]fakeAnswer{
		"an invalid stack":   {status: http.StatusBadRequest, body: `the stack demo is of the namespace "shop": send the request in that namespace`},
		"no right":           {status: http.StatusForbidden, body: "Permission denied"},
		"no answer":          {err: errors.New("connection refused")},
		"not a plan":         {status: http.StatusOK, body: `{"Plan":null}`},
		"not an answer":      {status: http.StatusOK, body: `<html>`},
		"a server in a mess": {status: http.StatusInternalServerError, body: "No cluster leader"},
	} {
		t.Run(name, func(t *testing.T) {
			f := &fakeNorca{plans: []fakeAnswer{answer}}
			_, _, err := syncStack(t.Context(), f, stackRun())
			if err == nil || !strings.Contains(err.Error(), "gitops binding b at commit 7f8b91a24d8c: plan: ") {
				t.Fatalf("got %v", err)
			}
			if answer.body != "" && answer.status != http.StatusOK && !strings.Contains(err.Error(), answer.body) {
				t.Errorf("the answer of the servers is not in: %v", err)
			}
			if len(f.called()) != 1 {
				t.Errorf("calls %v", f.called())
			}
		})
	}
}

func TestSync_StackThatCanNotBeSentFailsTheRun(t *testing.T) {
	f := &fakeNorca{}
	run := stackRun()
	run.Files = map[string][]byte{".env": []byte("x")}
	_, _, err := syncStack(t.Context(), f, run)
	if err == nil || !strings.Contains(err.Error(), "gitops binding b") || len(f.called()) != 0 {
		t.Fatalf("got %v, calls %v", err, f.called())
	}

	run = stackRun()
	run.Values = map[string]any{"bad": func() {}}
	_, _, err = syncStack(t.Context(), f, run)
	if err == nil || !strings.Contains(err.Error(), "values") || len(f.called()) != 0 {
		t.Fatalf("got %v, calls %v", err, f.called())
	}
}

func TestOriginOf(t *testing.T) {
	long := strings.Repeat("é", 200) // 400 bytes
	for name, tc := range map[string]struct {
		url  string
		res  FetchResult
		want Origin
	}{
		"a token in the address": {
			"https://oauth2:ghp_SECRET@github.com/o/r.git",
			FetchResult{CommitSHA: sha, Ref: "main", Signer: "release <release@example.com>", Signed: "commit"},
			Origin{Repository: "https://github.com/o/r.git", Ref: "main", Commit: sha, Signer: "release <release@example.com>"},
		},
		"a tag, nothing verified": {
			"https://git.example/o/r.git",
			FetchResult{CommitSHA: sha, Ref: "v1.2.0"},
			Origin{Repository: "https://git.example/o/r.git", Ref: "v1.2.0", Commit: sha},
		},
		"a commit written large": {
			"https://git.example/o/r.git",
			FetchResult{CommitSHA: "7F8B91A"},
			Origin{Repository: "https://git.example/o/r.git", Commit: "7f8b91a"},
		},
		"what the servers take as no commit": {
			"https://git.example/o/r.git",
			FetchResult{CommitSHA: "HEAD~1", Ref: "main"},
			Origin{Repository: "https://git.example/o/r.git", Ref: "main"},
		},
		"an archive without its commit": {
			"https://git.example/o/r.git",
			FetchResult{Ref: "main"},
			Origin{Repository: "https://git.example/o/r.git", Ref: "main"},
		},
		"a signer the servers would refuse": {
			"ssh://git@git.example/o/r.git",
			FetchResult{CommitSHA: sha, Signer: "re\tlease\n" + long},
			Origin{Repository: "ssh://git@git.example/o/r.git", Commit: sha, Signer: "re lease " + strings.Repeat("é", 123)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := OriginOf(tc.url, &tc.res)
			if *got != tc.want {
				t.Errorf("got  %+v\nwant %+v", *got, tc.want)
			}
			if len(got.Signer) > 255 || strings.Contains(got.Repository, "SECRET") {
				t.Errorf("got %+v", *got)
			}
		})
	}
}

// The address goes into logs, errors and the origin the servers keep.
func TestSanitizeURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/o/r.git":                     "https://github.com/o/r.git",
		"https://oauth2:ghp_SECRET@github.com/o/r.git":   "https://github.com/o/r.git",
		"https://ghp_SECRET@github.com/o/r.git":          "https://github.com/o/r.git",
		"http://user:SECRET@git.example:3000/o/r.git":    "http://git.example:3000/o/r.git",
		"ssh://git@git.example/o/r.git":                  "ssh://git@git.example/o/r.git",
		"ssh://git:SECRET@git.example:2222/o/r.git":      "ssh://git@git.example:2222/o/r.git",
		"git@github.com:o/r.git":                         "git@github.com:o/r.git",
		"file:///srv/git/r.git":                          "file:///srv/git/r.git",
		"https://user:SEC%ZZRET@git.example/o/r.git":     "https://git.example/o/r.git",
		"https://user:SEC RET@git.example/o/r.git?x=a@b": "https://git.example/o/r.git?x=a@b",
	} {
		got := SanitizeURL(in)
		if got != want || strings.Contains(got, "SEC") {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}

func TestLogLine(t *testing.T) {
	for name, tc := range map[string]struct {
		record map[string]any
		want   string // empty: not passed on
	}{
		"a record":          {map[string]any{"time": "2026-11-25T12:00:01Z", "level": "INFO", "msg": "started", "release": "db", "depth": float64(2), "took": "1.8ms"}, "INFO started depth=2 release=db took=1.8ms"},
		"a warning":         {map[string]any{"level": "WARN", "msg": "the module is deprecated"}, "WARN the module is deprecated"},
		"an error":          {map[string]any{"level": "ERROR", "msg": "failed", "error": "no such job"}, "ERROR failed error=no such job"},
		"below info":        {map[string]any{"level": "DEBUG", "msg": "evaluated"}, ""},
		"further below":     {map[string]any{"level": "DEBUG-4", "msg": "trace"}, ""},
		"without a level":   {map[string]any{"msg": "started"}, "started"},
		"a very long value": {map[string]any{"level": "INFO", "msg": strings.Repeat("x", 3*maxMessageBytes)}, "INFO " + strings.Repeat("x", maxMessageBytes-5)},
	} {
		t.Run(name, func(t *testing.T) {
			got, ok := logLine(tc.record)
			if ok != (tc.want != "") || got != tc.want {
				t.Errorf("got %q %t, want %q", got, ok, tc.want)
			}
		})
	}
}

// The servers evaluate a stack in their own process: a run has them apply
// only what a signed commit or tag vouches for, whatever the plan checked.
func TestRun_ApplyNeedsVerifyKeys(t *testing.T) {
	f := &fakeNorca{}
	r := &Runner{Client: f}
	b := &config.GitOpsBinding{Name: "b", Forgejo: &config.RepoConfig{URL: "file:///nowhere"}}

	_, err := r.Run(t.Context(), b, FetchOptionsFor(b), nil)
	if !errors.Is(err, ErrApplyNeedsVerify) || !strings.Contains(err.Error(), "gitops binding b") || !strings.Contains(err.Error(), "plan-only") {
		t.Fatalf("got %v", err)
	}

	// With keys, and when the binding only plans, the run goes on to the
	// fetch, which finds no repository here.
	_, key := newKey(t, "release")
	withKeys := FetchOptionsFor(b)
	withKeys.VerifyKeys = []string{key}
	if _, err := r.Run(t.Context(), b, withKeys, nil); err == nil || errors.Is(err, ErrApplyNeedsVerify) || !strings.Contains(err.Error(), "fetching the stack") {
		t.Fatalf("got %v", err)
	}
	b.Policy = &config.GitOpsPolicy{Mode: config.GitOpsModePlanOnly}
	if _, err := r.Run(t.Context(), b, FetchOptionsFor(b), nil); err == nil || errors.Is(err, ErrApplyNeedsVerify) || !strings.Contains(err.Error(), "fetching the stack") {
		t.Fatalf("got %v", err)
	}
	if len(f.called()) != 0 {
		t.Errorf("calls %v", f.called())
	}
}

// The error of a fetch, and the event before it, name the repository
// without the credentials of its address.
func TestRun_FetchErrorNamesNoCredentials(t *testing.T) {
	for url, shown := range map[string]string{
		"https://ghp_SECRET@127.0.0.1:1/o/r.git":      "https://127.0.0.1:1/o/r.git",
		"https://user:ghp_SECRET@127.0.0.1:1/o/r.git": "https://127.0.0.1:1/o/r.git",
		"ssh://git:ghp_SECRET@127.0.0.1:1/o/r.git":    "ssh://git@127.0.0.1:1/o/r.git",
	} {
		var events []Event
		r := &Runner{Client: &fakeNorca{}, Report: func(e Event) { events = append(events, e) }}
		b := &config.GitOpsBinding{Name: "b", Policy: &config.GitOpsPolicy{Mode: config.GitOpsModePlanOnly},
			Forgejo: &config.RepoConfig{URL: url, Subpath: "stacks/a"}}
		_, err := r.Run(t.Context(), b, FetchOptionsFor(b), nil)
		if err == nil || strings.Contains(err.Error(), "ghp_SECRET") ||
			!strings.Contains(err.Error(), "gitops binding b: fetching the stack from "+shown+" (main), directory stacks/a: ") {
			t.Errorf("%s: got %v", url, err)
		}
		if len(events) != 1 || events[0].Phase != PhaseFetch || strings.Contains(events[0].Details, "ghp_SECRET") || !strings.Contains(events[0].Details, shown) {
			t.Errorf("%s: events %+v", url, events)
		}
	}
}
