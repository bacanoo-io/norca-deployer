// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package engine

import (
	"errors"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
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

// writeFiles writes files, by their slash separated names, under dir.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()

	for name, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// testStackHCL is the stack.hcl writeStack writes.
const testStackHCL = "rigger {\n  name = \"platform\"\n}\n"

// writeStack writes a stack.hcl into the directory dir next to the plan,
// and returns the directory.
func writeStack(t *testing.T, r *Runner, dir string) string {
	t.Helper()

	dir = filepath.Join(r.plan.BaseDir(), dir)
	writeFiles(t, dir, map[string]string{"stack.hcl": testStackHCL})

	return dir
}

// riggerServer is the first server as norca rigger runs on it: it records
// the runs of the program and what was copied for each, writes a lock file
// on a sync, and removes a directory on rm -rf. The other commands go to
// the recorder of the API.
type riggerServer struct {
	conn *connector.FakeConnector
	// runs are the arguments of each run of norca rigger.
	runs []string
	// staged are the files of the copy of the stack at the last run, and env
	// the environment of that run.
	staged map[string]string
	env    string
	// lock is what a sync writes; lockAtSync what it found.
	lock       string
	lockAtSync string
	// fail is a stack command that fails ("apply"), failRegistry a registry
	// whose sync fails.
	fail         string
	failRegistry string
	// removed counts the rm -rf of the copy.
	removed int
}

var riggerRunRe = regexp.MustCompile(`/norca' 'rigger'((?: '[^']*')*); rc=`)

const testStage = "/var/lib/norca-deployer/rigger"

func newRiggerServer(t *testing.T, first *target) *riggerServer {
	t.Helper()

	conn, ok := first.conn.(*connector.FakeConnector)
	if !ok {
		t.Fatalf("the connector of the first server is a %T", first.conn)
	}

	s := &riggerServer{conn: conn}
	api := conn.Handler

	conn.Handler = func(cmd string) (string, error, bool) {
		if dir, ok := strings.CutPrefix(cmd, "rm -rf "); ok {
			s.removed++

			maps.DeleteFunc(conn.Files, func(p string, _ []byte) bool { return strings.HasPrefix(p, strings.Trim(dir, "'")+"/") })

			return "", nil, true
		}

		m := riggerRunRe.FindStringSubmatch(cmd)
		if m == nil {
			return api(cmd)
		}

		args := strings.TrimSpace(strings.ReplaceAll(m[1], "'", ""))
		s.runs = append(s.runs, args)
		s.env = string(conn.Files[cliStateDir+"/cli.env"])
		s.staged = map[string]string{}

		for p, data := range conn.Files {
			if strings.HasPrefix(p, testStage+"/") {
				s.staged[p] = string(data)
			}
		}

		switch {
		case s.fail != "" && strings.HasPrefix(args, "stack "+s.fail+" "):
			return "Error: Module not loaded", errors.New("exit status 1 (output: Error: Module not loaded)"), true
		case s.failRegistry != "" && args == "registry sync "+s.failRegistry:
			return "Error: repository not reached", errors.New("exit status 1 (output: Error: repository not reached)"), true
		case strings.HasPrefix(args, "registry sync "):
			return "synced " + strings.TrimPrefix(args, "registry sync ") + ": commit 0123abc, 19 modules, 0 stacks", nil, true
		case strings.HasPrefix(args, "stack sync "):
			s.lockAtSync = s.staged[testStage+"/stack/rigger.lock.hcl"]
			conn.Files[testStage+"/stack/rigger.lock.hcl"] = []byte(s.lock)

			return "wrote rigger.lock.hcl", nil, true
		}

		return "install default/cache: done (revision 1)", nil, true
	}

	return s
}

// left are the files of the copy of the stack that are on the server now.
func (s *riggerServer) left() []string {
	return slices.DeleteFunc(fileNames(s.conn), func(p string) bool { return !strings.HasPrefix(p, testStage) })
}

// fileNames are the files written through a connector, by name.
func fileNames(conn *connector.FakeConnector) []string {
	return slices.Sorted(maps.Keys(conn.Files))
}

func TestDeployRiggerOnServer(t *testing.T) {
	plan := bootstrapPlan + `
rigger:
  stack: stacks/platform
  values: [stacks/platform/values/prod.vars.hcl, site/prod.vars.hcl]
  vars: {count: "3", api_count: "2"}
  prune: true
`

	r, first, api := bootstrapRunner(t, plan)
	dir := writeStack(t, r, "stacks/platform")
	writeFiles(t, dir, map[string]string{
		"rigger.lock.hcl":          "lock",
		"modules/app/metadata.hcl": "module {}",
		"values/prod.vars.hcl":     "count = 1",
		// The local state of a stack and the secrets of a working directory
		// are no files of a stack.
		".rigger/generated": "secret",
		".env":              "TOKEN=x",
	})
	writeFiles(t, r.plan.BaseDir(), map[string]string{"site/prod.vars.hcl": "count = 2"})

	server := newRiggerServer(t, first)

	// The environment of the operator does not reach the server.
	t.Setenv("RIGGER_SERVER", "0")

	if err := r.deployRigger(first, "token"); err != nil {
		t.Fatal(err)
	}

	wantRun := "stack apply " + testStage + "/stack -f " + testStage + "/values/0/prod.vars.hcl -f " + testStage +
		"/values/1/prod.vars.hcl -var api_count=2 -var count=3 -prune -server"
	if len(server.runs) != 1 || server.runs[0] != wantRun {
		t.Fatalf("norca rigger was run as %q, want %q", server.runs, wantRun)
	}

	wantStaged := map[string]string{
		testStage + "/stack/stack.hcl":                testStackHCL,
		testStage + "/stack/rigger.lock.hcl":          "lock",
		testStage + "/stack/modules/app/metadata.hcl": "module {}",
		testStage + "/stack/values/prod.vars.hcl":     "count = 1",
		testStage + "/values/0/prod.vars.hcl":         "count = 1",
		testStage + "/values/1/prod.vars.hcl":         "count = 2",
	}
	if !maps.Equal(server.staged, wantStaged) {
		t.Fatalf("the copy of the stack at the run = %v, want %v", server.staged, wantStaged)
	}

	for p := range wantStaged {
		if mode := server.conn.Modes[p]; mode != 0o600 {
			t.Errorf("%s was copied with the mode %04o, want 0600", p, mode)
		}
	}

	if !strings.Contains(server.env, "NORCA_TOKEN='token'") || !strings.Contains(server.env, "NORCA_ADDR='http://127.0.0.1:4646'") {
		t.Errorf("the environment of norca rigger = %q, want the token and the agent of the server", server.env)
	}

	// Removed before the copy (what a run that was cut left) and after the run.
	if server.removed != 2 {
		t.Errorf("the copy was removed %d times, want before and after the run", server.removed)
	}

	if left := server.left(); len(left) != 0 {
		t.Errorf("%v left on the server", left)
	}

	// The program talks to the agent itself: the deployer serves no API.
	if len(api.calls) != 0 {
		t.Errorf("calls of the API by the deployer = %v, want none", api.calls)
	}
}

func TestDeployRiggerOnServerStandalone(t *testing.T) {
	r, first, _ := bootstrapRunner(t, bootstrapPlan+"\nrigger:\n  stack: stacks/platform\n  mode: standalone\n")
	writeStack(t, r, "stacks/platform")

	server := newRiggerServer(t, first)

	if err := r.deployRigger(first, "token"); err != nil {
		t.Fatal(err)
	}

	if want := "stack apply " + testStage + "/stack -server=false"; len(server.runs) != 1 || server.runs[0] != want {
		t.Fatalf("norca rigger was run as %q, want %q", server.runs, want)
	}
}

// A stack written in the plan is synced where the program runs, and its
// lock file comes back into the state of the deployer, from where the next
// apply takes it.
func TestDeployRiggerOnServerInlineStack(t *testing.T) {
	r, first, _ := bootstrapRunner(t, bootstrapPlan+"\nrigger:\n  stack_hcl: |\n    rigger {\n      name = \"platform\"\n    }\n")

	server := newRiggerServer(t, first)
	server.lock = "lock 1"

	if err := r.deployRigger(first, "token"); err != nil {
		t.Fatal(err)
	}

	wantRuns := []string{"stack sync " + testStage + "/stack", "stack apply " + testStage + "/stack -server"}
	if strings.Join(server.runs, "\n") != strings.Join(wantRuns, "\n") {
		t.Fatalf("norca rigger was run as %q, want %q", server.runs, wantRuns)
	}

	if !strings.Contains(server.staged[testStage+"/stack/stack.hcl"], `name = "platform"`) || server.staged[testStage+"/stack/rigger.lock.hcl"] != "lock 1" {
		t.Fatalf("the copy of the stack at the apply = %v, want the stack.hcl of the plan and the lock of the sync", server.staged)
	}

	lock := filepath.Join(r.store.Path("rigger-stack"), "rigger.lock.hcl")
	if data, err := os.ReadFile(lock); err != nil || string(data) != "lock 1" {
		t.Fatalf("the lock file in the state = %q, %v, want that of the sync", data, err)
	}

	server.lock = "lock 2"

	if err := r.deployRigger(first, "token"); err != nil {
		t.Fatal(err)
	}

	if server.lockAtSync != "lock 1" {
		t.Errorf("the second sync found the lock %q, want that of the state", server.lockAtSync)
	}

	if data, err := os.ReadFile(lock); err != nil || string(data) != "lock 2" {
		t.Errorf("the lock file in the state = %q, %v, want that of the second sync", data, err)
	}
}

func TestDeployRiggerOnServerFailure(t *testing.T) {
	r, first, _ := bootstrapRunner(t, bootstrapPlan+"\nrigger:\n  stack: stacks/platform\n")
	writeStack(t, r, "stacks/platform")

	server := newRiggerServer(t, first)
	server.fail = "apply"

	err := r.deployRigger(first, "token")
	if err == nil || !strings.Contains(err.Error(), "norca rigger stack apply") || !strings.Contains(err.Error(), "Module not loaded") {
		t.Fatalf("deployRigger = %v, want the error of norca rigger stack apply with its output", err)
	}

	if left := server.left(); server.removed != 2 || len(left) != 0 {
		t.Errorf("after a failed apply: removed %d times, left %v; want the copy removed", server.removed, left)
	}
}

// The servers fetch the modules of a stack only from a registry they have
// synced, and a new cluster has synced none: the registries of
// sync_registries are synced before the stack is applied.
func TestDeployRiggerOnServerSyncsRegistries(t *testing.T) {
	r, first, _ := bootstrapRunner(t, bootstrapPlan+"\nrigger:\n  stack: stacks/platform\n  sync_registries: [global, team-modules]\n")
	writeStack(t, r, "stacks/platform")

	server := newRiggerServer(t, first)

	if err := r.deployRigger(first, "token"); err != nil {
		t.Fatal(err)
	}

	want := []string{"registry sync global", "registry sync team-modules", "stack apply " + testStage + "/stack -server"}
	if !slices.Equal(server.runs, want) {
		t.Fatalf("norca rigger was run as %q, want %q", server.runs, want)
	}
}

// A registry that does not sync stops the apply: the stack is not sent.
func TestDeployRiggerOnServerRegistrySyncFailure(t *testing.T) {
	r, first, _ := bootstrapRunner(t, bootstrapPlan+"\nrigger:\n  stack: stacks/platform\n  sync_registries: [global]\n")
	writeStack(t, r, "stacks/platform")

	server := newRiggerServer(t, first)
	server.failRegistry = "global"

	err := r.deployRigger(first, "token")
	if err == nil || !strings.Contains(err.Error(), "norca rigger registry sync") || !strings.Contains(err.Error(), "repository not reached") {
		t.Fatalf("deployRigger = %v, want the error of norca rigger registry sync with its output", err)
	}

	if want := []string{"registry sync global"}; !slices.Equal(server.runs, want) {
		t.Errorf("norca rigger was run as %q, want %q: no apply after a failed sync", server.runs, want)
	}

	if left := server.left(); len(left) != 0 {
		t.Errorf("%v left on the server after a failed sync", left)
	}
}

func TestDeployRiggerOnWindowsServer(t *testing.T) {
	r, first, _ := bootstrapRunner(t, bootstrapPlan+"\nrigger:\n  stack: stacks/platform\n")
	writeStack(t, r, "stacks/platform")

	server := newRiggerServer(t, first)
	first.node.Platform = "windows"

	err := r.deployRigger(first, "token")
	if err == nil || !strings.Contains(err.Error(), "run_on: local") {
		t.Fatalf("deployRigger on a Windows server = %v, want an error that names run_on: local", err)
	}

	if len(server.runs) != 0 || len(server.conn.Files) != 0 {
		t.Errorf("runs %v, files %v: want nothing done on the server", server.runs, fileNames(server.conn))
	}
}

// Everything in the directory of a stack is copied to the server, or sent
// to the servers by the program: the directory of the plan, with the keys
// of the cluster in certs/, is never the stack or inside it.
func TestDeployRiggerRefusesStackAroundThePlan(t *testing.T) {
	for _, mode := range []string{"", "  run_on: local\n  mode: standalone\n"} {
		r, first, _ := bootstrapRunner(t, bootstrapPlan+"\nrigger:\n  stack: .\n"+mode)
		writeStack(t, r, ".")

		server := newRiggerServer(t, first)

		err := r.deployRigger(first, "token")
		if err == nil || !strings.Contains(err.Error(), "directory of its own") {
			t.Fatalf("deployRigger of a stack in the directory of the plan = %v, want an error", err)
		}

		if len(server.runs) != 0 || server.removed != 0 {
			t.Errorf("runs %v, removed %d: want nothing done on the server", server.runs, server.removed)
		}
	}
}

func TestCopyTreeBounds(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"stack.hcl": "12345", "modules/a/a.hcl": "12345", "modules/b/b.hcl": "12345"})

	tests := []struct {
		name     string
		maxFiles int
		maxBytes int64
		wantErr  bool
	}{
		{name: "within both", maxFiles: 3, maxBytes: 15},
		{name: "a file too many", maxFiles: 2, maxBytes: 15, wantErr: true},
		{name: "a byte too many", maxFiles: 3, maxBytes: 14, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := connector.NewFakeConnector()

			err := copyTree(conn, dir, "/dest", tt.maxFiles, tt.maxBytes)
			if (err != nil) != tt.wantErr {
				t.Fatalf("copyTree = %v, want an error: %t", err, tt.wantErr)
			}

			if err == nil && (len(conn.Files) != 3 || string(conn.Files["/dest/modules/a/a.hcl"]) != "12345") {
				t.Errorf("copied %v", fileNames(conn))
			}
		})
	}
}

