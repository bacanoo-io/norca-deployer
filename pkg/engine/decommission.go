package engine

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/connector"
	"github.com/bacanoo-io/norca-deployer/pkg/platform/storage"
)

// decommission takes the nodes of inventory.decommission out of the cluster
// and uninstalls Norca from them. Clients go first, each drained so that its
// allocations move to the remaining clients; then the servers, one at a time.
// It runs after the rollout, so the remaining servers already run without the
// leaving ones in their join lists and bootstrap_expect.
//
// Every step checks the current state first, so a second apply (or an apply
// after the machine was deleted) only reports the node as removed.
func (r *Runner) decommission(first *target, token string) error {
	for _, name := range r.decomDone {
		fmt.Printf("    %s was decommissioned earlier (the entry can be removed from the plan)\n", name)
	}
	nodes := r.decom
	if len(nodes) == 0 {
		return nil
	}
	if acl := r.plan.Security.ACL; acl != nil && acl.Enabled && token == "" {
		return fmt.Errorf("decommission needs the ACL management token (%s)", acl.ManagementTokenOutput)
	}
	cli := first.cli(r, token)

	var ordered []*config.DecommissionNode
	for _, role := range []string{config.RoleClient, config.RoleMaster} {
		for _, d := range nodes {
			if d.Role == role {
				ordered = append(ordered, d)
			}
		}
	}
	for _, d := range ordered {
		fmt.Printf("    Decommissioning %s %s (%s)...\n", d.Role, d.Hostname, d.Host)
		var err error
		if d.Role == config.RoleClient {
			err = r.decommissionClient(cli, d)
		} else {
			err = r.decommissionServer(cli, d)
		}
		if err != nil {
			return fmt.Errorf("decommission %s (%s): %w", d.Hostname, d.Host, err)
		}
		if err := r.forgetInstalled(d); err != nil {
			return fmt.Errorf("failed to save the installed nodes: %w", err)
		}
	}
	return nil
}

func (r *Runner) decommissionClient(cli *remoteCLI, d *config.DecommissionNode) error {
	id, err := nodeID(cli, d.Hostname)
	if err != nil {
		return err
	}
	if id != "" {
		if err := r.drain(cli, id, d); err != nil {
			return err
		}
		// Drained allocations keep their network (the pause container of
		// bridge mode) until the client garbage-collects them; collect them
		// now, before the agent is stopped.
		if status, out, err := cli.api("GET", "/v1/client/gc?node_id="+id, nil); err != nil || status != 200 {
			fmt.Printf("    [WARN] %s: client GC: HTTP %d: %v %s\n", d.Hostname, status, err, strings.TrimSpace(out))
		}
	}
	// The agent is stopped before the purge, or it would register again.
	r.uninstall(d)
	if id != "" {
		if status, out, err := cli.api("POST", "/v1/node/"+id+"/purge", nil); err != nil || status != 200 {
			return fmt.Errorf("purge node %s: HTTP %d: %v %s", id, status, err, out)
		}
		fmt.Printf("    [OK] %s purged from the cluster\n", d.Hostname)
	} else {
		fmt.Printf("    %s is not registered in the cluster\n", d.Hostname)
	}
	return nil
}

// nodeID returns the ID of the client node with the given name, or "".
func nodeID(cli *remoteCLI, name string) (string, error) {
	status, out, err := cli.api("GET", "/v1/nodes", nil)
	if err != nil || status != 200 {
		return "", fmt.Errorf("list nodes: HTTP %d: %v %s", status, err, out)
	}
	var nodes []struct{ ID, Name string }
	if err := json.Unmarshal([]byte(out), &nodes); err != nil {
		return "", fmt.Errorf("list nodes: %w", err)
	}
	for _, n := range nodes {
		if n.Name == name {
			return n.ID, nil
		}
	}
	return "", nil
}

