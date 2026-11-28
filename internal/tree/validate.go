// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package tree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// CheckSyntax parses every HCL file of the built stacks: a file that is no
// HCL is found with its file, line and column before any stack is planned
// (design §7.8, stage A check 1). The full evaluation is the servers' plan,
// or rigger stack validate offline (RiggerValidate).
func CheckSyntax(stacks []*Stack) error {
	var errs []error

	for _, s := range stacks {
		for _, name := range slices.Sorted(maps.Keys(s.Files)) {
			if !strings.HasSuffix(name, ".hcl") {
				continue
			}

			if _, diags := hclsyntax.ParseConfig(s.Files[name], s.Name+"/"+name, hcl.InitialPos); diags.HasErrors() {
				errs = append(errs, fmt.Errorf("stack %s: %w", s.Name, diags))
			}
		}
	}

	return errors.Join(errs...)
}

// WriteStacks writes each stack below out/<stack>/; the resolved secret
// values are not in the files, only in the values of a request.
func WriteStacks(out string, stacks []*Stack) error {
	for _, s := range stacks {
		for name, data := range s.Files {
			p := filepath.Join(out, s.Name, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				return err
			}

			if err := os.WriteFile(p, data, 0o600); err != nil {
				return err
			}
		}
	}

	return nil
}

// RiggerValidate runs the installed rigger program (rigger stack validate)
// on each stack, written to a temporary directory (design G23). The values
// of a stack are not given: a variable without a default is the servers'
// to check.
func RiggerValidate(ctx context.Context, rigger string, stacks []*Stack) error {
	if rigger == "" {
		rigger = "rigger"
	}

	bin, err := exec.LookPath(rigger)
	if err != nil {
		return fmt.Errorf("--rigger needs the rigger program: %w", err)
	}

	dir, err := os.MkdirTemp("", "norca-tree-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	if err := WriteStacks(dir, stacks); err != nil {
		return err
	}

	var errs []error

	for _, s := range stacks {
		var out bytes.Buffer

		cmd := exec.CommandContext(ctx, bin, "stack", "validate", filepath.Join(dir, s.Name))
		cmd.Stdout, cmd.Stderr = &out, &out

		if err := cmd.Run(); err != nil {
			errs = append(errs, fmt.Errorf("stack %s: rigger stack validate: %w\n%s", s.Name, err, strings.TrimSpace(out.String())))
		}
	}

	return errors.Join(errs...)
}
