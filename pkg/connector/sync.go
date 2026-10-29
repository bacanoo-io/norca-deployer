package connector

import (
	"bytes"
	"fmt"
	"os"
	"strings"
)

// SyncFile writes content to a remote file unless it already has exactly
// that content. It reports whether the file changed.
func SyncFile(conn Connector, path string, content []byte, mode os.FileMode) (bool, error) {
	if current, err := conn.ReadFile(path); err == nil && bytes.Equal(current, content) {
		return false, nil
	}
	if err := conn.CopyFile(bytes.NewReader(content), path, mode); err != nil {
		return false, fmt.Errorf("failed to write %s: %w", path, err)
	}
	return true, nil
}

// RemoteSHA256 returns the digest of a remote file, or "" if it is missing.
func RemoteSHA256(conn Connector, path string, windows bool) string {
	var cmd string
	if windows {
		cmd = "if (Test-Path -LiteralPath " + PSQuote(path) + ") { (Get-FileHash -Algorithm SHA256 -LiteralPath " +
			PSQuote(path) + ").Hash.ToLower() }"
	} else {
		cmd = "sha256sum " + ShellQuote(path) + " 2>/dev/null"
	}
	out, err := conn.ExecuteCommand(cmd)
	if err != nil {
		return ""
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToLower(fields[0])
}

// PendingSuffix marks a Windows binary waiting for the service restart: a
// running .exe cannot be replaced.
const PendingSuffix = ".new"

// SyncLocalFile copies a local file to the node unless the remote file has
// the same digest. On Linux the file is replaced atomically by rename, so a
// running binary keeps working until the restart. On Windows a changed
// binary is staged as <path>.new and swapped in by the restart.
func SyncLocalFile(conn Connector, localPath, sha, remotePath string, mode os.FileMode, windows bool) (bool, error) {
	if RemoteSHA256(conn, remotePath, windows) == sha {
		return false, nil
	}
	f, err := os.Open(localPath)
	if err != nil {
		return false, err
	}
	defer f.Close()

	staged := remotePath + PendingSuffix
	if err := conn.CopyFile(f, staged, mode); err != nil {
		return false, fmt.Errorf("failed to copy %s: %w", remotePath, err)
	}
	if windows {
		return true, nil
	}
	cmd := "mv -f " + ShellQuote(staged) + " " + ShellQuote(remotePath)
	if out, err := conn.ExecuteCommand(cmd); err != nil {
		return false, fmt.Errorf("failed to install %s: %w (output: %s)", remotePath, err, out)
	}
	return true, nil
}
