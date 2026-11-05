package engine

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// Node scaling: after apply the deployer writes what the node scaling
// operator (norca-nodescaler) needs to create nodes by itself. The operator
// signs the certificate of each node with the node CA and renders the agent
// configuration template below with the node's name, datacenter, pool,
// class and meta (see the package bootstrap of norca-platform).

// The paths of the Variables of the operator.
func nodeScalingBootstrapPath(region string) string { return "nodescaler/bootstrap/" + region }
func nodeScalingNetworkPath(region string) string   { return "nodescaler/network/" + region }

// nodeScalingSettings are the client settings of a scaled node:
// node_scaling.client over client_defaults.
func nodeScalingSettings(plan *config.DeploymentPlan) *config.ClientSettings {
	cs := &config.ClientSettings{}
	if plan.NodeScaling != nil && plan.NodeScaling.Client != nil {
		c := *plan.NodeScaling.Client
		cs = &c
	}
	if d := plan.ClientDefaults; d != nil {
		cs = config.MergeClientSettings(d, cs)
	}
	return cs
}

// GenerateNodeTemplate returns the agent configuration template of a scaled
// Linux node. It is the configuration of a client of the plan without what
// belongs to one node: the name, datacenter, pool, class and meta are fields
// of the template, the TLS files are where the operator writes them, and the
// addresses are left to the agent (its private address), since the address
// of a machine that does not exist yet is not known.
func GenerateNodeTemplate(plan *config.DeploymentPlan) string {
	node := &config.NodeConfig{Platform: "linux"}
	paths := storage.ResolveNodePaths(node, plan)
	cs := nodeScalingSettings(plan)

	var sb strings.Builder
	sb.WriteString("# Norca agent configuration of a scaled node: a template of norca-deployer,\n")
	sb.WriteString("# rendered for each node by norca-nodescaler.\n\n")
	sb.WriteString("name       = {{hcl .Name}}\n")
	sb.WriteString("region     = {{hcl .Region}}\n")
	sb.WriteString("datacenter = {{hcl .Datacenter}}\n")
	sb.WriteString(fmt.Sprintf("data_dir   = %q\n", paths.DataDir))
	sb.WriteString("log_level  = \"INFO\"\n\n")

	sb.WriteString("ports {\n")
	sb.WriteString(fmt.Sprintf("  http = %d\n", plan.Network.Ports.HTTP))
	sb.WriteString(fmt.Sprintf("  rpc  = %d\n", plan.Network.Ports.RPC))
	sb.WriteString(fmt.Sprintf("  serf = %d\n", plan.Network.Ports.Serf))
	busPort, busClusterPort := plan.Network.Ports.BusPorts()
	sb.WriteString(fmt.Sprintf("  bus  = %d\n", busPort))
	sb.WriteString(fmt.Sprintf("  bus_cluster = %d\n", busClusterPort))
	sb.WriteString("}\n\n")

	sb.WriteString("tls {\n")
	sb.WriteString("  http = true\n")
	sb.WriteString("  rpc  = true\n\n")
	sb.WriteString("  ca_file   = {{hcl .CAFile}}\n")
	sb.WriteString("  cert_file = {{hcl .CertFile}}\n")
	sb.WriteString("  key_file  = {{hcl .KeyFile}}\n\n")
	sb.WriteString(fmt.Sprintf("  verify_server_hostname = %t\n", plan.Security.VerifyServerHostname))
	sb.WriteString(fmt.Sprintf("  verify_https_client    = %t\n", plan.Security.VerifyHTTPSClient))
	sb.WriteString("}\n\n")

	if plan.Security.ACL != nil && plan.Security.ACL.Enabled {
		sb.WriteString("acl {\n  enabled = true\n}\n\n")
	}
	// Consul is not set up on a scaled node: it would need a token of its own.
	writeConsul(&sb, nil, "")

	sb.WriteString("client {\n")
	sb.WriteString("  enabled    = true\n")
	sb.WriteString("  node_pool  = {{hcl .Pool}}\n")
	sb.WriteString("{{if .Class}}  node_class = {{hcl .Class}}\n{{end}}")
	sb.WriteString(fmt.Sprintf("  cni_path       = %q\n", paths.CNIBinDir))
	sb.WriteString(fmt.Sprintf("  cni_config_dir = %q\n", paths.CNIConfigDir))
	if cs.NetworkInterface != "" {
		sb.WriteString(fmt.Sprintf("  network_interface = %q\n", cs.NetworkInterface))
	}
	if cs.GCDiskUsageThreshold > 0 {
		sb.WriteString(fmt.Sprintf("  gc_disk_usage_threshold = %d\n", cs.GCDiskUsageThreshold))
	}
	if plan.Network.DynamicPorts != nil {
		minPort, maxPort := plan.DynamicPortRange()
		sb.WriteString(fmt.Sprintf("  min_dynamic_port = %d\n", minPort))
		sb.WriteString(fmt.Sprintf("  max_dynamic_port = %d\n", maxPort))
	}
	sb.WriteString("\n")
	retryJoin(&sb, plan, plan.Network.Ports.RPC, false)

	// The meta of the node: the node group's, and the marks of the operator.
	sb.WriteString("\n  meta {\n")
	sb.WriteString("{{range $k, $v := .Meta}}    {{hcl $k}} = {{hcl $v}}\n{{end}}")
	sb.WriteString("  }\n")
	// The meta of the plan's client settings is left out: a scaled node gets
	// its meta from its node group.
	labels := *cs
	labels.Meta = nil
	writeClientLabels(&sb, &labels)

	if drivers := clientDrivers(cs); len(drivers) > 0 {
		sb.WriteString("\n  options = {\n")
		sb.WriteString(fmt.Sprintf("    \"driver.allowlist\" = %q\n", strings.Join(drivers, ",")))
		sb.WriteString("  }\n")
	}
	writeExtraHCL(&sb, cs.ExtraHCL)
	sb.WriteString("}\n")

	if cs.EnableRawExec {
		sb.WriteString("\nplugin \"raw_exec\" {\n  config {\n    enabled = true\n  }\n}\n")
	}
	return sb.String()
}

