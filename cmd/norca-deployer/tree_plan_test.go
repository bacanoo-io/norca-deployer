// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// treeServer answers the stacks API: the plan of each namespace from plans,
// and records the token of each plan and the registry syncs.
type treeServer struct {
	mu     sync.Mutex
	plans  map[string]string
	tokens map[string]string
	synced []string
}

func (s *treeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case strings.HasPrefix(r.URL.Path, "/v1/rigger/registry/") && strings.HasSuffix(r.URL.Path, "/sync"):
		s.synced = append(s.synced, strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/rigger/registry/"), "/sync"))
		_, _ = w.Write([]byte(`{}`))
	case r.URL.Path == "/v1/rigger/stack/plan":
		ns := r.URL.Query().Get("namespace")
		s.tokens[ns] = r.Header.Get("X-Nomad-Token")
		_, _ = fmt.Fprint(w, s.plans[ns])
	default:
		http.Error(w, "not in the fake", http.StatusNotFound)
	}
}

// stackPlanOf is the answer of a plan of one release.
func stackPlanOf(stack, ns, action, changes string) string {
	return fmt.Sprintf(`{"Plan":{"stack":%q,"region":"global","releases":[{"release":{"namespace":%q,"name":"r"},"action":%q,"module":{"name":"r"},"changes":[%s]}]}}`,
		stack, ns, action, changes)
}

// The exit codes and the JSON of tree plan and apply, the token of each
// stack from the startup configuration, and the registry sync before the
// plans.
func TestTreePlanCommand(t *testing.T) {
	t.Setenv(experimentalEnv, "tree")
	t.Setenv("T_DEFAULT", "token-default")
	t.Setenv("T_APPS", "token-apps")

	srv := &treeServer{tokens: map[string]string{}, plans: map[string]string{
		"default": stackPlanOf("tree-global", "default", "unchanged", ""),
		"apps":    stackPlanOf("tree-ns-apps", "apps", "upgrade", `{"kind":"job","name":"old","action":"delete"}`),
	}}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	dir := t.TempDir()
	for name, content := range map[string]string{
		"tree/tree.hcl": "tree {\n  name    = \"acme\"\n  version = \"norca.io/tree/v1alpha1\"\n}\n",
		"tree/global/namespaces/apps.namespace.hcl": "namespace \"apps\" {}\n",
		"tree/namespaces/apps/jobs/api.norca.hcl":   "job \"api\" {}\n",
		"norca-tree.hcl": "region \"global\" {\n  address   = \"" + ts.URL + "\"\n  token_env = \"T_DEFAULT\"\n\n" +
			"  scoped_token \"apps\" {\n    token_env = \"T_APPS\"\n    stacks    = [\"tree-ns-apps\"]\n  }\n}\n\nsync_registries = [\"global\"]\n",
	} {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	base := []string{"--config", filepath.Join(dir, "norca-tree.hcl"), "-d", filepath.Join(dir, "tree")}
	args := func(more ...string) []string { return append(append([]string{}, more...), base...) }

	// JSON: the plan of each stack, and why the run is blocked.
	code, stdout, stderr := execute(t, args("tree", "plan", "--format", "json")...)
	if code != exitOK {
		t.Fatalf("plan --format json = %d, stderr %q", code, stderr)
	}

	var doc planJSON
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}

	if doc.Schema != planSchema || doc.Tree != "acme" || len(doc.Stacks) != 2 || doc.Stacks[1].Blocked == "" || !strings.Contains(doc.Blocked, "needs --prune") {
		t.Fatalf("the JSON plan %+v", doc)
	}

	srv.mu.Lock()
	if srv.tokens["default"] != "token-default" || srv.tokens["apps"] != "token-apps" || len(srv.synced) != 1 || srv.synced[0] != "global" {
		t.Errorf("tokens %v, synced %v", srv.tokens, srv.synced)
	}
	srv.mu.Unlock()

	// A blocked run: 3 for plan --detailed-exitcode and for apply, which
	// applies nothing.
	if code, _, stderr := execute(t, args("tree", "plan", "--detailed-exitcode")...); code != exitBlocked || strings.Contains(stderr, "[ERROR]") {
		t.Fatalf("a blocked plan = %d, stderr %q", code, stderr)
	}

	code, _, stderr = execute(t, args("tree", "apply", "--yes")...)
	if code != exitBlocked || !strings.Contains(stderr, "the delete of the job old") || !strings.Contains(stderr, "needs --prune") {
		t.Fatalf("a blocked apply = %d, stderr %q", code, stderr)
	}

	// --kind blocks a stack that changes another kind.
	srv.mu.Lock()
	srv.plans["apps"] = stackPlanOf("tree-ns-apps", "apps", "upgrade", `{"kind":"job","name":"api","action":"update"}`)
	srv.mu.Unlock()

	if code, _, _ := execute(t, args("tree", "plan", "--detailed-exitcode")...); code != exitChanges {
		t.Fatalf("changes pending = %d, want %d", code, exitChanges)
	}

	if code, stdout, _ := execute(t, args("tree", "plan", "--detailed-exitcode", "--kind", "variable")...); code != exitBlocked || !strings.Contains(stdout, "a kind this run may not change") {
		t.Fatalf("--kind variable = %d, stdout %q", code, stdout)
	}

	if code, _, stderr := execute(t, args("tree", "plan", "--kind", "jobs")...); code != exitUsage || !strings.Contains(stderr, `"jobs" is no kind`) {
		t.Fatalf("--kind jobs = %d, stderr %q", code, stderr)
	}

	// Nothing to change: 0.
	srv.mu.Lock()
	srv.plans["apps"] = stackPlanOf("tree-ns-apps", "apps", "unchanged", "")
	srv.mu.Unlock()

	if code, stdout, _ := execute(t, args("tree", "plan", "--detailed-exitcode")...); code != exitOK || !strings.Contains(stdout, "nothing to do") {
		t.Fatalf("in sync = %d, stdout %q", code, stdout)
	}
}
