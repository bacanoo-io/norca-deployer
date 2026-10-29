package connector

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// knownHostsMu serializes writes to the tofu known_hosts file, which the
// per-node goroutines share.
var knownHostsMu sync.Mutex

// HostKeyCallback builds the host key check of a connection.
//
//   - off: accept any key.
//   - strict: the key must be in knownHostsFile or tofuFile.
//   - tofu: like strict, but an unknown host is trusted on first use and
//     recorded in tofuFile; a changed key is always rejected.
func HostKeyCallback(mode, knownHostsFile, tofuFile string) (ssh.HostKeyCallback, error) {
	if mode == "off" {
		return ssh.InsecureIgnoreHostKey(), nil
	}

	var files []string
	for _, f := range []string{knownHostsFile, tofuFile} {
		if f == "" {
			continue
		}
		if _, err := os.Stat(f); err == nil {
			files = append(files, f)
		}
	}

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		knownHostsMu.Lock()
		defer knownHostsMu.Unlock()

		// Re-read on every check: another goroutine may have recorded a key.
		existing := files
		if tofuFile != "" {
			if _, err := os.Stat(tofuFile); err == nil && !contains(existing, tofuFile) {
				existing = append(existing, tofuFile)
			}
		}
		if len(existing) > 0 {
			cb, err := knownhosts.New(existing...)
			if err != nil {
				return fmt.Errorf("failed to read known_hosts: %w", err)
			}
			err = cb(hostname, remote, key)
			if err == nil {
				return nil
			}
			var keyErr *knownhosts.KeyError
			if !errors.As(err, &keyErr) || len(keyErr.Want) > 0 {
				// A different key is recorded for this host.
				return fmt.Errorf("host key of %s changed (%s): %w", hostname, ssh.FingerprintSHA256(key), err)
			}
		}

		if mode != "tofu" || tofuFile == "" {
			return fmt.Errorf("unknown host key for %s (%s); add it to known_hosts or use host_key_checking: tofu",
				hostname, ssh.FingerprintSHA256(key))
		}

		if err := os.MkdirAll(filepath.Dir(tofuFile), 0700); err != nil {
			return err
		}
		f, err := os.OpenFile(tofuFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
		if _, err := fmt.Fprintln(f, line); err != nil {
			return err
		}
		fmt.Printf("    [TOFU] Trusting host key of %s: %s\n", hostname, ssh.FingerprintSHA256(key))
		return nil
	}, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
