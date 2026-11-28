// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package values

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/crypto"
)

// newRoot makes a values directory with one file, and a file outside it
// that a value must not reach.
func newRoot(t *testing.T) *os.Root {
	t.Helper()

	base := t.TempDir()
	dir := filepath.Join(base, "values")

	if err := os.MkdirAll(filepath.Join(dir, "certs"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "certs", "db-ca.pem"), []byte("CA"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(base, "outside"), []byte("secret of the host"), 0o600); err != nil {
		t.Fatal(err)
	}

	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = root.Close() })

	return root
}

func TestResolver_Resolve(t *testing.T) {
	t.Parallel()

	encrypted := crypto.EncryptedPrefix + "c2VjcmV0"
	full := &Resolver{
		Decrypt: func(v string) (string, error) {
			if v != encrypted {
				return "", errors.New("bad value")
			}

			return "decrypted", nil
		},
		Files: newRoot(t),
		Env:   []string{"APP_*", "DB_PASSWORD"},
		LookupEnv: func(name string) (string, bool) {
			return map[string]string{"APP_TOKEN": "tok", "NORCA_TOKEN": "mgmt"}[name], name != "APP_UNSET"
		},
	}

	tests := []struct {
		name     string
		resolver *Resolver
		in       string
		want     string
		wantErr  error
		errText  string
	}{
		{name: "plain text", resolver: &Resolver{}, in: "admin", want: "admin"},
		{name: "a generated value is left to the servers", resolver: &Resolver{}, in: "!generate:db", want: "!generate:db"},
		{name: "encrypted", resolver: full, in: encrypted, want: "decrypted"},
		{name: "encrypted with spaces around", resolver: full, in: " " + encrypted + " ", want: "decrypted"},
		{name: "encrypted without the key", resolver: &Resolver{}, in: encrypted, wantErr: ErrNoKey},
		{name: "encrypted, wrong", resolver: full, in: crypto.EncryptedPrefix + "eA==", errText: "decrypting a value"},
		{name: "an allowed variable", resolver: full, in: "!env:APP_TOKEN", want: "tok"},
		{name: "a variable not allowed", resolver: full, in: "!env:NORCA_TOKEN", wantErr: ErrEnvNotAllowed},
		{name: "no variable is allowed by default", resolver: &Resolver{}, in: "!env:APP_TOKEN", wantErr: ErrEnvNotAllowed},
		{name: "an allowed variable that is not set", resolver: full, in: "!env:APP_UNSET", errText: "not set"},
		{name: "no name", resolver: full, in: "!env:", errText: "names no environment variable"},
		{name: "a file of the values directory", resolver: full, in: "@certs/db-ca.pem", want: "CA"},
		{name: "files are refused by default", resolver: &Resolver{}, in: "@certs/db-ca.pem", wantErr: ErrNoFiles},
		{name: "a file outside the directory", resolver: full, in: "@../outside", errText: "@../outside"},
		{name: "an absolute path", resolver: full, in: "@/etc/passwd", errText: "relative to the values directory"},
		{name: "no path", resolver: full, in: "@", errText: "relative to the values directory"},
		{name: "a missing file", resolver: full, in: "@certs/none.pem", errText: "@certs/none.pem"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := tt.resolver.Resolve(tt.in)

			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Resolve(%q) error %v, want %v", tt.in, err, tt.wantErr)
				}
			case tt.errText != "":
				if err == nil || !strings.Contains(err.Error(), tt.errText) {
					t.Fatalf("Resolve(%q) error %v, want one with %q", tt.in, err, tt.errText)
				}
			case err != nil:
				t.Fatalf("Resolve(%q): %v", tt.in, err)
			case got != tt.want:
				t.Fatalf("Resolve(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestResolver_ResolveTree(t *testing.T) {
	t.Parallel()

	r := &Resolver{Env: []string{"A"}, LookupEnv: func(string) (string, bool) { return "a", true }}

	got, err := r.ResolveTree(map[string]any{
		"plain": "x",
		"list":  []any{"!env:A", 3},
		"inner": map[string]any{"gen": "!generate:g"},
	})
	if err != nil {
		t.Fatal(err)
	}

	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("ResolveTree = %T, want a map", got)
	}

	inner, _ := m["inner"].(map[string]any)
	list, _ := m["list"].([]any)

	if m["plain"] != "x" || len(list) != 2 || list[0] != "a" || list[1] != 3 || inner["gen"] != "!generate:g" {
		t.Fatalf("ResolveTree = %v", got)
	}

	// The error names where the value is.
	_, err = r.ResolveTree(map[string]any{"items": []any{"ok", "!env:B"}})
	if err == nil || !strings.HasPrefix(err.Error(), "items: [1]: ") {
		t.Fatalf("ResolveTree error %v, want one that starts with the path", err)
	}
}

func TestIsGenerated(t *testing.T) {
	t.Parallel()

	if !IsGenerated("!generate:db") || IsGenerated("generate:db") || IsGenerated("") {
		t.Fatal("IsGenerated answers wrong")
	}
}

func TestRedact(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		text    string
		secrets []string
		want    string
	}{
		{name: "a value", text: "denied for s3cret", secrets: []string{"s3cret"}, want: "denied for <redacted>"},
		{name: "the longer one first", text: "pw=s3cret-long", secrets: []string{"s3cret", "s3cret-long"}, want: "pw=<redacted>"},
		{name: "short values stay", text: "a b c", secrets: []string{"a", "bc"}, want: "a b c"},
		{name: "nothing to hide", text: "fine", secrets: nil, want: "fine"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := Redact(tt.text, tt.secrets); got != tt.want {
				t.Fatalf("Redact(%q) = %q, want %q", tt.text, got, tt.want)
			}
		})
	}
}
