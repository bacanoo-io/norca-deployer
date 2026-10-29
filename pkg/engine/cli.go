package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/connector"
	"github.com/bacanoo-io/norca-deployer/pkg/platform/storage"
)

// remoteCLI runs the norca CLI on a node against its local agent, with the
// TLS settings of the node and an optional ACL token. The token and the
// request body are written to private files instead of the command line, so
// that they do not show up in the process list.
type remoteCLI struct {
	conn     connector.Connector
	plan     *config.DeploymentPlan
	node     *config.NodeConfig
	paths    *storage.ResolvedPaths
	isMaster bool
	token    string
}

const cliStateDir = "/var/lib/norca-deployer"

func (c *remoteCLI) isWindows() bool { return strings.ToLower(c.node.Platform) == "windows" }

func (c *remoteCLI) env() map[string]string {
	env := map[string]string{"NORCA_REGION": c.plan.Metadata.Region}
	scheme := "http"
	if c.plan.Security.TLSEnabled {
		scheme = "https"
		cert, key := "client.pem", "client-key.pem"
		if c.isMaster {
			cert, key = "server.pem", "server-key.pem"
		}
		env["NORCA_CACERT"] = joinPath(c.paths, c.paths.TLSDir, "ca.pem")
		env["NORCA_CLIENT_CERT"] = joinPath(c.paths, c.paths.TLSDir, cert)
		env["NORCA_CLIENT_KEY"] = joinPath(c.paths, c.paths.TLSDir, key)
	}
	env["NORCA_ADDR"] = fmt.Sprintf("%s://127.0.0.1:%d", scheme, c.plan.Network.Ports.HTTP)
	return env
}

func joinPath(paths *storage.ResolvedPaths, dir, name string) string {
	if strings.EqualFold(paths.Platform, "windows") {
		return dir + `\` + name
	}
	return dir + "/" + name
}

// run executes `norca <args>` with an optional stdin body.
func (c *remoteCLI) run(args []string, body []byte) (string, error) {
	env := c.env()
	if c.isWindows() {
		return c.runWindows(args, body, env)
	}

	envFile := cliStateDir + "/cli.env"
	bodyFile := cliStateDir + "/cli.body"
	if c.token != "" {
		env["NORCA_TOKEN"] = c.token
	}
	var sb strings.Builder
	for _, k := range sortedKeys(env) {
		sb.WriteString(k + "=" + connector.ShellQuote(env[k]) + "\n")
	}
	if err := c.conn.MkdirAll(cliStateDir, 0700); err != nil {
		return "", err
	}
	if err := c.conn.CopyFile(bytes.NewReader([]byte(sb.String())), envFile, 0600); err != nil {
		return "", err
	}
	cmd := "set -a; . " + connector.ShellQuote(envFile) + "; set +a; " +
		connector.ShellQuote(c.paths.BinDir+"/norca")
	for _, a := range args {
		cmd += " " + connector.ShellQuote(a)
	}
	cleanup := "rm -f " + connector.ShellQuote(envFile)
	if body != nil {
		if err := c.conn.CopyFile(bytes.NewReader(body), bodyFile, 0600); err != nil {
			return "", err
		}
		cmd += " < " + connector.ShellQuote(bodyFile)
		cleanup += " " + connector.ShellQuote(bodyFile)
	}
	return c.conn.ExecuteCommand(cmd + "; rc=$?; " + cleanup + "; exit $rc")
}

func (c *remoteCLI) runWindows(args []string, body []byte, env map[string]string) (string, error) {
	stateDir := c.paths.DataDir + `\deployer`
	tokenFile := stateDir + `\cli.token`
	bodyFile := stateDir + `\cli.body`
	if err := c.conn.MkdirAll(stateDir, 0700); err != nil {
		return "", err
	}

	var sb strings.Builder
	for _, k := range sortedKeys(env) {
		sb.WriteString("$env:" + k + " = " + connector.PSQuote(env[k]) + "; ")
	}
	cleanup := []string{}
	if c.token != "" {
		if err := c.conn.CopyFile(strings.NewReader(c.token), tokenFile, 0600); err != nil {
			return "", err
		}
		sb.WriteString("$env:NORCA_TOKEN = (Get-Content -Raw " + connector.PSQuote(tokenFile) + ").Trim(); ")
		cleanup = append(cleanup, connector.PSQuote(tokenFile))
	}
	call := "& " + connector.PSQuote(c.paths.BinDir+`\norca.exe`)
	for _, a := range args {
		call += " " + connector.PSQuote(a)
	}
	if body != nil {
		if err := c.conn.CopyFile(bytes.NewReader(body), bodyFile, 0600); err != nil {
			return "", err
		}
		call = "Get-Content -Raw " + connector.PSQuote(bodyFile) + " | " + call
		cleanup = append(cleanup, connector.PSQuote(bodyFile))
	}
	sb.WriteString("try { " + call + " } finally { ")
	if len(cleanup) > 0 {
		sb.WriteString("Remove-Item -Force -ErrorAction SilentlyContinue " + strings.Join(cleanup, ", "))
	}
	sb.WriteString(" }")
	return c.conn.ExecuteCommand(sb.String())
}

var httpStatusRe = regexp.MustCompile(`(?m)^< HTTP/[0-9.]+ ([0-9]{3})`)

// api calls the HTTP API through `norca operator api -verbose` and returns
// the status code and the body. operator api exits 0 for every HTTP status,
// so the status is read from its verbose output.
func (c *remoteCLI) api(method, path string, body []byte) (int, string, error) {
	args := []string{"operator", "api", "-verbose", "-X", method, path}
	out, err := c.run(args, body)
	if err != nil {
		return 0, out, err
	}
	m := httpStatusRe.FindStringSubmatch(out)
	if m == nil {
		return 0, out, fmt.Errorf("no HTTP status in the response of %s %s: %s", method, path, strings.TrimSpace(out))
	}
	status, _ := strconv.Atoi(m[1])
	var bodyLines []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "> ") || strings.HasPrefix(line, "< ") || strings.HasPrefix(line, "* ") ||
			line == ">" || line == "<" {
			continue
		}
		bodyLines = append(bodyLines, line)
	}
	return status, strings.TrimSpace(strings.Join(bodyLines, "\n")), nil
}

// healthy reports whether the local agent answers /v1/agent/health for its
// role. Servers must also see a leader and the expected number of peers.
func (c *remoteCLI) healthy(expectPeers int) (bool, string) {
	kind := "client"
	if c.isMaster {
		kind = "server"
	}
	status, body, err := c.api("GET", "/v1/agent/health?type="+kind, nil)
	if err != nil {
		return false, err.Error()
	}
	var health map[string]struct {
		Ok      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if status != 200 || json.Unmarshal([]byte(body), &health) != nil || !health[kind].Ok {
		return false, fmt.Sprintf("agent health %d: %s", status, body)
	}
	if !c.isMaster {
		return true, ""
	}

	status, body, err = c.api("GET", "/v1/status/leader", nil)
	if err != nil || status != 200 {
		return false, fmt.Sprintf("leader %d: %s", status, body)
	}
	var leader string
	if json.Unmarshal([]byte(body), &leader) != nil || leader == "" {
		return false, "no leader elected yet"
	}
	status, body, err = c.api("GET", "/v1/status/peers", nil)
	if err != nil || status != 200 {
		return false, fmt.Sprintf("peers %d: %s", status, body)
	}
	var peers []string
	if json.Unmarshal([]byte(body), &peers) != nil || len(peers) < expectPeers {
		return false, fmt.Sprintf("%d of %d raft peers", len(peers), expectPeers)
	}
	return true, ""
}
