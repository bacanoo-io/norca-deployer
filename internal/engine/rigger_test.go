// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package engine

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/state"
)

func TestRiggerProxy(t *testing.T) {
	r, first, api := bootstrapRunner(t, bootstrapPlan)

	addr, stop, err := startRiggerProxy(first.cli(r, "token"))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	resp, err := http.Get(addr + "/v1/release/web?namespace=apps")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "{}" || resp.Header.Get("X-Nomad-Index") == "" {
		t.Fatalf("answer = %d %q %v", resp.StatusCode, body, resp.Header)
	}

	if len(api.calls) != 1 || api.calls[0] != "GET /v1/release/web?namespace=apps" {
		t.Fatalf("calls = %v", api.calls)
	}

	api.conflict["/v1/secret/x"] = true

	req, _ := http.NewRequest(http.MethodPut, addr+"/v1/secret/x?cas=0", strings.NewReader(`{"Items":{"a":"b"}}`))
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()

	if resp2.StatusCode != http.StatusConflict || api.bodies["PUT /v1/secret/x?cas=0"] != `{"Items":{"a":"b"}}` {
		t.Fatalf("PUT: %d, body %q", resp2.StatusCode, api.bodies["PUT /v1/secret/x?cas=0"])
	}
}

// fakeRigger is a rigger program that records how it was run: a line for
// each run with the address and the region of its environment and its
// arguments.
func fakeRigger(t *testing.T) (binary, record string) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the fake rigger is a shell script")
	}

	dir := t.TempDir()
	record = filepath.Join(dir, "calls")
	binary = filepath.Join(dir, "rigger")

	script := "#!/bin/sh\necho \"addr=$NORCA_ADDR region=${NORCA_REGION-unset} $*\" >> " + record + "\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	return binary, record
}

// writeStack writes a stack.hcl into the directory dir next to the plan,
// and returns the directory.
func writeStack(t *testing.T, r *Runner, dir string) string {
	t.Helper()

	dir = filepath.Join(r.plan.BaseDir(), dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "stack.hcl"), []byte("rigger {\n  name = \"platform\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	return dir
}

func TestDeployRigger(t *testing.T) {
	binary, record := fakeRigger(t)

	plan := bootstrapPlan + `
rigger:
  stack: stacks/platform
  values: [stacks/platform/values/prod.vars.hcl]
  vars: {count: "3", api_count: "2"}
  prune: true
  binary: ` + binary + "\n"

	r, first, _ := bootstrapRunner(t, plan)
	dir := writeStack(t, r, "stacks/platform")

	// The environment of the operator chooses neither the cluster nor the
	// way of the apply.
	t.Setenv("NORCA_TOKEN", "must-not-pass")
	t.Setenv("NORCA_REGION", "must-not-pass")
	t.Setenv("RIGGER_SERVER", "1")

	if err := r.deployRigger(first, "token"); err != nil {
		t.Fatal(err)
	}

	calls, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}

	// The region is the stack's: the program reads none of its environment.
	got := strings.TrimSpace(string(calls))
	for _, want := range []string{
		"addr=http://127.0.0.1:", " region=unset stack apply " + dir + " ",
		"-f " + r.plan.ResolveLocalPath("stacks/platform/values/prod.vars.hcl"),
		"-var api_count=2 -var count=3 -prune -server=false",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rigger was run as %q, want %q in it", got, want)
		}
	}
}

// A name that is no directory is, for the rigger program, a published stack
// of the global registry (stacks/platform: the stack platform of the
// namespace stacks), which it would fetch and apply.
func TestDeployRiggerRefusesMissingStackDirectory(t *testing.T) {
	binary, record := fakeRigger(t)

	r, first, _ := bootstrapRunner(t, bootstrapPlan+"\nrigger:\n  stack: stacks/platform\n  binary: "+binary+"\n")

	err := r.deployRigger(first, "token")
	if err == nil || !strings.Contains(err.Error(), "stack.hcl") || !strings.Contains(err.Error(), filepath.Join("stacks", "platform")) {
		t.Fatalf("deployRigger of a missing stack directory = %v, want an error that names the directory and stack.hcl", err)
	}

	if calls, err := os.ReadFile(record); err == nil {
		t.Fatalf("the program was run for a missing stack directory: %s", calls)
	}
}

// The plan of "apply -c plan.yaml" has relative paths: the directory of the
// stack is given to the program as an absolute one, which it never reads as
// the source of a published stack.
func TestRiggerStackDirIsAbsolute(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	plan := `
metadata: {name: dev, region: eu}
release: {version: v0.2.0}
rigger:
  stack: stacks/platform
inventory:
  masters:
    - {host: 10.0.0.1, hostname: s1}
`
	if err := os.WriteFile("plan.yaml", []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}

	p := loadPlanFile(t, "plan.yaml")
	r := NewRunner(p, nil)

	var err error
	if r.store, err = state.Open(p, nil); err != nil {
		t.Fatal(err)
	}

	want := writeStack(t, r, "stacks/platform")
	if want, err = filepath.Abs(want); err != nil {
		t.Fatal(err)
	}

	got, err := r.riggerStackDir(p.Rigger.Stack, "")
	if err != nil || got != want {
		t.Fatalf("riggerStackDir = %q, %v, want %q", got, err, want)
	}

	got, err = r.riggerStackDir("", "rigger {\n  name = \"platform\"\n}\n")
	if err != nil || !filepath.IsAbs(got) {
		t.Fatalf("riggerStackDir of a stack written in the plan = %q, %v, want an absolute directory", got, err)
	}
}

func TestDeployRiggerInlineStackSyncsFirst(t *testing.T) {
	binary, record := fakeRigger(t)

	plan := bootstrapPlan + `
rigger:
  stack_hcl: |
    rigger {
      name = "platform"
    }
  binary: ` + binary + "\n"

	r, first, _ := bootstrapRunner(t, plan)
	if err := r.deployRigger(first, "token"); err != nil {
		t.Fatal(err)
	}

	dir := r.store.Path("rigger-stack")

	calls, _ := os.ReadFile(record)

	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], " stack sync "+dir) || !strings.Contains(lines[1], " stack apply "+dir+" ") {
		t.Fatalf("rigger was run as %q, want sync then apply of %s", lines, dir)
	}

	if data, err := os.ReadFile(filepath.Join(dir, "stack.hcl")); err != nil || !strings.Contains(string(data), `name = "platform"`) {
		t.Fatalf("stack.hcl of the plan: %q, %v", data, err)
	}
}
