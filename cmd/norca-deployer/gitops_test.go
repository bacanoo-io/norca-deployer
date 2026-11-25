// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// stackRepo serves the archive of a repository with a stack at stacks/a, as
// a Forgejo does.
func stackRepo(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"r/stacks/a/stack.hcl": "rigger {\n  name      = \"demo\"\n  namespace = \"production\"\n}\n",
		"r/stacks/a/.env":      "TOKEN=secret",
		"r/README.md":          "not of the stack",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(buf.Bytes())
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/o/r.git"
}

// cluster is a Norca API that answers a plan with one release to install,
// and records what it is asked.
type cluster struct {
	mu     sync.Mutex
	calls  []string
	tokens []string
	bodies []map[string]json.RawMessage
}

func newCluster(t *testing.T) (*cluster, string) {
	t.Helper()
	c := &cluster{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		defer c.mu.Unlock()
		c.calls = append(c.calls, r.Method+" "+r.URL.RequestURI())
		c.tokens = append(c.tokens, r.Header.Get("X-Nomad-Token"))
		var members map[string]json.RawMessage
		_ = json.Unmarshal(body, &members)
		c.bodies = append(c.bodies, members)
		if r.Method != http.MethodPut || r.URL.Path != "/v1/rigger/stack/plan" {
			http.Error(w, "the test cluster only plans", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"Plan":{"stack":"demo","region":"global","releases":[` +
			`{"release":{"namespace":"production","name":"cache"},"action":"install","module":{"name":"redis","version":"0.4.1"}}],"outputs":[]},"Warnings":null}`))
	}))
	t.Cleanup(srv.Close)
	return c, srv.URL
}

func TestGitOpsRun_PlansAStackOfTheFlags(t *testing.T) {
	c, server := newCluster(t)
	t.Setenv("NORCA_TOKEN", "cluster-token")
	t.Setenv("GIT_TOKEN", "git-token")

	code, stdout, stderr := execute(t, "gitops", "run", "--server", server,
		"--provider", "forgejo", "--url", stackRepo(t), "--subpath", "stacks/a", "--token-env", "GIT_TOKEN",
		"--namespace", "production", "--mode", "plan-only", "--prune", "--var", "replicas=5", "--var", "name=web")
	if code != exitOK {
		t.Fatalf("code %d stderr %s", code, stderr)
	}
	for _, want := range []string{
		"Binding:      command-line",
		`Target:       namespace "production", region "" (mode: plan-only, prune: true)`,
		"[FETCH] fetching the stack from ",
		"[PLAN] plan of the stack demo (region global): 1 install",
		"[PLAN] install production/cache: module redis 0.4.1",
		"[PLAN] the binding only plans: nothing is applied",
		"[OK] stack demo planned, nothing applied",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout+stderr, "git-token") || strings.Contains(stdout+stderr, "cluster-token") {
		t.Errorf("a credential is in the output:\n%s\n%s", stdout, stderr)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) != 1 || c.calls[0] != "PUT /v1/rigger/stack/plan?namespace=production" || c.tokens[0] != "cluster-token" {
		t.Fatalf("calls %v tokens %v", c.calls, c.tokens)
	}
	body := c.bodies[0]
	// The values of --var are text: the servers convert them to the types
	// of the variables.
	if string(body["ValuesJSON"]) != `{"name":"web","replicas":"5"}` || string(body["Prune"]) != "true" {
		t.Errorf("values %s prune %s", body["ValuesJSON"], body["Prune"])
	}
	var origin map[string]string
	if err := json.Unmarshal(body["Origin"], &origin); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(origin["repository"], "/o/r.git") || origin["ref"] != "main" {
		t.Errorf("origin %v", origin)
	}
	var files []byte
	if err := json.Unmarshal(body["Files"], &files); err != nil || len(files) == 0 {
		t.Fatalf("files: %v", err)
	}
	names := tarNames(t, files)
	if strings.Join(names, " ") != ".rigger-stack.json stack.hcl" {
		t.Errorf("the archive has %v", names)
	}
}

func tarNames(t *testing.T, archive []byte) []string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return names
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
}

// The servers evaluate a stack in their own process: gitops run has them
// apply only a signed commit or tag, as a binding of a plan does.
func TestGitOpsRun_ApplyNeedsVerifyKeys(t *testing.T) {
	c, server := newCluster(t)
	repo := stackRepo(t)
	key := filepath.Join(t.TempDir(), "release.asc")
	if err := os.WriteFile(key, []byte("not a key"), 0600); err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		args []string
		code int
		want []string
	}{
		"the default mode":        {nil, exitError, []string{"gitops binding command-line", "mode apply needs a verify block", "own process", "--verify-key-file", "--mode plan-only"}},
		"apply":                   {[]string{"--mode", "apply"}, exitError, []string{"--verify-key-file", "--mode plan-only"}},
		"apply without the check": {[]string{"--mode", "apply", "--no-verify"}, exitError, []string{"--verify-key-file", "--mode plan-only"}},
		"an unknown mode":         {[]string{"--mode", "dry-run"}, exitError, []string{`--mode "dry-run" is not apply or plan-only`}},
		// With a key the run goes on to the fetch, where the key is read.
		"apply with a key file": {[]string{"--verify-key-file", key}, exitError, []string{"public key 1"}},
		// What a release was given is of the stack now.
		"a release name": {[]string{"--name", "payment"}, exitUsage, []string{"unknown flag: --name"}},
		"atomic":         {[]string{"--atomic"}, exitUsage, []string{"unknown flag: --atomic"}},
	} {
		t.Run(name, func(t *testing.T) {
			args := append([]string{"gitops", "run", "--server", server, "--provider", "forgejo", "--url", repo, "--subpath", "stacks/a"}, tc.args...)
			code, _, stderr := execute(t, args...)
			if code != tc.code {
				t.Fatalf("code %d stderr %s", code, stderr)
			}
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr lacks %q: %s", want, stderr)
				}
			}
		})
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) != 0 {
		t.Fatalf("the cluster was asked: %v", c.calls)
	}
}

// A binding of the plan runs with its policy; the flags change it for one
// run.
func TestGitOpsRun_BindingOfThePlan(t *testing.T) {
	c, server := newCluster(t)
	plan := filepath.Join(t.TempDir(), "plan.yaml")
	content := "inventory:\n  masters:\n    - host: 10.0.0.1\nrigger:\n  gitops:\n    enabled: true\n    bindings:\n" +
		"      - name: platform\n        namespace: production\n        forgejo:\n          url: " + stackRepo(t) + "\n          subpath: stacks/a\n" +
		"        values:\n          replicas: 3\n        policy:\n          mode: plan-only\n"
	if err := os.WriteFile(plan, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := execute(t, "gitops", "run", "-c", plan, "--server", server, "--var", "name=web")
	if code != exitOK {
		t.Fatalf("code %d stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "Binding:      platform") || !strings.Contains(stdout, "(mode: plan-only, prune: false)") {
		t.Errorf("stdout:\n%s", stdout)
	}
	c.mu.Lock()
	if got := string(c.bodies[0]["ValuesJSON"]); got != `{"name":"web","replicas":3}` {
		t.Errorf("values %s", got)
	}
	c.mu.Unlock()

	// The flag turns the binding to apply, which it has no keys for.
	code, _, stderr = execute(t, "gitops", "run", "-c", plan, "--server", server, "--mode", "apply")
	if code != exitError || !strings.Contains(stderr, "gitops binding platform") || !strings.Contains(stderr, "--verify-key-file") {
		t.Fatalf("code %d stderr %s", code, stderr)
	}

	// The status of the bindings says what each does.
	code, stdout, stderr = execute(t, "gitops", "status", "-c", plan)
	if code != exitOK {
		t.Fatalf("code %d stderr %s", code, stderr)
	}
	for _, want := range []string{"Binding:   platform", "Stack:   stacks/a (Namespace: production, Region: )", "Policy:  mode: plan-only, prune: false, verify: false"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
}
