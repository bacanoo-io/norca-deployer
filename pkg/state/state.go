// Package state keeps the local state of the deployer next to the
// configuration file (.norca-deployer/): the gossip key, the ACL management
// token, the SSH known_hosts file and the download cache. Secrets are stored
// encrypted with the master key when one is available.
package state

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bacanoo-io/norca-deployer/pkg/config"
	"github.com/bacanoo-io/norca-deployer/pkg/crypto"
)

// Store is the state directory of one deployment plan.
type Store struct {
	Dir   string
	vault *crypto.Vault
}

// Open returns the store of a plan and creates its directory.
func Open(plan *config.DeploymentPlan, vault *crypto.Vault) (*Store, error) {
	dir := filepath.Join(plan.BaseDir(), config.StateDirName)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create state directory %s: %w", dir, err)
	}
	return &Store{Dir: dir, vault: vault}, nil
}

// OpenReadOnly returns the store of a plan without creating its directory
// (for commands that do not change anything).
func OpenReadOnly(plan *config.DeploymentPlan, vault *crypto.Vault) *Store {
	return &Store{Dir: filepath.Join(plan.BaseDir(), config.StateDirName), vault: vault}
}

// Path returns a path inside the state directory.
func (s *Store) Path(name string) string { return filepath.Join(s.Dir, name) }

// CacheDir is the download cache.
func (s *Store) CacheDir() string { return s.Path("cache") }

// KnownHostsPath is the known_hosts file of the tofu host key checking.
func (s *Store) KnownHostsPath() string { return s.Path("known_hosts") }

// ReadSecret reads a secret file; encrypted content is decrypted.
func (s *Store) ReadSecret(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	val := strings.TrimSpace(string(data))
	if strings.HasPrefix(val, crypto.EncryptedPrefix) {
		if s.vault == nil {
			return "", false, fmt.Errorf("%s is encrypted but no master key is available", path)
		}
		val, err = s.vault.Decrypt(val)
		if err != nil {
			return "", false, fmt.Errorf("%s: %w", path, err)
		}
	}
	return val, true, nil
}

// WriteSecret writes a secret file (0600), encrypted when a master key is
// available. It reports whether the value is stored in plain text.
func (s *Store) WriteSecret(path, val string) (plain bool, err error) {
	out := val
	if s.vault != nil {
		if out, err = s.vault.Encrypt(val); err != nil {
			return false, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false, err
	}
	if err := os.WriteFile(path, []byte(out+"\n"), 0600); err != nil {
		return false, err
	}
	return s.vault == nil, nil
}

// GossipKey resolves the gossip encryption key of the plan: empty means no
// encryption, AUTO_GENERATE creates a 32-byte key once and keeps it in the
// store, anything else is the key itself (possibly an !enc:v1 secret).
func (s *Store) GossipKey(configured string) (string, error) {
	switch {
	case configured == "":
		return "", nil
	case configured == config.GossipAutoGenerate:
		path := s.Path("gossip.key")
		if key, ok, err := s.ReadSecret(path); err != nil || ok {
			return key, err
		}
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		key := base64.StdEncoding.EncodeToString(raw)
		plain, err := s.WriteSecret(path, key)
		if err != nil {
			return "", fmt.Errorf("failed to save the gossip key: %w", err)
		}
		if plain {
			fmt.Printf("    [WARN] Gossip key stored unencrypted in %s (create a master key to encrypt it)\n", path)
		}
		return key, nil
	case strings.HasPrefix(configured, crypto.EncryptedPrefix):
		if s.vault == nil {
			return "", fmt.Errorf("security.gossip_encryption_key is encrypted but no master key is available")
		}
		return s.vault.Decrypt(configured)
	default:
		return configured, nil
	}
}
