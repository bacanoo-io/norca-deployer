// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// riggerHeaderRe is a header line of the verbose output of operator api.
var riggerHeaderRe = regexp.MustCompile(`^< ([A-Za-z0-9-]+): (.*)$`)

// apiResponse calls the HTTP API through operator api, like api, and also
// returns the headers of the answer: the client of the Norca API that the
// rigger program uses reads the index headers.
func (c *remoteCLI) apiResponse(method, path string, body []byte) (int, http.Header, string, error) {
	args := []string{"operator", "api", "-verbose", "-X", method, path}

	out, err := c.run(args, body)
	if err != nil {
		return 0, nil, out, err
	}

	m := httpStatusRe.FindStringSubmatch(out)
	if m == nil {
		return 0, nil, out, fmt.Errorf("no HTTP status in the response of %s %s: %s", method, path, strings.TrimSpace(out))
	}

	var status int
	if _, err := fmt.Sscanf(m[1], "%d", &status); err != nil {
		return 0, nil, out, err
	}

	header := http.Header{}

	var bodyLines []string

	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if h := riggerHeaderRe.FindStringSubmatch(line); h != nil {
			header.Add(h[1], h[2])

			continue
		}

		if strings.HasPrefix(line, "> ") || strings.HasPrefix(line, "< ") || strings.HasPrefix(line, "* ") || line == ">" || line == "<" {
			continue
		}

		bodyLines = append(bodyLines, line)
	}

	return status, header, strings.TrimSpace(strings.Join(bodyLines, "\n")), nil
}

// riggerProxy serves the HTTP API of the cluster on a local address, for
// the rigger program, through the CLI of a server: the deployer reaches the
// cluster that way, and rigger, a client of the HTTP API, needs an address.
// The token of the deployer goes with every call; requests are sent one at
// a time, as the CLI of a server takes them.
type riggerProxy struct {
	cli *remoteCLI
	mu  sync.Mutex
}

func (p *riggerProxy) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	body, err := io.ReadAll(io.LimitReader(req.Body, 64<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	path := req.URL.Path
	if req.URL.RawQuery != "" {
		path += "?" + req.URL.RawQuery
	}

	p.mu.Lock()
	status, header, out, err := p.cli.apiResponse(req.Method, path, body)
	p.mu.Unlock()

	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)

		return
	}

	for k, v := range header {
		if k == "Content-Length" || k == "Transfer-Encoding" || k == "Connection" {
			continue
		}

		w.Header()[k] = v
	}

	// The client of the Norca API reads these on every query.
	for k, v := range map[string]string{"X-Nomad-Index": "0", "X-Nomad-LastContact": "0", "X-Nomad-KnownLeader": "true"} {
		if w.Header().Get(k) == "" {
			w.Header().Set(k, v)
		}
	}

	w.WriteHeader(status)
	_, _ = io.WriteString(w, out)
}

// startRiggerProxy serves the proxy on a local port until stop.
func startRiggerProxy(cli *remoteCLI) (addr string, stop func(), err error) {
	var lc net.ListenConfig

	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}

	srv := &http.Server{Handler: &riggerProxy{cli: cli}, ReadHeaderTimeout: 10 * time.Second}

	go func() { _ = srv.Serve(ln) }()

	return "http://" + ln.Addr().String(), func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		_ = srv.Shutdown(ctx)
	}, nil
}

// deployRigger applies the stack of the rigger section with the rigger
// program, after the packs: rigger stack apply, against the cluster through
// a local proxy of the API. What is unchanged Rigger decides from the
// releases of the cluster, so the deployer keeps no marker for it.
func (r *Runner) deployRigger(first *target, token string) error {
	rc := r.plan.Rigger
	if rc == nil {
		return nil
	}

	if r.plan.Security.ACL != nil && r.plan.Security.ACL.Enabled && token == "" {
		fmt.Printf("    [WARN] No management token: the Rigger stack is not applied\n")

		return nil
	}

	dir, err := r.riggerStackDir(rc.Stack, rc.StackHCL)
	if err != nil {
		return err
	}

	addr, stop, err := startRiggerProxy(first.cli(r, token))
	if err != nil {
		return err
	}
	defer stop()

	args := []string{"stack", "apply", dir}
	for _, f := range rc.Values {
		args = append(args, "-f", r.plan.ResolveLocalPath(f))
	}

	names := make([]string, 0, len(rc.Vars))
	for name := range rc.Vars {
		names = append(names, name)
	}

	sort.Strings(names)

	for _, name := range names {
		args = append(args, "-var", name+"="+rc.Vars[name])
	}

	if rc.Prune {
		args = append(args, "-prune")
	}

	// Always said: without the flag RIGGER_SERVER of the environment of the
	// operator would send the stack to the servers.
	args = append(args, "-server=false")

	binary := rc.Binary
	if binary == "" {
		binary = "rigger"
	}

	// No region is given: the program talks to the region of the stack
	// (global when the stack names none) and reads none of its environment.
	env := append(riggerEnv(os.Environ()), "NORCA_ADDR="+addr)

	// A stack written in the plan has its lock in the state of the
	// deployer: sync keeps its versions from one apply to the next.
	if rc.StackHCL != "" {
		if err := runRigger(binary, env, "stack", "sync", dir); err != nil {
			return err
		}
	}

	fmt.Printf("    Applying the Rigger stack (%s)...\n", dir)

	if err := runRigger(binary, env, args...); err != nil {
		return err
	}

	fmt.Printf("    [OK] Rigger stack applied\n")

	return nil
}

// runRigger runs the rigger program, its output on the deployer's.
func runRigger(binary string, env []string, args ...string) error {
	cmd := exec.CommandContext(context.Background(), binary, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = env

	if err := cmd.Run(); err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok {
			return fmt.Errorf("rigger %s %s: exit code %d", args[0], args[1], exit.ExitCode())
		}

		return fmt.Errorf("rigger %s %s: %w", args[0], args[1], err)
	}

	return nil
}

// riggerEnv drops the settings of a Norca client from the environment:
// rigger talks to the proxy, which carries the token of the deployer.
func riggerEnv(env []string) []string {
	out := make([]string, 0, len(env))

	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "NORCA_") || strings.HasPrefix(name, "NOMAD_") {
			continue
		}

		out = append(out, kv)
	}

	return out
}

// riggerStackDir is the directory of the stack: the one of the plan, or for
// a stack written in the plan a directory of the state with its stack.hcl.
//
// It is absolute and has a stack.hcl. The rigger program takes a name that
// is no directory and has a slash for the source of a published stack
// (stacks/platform: the stack platform of the namespace stacks in the
// global registry), which it would fetch and apply: a directory of the plan
// that is missing, or relative to another working directory, must not get
// that far.
func (r *Runner) riggerStackDir(stack, stackHCL string) (string, error) {
	if stack != "" {
		dir, err := filepath.Abs(r.plan.ResolveLocalPath(stack))
		if err != nil {
			return "", err
		}

		if info, err := os.Stat(filepath.Join(dir, "stack.hcl")); err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("rigger: %s is not the directory of a stack: it has no stack.hcl", dir)
		}

		return dir, nil
	}

	dir, err := filepath.Abs(r.store.Path("rigger-stack"))
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}

	if err := os.WriteFile(filepath.Join(dir, "stack.hcl"), []byte(stackHCL), 0o600); err != nil {
		return "", err
	}

	return dir, nil
}
