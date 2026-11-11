// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package plan

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestServerRoles checks the default and the validation of bootstrap_expect
// with server roles: only the masters with the role voter bootstrap.
func TestServerRoles(t *testing.T) {
	load := func(t *testing.T, cfg string) (*DeploymentPlan, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "cluster-deploy.yaml")
		if err := os.WriteFile(path, []byte(cfg), 0644); err != nil {
			t.Fatal(err)
		}
		return LoadFromFile(path)
	}

	p, err := load(t, `
inventory:
  masters:
    - host: 10.0.0.1
    - host: 10.0.0.2
      server_settings: {role: voter}
    - host: 10.0.0.3
    - host: 10.0.0.4
      server_settings: {role: standby}
    - host: 10.0.0.5
      server_settings: {role: observer}
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, m := range p.Inventory.Masters {
		if m.ServerSettings.BootstrapExpect != 3 {
			t.Errorf("%s: bootstrap_expect %d, want 3 (the voter-role masters)", m.Host, m.ServerSettings.BootstrapExpect)
		}
	}

	cases := map[string]struct {
		cfg string
		err string
	}{
		"unknown role": {`
inventory:
  masters:
    - host: 10.0.0.1
    - host: 10.0.0.2
      server_settings: {role: witness}
`, `role "witness" must be voter, standby or observer`},
		"bootstrap_expect counts every master": {`
inventory:
  masters:
    - host: 10.0.0.1
      server_settings: {bootstrap_expect: 2}
    - host: 10.0.0.2
      server_settings: {role: observer, bootstrap_expect: 2}
`, "does not match the number of masters with the role voter (1)"},
		"no voter": {`
inventory:
  masters:
    - host: 10.0.0.1
      server_settings: {role: standby}
`, `at least one master must have the role "voter"`},
	}
	for name, tc := range cases {
		p, err := load(t, tc.cfg)
		if err == nil {
			err = p.Validate()
		}
		if err == nil || !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: got %v, want error containing %q", name, err, tc.err)
		}
	}
}
