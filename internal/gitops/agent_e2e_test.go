// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package gitops

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSyncAgainstAgent has the servers of a running agent without ACLs
// (norca agent -dev) plan and apply a stack of one Variable, as a binding
// does after its fetch: a plan alone, the apply with its operation followed
// to the end, the same stack again (in sync), and the stack with another
// value (applied again). The servers record the origin in the operation and
// the commit in the release. NORCA_DEPLOYER_GITOPS_E2E=1, with NORCA_ADDR
// (http://127.0.0.1:4646 by default); for an agent of another region than
// global, NORCA_REGION names it.
func TestSyncAgainstAgent(t *testing.T) {
	if os.Getenv("NORCA_DEPLOYER_GITOPS_E2E") == "" {
		t.Skip("NORCA_DEPLOYER_GITOPS_E2E is not set")
	}

	addr := cmp.Or(os.Getenv("NORCA_ADDR"), "http://127.0.0.1:4646")
	region := cmp.Or(os.Getenv("NORCA_REGION"), "global")

	client, err := NewHTTPClient(addr, "", "", false)
	if err != nil {
		t.Fatal(err)
	}

	var events []string

	runner := &Runner{Client: client, Report: func(e Event) { events = append(events, e.Phase+" "+e.Details) }}
	commit := strings.Repeat("a", 40)

	run := func(text string, planOnly bool) *StackRun {
		return &StackRun{
			Binding:   "e2e",
			Namespace: "default",
			Region:    region,
			PlanOnly:  planOnly,
			Commit:    commit,
			Values:    map[string]any{"text": text},
			Origin:    &Origin{Repository: "https://git.example.org/acme/env.git", Ref: "main", Commit: commit, Signer: "ops@example.org"},
			Files: map[string][]byte{
				"stack.hcl": []byte(`rigger {
  name      = "gitops-e2e"
  region    = "` + region + `"
  namespace = "default"
}

module "note" {
  source = "./modules/note"
  values = { text = var.text }
}
`),
				"variables.hcl":               []byte("variable \"text\" {\n  default = \"one\"\n}\n"),
				"modules/note/metadata.hcl":   []byte("module {\n  name    = \"note\"\n  version = \"1.0.0\"\n}\n"),
				"modules/note/variables.hcl":  []byte("variable \"text\" {}\n"),
				"modules/note/note.var.hcl":   []byte("variable \"note\" {\n  path = \"gitops-e2e/note\"\n\n  items {\n    text = var.text\n  }\n}\n"),
				".env":                        []byte("TOKEN=not-sent"),
				"modules/note/.hidden/secret": []byte("not sent"),
			},
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	sync := func(text string, planOnly bool, want string) *Result {
		t.Helper()

		res, err := runner.Sync(ctx, run(text, planOnly))
		if err != nil {
			t.Fatalf("Sync(%q, plan only %t) = %v\nevents:\n%s", text, planOnly, err, strings.Join(events, "\n"))
		}

		if res.Outcome != want || res.Stack != "gitops-e2e" {
			t.Fatalf("Sync(%q, plan only %t) = %+v, want the outcome %s of the stack gitops-e2e\nevents:\n%s",
				text, planOnly, res, want, strings.Join(events, "\n"))
		}

		return res
	}

	sync("one", true, OutcomePlanned)

	applied := sync("one", false, OutcomeApplied)
	if applied.Operation == "" {
		t.Fatalf("the apply names no operation: %+v", applied)
	}

	if !strings.Contains(strings.Join(events, "\n"), "succeeded") {
		t.Errorf("the events of the apply do not say that the operation succeeded:\n%s", strings.Join(events, "\n"))
	}

	sync("one", false, OutcomeInSync)
	sync("two", false, OutcomeApplied)

	get := func(path string, out any) {
		t.Helper()

		status, body, err := client.API(ctx, http.MethodGet, path, nil)
		if err != nil || status != http.StatusOK {
			t.Fatalf("GET %s = %d, %v: %s", path, status, err, body)
		}

		if err := json.Unmarshal([]byte(body), out); err != nil {
			t.Fatalf("GET %s: %v: %s", path, err, body)
		}
	}

	q := url.Values{"namespace": {"default"}, "region": {region}}.Encode()

	// The release keys are PascalCase or camelCase, by the build of the
	// agent: read them without their case.
	var release map[string]any

	get("/v1/release/note?"+q, &release)

	if got := lookup(release, "spec", "pack", "commit"); got != commit {
		t.Errorf("the release records the commit %v, want %s: %v", got, commit, release)
	}

	var op map[string]any

	get("/v1/object/rigger.norca.io/Operation/"+url.PathEscape(applied.Operation)+"?"+q, &op)

	if got := lookup(op, "spec", "origin", "repository"); got != "https://git.example.org/acme/env.git" {
		t.Errorf("the operation records the repository %v: %v", got, op)
	}

	if got := lookup(op, "spec", "origin", "signer"); got != "ops@example.org" {
		t.Errorf("the operation records the signer %v: %v", got, op)
	}
}

// lookup reads a value of nested JSON objects by its keys, whatever their
// case.
func lookup(v any, keys ...string) any {
	for _, key := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}

		v = nil

		for k, val := range m {
			if strings.EqualFold(k, key) {
				v = val

				break
			}
		}
	}

	return v
}
