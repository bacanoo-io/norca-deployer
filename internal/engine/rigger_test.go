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

func TestDeployRigger(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake rigger is a shell script")
	}

	dir := t.TempDir()
	record := filepath.Join(dir, "calls")
	fake := filepath.Join(dir, "rigger")

	script := "#!/bin/sh\necho \"$NORCA_ADDR $NORCA_REGION $*\" >> " + record + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	plan := bootstrapPlan + `
rigger:
  stack: stacks/platform
  values: [stacks/platform/values/prod.vars.hcl]
  vars: {count: "3", api_count: "2"}
  prune: true
  binary: ` + fake + "\n"

	r, first, _ := bootstrapRunner(t, plan)
	t.Setenv("NORCA_TOKEN", "must-not-pass")

	if err := r.deployRigger(first, "token"); err != nil {
		t.Fatal(err)
	}

	calls, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}

	got := strings.TrimSpace(string(calls))
	for _, want := range []string{"http://127.0.0.1:", " eu stack apply ", filepath.Join("stacks", "platform"),
		"-f " + r.plan.ResolveLocalPath("stacks/platform/values/prod.vars.hcl"), "-var api_count=2 -var count=3 -prune"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rigger was run as %q, want %q in it", got, want)
		}
	}
}

func TestDeployRiggerInlineStackSyncsFirst(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake rigger is a shell script")
	}

	dir := t.TempDir()
	record := filepath.Join(dir, "calls")
	fake := filepath.Join(dir, "rigger")

	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"$1 $2\" >> "+record+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	plan := bootstrapPlan + `
rigger:
  stack_hcl: |
    rigger {
      name = "platform"
    }
  binary: ` + fake + "\n"

	r, first, _ := bootstrapRunner(t, plan)
	if err := r.deployRigger(first, "token"); err != nil {
		t.Fatal(err)
	}

	calls, _ := os.ReadFile(record)
	if got := strings.TrimSpace(string(calls)); got != "stack sync\nstack apply" {
		t.Fatalf("rigger was run as %q, want sync then apply", got)
	}

	if data, err := os.ReadFile(filepath.Join(r.store.Path("rigger-stack"), "stack.hcl")); err != nil || !strings.Contains(string(data), `name = "platform"`) {
		t.Fatalf("stack.hcl of the plan: %q, %v", data, err)
	}
}