func TestDeployRiggerLocal(t *testing.T) {
	binary, record := fakeRigger(t)

	plan := bootstrapPlan + `
rigger:
  stack: stacks/platform
  values: [stacks/platform/values/prod.vars.hcl]
  vars: {count: "3", api_count: "2"}
  prune: true
  run_on: local
  mode: standalone
  binary: ` + binary + "\n"

	r, first, _ := bootstrapRunner(t, plan)
	dir := writeStack(t, r, "stacks/platform")
	server := newRiggerServer(t, first)

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

	// Nothing of the stack goes to the server.
	if left := server.left(); len(server.runs) != 0 || server.removed != 0 || len(left) != 0 {
		t.Errorf("runs %v, removed %d, files %v: want nothing done on the server", server.runs, server.removed, left)
	}
}

// The rigger program of this machine sends the stack to the servers by
// default, through the API the deployer serves for it.
func TestDeployRiggerLocalEmbedded(t *testing.T) {
	binary, record := fakeRigger(t)

	r, first, _ := bootstrapRunner(t, bootstrapPlan+"\nrigger:\n  stack: stacks/platform\n  run_on: local\n  binary: "+binary+"\n")
	dir := writeStack(t, r, "stacks/platform")

	t.Setenv("RIGGER_SERVER", "0")

	if err := r.deployRigger(first, "token"); err != nil {
		t.Fatal(err)
	}

	calls, _ := os.ReadFile(record)
	if got := strings.TrimSpace(string(calls)); !strings.HasPrefix(got, "addr=http://127.0.0.1:") || !strings.HasSuffix(got, " stack apply "+dir+" -server") {
		t.Fatalf("rigger was run as %q, want stack apply %s -server against the local port", got, dir)
	}
}

