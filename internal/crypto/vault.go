package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	EncryptedPrefix = "!enc:v1:aes-gcm:"
	KeySize         = 32 // AES-256 (32 bytes)
)

// Vault manages AES-256-GCM encryption and decryption of secrets and credentials.
type Vault struct {
	key []byte
}

// NewVault creates a new Vault instance with a 32-byte key.
func NewVault(key []byte) (*Vault, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("invalid key size: %d bytes (strictly %d bytes required)", len(key), KeySize)
	}
	return &Vault{key: key}, nil
}

// ResolveMasterKey resolves the encryption key from available sources in order of precedence.
func ResolveMasterKey(keyFlag, keyFile string) ([]byte, error) {
	// 1. Direct command line flag
	if keyFlag != "" {
		return parseKeyString(keyFlag)
	}

	// 2. Environment variable
	if envKey := os.Getenv("NORCA_DEPLOYER_SECRET_KEY"); envKey != "" {
		return parseKeyString(envKey)
	}

	// 3. User specified key file
	if keyFile != "" {
		data, err := os.ReadFile(keyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read key file (%s): %w", keyFile, err)
		}
		return parseKeyString(strings.TrimSpace(string(data)))
	}

	// 4. Local default key file
	if _, err := os.Stat(".norca-deployer.key"); err == nil {
		data, err := os.ReadFile(".norca-deployer.key")
		if err == nil {
			return parseKeyString(strings.TrimSpace(string(data)))
		}
	}

	return nil, errors.New("master encryption key not found (use --secret-key, --secret-key-file or NORCA_DEPLOYER_SECRET_KEY)")
}

func parseKeyString(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	// Try hex decoding
	if raw, err := hex.DecodeString(s); err == nil && len(raw) == KeySize {
		return raw, nil
	}
	// Try base64 decoding
	if raw, err := base64.StdEncoding.DecodeString(s); err == nil && len(raw) == KeySize {
		return raw, nil
	}
	// Raw 32 character string
	if len(s) == KeySize {
		return []byte(s), nil
	}
	return nil, fmt.Errorf("provided key has invalid format (32-byte hex or base64 string required)")
}

// GenerateMasterKey creates a new random 32-byte AES key in hex format.
func GenerateMasterKey() (string, []byte, error) {
	key := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return "", nil, err
	}
	return hex.EncodeToString(key), key, nil
}

// Encrypt encrypts a plaintext string with AES-256-GCM and returns the inline prefixed string.
func (v *Vault) Encrypt(plaintext string) (string, error) {
	block, err := aes.NewCipher(v.key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return EncryptedPrefix + base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt decodes an inline encrypted string back to plaintext.
func (v *Vault) Decrypt(encoded string) (string, error) {
	clean := strings.TrimSpace(encoded)
	if !strings.HasPrefix(clean, EncryptedPrefix) {
		return "", fmt.Errorf("value does not match encrypted prefix (%s...)", EncryptedPrefix)
	}

	payload := strings.TrimPrefix(clean, EncryptedPrefix)
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", fmt.Errorf("invalid base64 content: %w", err)
	}

	block, err := aes.NewCipher(v.key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", errors.New("ciphertext too short")
	}

	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decryption failed (bad key or corrupted data): %w", err)
	}

	return string(plaintext), nil
}

// ResolveSecret decrypts value if prefixed, otherwise returns it unchanged.
func (v *Vault) ResolveSecret(val string) (string, error) {
	if strings.HasPrefix(strings.TrimSpace(val), EncryptedPrefix) {
		return v.Decrypt(val)
	}
	return val, nil
}
