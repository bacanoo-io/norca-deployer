package connector

import (
	"io"
	"os"
)

// Connector defines a unified interface for local and remote command execution and file transfer.
type Connector interface {
	// ExecuteCommand runs a command on the target host and returns its output.
	ExecuteCommand(cmd string) (string, error)

	// CopyFile streams data from a reader to a destination path on the target host.
	CopyFile(srcReader io.Reader, destPath string, mode os.FileMode) error

	// ReadFile reads the full contents of a file on the target host.
	ReadFile(remotePath string) ([]byte, error)

	// FileExists checks whether a file exists on the target host.
	FileExists(path string) (bool, error)

	// MkdirAll creates a directory including any necessary parent directories.
	MkdirAll(dirPath string, mode os.FileMode) error

	// Close releases any open connections and resources.
	Close() error
}