// The rigger program of this machine asks the servers for the sync of the
// registries as well, through the same local port.
func TestDeployRiggerLocalSyncsRegistries(t *testing.T) {
	binary, record := fakeRigger(t)

	r, first, _ := bootstrapRunner(t, bootstrapPlan+"\nrigger:\n  stack: stacks/platform\n  run_on: local\n  sync_registries: [global]\n  binary: "+binary+"\n")
	dir := writeStack(t, r, "stacks/platform")

	if err := r.deployRigger(first, "token"); err != nil {
		t.Fatal(err)
	}

	calls, _ := os.ReadFile(record)

	lines := strings.Split(strings.TrimSpace(string(calls)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "addr=http://127.0.0.1:") || !strings.HasSuffix(lines[0], " registry sync global") ||
		!strings.HasSuffix(lines[1], " stack apply "+dir+" -server") {
		t.Fatalf("rigger was run as %q, want registry sync global, then stack apply %s -server, against the local port", lines, dir)
	}
}

// A name that is no directory is, for the rigger program, a published stack
// of the global registry (stacks/platform: the stack platform of the
// namespace stacks), which it would fetch and apply.
func TestDeployRiggerRefusesMissingStackDirectory(t *testing.T) {
	r, first, _ := bootstrapRunner(t, bootstrapPlan+"\nrigger:\n  stack: stacks/platform\n")
	server := newRiggerServer(t, first)

	err := r.deployRigger(first, "token")
	if err == nil || !strings.Contains(err.Error(), "stack.hcl") || !strings.Contains(err.Error(), filepath.Join("stacks", "platform")) {
		t.Fatalf("deployRigger of a missing stack directory = %v, want an error that names the directory and stack.hcl", err)
	}

	if len(server.runs) != 0 || server.removed != 0 {
		t.Fatalf("runs %v, removed %d: the program was run for a missing stack directory", server.runs, server.removed)
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

	got, err = r.riggerStackDir("", testStackHCL)
	if err != nil || !filepath.IsAbs(got) {
		t.Fatalf("riggerStackDir of a stack written in the plan = %q, %v, want an absolute directory", got, err)
	}
}

func TestDeployRiggerLocalInlineStackSyncsFirst(t *testing.T) {
	binary, record := fakeRigger(t)

	plan := bootstrapPlan + `
rigger:
  stack_hcl: |
    rigger {
      name = "platform"
    }
  run_on: local
  mode: standalone
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