// drain drains a client and waits until the drain is complete: the node then
// has no drain strategy any more and stays ineligible.
func (r *Runner) drain(cli *remoteCLI, id string, d *config.DecommissionNode) error {
	deadline := time.Duration(d.DrainDeadline) * time.Second
	body, _ := json.Marshal(map[string]any{
		"DrainSpec":    map[string]any{"Deadline": int64(deadline), "IgnoreSystemJobs": false},
		"MarkEligible": false,
	})
	if status, out, err := cli.api("POST", "/v1/node/"+id+"/drain", body); err != nil || status != 200 {
		return fmt.Errorf("drain: HTTP %d: %v %s", status, err, out)
	}
	fmt.Printf("    Draining %s (deadline %s)...\n", d.Hostname, deadline)

	// Allocations still running at the deadline are stopped by the drainer;
	// allow it some time to finish after that.
	wait := time.Now().Add(deadline + 2*time.Minute)
	for {
		status, out, err := cli.api("GET", "/v1/node/"+id, nil)
		var node struct {
			DrainStrategy *json.RawMessage
			Status        string
		}
		if err == nil && status == 200 && json.Unmarshal([]byte(out), &node) == nil {
			if node.DrainStrategy == nil || string(*node.DrainStrategy) == "null" {
				fmt.Printf("    [OK] %s drained\n", d.Hostname)
				return nil
			}
		}
		if time.Now().After(wait) {
			return fmt.Errorf("drain of %s did not finish", d.Hostname)
		}
		time.Sleep(healthPollInterval)
	}
}

func (r *Runner) decommissionServer(cli *remoteCLI, d *config.DecommissionNode) error {
	// Stop the agent first: the remaining servers keep the quorum, and the
	// leaving server cannot rejoin after it was removed from Raft.
	r.uninstall(d)

	member := d.Hostname + "." + r.plan.Metadata.Region
	status, out, err := cli.api("GET", "/v1/operator/raft/configuration", nil)
	if err != nil || status != 200 {
		return fmt.Errorf("raft configuration: HTTP %d: %v %s", status, err, out)
	}
	var cfg struct {
		Servers []struct{ ID, Node, Address string }
	}
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		return fmt.Errorf("raft configuration: %w", err)
	}
	removed := false
	for _, s := range cfg.Servers {
		if s.Node != member && s.Node != d.Hostname {
			continue
		}
		if status, out, err := cli.api("DELETE", "/v1/operator/raft/peer?id="+url.QueryEscape(s.ID), nil); err != nil || status != 200 {
			return fmt.Errorf("remove raft peer %s: HTTP %d: %v %s", s.ID, status, err, out)
		}
		removed = true
	}
	// Serf would otherwise list the server as failed for a long time.
	if status, out, err := cli.api("POST", "/v1/agent/force-leave?node="+url.QueryEscape(member), nil); err != nil || status != 200 {
		fmt.Printf("    [WARN] force-leave %s: HTTP %d: %v %s\n", member, status, err, strings.TrimSpace(out))
	}
	if removed {
		fmt.Printf("    [OK] %s removed from Raft and Serf\n", d.Hostname)
	} else {
		fmt.Printf("    %s is not a Raft peer\n", d.Hostname)
	}

	// The remaining servers must agree on the new peer set.
	want := len(r.plan.Inventory.Masters)
	deadline := time.Now().Add(time.Duration(r.plan.Upgrade.HealthTimeout) * time.Second)
	for {
		status, out, err := cli.api("GET", "/v1/status/peers", nil)
		var peers []string
		if err == nil && status == 200 && json.Unmarshal([]byte(out), &peers) == nil && len(peers) == want {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the cluster has %d raft peers after removing %s, want %d: %s", len(peers), d.Hostname, want, out)
		}
		time.Sleep(healthPollInterval)
	}
}

