package connector

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// SSHConnector executes commands and manages files on remote nodes over native SSH and SFTP.
type SSHConnector struct {
	client         *ssh.Client
	sftpClient     *sftp.Client
	host           string
	user           string
	become         bool
	becomePassword string
	windows        bool
}

// SSHOptions provides connection parameters for SSH.
type SSHOptions struct {
	Host           string
	Port           int
	User           string
	Password       string
	KeyPath        string
	KeyPassphrase  string
	PrivateKeyData []byte
	Timeout        time.Duration

	// Become runs commands and privileged file writes through sudo when the
	// SSH user is not root. BecomePassword is fed to sudo on stdin; without
	// it sudo must not ask for a password (NOPASSWD).
	Become         bool
	BecomePassword string

	// HostKeyCallback checks the host key; nil accepts any key.
	HostKeyCallback ssh.HostKeyCallback

	// Windows wraps commands in PowerShell and maps paths for the Windows
	// OpenSSH SFTP server.
	Windows bool
}

// NewSSHConnector establishes an SSH and SFTP session to the remote target.
func NewSSHConnector(opts SSHOptions) (*SSHConnector, error) {
	if opts.Port == 0 {
		opts.Port = 22
	}
	if opts.Timeout == 0 {
		opts.Timeout = 15 * time.Second
	}

	var authMethods []ssh.AuthMethod

	// 1. Private key authentication
	if len(opts.PrivateKeyData) > 0 {
		signer, err := parseSigner(opts.PrivateKeyData, opts.KeyPassphrase)
		if err != nil {
			return nil, fmt.Errorf("failed to parse private key data: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	} else if opts.KeyPath != "" {
		keyBytes, err := os.ReadFile(ExpandHome(opts.KeyPath))
		if err != nil {
			return nil, fmt.Errorf("failed to read private key file (%s): %w", opts.KeyPath, err)
		}
		signer, err := parseSigner(keyBytes, opts.KeyPassphrase)
		if err != nil {
			return nil, fmt.Errorf("failed to parse private key (%s): %w", opts.KeyPath, err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	// 2. Password authentication
	if opts.Password != "" {
		authMethods = append(authMethods, ssh.Password(opts.Password))
	}

	if len(authMethods) == 0 {
		return nil, fmt.Errorf("no valid authentication method provided for SSH target (%s@%s)", opts.User, opts.Host)
	}

	hostKeyCallback := opts.HostKeyCallback
	if hostKeyCallback == nil {
		hostKeyCallback = ssh.InsecureIgnoreHostKey()
	}
	config := &ssh.ClientConfig{
		User:            opts.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         opts.Timeout,
	}

	addr := net.JoinHostPort(opts.Host, fmt.Sprintf("%d", opts.Port))
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return nil, fmt.Errorf("failed to dial SSH target (%s): %w", addr, err)
	}

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("failed to initialize SFTP subsystem (%s): %w", addr, err)
	}

	return &SSHConnector{
		client:         client,
		sftpClient:     sftpClient,
		host:           opts.Host,
		user:           opts.User,
		become:         opts.Become && !opts.Windows && opts.User != "" && opts.User != "root",
		becomePassword: opts.BecomePassword,
		windows:        opts.Windows,
	}, nil
}

func parseSigner(pemBytes []byte, passphrase string) (ssh.Signer, error) {
	if passphrase != "" {
		return ssh.ParsePrivateKeyWithPassphrase(pemBytes, []byte(passphrase))
	}
	return ssh.ParsePrivateKey(pemBytes)
}

// run executes a command, wrapped in sudo when become is active, and returns
// stdout and stderr separately.
func (s *SSHConnector) run(cmd string) (string, string, error) {
	session, err := s.client.NewSession()
	if err != nil {
		return "", "", fmt.Errorf("failed to create SSH session: %w", err)
	}
	defer session.Close()

	if s.windows {
		cmd = PowerShellCommand(cmd)
	} else if s.become {
		if s.becomePassword != "" {
			cmd = "sudo -S -p '' -- bash -c " + ShellQuote(cmd)
			session.Stdin = strings.NewReader(s.becomePassword + "\n")
		} else {
			cmd = "sudo -n -- bash -c " + ShellQuote(cmd)
		}
	}

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	err = session.Run(cmd)
	return stdout.String(), stderr.String(), err
}

func (s *SSHConnector) ExecuteCommand(cmd string) (string, error) {
	stdout, stderr, err := s.run(cmd)
	output := stdout
	if stderr != "" {
		if output != "" {
			output += "\n"
		}
		output += stderr
	}

	if err != nil {
		return output, fmt.Errorf("remote command failed: %w (output: %s)", err, output)
	}
	return output, nil
}

func (s *SSHConnector) CopyFile(srcReader io.Reader, destPath string, mode os.FileMode) error {
	if s.become {
		return s.copyFileBecome(srcReader, destPath, mode)
	}

	_ = s.sftpClient.MkdirAll(s.sftpPath(RemoteDir(destPath)))
	destPath = s.sftpPath(destPath)

	destFile, err := s.sftpClient.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("failed to open remote destination file (%s): %w", destPath, err)
	}
	defer destFile.Close()

	if _, err := io.Copy(destFile, srcReader); err != nil {
		return fmt.Errorf("SFTP file transfer error (%s): %w", destPath, err)
	}

	if err := s.sftpClient.Chmod(destPath, mode); err != nil {
		// Non-fatal warning on Windows SFTP servers
		_ = err
	}
	return nil
}

// copyFileBecome uploads to a private temporary file as the SSH user and moves
// it into place with sudo, so that root-owned destinations can be written.
func (s *SSHConnector) copyFileBecome(srcReader io.Reader, destPath string, mode os.FileMode) error {
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	tmpPath := "/tmp/.norca-deployer-" + hex.EncodeToString(suffix)

	tmpFile, err := s.sftpClient.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL)
	if err != nil {
		return fmt.Errorf("failed to open remote temporary file (%s): %w", tmpPath, err)
	}
	_ = s.sftpClient.Chmod(tmpPath, 0600)
	_, copyErr := io.Copy(tmpFile, srcReader)
	tmpFile.Close()
	if copyErr != nil {
		_ = s.sftpClient.Remove(tmpPath)
		return fmt.Errorf("SFTP file transfer error (%s): %w", destPath, copyErr)
	}

	cmd := fmt.Sprintf("install -D -o root -g root -m %04o %s %s; rc=$?; rm -f %s; exit $rc",
		mode.Perm(), ShellQuote(tmpPath), ShellQuote(destPath), ShellQuote(tmpPath))
	if out, err := s.ExecuteCommand(cmd); err != nil {
		return fmt.Errorf("failed to install remote file (%s): %w (output: %s)", destPath, err, out)
	}
	return nil
}

func (s *SSHConnector) ReadFile(remotePath string) ([]byte, error) {
	if s.become {
		stdout, stderr, err := s.run("cat -- " + ShellQuote(remotePath))
		if err != nil {
			return nil, fmt.Errorf("failed to read remote file (%s): %w (%s)", remotePath, err, strings.TrimSpace(stderr))
		}
		return []byte(stdout), nil
	}

	file, err := s.sftpClient.Open(s.sftpPath(remotePath))
	if err != nil {
		return nil, fmt.Errorf("failed to open remote file (%s): %w", remotePath, err)
	}
	defer file.Close()

	return io.ReadAll(file)
}

func (s *SSHConnector) FileExists(path string) (bool, error) {
	if s.become {
		_, _, err := s.run("test -e " + ShellQuote(path))
		if err == nil {
			return true, nil
		}
		if _, ok := err.(*ssh.ExitError); ok {
			return false, nil
		}
		return false, err
	}

	_, err := s.sftpClient.Stat(s.sftpPath(path))
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "not found") {
		return false, nil
	}
	return false, err
}

func (s *SSHConnector) MkdirAll(dirPath string, mode os.FileMode) error {
	if s.become {
		cmd := fmt.Sprintf("mkdir -p %s && chmod %04o %s", ShellQuote(dirPath), mode.Perm(), ShellQuote(dirPath))
		_, err := s.ExecuteCommand(cmd)
		return err
	}
	return s.sftpClient.MkdirAll(s.sftpPath(dirPath))
}

func (s *SSHConnector) sftpPath(p string) string {
	if s.windows {
		return SFTPPath(p)
	}
	return p
}

func (s *SSHConnector) Close() error {
	var errs []string
	if s.sftpClient != nil {
		if err := s.sftpClient.Close(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if s.client != nil {
		if err := s.client.Close(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("errors closing connection: %s", strings.Join(errs, "; "))
	}
	return nil
}
