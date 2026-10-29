package connector

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
)

// LocalConnector executes commands and file operations directly on the local operating system.
type LocalConnector struct{}

func NewLocalConnector() *LocalConnector {
	return &LocalConnector{}
}

func (l *LocalConnector) ExecuteCommand(cmd string) (string, error) {
	var execCmd *exec.Cmd

	if runtime.GOOS == "windows" {
		execCmd = exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", cmd)
	} else {
		execCmd = exec.Command("bash", "-c", cmd)
	}

	var stdout, stderr bytes.Buffer
	execCmd.Stdout = &stdout
	execCmd.Stderr = &stderr

	err := execCmd.Run()
	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += stderr.String()
	}

	if err != nil {
		return output, fmt.Errorf("command execution failed: %w (output: %s)", err, output)
	}
	return output, nil
}

func (l *LocalConnector) CopyFile(srcReader io.Reader, destPath string, mode os.FileMode) error {
	destFile, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("failed to create destination file (%s): %w", destPath, err)
	}
	defer destFile.Close()

	if _, err := io.Copy(destFile, srcReader); err != nil {
		return fmt.Errorf("failed to copy file contents (%s): %w", destPath, err)
	}
	return nil
}

func (l *LocalConnector) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

func (l *LocalConnector) FileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (l *LocalConnector) MkdirAll(dirPath string, mode os.FileMode) error {
	return os.MkdirAll(dirPath, mode)
}

func (l *LocalConnector) Close() error {
	return nil
}
