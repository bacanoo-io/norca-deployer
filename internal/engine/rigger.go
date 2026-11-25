// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
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

// deployRigger applies the stack of the rigger section with rigger stack
// apply, after the packs. By default norca rigger runs on the first server,
// on a copy of the stack, and sends it to the servers, which apply it. The
// plan may name the rigger program of this machine instead (run_on), and
// have the program apply the stack itself (mode). What is unchanged Rigger
// decides from the releases of the cluster, so the deployer keeps no marker
// for it.
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

	values := make([]string, len(rc.Values))
	for i, f := range rc.Values {
		values[i] = r.plan.ResolveLocalPath(f)
	}

	var prog *riggerProgram
	if rc.RunsOnServer() {
		prog, err = serverRigger(first.cli(r, token), dir, values)
	} else {
		prog, err = localRigger(first.cli(r, token), rc.Binary, dir, values)
	}

	if err != nil {
		return err
	}
	defer prog.close()

	// The servers fetch the modules of a stack only from a registry they
	// have synced.
	for _, name := range rc.SyncRegistries {
		fmt.Printf("    Syncing the Rigger registry %s (%s)...\n", name, prog.name)

		if err := prog.run("registry", "sync", name); err != nil {
			return err
		}
	}

	// A stack written in the plan has its lock in the state of the
	// deployer: sync keeps its versions from one apply to the next.
	if rc.StackHCL != "" {
		if err := prog.sync(dir); err != nil {
			return err
		}
	}

	mode := config.RiggerEmbedded
	if !rc.Embedded() {
		mode = config.RiggerStandalone
	}

	fmt.Printf("    Applying the Rigger stack %s (%s, %s)...\n", dir, prog.name, mode)

	if err := prog.run(riggerApplyArgs(rc, prog.dir, prog.values)...); err != nil {
		return err
	}

	fmt.Printf("    [OK] Rigger stack applied\n")

	return nil
}

// riggerApplyArgs are the arguments of rigger stack apply for the section:
// the stack in dir with the value files, as the program sees them.
func riggerApplyArgs(rc *config.RiggerConfig, dir string, values []string) []string {
	args := []string{"stack", "apply", dir}
	for _, f := range values {
		args = append(args, "-f", f)
	}

	for _, name := range slices.Sorted(maps.Keys(rc.Vars)) {
		args = append(args, "-var", name+"="+rc.Vars[name])
	}

	if rc.Prune {
		args = append(args, "-prune")
	}

	// Who applies is always said: without the flag RIGGER_SERVER of the
	// environment the program runs in would choose.
	if rc.Embedded() {
		return append(args, "-server")
	}

	return append(args, "-server=false")
}

// riggerProgram is the program that applies the stack, where the plan runs
// it, with the stack and the value files as it sees them there.
type riggerProgram struct {
	// name is the program and its place, for the messages.
	name   string
	dir    string
	values []string
	// run runs a command of the program and shows its output.
	run func(args ...string) error
	// readLock reads the lock file a sync wrote, nil without one, where the
	// program works on a copy of the stack; it is nil where the program
	// wrote into the state of the deployer itself.
	readLock func() ([]byte, error)
	// close ends the use of the program.
	close func()
}

// riggerLockFile is the lock file of a stack, next to its stack.hcl.
const riggerLockFile = "rigger.lock.hcl"

// sync runs stack sync, and brings the lock file it wrote into local, the
// directory of the stack in the state of the deployer.
func (p *riggerProgram) sync(local string) error {
	if err := p.run("stack", "sync", p.dir); err != nil {
		return err
	}

	if p.readLock == nil {
		return nil
	}

	lock, err := p.readLock()
	if err != nil {
		return fmt.Errorf("rigger: the lock file of the stack was not read back: %w", err)
	}

	// A stack without a module of a source has no lock file.
	if lock == nil {
		return nil
	}

	return os.WriteFile(filepath.Join(local, riggerLockFile), lock, 0o600)
}

// localRigger is the rigger program of this machine. It is a client of the
// HTTP API, and the deployer reaches the cluster through the CLI of a
// server, so the program gets the API on a local port (riggerProxy).
func localRigger(cli *remoteCLI, binary, dir string, values []string) (*riggerProgram, error) {
	addr, stop, err := startRiggerProxy(cli)
	if err != nil {
		return nil, err
	}

	if binary == "" {
		binary = "rigger"
	}

	// No region is given: the program talks to the region of the stack
	// (global when the stack names none) and reads none of its environment.
	env := append(riggerEnv(os.Environ()), "NORCA_ADDR="+addr)

	return &riggerProgram{
		name: binary + " on this machine", dir: dir, values: values, close: stop,
		run: func(args ...string) error { return runRigger(binary, env, args...) },
	}, nil
}

// riggerStage is the directory of the first server that holds the copy of
// the stack and of its value files while norca rigger runs.
const riggerStage = cliStateDir + "/rigger"

// The bounds of the copy of a stack: those of the archive in which the
// rigger program sends a stack to the servers.
const (
	riggerStageFiles = 4096
	riggerStageBytes = 64 << 20
)

