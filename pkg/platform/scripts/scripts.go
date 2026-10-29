// Package scripts runs user-supplied bash scripts on Linux nodes at fixed
// phases of the deployment.
package scripts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/connector"
)

const (
	// StateDir keeps the uploaded scripts and the run markers on the node.
	StateDir = "/var/lib/norca-deployer/scripts"

	defaultTimeout = 600
)

// Resolver decrypts inline secrets in environment values.
type Resolver func(string) (string, error)

// Prepared is a script ready to run on a node.
type Prepared struct {
	Config *config.ScriptConfig
	Body   string
	Env    map[string]string
	// Digest covers the body and the environment, so that a changed value
	// re-runs the script.
	Digest string
}

// ScriptPath is where the script is stored on the node.
func ScriptPath(name string) string { return StateDir + "/" + name + ".sh" }

// MarkerPath holds the digest of the last successful run.
func MarkerPath(name string) string { return StateDir + "/" + name + ".sha256" }

// Prepare reads the body and resolves the environment of a script.
func Prepare(plan *config.DeploymentPlan, s *config.ScriptConfig, resolve Resolver) (*Prepared, error) {
	body, err := plan.ScriptBody(s)
	if err != nil {
		return nil, err
	}
	env := make(map[string]string, len(s.Env))
	for k, v := range s.Env {
		if resolve != nil {
			rv, err := resolve(v)
			if err != nil {
				return nil, fmt.Errorf("script %q: env %s: %w", s.Name, k, err)
			}
			v = rv
		}
		env[k] = v
	}
	return &Prepared{Config: s, Body: body, Env: env, Digest: digest(body, env)}, nil
}

func digest(body string, env map[string]string) string {
	h := sha256.New()
	h.Write([]byte(body))
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "\x00%s=%s", k, env[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Command builds the shell command that runs the uploaded script. The
// environment is written to a private file next to the script instead of the
// command line, so that secret values do not show up in the process list.
func Command(p *Prepared, envFile string) string {
	timeout := p.Config.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return fmt.Sprintf("set -a; . %s; set +a; timeout --kill-after=10 %d bash %s",
		connector.ShellQuote(envFile), timeout, connector.ShellQuote(ScriptPath(p.Config.Name)))
}

// RenderEnvFile renders KEY='value' lines for the environment file.
func RenderEnvFile(env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(k + "=" + connector.ShellQuote(env[k]) + "\n")
	}
	return sb.String()
}

// Select returns the scripts of a phase that apply to the node, in the order
// of the configuration.
func Select(all []config.ScriptConfig, phase string, node *config.NodeConfig, isMaster bool) []*config.ScriptConfig {
	var res []*config.ScriptConfig
	for i := range all {
		s := &all[i]
		if s.Phase == phase && config.AppliesTo(s.Roles, s.Hosts, node, isMaster) {
			res = append(res, s)
		}
	}
	return res
}

// Run uploads and runs the scripts of a phase. A script runs when its digest
// differs from the marker of its last successful run, or when it is marked
// always. A failing script stops the deployment of the node.
func Run(conn connector.Connector, plan *config.DeploymentPlan, phase string, node *config.NodeConfig, isMaster bool, resolve Resolver) error {
	selected := Select(plan.Scripts, phase, node, isMaster)
	if len(selected) == 0 {
		return nil
	}
	if strings.ToLower(node.Platform) == "windows" {
		fmt.Printf("    [WARN] [%s] %d bash script(s) of phase %s skipped on a Windows node\n", node.Host, len(selected), phase)
		return nil
	}

	if err := conn.MkdirAll(StateDir, 0700); err != nil {
		return fmt.Errorf("failed to create %s: %w", StateDir, err)
	}

	for _, s := range selected {
		p, err := Prepare(plan, s, resolve)
		if err != nil {
			return err
		}

		if !s.Always {
			if prev, err := conn.ReadFile(MarkerPath(s.Name)); err == nil && strings.TrimSpace(string(prev)) == p.Digest {
				fmt.Printf("    -> [%s] Script %s unchanged (skipped)\n", node.Host, s.Name)
				continue
			}
		}

		if err := conn.CopyFile(bytes.NewReader([]byte(p.Body)), ScriptPath(s.Name), 0700); err != nil {
			return fmt.Errorf("script %q: upload failed: %w", s.Name, err)
		}
		envFile := StateDir + "/" + s.Name + ".env"
		if err := conn.CopyFile(bytes.NewReader([]byte(RenderEnvFile(p.Env))), envFile, 0600); err != nil {
			return fmt.Errorf("script %q: env upload failed: %w", s.Name, err)
		}

		fmt.Printf("    -> [%s] Running script %s (%s)...\n", node.Host, s.Name, phase)
		out, runErr := conn.ExecuteCommand(Command(p, envFile))
		_, _ = conn.ExecuteCommand("rm -f " + connector.ShellQuote(envFile))
		if runErr != nil {
			return fmt.Errorf("script %q failed: %w", s.Name, runErr)
		}
		if trimmed := strings.TrimSpace(out); trimmed != "" {
			for _, line := range strings.Split(trimmed, "\n") {
				fmt.Printf("       | %s\n", line)
			}
		}

		if err := conn.CopyFile(bytes.NewReader([]byte(p.Digest+"\n")), MarkerPath(s.Name), 0600); err != nil {
			return fmt.Errorf("script %q: failed to record run: %w", s.Name, err)
		}
	}
	return nil
}
