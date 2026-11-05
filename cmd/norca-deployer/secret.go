package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca-deployer/pkg/crypto"
)

// newSecretCmd builds the `secret` command group (create-key, encrypt).
func newSecretCmd() *cobra.Command {
	secretCmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage encrypted credentials and Master Key",
	}

	secretCmd.AddCommand(&cobra.Command{
		Use:   "create-key",
		Short: "Generate a new 256-bit AES Master Key file (.norca-deployer.key)",
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			hexKey, _, err := crypto.GenerateMasterKey()
			if err != nil {
				return fmt.Errorf("key generation error: %w", err)
			}
			if _, err := os.Stat(".norca-deployer.key"); err == nil {
				return errors.New(".norca-deployer.key already exists; it decrypts your existing secrets")
			}
			err = os.WriteFile(".norca-deployer.key", []byte(hexKey), 0600)
			if err != nil {
				return fmt.Errorf("failed to save key file: %w", err)
			}
			fmt.Fprintf(out, "[OK] New Master Key saved to .norca-deployer.key\n")
			return nil
		},
	})

	secretCmd.AddCommand(&cobra.Command{
		Use:   "encrypt <plaintext>",
		Short: "Encrypt text into inline secret format (!enc:v1:aes-gcm:...)",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			key, err := crypto.ResolveMasterKey(secretKey, secretKeyFile)
			if err != nil {
				return err
			}
			v, err := crypto.NewVault(key)
			if err != nil {
				return err
			}
			encrypted, err := v.Encrypt(args[0])
			if err != nil {
				return fmt.Errorf("encryption failed: %w", err)
			}
			fmt.Fprintf(out, "Encrypted value:\n%s\n", encrypted)
			return nil
		},
	})

	return secretCmd
}

// vaultIfAvailable returns the vault of the master key, or nil without a key.
func vaultIfAvailable() *crypto.Vault {
	key, err := crypto.ResolveMasterKey(secretKey, secretKeyFile)
	if err != nil {
		return nil
	}
	v, _ := crypto.NewVault(key)
	return v
}
