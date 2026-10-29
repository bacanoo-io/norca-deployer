package state

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"

	"github.com/nats-io/nkeys"
)

// Generated values of the plan ("!generate:<name>", "!nkey-seed:<name>"):
// created once, kept in the state directory (encrypted with the master key
// when there is one), so every apply and every entry that names them gets
// the same value.

// GeneratedValue returns the random value <name>, creating it with size
// random bytes (base64url) on first use.
func (s *Store) GeneratedValue(name string, size int) (string, error) {
	path := s.Path("generated/" + name)
	if v, ok, err := s.ReadSecret(path); err != nil || ok {
		return v, err
	}
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	v := base64.RawURLEncoding.EncodeToString(raw)
	if _, err := s.WriteSecret(path, v); err != nil {
		return "", fmt.Errorf("failed to save generated value %s: %w", name, err)
	}
	return v, nil
}

// NKeySeed returns the seed of the NATS user NKey <name>, creating the key
// pair on first use.
func (s *Store) NKeySeed(name string) (string, error) {
	path := s.Path("nkeys/" + name)
	if v, ok, err := s.ReadSecret(path); err != nil || ok {
		return v, err
	}
	kp, err := nkeys.CreateUser()
	if err != nil {
		return "", err
	}
	seed, err := kp.Seed()
	if err != nil {
		return "", err
	}
	if _, err := s.WriteSecret(path, string(seed)); err != nil {
		return "", fmt.Errorf("failed to save NKey %s: %w", name, err)
	}
	return string(seed), nil
}

// NKeyPublic returns the public key (U...) of the NATS user NKey <name>.
func (s *Store) NKeyPublic(name string) (string, error) {
	seed, err := s.NKeySeed(name)
	if err != nil {
		return "", err
	}
	kp, err := nkeys.FromSeed([]byte(seed))
	if err != nil {
		return "", fmt.Errorf("NKey %s: %w", name, err)
	}
	return kp.PublicKey()
}
