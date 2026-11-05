package connector

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
)

// FakeConnector records commands and file writes. It is meant for tests.
type FakeConnector struct {
	Commands []string
	Files    map[string][]byte
	Modes    map[string]os.FileMode
	// Responses maps a command prefix to its output; Failures maps a command
	// prefix to an error.
	Responses map[string]string
	Failures  map[string]error
	// Handler, when set, answers every command first; handled=false falls
	// through to Failures and Responses.
	Handler func(cmd string) (out string, err error, handled bool)
}

func NewFakeConnector() *FakeConnector {
	return &FakeConnector{
		Files:     map[string][]byte{},
		Modes:     map[string]os.FileMode{},
		Responses: map[string]string{},
		Failures:  map[string]error{},
	}
}

func (f *FakeConnector) ExecuteCommand(cmd string) (string, error) {
	f.Commands = append(f.Commands, cmd)
	if f.Handler != nil {
		if out, err, ok := f.Handler(cmd); ok {
			return out, err
		}
	}
	for prefix, err := range f.Failures {
		if strings.HasPrefix(cmd, prefix) {
			return "", fmt.Errorf("fake failure: %w", err)
		}
	}
	for prefix, out := range f.Responses {
		if strings.HasPrefix(cmd, prefix) {
			return out, nil
		}
	}
	return "", nil
}

func (f *FakeConnector) CopyFile(src io.Reader, dest string, mode os.FileMode) error {
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, src); err != nil {
		return err
	}
	f.Files[dest] = buf.Bytes()
	f.Modes[dest] = mode
	return nil
}

func (f *FakeConnector) ReadFile(p string) ([]byte, error) {
	data, ok := f.Files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return data, nil
}

func (f *FakeConnector) FileExists(p string) (bool, error) {
	_, ok := f.Files[p]
	return ok, nil
}

func (f *FakeConnector) MkdirAll(string, os.FileMode) error { return nil }

func (f *FakeConnector) Close() error { return nil }

// CommandsWithPrefix returns the recorded commands that start with prefix.
func (f *FakeConnector) CommandsWithPrefix(prefix string) []string {
	var res []string
	for _, c := range f.Commands {
		if strings.HasPrefix(c, prefix) {
			res = append(res, c)
		}
	}
	return res
}
