package connector

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// PowerShellCommand wraps a PowerShell script for a Windows OpenSSH server,
// whose default shell is cmd.exe. -EncodedCommand avoids every cmd.exe and
// PowerShell quoting problem.
func PowerShellCommand(script string) string {
	// Make native command failures and cmdlet errors fail the command.
	full := "$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue'; " + script +
		"; if ($LASTEXITCODE -and $LASTEXITCODE -ne 0) { exit $LASTEXITCODE }"
	u := utf16.Encode([]rune(full))
	b := make([]byte, len(u)*2)
	for i, c := range u {
		b[2*i] = byte(c)
		b[2*i+1] = byte(c >> 8)
	}
	return "powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -EncodedCommand " +
		base64.StdEncoding.EncodeToString(b)
}

// PSQuote quotes a string as a PowerShell single-quoted literal.
func PSQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// SFTPPath converts a Windows path (C:\dir\file) to the form the Windows
// OpenSSH SFTP server expects (/C:/dir/file). POSIX paths are unchanged.
func SFTPPath(p string) string {
	if len(p) >= 2 && p[1] == ':' {
		return "/" + strings.ReplaceAll(p, `\`, "/")
	}
	return p
}

// ExpandHome expands a leading ~ in a local path.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[1:])
		}
	}
	return p
}
