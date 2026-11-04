package connector

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/pkg/sftp"
)

// recorder logs what the SFTP server sees, in order.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(e string) {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
}

func (r *recorder) index(e string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, v := range r.events {
		if v == e {
			return i
		}
	}
	return -1
}

type recWriter struct {
	io.WriterAt
	rec *recorder
}

func (w recWriter) WriteAt(p []byte, off int64) (int, error) {
	w.rec.add("write")
	return w.WriterAt.WriteAt(p, off)
}

type putFunc func(*sftp.Request) (io.WriterAt, error)

func (f putFunc) Filewrite(r *sftp.Request) (io.WriterAt, error) { return f(r) }

type cmdFunc func(*sftp.Request) error

func (f cmdFunc) Filecmd(r *sftp.Request) error { return f(r) }

// newSFTPConnector returns a connector whose SFTP client talks to an in-memory
// server that records the order of the operations. failSetstat makes every
// chmod fail.
func newSFTPConnector(t *testing.T, windows, failSetstat bool) (*SSHConnector, *recorder) {
	t.Helper()
	rec := &recorder{}
	h := sftp.InMemHandler()
	put, cmd := h.FilePut, h.FileCmd
	h.FilePut = putFunc(func(r *sftp.Request) (io.WriterAt, error) {
		w, err := put.Filewrite(r)
		rec.add("open")
		return recWriter{w, rec}, err
	})
	h.FileCmd = cmdFunc(func(r *sftp.Request) error {
		if r.Method == "Setstat" {
			rec.add("chmod")
			if failSetstat {
				return errors.New("operation not permitted")
			}
		}
		return cmd.Filecmd(r)
	})

	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()
	srv := sftp.NewRequestServer(struct {
		io.Reader
		io.WriteCloser
	}{serverRead, serverWrite}, h)
	// Serve returns when the client closes its side; closing the server then
	// ends the other pipe, which lets the client's Close return.
	go func() {
		_ = srv.Serve()
		_ = srv.Close()
	}()

	client, err := sftp.NewClientPipe(clientRead, clientWrite)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return &SSHConnector{sftpClient: client, windows: windows}, rec
}

// A private key must never be written into a file with the default mode: the
// mode is set first.
func TestCopyFileSetsTheModeBeforeWriting(t *testing.T) {
	conn, rec := newSFTPConnector(t, false, false)
	if err := conn.CopyFile(strings.NewReader("-----BEGIN EC PRIVATE KEY-----"), "/key.pem", 0600); err != nil {
		t.Fatal(err)
	}
	chmod, write := rec.index("chmod"), rec.index("write")
	if chmod < 0 || write < 0 {
		t.Fatalf("events = %v, want a chmod and a write", rec.events)
	}
	if chmod > write {
		t.Fatalf("the content was written before the chmod: %v", rec.events)
	}
}

// When the mode cannot be set, nothing is written (fail closed).
func TestCopyFileFailsClosedWhenTheModeCannotBeSet(t *testing.T) {
	conn, rec := newSFTPConnector(t, false, true)
	err := conn.CopyFile(strings.NewReader("secret"), "/key.pem", 0600)
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("err = %v, want the failed chmod", err)
	}
	if rec.index("write") >= 0 {
		t.Fatalf("content was written although the chmod failed: %v", rec.events)
	}
}

// Windows SFTP servers do not map POSIX modes: there a failed chmod is not
// an error.
func TestCopyFileToleratesAFailedModeOnWindows(t *testing.T) {
	conn, rec := newSFTPConnector(t, true, true)
	if err := conn.CopyFile(strings.NewReader("data"), "/file.txt", 0644); err != nil {
		t.Fatal(err)
	}
	if rec.index("write") < 0 {
		t.Fatalf("nothing was written: %v", rec.events)
	}
}

func TestNewSSHConnectorRequiresAHostKeyCheck(t *testing.T) {
	_, err := NewSSHConnector(SSHOptions{Host: "127.0.0.1", Port: 1, User: "root", Password: "x"})
	if err == nil || !strings.Contains(err.Error(), "host key") {
		t.Fatalf("err = %v, want a refusal without a host key check", err)
	}
}
