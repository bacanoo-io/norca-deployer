package gitops

import (
	"fmt"
	"sort"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"

	config "github.com/bacanoo-io/norca-deployer/pkg/plan"
)

// CheckVerifyKeys refuses public keys that can't be read, so that a typing
// error shows when a binding starts instead of as a refused pack.
func CheckVerifyKeys(keys []string) error {
	for i, key := range keys {
		if _, err := openpgp.ReadArmoredKeyRing(strings.NewReader(key)); err != nil {
			return fmt.Errorf("public key %d: %w", i+1, err)
		}
	}
	return nil
}

// CheckBinding checks the settings of a binding whose secrets are resolved.
func CheckBinding(b *config.GitOpsBinding) error {
	if err := CheckVerifyKeys(FetchOptionsFor(b).VerifyKeys); err != nil {
		return fmt.Errorf("gitops binding %s: %w", b.Name, err)
	}
	return nil
}

// signerName returns the identity of a key.
func signerName(e *openpgp.Entity) string {
	names := make([]string, 0, len(e.Identities))
	for name := range e.Identities {
		names = append(names, name)
	}
	if len(names) == 0 {
		return e.PrimaryKey.KeyIdString()
	}
	sort.Strings(names)
	return names[0]
}

type verifier interface {
	Verify(armoredKeyRing string) (*openpgp.Entity, error)
}

// verifyAny tries the keys one by one: a keyring is read from one armor block.
func verifyAny(v verifier, keys []string) (*openpgp.Entity, error) {
	var last error
	for _, key := range keys {
		e, err := v.Verify(key)
		if err == nil {
			return e, nil
		}
		last = err
	}
	return nil, last
}

// verifySignature accepts a signed annotated tag that points at the commit, or
// a signed commit. It returns who signed.
func verifySignature(repo *git.Repository, commit *object.Commit, tagName string, keys []string) (string, error) {
	if tagName != "" {
		if ref, err := repo.Tag(tagName); err == nil {
			if tag, err := repo.TagObject(ref.Hash()); err == nil && tag.Target == commit.Hash && tag.Signature != "" {
				if e, err := verifyAny(tag, keys); err == nil {
					return "tag " + tagName + " signed by " + signerName(e), nil
				}
			}
		}
	}
	if commit.Signature == "" {
		return "", fmt.Errorf("commit %s has no signature", commit.Hash)
	}
	e, err := verifyAny(commit, keys)
	if err != nil {
		return "", fmt.Errorf("commit %s is not signed by a trusted key: %w", commit.Hash, err)
	}
	return "commit signed by " + signerName(e), nil
}
