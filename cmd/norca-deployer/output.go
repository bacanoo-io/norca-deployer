package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca-deployer/internal/engine"
	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// The output formats of the commands that report on a plan. Text is for people;
// JSON is for scripts, which cannot parse the text.
const (
	formatText = "text"
	formatJSON = "json"
)

// addOutputFlag registers -o/--output on a command and returns the pointer to
// the value.
func addOutputFlag(cmd *cobra.Command) *string {
	format := new(string)
	cmd.Flags().StringVarP(format, "output", "o", formatText, "Output format: text or json")
	return format
}

// checkFormat rejects an unknown output format as a usage error.
func checkFormat(format string) error {
	switch format {
	case formatText, formatJSON:
		return nil
	}
	return usageError{fmt.Errorf("unknown output format %q (use %s or %s)", format, formatText, formatJSON)}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// validateResult is the JSON output of `validate`. On an invalid plan it is
// printed too (valid: false, with the error), so that a script reading stdout
// does not get an empty document; the exit code is 1 as well.
type validateResult struct {
	Valid   bool   `json:"valid"`
	Error   string `json:"error,omitempty"`
	Cluster string `json:"cluster,omitempty"`
	Region  string `json:"region,omitempty"`
	Masters int    `json:"masters"`
	Clients int    `json:"clients"`
}

// planResult is the JSON output of `plan`.
type planResult struct {
	Cluster    string       `json:"cluster"`
	Region     string       `json:"region"`
	Datacenter string       `json:"datacenter"`
	Release    releaseInfo  `json:"release"`
	Masters    []nodeInfo   `json:"masters"`
	Clients    []nodeInfo   `json:"clients"`
	Nodes      []nodeStatus `json:"nodes"`
}

type releaseInfo struct {
	Version string `json:"version"`
	Source  string `json:"source"`
}

type nodeInfo struct {
	Hostname string `json:"hostname"`
	Host     string `json:"host"`
	Platform string `json:"platform"`
	Mode     string `json:"mode"`
}

// nodeStatus is a node of the plan compared with the nodes installed by
// earlier applies: new, installed, decommission, or missing from the plan.
type nodeStatus struct {
	Key    string `json:"key"`
	Host   string `json:"host"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

func nodeInfos(nodes []config.NodeConfig) []nodeInfo {
	res := make([]nodeInfo, 0, len(nodes))
	for _, n := range nodes {
		res = append(res, nodeInfo{Hostname: n.Hostname, Host: n.Host, Platform: n.Platform, Mode: n.Connection.Mode})
	}
	return res
}

func buildPlanResult(plan *config.DeploymentPlan, report []engine.NodeStatus) planResult {
	res := planResult{
		Cluster:    plan.Metadata.Name,
		Region:     plan.Metadata.Region,
		Datacenter: plan.Metadata.Datacenter,
		Release:    releaseInfo{Version: plan.Release.Version, Source: plan.Release.Source},
		Masters:    nodeInfos(plan.Inventory.Masters),
		Clients:    nodeInfos(plan.Inventory.Clients),
		Nodes:      make([]nodeStatus, 0, len(report)),
	}
	for _, n := range report {
		res.Nodes = append(res.Nodes, nodeStatus{Key: n.Key, Host: n.Host, Role: n.Role, Status: n.Status})
	}
	return res
}
