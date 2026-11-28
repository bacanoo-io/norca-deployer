// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// gitattributes keeps Git from converting the line endings of the files
// whose bytes are values (design §4, file-system hazards).
const gitattributes = `# Written by norca-deployer tree init: these files are compared byte for byte.
kv/** -text
**/secrets/** -text
**/*.pem -text
**/*.der -text
`

// gitignore keeps the startup configuration and keys out of Git.
const gitignore = `# Written by norca-deployer tree init: credentials never go into the tree.
norca-tree.hcl
*.key
`

// ConfigTemplate is the commented startup configuration tree init writes
// (design §5.2).
const ConfigTemplate = `# Startup configuration of norca-deployer tree: how this process reaches the
# cluster and what it may do. It holds credentials: keep it out of Git, mode 0600.

region "global" {
  address = "https://127.0.0.1:4646"
  # One of: token_env, token_file, token (a token may be encrypted, !enc:...).
  token_env = "NORCA_TOKEN"
  # ca_cert = "certs/ca.pem"

  # Values of the written stacks of stacks/, over their values/default.vars.hcl.
  # values = {
  #   replicas = 3
  # }

  # A token used only for the stacks it names (names or patterns); for = ["plan"]
  # is a read token for the plans.
  # scoped_token "apps" {
  #   token_env = "NORCA_TOKEN_APPS"
  #   stacks    = ["tree-ns-apps"]
  # }
}

# Registries the servers sync before the plans.
# sync_registries = ["global"]

# What the value forms of the tree may read.
# values {
#   key_file = "tree.key"   # the tree's own key for !enc: values, never the node plan's
#   dir      = "values"     # where @path values are read from
#   env      = ["APP_*"]    # the variables !env: may read
# }

# limits {
#   kinds       = ["foundation", "job"]  # the kinds a run may change
#   prune       = false                  # refuse --prune
#   max_changes = 50                     # more needs --yes-really
# }
`

// readme is the README.md of a new tree.
const readme = `# %s

A tree of norca-deployer: the objects of a Norca platform, applied as Rigger
stacks with ` + "`norca-deployer tree plan`" + ` and ` + "`tree apply`" + `.

- global/: cluster-wide objects (namespaces, quotas, ACL policies, admission
  policies, object kinds), the stack tree-global
- namespaces/<ns>/: the objects of a namespace (jobs, Variables, external
  services, objects, secrets), the stack tree-ns-<ns>
- stacks/<name>/: Rigger stacks written by hand
- modules/<name>/: local Rigger modules the written stacks share
`

// Init writes the skeleton of a new tree into dir: tree.hcl, README.md,
// .gitattributes and .gitignore. It refuses a directory that has a
// tree.hcl already, and never overwrites a file.
func Init(dir, name string) ([]string, error) {
	for _, m := range []string{manifestHCL, manifestYAML, manifestYML} {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			return nil, fmt.Errorf("%s has a %s already", dir, m)
		}
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}

	files := []struct{ name, data string }{
		{manifestHCL, fmt.Sprintf("tree {\n  name    = %q\n  version = %q\n}\n", name, FormatVersion)},
		{"README.md", fmt.Sprintf(readme, name)},
		{".gitattributes", gitattributes},
		{".gitignore", gitignore},
	}

	var written []string

	for _, f := range files {
		p := filepath.Join(dir, f.name)

		err := writeNew(p, []byte(f.data), 0o644)
		if errors.Is(err, fs.ErrExist) {
			continue
		}

		if err != nil {
			return written, err
		}

		written = append(written, p)
	}

	return written, nil
}

// WriteConfigTemplate writes the commented startup configuration to path
// with mode 0600, unless the file exists.
func WriteConfigTemplate(path string) (bool, error) {
	err := writeNew(path, []byte(ConfigTemplate), 0o600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}

	return err == nil, err
}

// writeNew writes a file that must not exist yet.
func writeNew(p string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()

		return err
	}

	return f.Close()
}
