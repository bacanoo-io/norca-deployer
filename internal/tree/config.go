// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// ConfigEnv names the startup configuration when --config does not.
const ConfigEnv = "NORCA_DEPLOYER_PLATFORM_CONFIG"

// DefaultConfigFile is the startup configuration read from the working
// directory when neither --config nor ConfigEnv names one.
const DefaultConfigFile = "norca-tree.hcl"

// defaultMaxChanges bounds the changes of one run (design §7.6).
const defaultMaxChanges = 50

// Config is the startup configuration (design §5.2): how this process
// reaches the cluster and what it may do. It lives outside the tree, so a
// commit can never widen it.
type Config struct {
	Regions        []RegionConfig `yaml:"region"`
	SyncRegistries []string       `yaml:"sync_registries"`
	Values         ValuesConfig   `yaml:"values"`
	Limits         LimitsConfig   `yaml:"limits"`

	// dir is the directory of the file: relative paths are below it.
	dir string
}

// RegionConfig is a region and its endpoint (design §6.1).
type RegionConfig struct {
	Name      string `yaml:"name"`
	Address   string `yaml:"address"`
	Token     string `yaml:"token"`
	TokenFile string `yaml:"token_file"`
	TokenEnv  string `yaml:"token_env"`
	CACert    string `yaml:"ca_cert"`
	// Insecure skips the check of the certificate of the endpoint.
	Insecure bool `yaml:"insecure_skip_tls_verify"`
	// Values are the values of the runs in this region (design §6.1).
	Values       map[string]any `yaml:"values"`
	ScopedTokens []ScopedToken  `yaml:"scoped_token"`
}

// LabelledBlock makes region "eu-1" { ... } a region named eu-1.
func (RegionConfig) LabelledBlock() {}

// ScopedToken is a token used only for the stacks it names, or with for =
// ["plan"] for the reads of plans and statuses (design §5.3).
type ScopedToken struct {
	Name      string   `yaml:"name"`
	Token     string   `yaml:"token"`
	TokenFile string   `yaml:"token_file"`
	TokenEnv  string   `yaml:"token_env"`
	Stacks    []string `yaml:"stacks"`
	For       []string `yaml:"for"`
}

// LabelledBlock makes scoped_token "apps" { ... } a token named apps.
func (ScopedToken) LabelledBlock() {}

// ValuesConfig is what the value forms of a tree may read (design §9).
type ValuesConfig struct {
	KeyFile string   `yaml:"key_file"`
	Dir     string   `yaml:"dir"`
	Env     []string `yaml:"env"`
}

// LimitsConfig is what this process may do (design §5.2, §7.6).
type LimitsConfig struct {
	// Kinds are the kinds the stacks may change: kind identifiers and
	// groups (design §8.1); empty is all.
	Kinds []string `yaml:"kinds"`
	// Prune false refuses --prune; unset allows it.
	Prune *bool `yaml:"prune"`
	// MaxChanges bounds the changes of one run; 0 is the default (50).
	MaxChanges int `yaml:"max_changes"`
}

// LoadConfig reads the startup configuration: the file named, else the one
// ConfigEnv names, else ./norca-tree.hcl when it exists. No file is an
// empty configuration, which the flags fill. treeDir is the root of the
// tree, for the warning about a configuration inside it.
func LoadConfig(name, treeDir string) (*Config, []string, error) {
	explicit := name != ""
	if name == "" {
		name = os.Getenv(ConfigEnv)
		explicit = name != ""
	}

	if name == "" {
		name = DefaultConfigFile
	}

	data, err := os.ReadFile(name) //nolint:gosec // G304: the startup configuration is named by the operator
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return &Config{}, nil, nil
	}

	if err != nil {
		return nil, nil, err
	}

	c := &Config{dir: filepath.Dir(name)}
	if err := config.DecodeInto(data, config.FormatOf(name), c); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}

	if err := c.check(); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", name, err)
	}

	return c, configWarnings(name, treeDir), nil
}

// configWarnings warns about a startup configuration that others may read
// (it can hold tokens) or that lies inside the tree (a commit could change
// it).
func configWarnings(name, treeDir string) []string {
	var warnings []string

	//nolint:gosec // G703: the startup configuration is named by the operator
	if info, err := os.Stat(name); err == nil && runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		warnings = append(warnings, fmt.Sprintf("%s can be read by others (mode %o): it may hold tokens", name, info.Mode().Perm()))
	}

	if absName, err := filepath.Abs(name); err == nil && treeDir != "" {
		if absTree, err := filepath.Abs(treeDir); err == nil {
			if rel, err := filepath.Rel(absTree, absName); err == nil && filepath.IsLocal(rel) {
				warnings = append(warnings, fmt.Sprintf("%s is inside the tree: a commit could change what this process may do", name))
			}
		}
	}

	return warnings
}

