package connector

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"golang.org/x/crypto/ssh"
)

func TestShellQuote(t *testing.T) {
	if got := ShellQuote(`it's "x" $HOME`); got != `'it'\''s "x" $HOME'` {
		t.Fatalf("ShellQuote = %s", got)
	}
}

func TestRemoteDir(t *testing.T) {
	for in, want := range map[string]string{
		"/etc/norca.d/norca.hcl":                "/etc/norca.d",
		`C:\Program Files\norca\bin\norca.exe`:  `C:\Program Files\norca\bin`,
		`C:\ProgramData\norca\config/norca.hcl`: `C:\ProgramData\norca\config`,
	} {
		if got := RemoteDir(in); got != want {
			t.Errorf("RemoteDir(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSFTPPath(t *testing.T) {
	if got := SFTPPath(`C:\Program Files\norca\bin\norca.exe`); got != "/C:/Program Files/norca/bin/norca.exe" {
		t.Fatalf("SFTPPath = %s", got)
	}
	if got := SFTPPath("/etc/norca.d"); got != "/etc/norca.d" {
		t.Fatalf("POSIX path changed: %s", got)
	}
}

func TestPowerShellCommand(t *testing.T) {
	cmd := PowerShellCommand(`Write-Output 'a"b'`)
	prefix := "powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand "
	if !strings.HasPrefix(cmd, prefix) {
		t.Fatalf("cmd = %s", cmd)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(cmd, prefix))
	if err != nil {
		t.Fatal(err)
	}
	u := make([]uint16, len(raw)/2)
	for i := range u {
		u[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	if script := string(utf16.Decode(u)); !strings.Contains(script, `Write-Output 'a"b'`) {
		t.Fatalf("decoded script = %s", script)
	}
	if got := PSQuote("it's"); got != "'it''s'" {
		t.Fatalf("PSQuote = %s", got)
	}
}

func testKey(t *testing.T) ssh.PublicKey {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestHostKeyTOFU(t *testing.T) {
	tofu := filepath.Join(t.TempDir(), "known_hosts")
	addr := &net.TCPAddr{IP: net.ParseIP("10.0.0.1"), Port: 22}
	key1, key2 := testKey(t), testKey(t)

	cb, err := HostKeyCallback("tofu", "", tofu)
	if err != nil {
		t.Fatal(err)
	}
	if err := cb("10.0.0.1:22", addr, key1); err != nil {
		t.Fatalf("first use must be trusted: %v", err)
	}
	if data, _ := os.ReadFile(tofu); !strings.Contains(string(data), "10.0.0.1") {
		t.Fatalf("key not recorded: %s", data)
	}
	if err := cb("10.0.0.1:22", addr, key1); err != nil {
		t.Fatalf("known key rejected: %v", err)
	}
	if err := cb("10.0.0.1:22", addr, key2); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed key accepted: %v", err)
	}

	strict, _ := HostKeyCallback("strict", "", filepath.Join(t.TempDir(), "none"))
	if err := strict("10.0.0.2:22", addr, key1); err == nil {
		t.Fatalf("strict mode accepted an unknown host")
	}
}
