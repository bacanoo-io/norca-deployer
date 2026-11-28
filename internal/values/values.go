// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package values resolves the value forms that the files of a tree may hold
// (design/platform-management-design.md §9): an encrypted value, an
// environment variable, the content of a file, and a generated value, which
// is left to the servers.
//
// A tree comes from a commit, so whoever can push decides which forms its
// files use. A form must therefore never reach more than the operator gave
// the process on purpose: a file is read only below one directory, an
// environment variable only when its name is allowed, and no form is
// resolved without the setting that enables it.
package values

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bacanoo-io/norca-deployer/internal/crypto"
)

// The value forms besides the encrypted one (crypto.EncryptedPrefix).
const (
	// EnvPrefix starts the form "!env:NAME", the value of an environment
	// variable.
	EnvPrefix = "!env:"
	// FilePrefix starts the form "@path", the content of a file below the
	// values directory.
	FilePrefix = "@"
	// GeneratePrefix starts the form "!generate:name": a random value the
	// servers create once for the stack (Rigger) and keep.
	GeneratePrefix = "!generate:"
)

// maxFileBytes bounds the content of a file value: it becomes an item of a
// secret, which the secret store bounds far lower.
const maxFileBytes = 1 << 20

// redacted replaces a secret value in a text (Redact).
const redacted = "<redacted>"

// The refusals of a form whose setting is not given.
var (
	// ErrNoKey refuses an encrypted value without the master key.
	ErrNoKey = errors.New("an encrypted value needs the master key (--secret-key, --secret-key-file or NORCA_DEPLOYER_SECRET_KEY)")
	// ErrNoFiles refuses a file value without the values directory.
	ErrNoFiles = errors.New("a file value (@path) needs the values directory of the startup configuration")
	// ErrEnvNotAllowed refuses an environment variable the startup
	// configuration does not allow.
	ErrEnvNotAllowed = errors.New("the environment variable is not allowed by the startup configuration")
)

// Resolver resolves value forms. The zero value resolves plain text only:
// every form that reads something is refused until its setting is given.
type Resolver struct {
	// Decrypt decrypts an encrypted value; nil refuses one (ErrNoKey).
	Decrypt func(string) (string, error)

	// Files is the directory file values are read from; nil refuses them
	// (ErrNoFiles). The root keeps a path from leaving it, through ".." or
	// a symbolic link.
	Files *os.Root

	// Env are the environment variables an "!env:" value may read: names,
	// or patterns of path.Match ("APP_*"). Empty allows none.
	Env []string

	// LookupEnv reads an environment variable; nil is os.LookupEnv.
	LookupEnv func(string) (string, bool)
}

// Resolve resolves one value. A generated value ("!generate:name") is
// returned as it is: the servers create it.
func (r *Resolver) Resolve(v string) (string, error) {
	switch {
	case strings.HasPrefix(strings.TrimSpace(v), crypto.EncryptedPrefix):
		return r.decrypt(strings.TrimSpace(v))
	case strings.HasPrefix(v, EnvPrefix):
		return r.env(strings.TrimPrefix(v, EnvPrefix))
	case strings.HasPrefix(v, FilePrefix):
		return r.file(strings.TrimPrefix(v, FilePrefix))
	}

	return v, nil
}

// ResolveTree resolves every string of a tree of values (maps, lists and
// strings, as a decoder gives them) into a copy. An error names the path
// of the value.
func (r *Resolver) ResolveTree(v any) (any, error) {
	switch t := v.(type) {
	case string:
		return r.Resolve(t)
	case map[string]any:
		out := make(map[string]any, len(t))

		for k, e := range t {
			x, err := r.ResolveTree(e)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}

			out[k] = x
		}

		return out, nil
	case []any:
		out := make([]any, len(t))

		for i, e := range t {
			x, err := r.ResolveTree(e)
			if err != nil {
				return nil, fmt.Errorf("[%d]: %w", i, err)
			}

			out[i] = x
		}

		return out, nil
	}

	return v, nil
}

func (r *Resolver) decrypt(v string) (string, error) {
	if r.Decrypt == nil {
		return "", ErrNoKey
	}

	out, err := r.Decrypt(v)
	if err != nil {
		return "", fmt.Errorf("decrypting a value: %w", err)
	}

	return out, nil
}

func (r *Resolver) env(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("%q names no environment variable", EnvPrefix)
	}

	if !slices.ContainsFunc(r.Env, func(pattern string) bool {
		ok, err := path.Match(pattern, name)
		return err == nil && ok
	}) {
		return "", fmt.Errorf("%s%s: %w", EnvPrefix, name, ErrEnvNotAllowed)
	}

	lookup := r.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}

	v, ok := lookup(name)
	if !ok {
		return "", fmt.Errorf("%s%s: the environment variable is not set", EnvPrefix, name)
	}

	return v, nil
}

func (r *Resolver) file(name string) (string, error) {
	if r.Files == nil {
		return "", ErrNoFiles
	}

	// os.Root refuses a path that leaves the directory; an absolute or an
	// empty one is refused here with a clearer message.
	if name == "" || filepath.IsAbs(name) || path.IsAbs(name) {
		return "", fmt.Errorf("%s%s: a file value is a path relative to the values directory", FilePrefix, name)
	}

	f, err := r.Files.Open(filepath.FromSlash(name))
	if err != nil {
		return "", fmt.Errorf("%s%s: %w", FilePrefix, name, err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("%s%s: %w", FilePrefix, name, err)
	}

	if len(data) > maxFileBytes {
		return "", fmt.Errorf("%s%s: the file is larger than %d bytes", FilePrefix, name, maxFileBytes)
	}

	return string(data), nil
}

// IsGenerated reports whether v is a generated value ("!generate:name"),
// which the servers create.
func IsGenerated(v string) bool {
	return strings.HasPrefix(v, GeneratePrefix)
}

// Redact replaces each secret value in text, the longest first so that a
// value inside another is not left half shown. Values shorter than four
// bytes are left alone: replacing them would garble the text and hide
// nothing.
func Redact(text string, secrets []string) string {
	sorted := slices.Clone(secrets)
	slices.SortFunc(sorted, func(a, b string) int { return len(b) - len(a) })

	for _, s := range sorted {
		if len(s) >= 4 {
			text = strings.ReplaceAll(text, s, redacted)
		}
	}

	return text
}
