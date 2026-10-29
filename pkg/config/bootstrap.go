package config

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var (
	// Secret and Variable paths, as the Norca CLI and the packs use them.
	secretPathRe = regexp.MustCompile(`^[a-zA-Z0-9_-]+(/[a-zA-Z0-9_.-]+)*$`)
	itemNameRe   = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,128}$`)
	namespaceRe  = regexp.MustCompile(`^[a-zA-Z0-9-]{1,128}$`)
	generatedRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// Prefixes of generated values (see BootstrapSecret).
const (
	GeneratePrefix   = "!generate:"
	NKeySeedPrefix   = "!nkey-seed:"
	NKeyPublicPrefix = "!nkey-public:"
)

// ParseGenerated parses "!generate:<name>[:<bytes>]".
func ParseGenerated(v string) (name string, size int, err error) {
	spec := strings.TrimPrefix(v, GeneratePrefix)
	name, sizeStr, hasSize := strings.Cut(spec, ":")
	size = 32
	if hasSize {
		if size, err = strconv.Atoi(sizeStr); err != nil || size < 16 || size > 512 {
			return "", 0, fmt.Errorf("%q: the size must be 16 to 512 bytes", v)
		}
	}
	if !generatedRe.MatchString(name) {
		return "", 0, fmt.Errorf("%q: the name must be lower case letters, digits and '-'", v)
	}
	return name, size, nil
}

// checkValue checks a value that may be generated; files ("@path") are
// allowed where fileOK.
func (p *DeploymentPlan) checkValue(what, v string, fileOK bool) error {
	switch {
	case strings.HasPrefix(v, GeneratePrefix):
		_, _, err := ParseGenerated(v)
		return err
	case strings.HasPrefix(v, NKeySeedPrefix), strings.HasPrefix(v, NKeyPublicPrefix):
		name := v[strings.Index(v, ":")+1:]
		if !generatedRe.MatchString(name) {
			return fmt.Errorf("%s: %q: the name must be lower case letters, digits and '-'", what, v)
		}
	case fileOK && strings.HasPrefix(v, "@"):
		if _, err := os.Stat(p.ResolveLocalPath(v[1:])); err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
	}
	return nil
}

func (p *DeploymentPlan) validatePacks() error {
	pk := p.Packs
	if pk == nil || len(pk.Deploy) == 0 {
		return nil
	}
	regs := pk.AllRegistries()
	if len(regs) == 0 {
		return fmt.Errorf("packs: no registry (packs.registry or packs.registries)")
	}
	byName := map[string]bool{}
	for i, r := range regs {
		what := fmt.Sprintf("packs registry %d (%s)", i, r.Name)
		if !releaseNameRe.MatchString(r.Name) {
			return fmt.Errorf("%s: name must be lower case letters, digits and '-'", what)
		}
		if byName[r.Name] {
			return fmt.Errorf("%s: duplicate registry name", what)
		}
		byName[r.Name] = true
		if r.URL == "" {
			return fmt.Errorf("%s: url is required", what)
		}
		switch r.Type {
		case "git", "oci", "dir":
		default:
			return fmt.Errorf("%s: type %q must be git, oci or dir", what, r.Type)
		}
	}
	names := map[string]bool{}
	for i, d := range pk.Deploy {
		if !releaseNameRe.MatchString(d.Name) {
			return fmt.Errorf("packs.deploy[%d]: name %q must be lower case letters, digits and '-'", i, d.Name)
		}
		if names[d.Name] {
			return fmt.Errorf("packs.deploy[%d]: duplicate release name %q", i, d.Name)
		}
		names[d.Name] = true
		if d.Pack == "" {
			return fmt.Errorf("packs.deploy[%d]: pack is required", i)
		}
		switch {
		case d.Registry == "" && len(regs) > 1:
			return fmt.Errorf("packs.deploy[%d] (%s): registry is required with several registries", i, d.Name)
		case d.Registry != "" && !byName[d.Registry]:
			return fmt.Errorf("packs.deploy[%d] (%s): unknown registry %q", i, d.Name, d.Registry)
		}
		if err := walkStrings(d.Values, func(v string) error {
			return p.checkValue(fmt.Sprintf("packs.deploy[%d] (%s) values", i, d.Name), v, false)
		}); err != nil {
			return err
		}
	}
	if p.Security.ACL != nil && p.Security.ACL.Enabled && !p.Security.ACL.AutoBootstrap && p.Security.ACL.ManagementTokenOutput == "" {
		return fmt.Errorf("packs with ACLs need security.acl.auto_bootstrap or a management_token_output file")
	}
	return nil
}

// walkStrings calls fn for every string in a pack values tree.
func walkStrings(v any, fn func(string) error) error {
	switch t := v.(type) {
	case string:
		return fn(t)
	case map[string]any:
		for _, e := range t {
			if err := walkStrings(e, fn); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range t {
			if err := walkStrings(e, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *DeploymentPlan) validateSecrets() error {
	s := p.Secrets
	if s == nil {
		return nil
	}
	switch s.Store {
	case SecretStoreBuiltin, SecretStoreExternal:
	default:
		return fmt.Errorf("secrets.store %q must be builtin or external", s.Store)
	}
	seen := map[string]bool{}
	for i, b := range s.Bootstrap {
		what := fmt.Sprintf("secrets.bootstrap[%d] (%s)", i, b.Path)
		switch b.Type {
		case SecretTypeSecret, SecretTypeVariable:
		default:
			return fmt.Errorf("%s: type %q must be secret or variable", what, b.Type)
		}
		if !secretPathRe.MatchString(b.Path) {
			return fmt.Errorf("%s: invalid path", what)
		}
		if !namespaceRe.MatchString(b.Namespace) {
			return fmt.Errorf("%s: invalid namespace %q", what, b.Namespace)
		}
		key := b.Type + "|" + b.Namespace + "|" + b.Path
		if seen[key] {
			return fmt.Errorf("%s: listed twice", what)
		}
		seen[key] = true
		if len(b.Items) == 0 {
			return fmt.Errorf("%s: items are required", what)
		}
		for k, v := range b.Items {
			if !itemNameRe.MatchString(k) {
				return fmt.Errorf("%s: invalid item name %q", what, k)
			}
			if err := p.checkValue(what+" item "+k, v, true); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *DeploymentPlan) validateACLPolicies() error {
	a := p.Security.ACL
	if a == nil || len(a.Policies) == 0 {
		return nil
	}
	if !a.Enabled {
		return fmt.Errorf("security.acl.policies need security.acl.enabled")
	}
	seen := map[string]bool{}
	for i, pol := range a.Policies {
		what := fmt.Sprintf("security.acl.policies[%d] (%s)", i, pol.Name)
		if !namespaceRe.MatchString(pol.Name) {
			return fmt.Errorf("%s: name must be letters, digits and '-'", what)
		}
		if seen[pol.Name] {
			return fmt.Errorf("%s: duplicate policy", what)
		}
		seen[pol.Name] = true
		if (pol.Rules == "") == (pol.RulesFile == "") {
			return fmt.Errorf("%s: exactly one of rules or rules_file is required", what)
		}
		if pol.RulesFile != "" {
			if _, err := os.Stat(p.ResolveLocalPath(pol.RulesFile)); err != nil {
				return fmt.Errorf("%s: %w", what, err)
			}
		}
		if j := pol.Job; j != nil {
			if j.JobID == "" || !namespaceRe.MatchString(j.Namespace) {
				return fmt.Errorf("%s: job needs job_id and a valid namespace", what)
			}
			if j.Task != "" && j.Group == "" {
				return fmt.Errorf("%s: job.task needs job.group", what)
			}
		}
	}
	return nil
}

// PolicyRules returns the rules of a policy.
func (p *DeploymentPlan) PolicyRules(pol *ACLPolicyConfig) (string, error) {
	if pol.Rules != "" {
		return pol.Rules, nil
	}
	data, err := os.ReadFile(p.ResolveLocalPath(pol.RulesFile))
	return string(data), err
}

func isArchivePath(p string) bool {
	return strings.HasSuffix(p, ".tar.gz") || strings.HasSuffix(p, ".tgz") || strings.HasSuffix(p, ".zip")
}
