package state

import (
	"crypto/rand"
	"fmt"
	"os"
)

// The ACL bootstrap secret of an authoritative region. Its servers carry the
// management token as acl.replication_token, so they need it in their
// configuration before the first start, when the ACL system is not
// bootstrapped yet. The deployer therefore chooses the secret itself, keeps
// it next to the management token file until the bootstrap is done, and
// hands it to `norca acl bootstrap`, which makes it the management token.

// pendingSuffix names the file of a bootstrap secret not yet used.
const pendingSuffix = ".pending"

// PendingBootstrapSecret returns the bootstrap secret kept for the
// management token file managementPath, creating a random UUID on first use.
// An apply that stopped before the bootstrap finds the same secret again.
func (s *Store) PendingBootstrapSecret(managementPath string) (string, error) {
	path := managementPath + pendingSuffix
	if v, ok, err := s.ReadSecret(path); err != nil || ok {
		return v, err
	}
	secret, err := newUUID()
	if err != nil {
		return "", err
	}
	plain, err := s.WriteSecret(path, secret)
	if err != nil {
		return "", fmt.Errorf("failed to save the ACL bootstrap secret: %w", err)
	}
	if plain {
		fmt.Printf("    [WARN] ACL bootstrap secret stored UNENCRYPTED in %s (create a master key with 'secret create-key')\n", path)
	}
	return secret, nil
}

// ConfirmBootstrapSecret stores the secret the bootstrap used as the
// management token and removes the pending file. It reports whether the
// token is stored in plain text.
func (s *Store) ConfirmBootstrapSecret(managementPath, secret string) (plain bool, err error) {
	if plain, err = s.WriteSecret(managementPath, secret); err != nil {
		return false, err
	}
	if err := os.Remove(managementPath + pendingSuffix); err != nil && !os.IsNotExist(err) {
		return plain, err
	}
	return plain, nil
}

// newUUID returns a random (version 4) UUID, the form Norca accepts as a
// bootstrap secret.
func newUUID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
