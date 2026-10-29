package state

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
)

// InstalledNode is a node that an apply installed. The node definition is
// kept so that a node can be decommissioned after it was deleted from the
// plan, with the connection settings it was installed with.
type InstalledNode struct {
	Role        string            `json:"role"` // master or client
	Node        config.NodeConfig `json:"node"`
	Release     string            `json:"release"`
	InstalledAt time.Time         `json:"installed_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// Installed is the list of installed nodes of one region, keyed by
// config.NodeKey.
type Installed struct {
	Region string                    `json:"region"`
	Nodes  map[string]*InstalledNode `json:"nodes"`

	path string
}

// LoadInstalled reads the installed nodes of a region. Plans of several
// regions may share the state directory, so each region has its own file.
// The file holds the node connection settings; it is encrypted when a master
// key is available.
func (s *Store) LoadInstalled(region string) (*Installed, error) {
	inst := &Installed{Region: region, Nodes: map[string]*InstalledNode{}, path: s.Path("nodes-" + region + ".json")}
	data, ok, err := s.ReadSecret(inst.path)
	if err != nil || !ok {
		return inst, err
	}
	if err := json.Unmarshal([]byte(data), inst); err != nil {
		return nil, fmt.Errorf("%s: %w", inst.path, err)
	}
	if inst.Nodes == nil {
		inst.Nodes = map[string]*InstalledNode{}
	}
	return inst, nil
}

// Save writes the list (0600, encrypted with the master key if any).
func (s *Store) SaveInstalled(inst *Installed) error {
	data, err := json.MarshalIndent(inst, "", "  ")
	if err != nil {
		return err
	}
	_, err = s.WriteSecret(inst.path, string(data))
	return err
}

// Record marks a node as installed with the given release.
func (inst *Installed) Record(node *config.NodeConfig, role, release string) {
	now := time.Now().UTC()
	key := config.NodeKey(node)
	e := inst.Nodes[key]
	if e == nil {
		e = &InstalledNode{InstalledAt: now}
		inst.Nodes[key] = e
	}
	e.Role, e.Node, e.Release, e.UpdatedAt = role, *node, release, now
}

// Lookup finds an installed node by its key, host or hostname.
func (inst *Installed) Lookup(hostOrName string) (string, *InstalledNode) {
	if e, ok := inst.Nodes[hostOrName]; ok {
		return hostOrName, e
	}
	for key, e := range inst.Nodes {
		if e.Node.Host == hostOrName || e.Node.Hostname == hostOrName {
			return key, e
		}
	}
	return "", nil
}

// Keys returns the node keys in order.
func (inst *Installed) Keys() []string {
	keys := make([]string, 0, len(inst.Nodes))
	for k := range inst.Nodes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