// serverRigger is norca rigger on the first server, on a copy of the stack
// and of the value files: the server has the program and reaches its agent
// with the settings of every CLI call of the deployer, so the machine of
// the deployer needs no rigger program. The copy is private, replaces what
// a run that was cut left behind, and is removed when the program is done:
// a value file may hold a secret.
func serverRigger(cli *remoteCLI, dir string, values []string) (*riggerProgram, error) {
	if cli.isWindows() {
		return nil, errors.New("rigger: a stack can't be copied to a Windows server; run the first master on Linux or set run_on: local")
	}

	host := cli.node.Host

	remove := func() error {
		if _, err := cli.conn.ExecuteCommand("rm -rf " + connector.ShellQuote(riggerStage)); err != nil {
			return fmt.Errorf("rigger: the copy of the stack on %s (%s) was not removed: %w", host, riggerStage, err)
		}

		return nil
	}

	if err := remove(); err != nil {
		return nil, err
	}

	p := &riggerProgram{name: "norca rigger on " + host, dir: riggerStage + "/stack"}
	p.close = func() {
		if err := remove(); err != nil {
			fmt.Printf("    [WARN] %v\n", err)
		}
	}

	if err := stageRigger(cli.conn, p, dir, values); err != nil {
		p.close()

		return nil, fmt.Errorf("rigger: the stack was not copied to %s: %w", host, err)
	}

	p.run = func(args ...string) error {
		out, err := cli.run(append([]string{"rigger"}, args...), nil)
		if err != nil {
			return apiError(fmt.Sprintf("norca rigger %s %s on %s", args[0], args[1], host), 0, out, err)
		}

		if out = strings.TrimRight(out, "\r\n"); out != "" {
			fmt.Println(out)
		}

		return nil
	}

	lock := p.dir + "/" + riggerLockFile
	p.readLock = func() ([]byte, error) {
		if ok, err := cli.conn.FileExists(lock); err != nil || !ok {
			return nil, err
		}

		return cli.conn.ReadFile(lock)
	}

	return p, nil
}

// stageRigger copies the stack in dir and the value files to the stage of a
// node, and sets where the program p finds them.
func stageRigger(conn connector.Connector, p *riggerProgram, dir string, values []string) error {
	if err := copyTree(conn, dir, p.dir, riggerStageFiles, riggerStageBytes); err != nil {
		return err
	}

	// A directory for each: two value files may have one name.
	for i, f := range values {
		dest := path.Join(riggerStage, "values", strconv.Itoa(i), filepath.Base(f))
		if err := conn.MkdirAll(path.Dir(dest), 0o700); err != nil {
			return err
		}

		if err := copyLocalFile(conn, f, dest); err != nil {
			return err
		}

		p.values = append(p.values, dest)
	}

	return nil
}

// copyTree copies the regular files under the local directory dir to the
// directory dest of a node, private, without the files and directories
// whose name starts with a dot. Those are no part of a stack, and the
// rigger program leaves them out of what it sends to the servers too:
// .rigger holds the local state of a stack, and a working directory keeps
// secrets in files as .env. More than maxFiles files or maxBytes bytes are
// an error.
func copyTree(conn connector.Connector, dir, dest string, maxFiles int, maxBytes int64) error {
	var (
		files int
		total int64
	)

	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if p != dir && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}

			return nil
		}

		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}

		remote := path.Join(dest, filepath.ToSlash(rel))

		if d.IsDir() {
			return conn.MkdirAll(remote, 0o700)
		}

		if !d.Type().IsRegular() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		files++
		total += info.Size()

		if files > maxFiles || total > maxBytes {
			return fmt.Errorf("%s has more than %d files or %d bytes", dir, maxFiles, maxBytes)
		}

		return copyLocalFile(conn, p, remote)
	})
}

// copyLocalFile copies a file of this machine to a private file of a node.
func copyLocalFile(conn connector.Connector, local, remote string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()

	return conn.CopyFile(f, remote, 0o600)
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
//
// The directory of the plan is not in it. Every file of the directory of a
// stack is copied to the first server, or sent to the servers by the
// program, and next to the plan are the keys of the cluster (certs/).
func (r *Runner) riggerStackDir(stack, stackHCL string) (string, error) {
	if stack != "" {
		dir, err := filepath.Abs(r.plan.ResolveLocalPath(stack))
		if err != nil {
			return "", err
		}

		if info, err := os.Stat(filepath.Join(dir, "stack.hcl")); err != nil || !info.Mode().IsRegular() {
			return "", fmt.Errorf("rigger: %s is not the directory of a stack: it has no stack.hcl", dir)
		}

		base, err := filepath.Abs(r.plan.BaseDir())
		if err != nil {
			return "", err
		}

		if rel, err := filepath.Rel(dir, base); err == nil && filepath.IsLocal(rel) {
			return "", fmt.Errorf("rigger: the stack needs a directory of its own: %s holds the plan, and all files of a stack are copied to the first server or sent to the servers", dir)
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