// uninstall stops the agent on the node and removes the service, the
// configuration, the certificates and the binary; with purge_data also the
// data, log and application directories. Host packages, CNI plugins, sysctl
// and firewall settings stay. A node that cannot be reached (e.g. a deleted
// machine) is only reported.
func (r *Runner) uninstall(d *config.DecommissionNode) {
	conn, err := r.connect(&d.NodeConfig)
	if err != nil {
		fmt.Printf("    [WARN] %s not reachable, Norca not uninstalled from it: %v\n", d.Host, err)
		return
	}
	defer conn.Close()
	paths := storage.ResolveNodePaths(&d.NodeConfig, r.plan)
	var script string
	if strings.EqualFold(d.Platform, "windows") {
		script = windowsUninstallScript(paths, d.PurgeData)
	} else {
		script = linuxUninstallScript(paths, d.PurgeData)
	}
	if out, err := conn.ExecuteCommand(script); err != nil {
		fmt.Printf("    [WARN] %s: uninstall incomplete: %v %s\n", d.Host, err, strings.TrimSpace(out))
		return
	}
	what := "service, configuration, certificates and binary"
	if d.PurgeData {
		what += ", data, logs and applications"
	}
	fmt.Printf("    [OK] %s: Norca stopped; %s removed\n", d.Host, what)
}

// linuxUninstallScript removes only the files the deployer wrote; the
// directories are removed when they are empty afterwards. purge removes the
// Norca data, log and application directories completely.
func linuxUninstallScript(p *storage.ResolvedPaths, purge bool) string {
	q := connector.ShellQuote
	var sb strings.Builder
	sb.WriteString("systemctl disable --now norca >/dev/null 2>&1 || true\n")
	sb.WriteString("rm -f /etc/systemd/system/norca.service && systemctl daemon-reload && systemctl reset-failed norca >/dev/null 2>&1 || true\n")
	// Containers the agent did not clean up (e.g. the GC could not reach it)
	// would keep running without an agent.
	sb.WriteString(`if command -v docker >/dev/null 2>&1; then ids=$(docker ps -aq --filter label=com.hashicorp.nomad.alloc_id); ` +
		`if [ -n "$ids" ]; then docker rm -f $ids >/dev/null; fi; fi` + "\n")
	sb.WriteString("rm -f " + q(p.BinDir+"/norca") + " " + q(p.ConfigDir+"/norca.hcl"))
	for _, f := range []string{"ca.pem", "server.pem", "server-key.pem", "client.pem", "client-key.pem"} {
		sb.WriteString(" " + q(p.TLSDir+"/"+f))
	}
	sb.WriteString("\nrmdir " + q(p.TLSDir) + " " + q(p.ConfigDir) + " 2>/dev/null || true\n")
	sb.WriteString("rm -rf /var/lib/norca-deployer\n")
	if purge {
		// Allocation directories can hold mounts (secrets, volumes).
		sb.WriteString("awk -v d=" + q(p.DataDir+"/") + " 'index($2, d) == 1 {print $2}' /proc/mounts | sort -r | xargs -r -n1 umount -l 2>/dev/null || true\n")
		sb.WriteString("rm -rf " + q(p.DataDir) + " " + q(p.LogDir) + " " + q(p.AppsDir) + "\n")
	}
	return sb.String()
}

func windowsUninstallScript(p *storage.ResolvedPaths, purge bool) string {
	q := connector.PSQuote
	var sb strings.Builder
	sb.WriteString("Stop-Service -Name norca -ErrorAction SilentlyContinue; ")
	sb.WriteString("if (Get-Service -Name norca -ErrorAction SilentlyContinue) { sc.exe delete norca | Out-Null }; ")
	files := []string{p.BinDir + `\norca.exe`, p.BinDir + `\norca.exe` + connector.PendingSuffix, p.ConfigDir + `\norca.hcl`}
	for _, f := range []string{"ca.pem", "server.pem", "server-key.pem", "client.pem", "client-key.pem"} {
		files = append(files, p.TLSDir+`\`+f)
	}
	for _, f := range files {
		sb.WriteString("Remove-Item -Force -ErrorAction SilentlyContinue -LiteralPath " + q(f) + "; ")
	}
	if purge {
		for _, dir := range []string{p.DataDir, p.LogDir, p.AppsDir} {
			sb.WriteString("Remove-Item -Recurse -Force -ErrorAction SilentlyContinue -LiteralPath " + q(dir) + "; ")
		}
	}
	return sb.String()
}