// check checks the configuration.
func (c *Config) check() error {
	var errs []error

	seen := map[string]bool{}

	for _, r := range c.Regions {
		if r.Name == "" {
			errs = append(errs, errors.New("a region block has a label: region \"eu-1\" { ... }"))
		}

		if seen[r.Name] {
			errs = append(errs, fmt.Errorf("region %q is given twice", r.Name))
		}

		seen[r.Name] = true

		for _, st := range r.ScopedTokens {
			switch {
			case len(st.For) > 0 && !slices.Equal(st.For, []string{"plan"}):
				errs = append(errs, fmt.Errorf("region %s: scoped_token %q: for takes only \"plan\"", r.Name, st.Name))
			case len(st.Stacks) == 0 && len(st.For) == 0:
				errs = append(errs, fmt.Errorf("region %s: scoped_token %q names stacks, or for = [\"plan\"]", r.Name, st.Name))
			}

			for _, pattern := range st.Stacks {
				if _, err := path.Match(pattern, ""); err != nil {
					errs = append(errs, fmt.Errorf("region %s: scoped_token %q: %q is no pattern", r.Name, st.Name, pattern))
				}
			}
		}
	}

	if c.Limits.MaxChanges < 0 {
		errs = append(errs, errors.New("limits.max_changes can't be negative"))
	}

	if _, err := KindSelector(c.Limits.Kinds); err != nil {
		errs = append(errs, fmt.Errorf("limits.kinds: %w", err))
	}

	return errors.Join(errs...)
}

// Region returns the region block of name; with name empty, the only one.
// No block at all is an empty region of that name, which the flags fill.
func (c *Config) Region(name string) (*RegionConfig, error) {
	switch {
	case name != "":
		for i := range c.Regions {
			if c.Regions[i].Name == name {
				return &c.Regions[i], nil
			}
		}

		if len(c.Regions) == 0 {
			return &RegionConfig{Name: name}, nil
		}

		return nil, fmt.Errorf("region %q is not in the startup configuration", name)
	case len(c.Regions) == 1:
		return &c.Regions[0], nil
	case len(c.Regions) == 0:
		return nil, errors.New("no region: give --region or a region block in the startup configuration")
	}

	names := make([]string, 0, len(c.Regions))
	for _, r := range c.Regions {
		names = append(names, r.Name)
	}

	return nil, fmt.Errorf("the startup configuration has several regions (%s): give --region", strings.Join(names, ", "))
}

// Path resolves a path of the configuration against its directory.
func (c *Config) Path(p string) string {
	if p == "" || filepath.IsAbs(p) || c.dir == "" {
		return p
	}

	return filepath.Join(c.dir, p)
}

// MaxChanges is the limit of changes of a run.
func (c *Config) MaxChanges() int {
	if c.Limits.MaxChanges == 0 {
		return defaultMaxChanges
	}

	return c.Limits.MaxChanges
}

// PruneAllowed reports whether this process may prune.
func (c *Config) PruneAllowed() bool {
	return c.Limits.Prune == nil || *c.Limits.Prune
}

// TokenFor returns the token source of a stack: the most specific scoped
// token that names it (a name over a pattern), else the region's own. With
// plan set, the token for = ["plan"] comes first.
func (r *RegionConfig) TokenFor(stack string, plan bool) (TokenSource, string) {
	if plan {
		for _, st := range r.ScopedTokens {
			if slices.Equal(st.For, []string{"plan"}) {
				return TokenSource{Value: st.Token, File: st.TokenFile, Env: st.TokenEnv}, st.Name
			}
		}
	}

	var byPattern *ScopedToken

	for i, st := range r.ScopedTokens {
		for _, pattern := range st.Stacks {
			if pattern == stack {
				return TokenSource{Value: st.Token, File: st.TokenFile, Env: st.TokenEnv}, st.Name
			}

			if ok, _ := path.Match(pattern, stack); ok && byPattern == nil {
				byPattern = &r.ScopedTokens[i]
			}
		}
	}

	if byPattern != nil {
		return TokenSource{Value: byPattern.Token, File: byPattern.TokenFile, Env: byPattern.TokenEnv}, byPattern.Name
	}

	return TokenSource{Value: r.Token, File: r.TokenFile, Env: r.TokenEnv}, ""
}

// TokenSource says where a token comes from: an environment variable, a
// file, or a value (an encrypted one is decrypted by the caller), in that
// order.
type TokenSource struct {
	Value string
	File  string
	Env   string
}

// IsZero reports whether no source is given.
func (t TokenSource) IsZero() bool { return t.Value == "" && t.File == "" && t.Env == "" }