// nodeScalingBootstrap returns the items of the bootstrap Variable, or an
// error naming what is missing.
func nodeScalingBootstrap(plan *config.DeploymentPlan) (map[string]string, error) {
	region := plan.Metadata.Region
	caCert, _, _ := caPaths(plan)
	ca, err := os.ReadFile(caCert)
	if err != nil {
		return nil, fmt.Errorf("the cluster CA: %w", err)
	}
	prefix := filepath.Join(plan.BaseDir(), "certs", "node-ca-"+region)
	nodeCA, err := os.ReadFile(prefix + ".pem")
	if err != nil {
		return nil, fmt.Errorf("no node CA for region %s (run `norca-deployer pki issue-node-ca`): %w", region, err)
	}
	nodeKey, err := os.ReadFile(prefix + "-key.pem")
	if err != nil {
		return nil, fmt.Errorf("the key of the node CA of region %s: %w", region, err)
	}
	paths := storage.ResolveNodePaths(&config.NodeConfig{Platform: "linux"}, plan)
	items := map[string]string{
		"ca":           string(ca),
		"node_ca":      string(nodeCA),
		"node_ca_key":  string(nodeKey),
		"agent_config": GenerateNodeTemplate(plan),
		"tls_dir":      paths.TLSDir,
		"config_file":  joinPath(paths, paths.ConfigDir, "norca.hcl"),
		"service":      "norca",
	}
	if v := plan.NodeScaling.Validity; v != "" {
		items["validity"] = v
	}
	td, err := plan.SPIFFETrustDomain()
	if err != nil {
		return nil, err
	}
	if td != nil {
		items["trust_domain"] = td.Host
	}
	return items, nil
}

// nodeScalingNetwork returns the items of the network Variable: the firewall
// ranges of the region, which the operator checks the network of a node
// group against.
func nodeScalingNetwork(plan *config.DeploymentPlan) map[string]string {
	items := map[string]string{"cluster_cidrs": "", "api_cidrs": ""}
	if fw := plan.Tuning.Firewall; fw != nil {
		items["cluster_cidrs"] = strings.Join(fw.ClusterCIDRs, ",")
		items["api_cidrs"] = strings.Join(fw.APICIDRs, ",")
	}
	return items
}

// publishNodeScaling writes the Variables of the node scaling operator. A
// Variable is written again only when its items changed since the last
// apply.
func (r *Runner) publishNodeScaling(first *target, token string) error {
	ns := r.plan.NodeScaling
	if ns == nil || !ns.Enabled {
		return nil
	}
	if acl := r.plan.Security.ACL; acl != nil && acl.Enabled && token == "" {
		fmt.Println("    [WARN] No management token: the node scaling Variables are not written")
		return nil
	}
	namespace := ns.Namespace
	if namespace == "" {
		namespace = "default"
	}
	region := r.plan.Metadata.Region
	vars := []struct {
		path  string
		items map[string]string
	}{{nodeScalingNetworkPath(region), nodeScalingNetwork(r.plan)}}
	if items, err := nodeScalingBootstrap(r.plan); err != nil {
		fmt.Printf("    [WARN] node_scaling: the bootstrap is not written: %v\n", err)
	} else {
		vars = append(vars, struct {
			path  string
			items map[string]string
		}{nodeScalingBootstrapPath(region), items})
	}

	cli := first.cli(r, token)
	for _, v := range vars {
		label := fmt.Sprintf("Variable %s/%s", namespace, v.path)
		body := map[string]any{"Namespace": namespace, "Path": v.path, "Items": v.items}
		d := digest(body)
		prev, save := r.marker("nodescaler", namespace+"|"+v.path)
		if prev == d {
			fmt.Printf("    %s unchanged (skipped)\n", label)
			continue
		}
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		status, out, err := cli.api("PUT", "/v1/var/"+v.path+"?"+url.Values{"namespace": {namespace}}.Encode(), data)
		switch {
		case err != nil:
			return fmt.Errorf("%s: %w", label, err)
		case status != 200:
			return fmt.Errorf("%s: HTTP %d: %s", label, status, redact(out, v.items))
		}
		save(d)
		fmt.Printf("    [OK] %s written\n", label)
	}
	return nil
}
