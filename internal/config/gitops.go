package config

import (
	"fmt"
	"os"
	"path"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"
)

// FilePrefix makes a GitOps setting the content of a local file.
const FilePrefix = "!file:"

var gitOpsNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// expandLabelledBindings turns `binding "name" { ... }` blocks, which HCL
// decodes as objects keyed by their label, into bindings with a name.
func expandLabelledBindings(list []interface{}, at string) ([]interface{}, error) {
	var res []interface{}
	for i, item := range list {
		obj, err := single(item, fmt.Sprintf("%s[%d]", at, i))
		if err != nil {
			return nil, err
		}
		labels := make([]string, 0, len(obj))
		for key := range obj {
			if _, isField := fieldByTag(reflectBinding, key); isField {
				labels = nil
				break
			}
			labels = append(labels, key)
		}
		if len(labels) == 0 {
			res = append(res, item)
			continue
		}
		sort.Strings(labels)
		for _, label := range labels {
			blocks, ok := obj[label].([]map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("%s[%d]: unknown key %q", at, i, label)
			}
			for _, block := range blocks {
				named := map[string]interface{}{"name": label}
				for k, v := range block {
					named[k] = v
				}
				res = append(res, named)
			}
		}
	}
	return res, nil
}

func (p *DeploymentPlan) validateGitOps() error {
	configs := []*GitOpsConfig{p.GitOps}
	if p.Packs != nil {
		configs = append(configs, p.Packs.GitOps)
	}
	seen := map[string]bool{}
	for _, g := range configs {
		if g == nil {
			continue
		}
		if g.LogBufferSize < 0 {
			return fmt.Errorf("gitops: log_buffer_size can't be negative")
		}
		if g.DebounceDuration != "" {
			if d, err := time.ParseDuration(g.DebounceDuration); err != nil || d < 0 {
				return fmt.Errorf("gitops: invalid debounce_duration %q", g.DebounceDuration)
			}
		}
		for _, b := range g.AllBindings() {
			if err := validateBinding(&b, seen); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateBinding(b *GitOpsBinding, seen map[string]bool) error {
	if !gitOpsNameRe.MatchString(b.Name) {
		return fmt.Errorf("gitops: binding name %q is invalid", b.Name)
	}
	if seen[b.Name] {
		return fmt.Errorf("gitops: duplicate binding %q", b.Name)
	}
	seen[b.Name] = true
	if b.ReleaseName == "" {
		return fmt.Errorf("gitops binding %s: release_name is required", b.Name)
	}

	blocks := 0
	for _, set := range []bool{b.Forgejo != nil, b.Gitea != nil, b.GitHub != nil, b.GitLab != nil} {
		if set {
			blocks++
		}
	}
	if blocks != 1 {
		return fmt.Errorf("gitops binding %s: needs exactly one of forgejo, gitea, github and gitlab", b.Name)
	}
	switch b.Provider {
	case "", "forgejo", "gitea", "github", "gitlab":
	default:
		return fmt.Errorf("gitops binding %s: unknown provider %q", b.Name, b.Provider)
	}
	repo := b.EffectiveRepo()
	if repo.URL == "" {
		return fmt.Errorf("gitops binding %s: the url of the %s block is required", b.Name, b.EffectiveProvider())
	}
	for _, part := range strings.Split(repo.Subpath, "/") {
		if part == ".." {
			return fmt.Errorf("gitops binding %s: subpath %q leaves the repository", b.Name, repo.Subpath)
		}
	}

	if b.Verify.Active() && len(b.Verify.PublicKeys) == 0 {
		return fmt.Errorf("gitops binding %s: verify needs public_keys", b.Name)
	}

	if w := b.Webhook; w != nil {
		if w.SecretKey == "" {
			return fmt.Errorf("gitops binding %s: the webhook needs a secret_key", b.Name)
		}
		for _, list := range [][]string{w.FilterBranches, w.FilterTags} {
			for _, pattern := range list {
				if _, err := path.Match(pattern, ""); err != nil {
					return fmt.Errorf("gitops binding %s: invalid filter %q", b.Name, pattern)
				}
			}
		}
	}
	return nil
}

// GitOpsDebounce returns the debounce window; 0 means the default.
func (p *DeploymentPlan) GitOpsDebounce() time.Duration {
	if g := p.gitOpsConfig(); g != nil {
		d, _ := time.ParseDuration(g.DebounceDuration)
		return d
	}
	return 0
}

// GitOpsLogBufferSize returns the size of the log buffer; 0 means the default.
func (p *DeploymentPlan) GitOpsLogBufferSize() int {
	if g := p.gitOpsConfig(); g != nil {
		return g.LogBufferSize
	}
	return 0
}

func (p *DeploymentPlan) gitOpsConfig() *GitOpsConfig {
	if p.Packs != nil && p.Packs.GitOps != nil {
		return p.Packs.GitOps
	}
	return p.GitOps
}

// ResolvedGitOpsBindings returns copies of the bindings with their secrets
// decrypted by resolve and their !file: settings read. Only the copies hold
// them, and resolve must refuse what it can't decrypt.
func (p *DeploymentPlan) ResolvedGitOpsBindings(resolve func(string) (string, error)) ([]GitOpsBinding, error) {
	bindings := p.AllGitOpsBindings()
	value := func(name, v string) (string, error) {
		if file, ok := strings.CutPrefix(v, FilePrefix); ok {
			data, err := os.ReadFile(p.ResolveLocalPath(file))
			if err != nil {
				return "", fmt.Errorf("gitops binding %s: %w", name, err)
			}
			return string(data), nil
		}
		res, err := resolve(v)
		if err != nil {
			return "", fmt.Errorf("gitops binding %s: %w", name, err)
		}
		return res, nil
	}

	for i := range bindings {
		b := &bindings[i]
		repo := *b.EffectiveRepo()
		for _, field := range []*string{&repo.AuthSecret, &repo.SSHPrivateKey, &repo.KnownHosts, &repo.CACertPEM} {
			v, err := value(b.Name, *field)
			if err != nil {
				return nil, err
			}
			*field = v
		}
		switch b.EffectiveProvider() {
		case "gitea":
			b.Gitea = &repo
		case "github":
			gh := *b.GitHub
			gh.RepoConfig = repo
			b.GitHub = &gh
		case "gitlab":
			gl := *b.GitLab
			gl.RepoConfig = repo
			b.GitLab = &gl
		default:
			b.Forgejo = &repo
		}
		if b.Webhook != nil {
			w := *b.Webhook
			v, err := value(b.Name, w.SecretKey)
			if err != nil {
				return nil, err
			}
			w.SecretKey = v
			b.Webhook = &w
		}
		if b.Verify != nil {
			v := *b.Verify
			v.PublicKeys = make([]string, len(b.Verify.PublicKeys))
			for j, key := range b.Verify.PublicKeys {
				res, err := value(b.Name, key)
				if err != nil {
					return nil, err
				}
				v.PublicKeys[j] = res
			}
			b.Verify = &v
		}
		values, err := resolveTree(b.Values, resolve)
		if err != nil {
			return nil, fmt.Errorf("gitops binding %s values: %w", b.Name, err)
		}
		b.Values, _ = values.(map[string]interface{})
	}
	return bindings, nil
}

func resolveTree(v interface{}, resolve func(string) (string, error)) (interface{}, error) {
	switch t := v.(type) {
	case string:
		return resolve(t)
	case map[string]interface{}:
		if t == nil {
			return t, nil
		}
		res := make(map[string]interface{}, len(t))
		for k, e := range t {
			x, err := resolveTree(e, resolve)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			res[k] = x
		}
		return res, nil
	case []interface{}:
		res := make([]interface{}, len(t))
		for i, e := range t {
			x, err := resolveTree(e, resolve)
			if err != nil {
				return nil, err
			}
			res[i] = x
		}
		return res, nil
	}
	return v, nil
}

var reflectBinding = reflect.TypeOf(GitOpsBinding{})
