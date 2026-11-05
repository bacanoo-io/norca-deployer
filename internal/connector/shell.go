package connector

import (
	"path"
	"strings"
)

// ShellQuote quotes a string for safe use as a single POSIX shell word.
func ShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// RemoteDir returns the parent directory of a remote path. Windows paths
// (containing a backslash) are split on either separator; all other paths
// are treated as POSIX paths regardless of the platform the deployer runs on.
func RemoteDir(p string) string {
	if strings.Contains(p, `\`) {
		idx := strings.LastIndexAny(p, `\/`)
		if idx <= 0 {
			return p
		}
		return p[:idx]
	}
	return path.Dir(p)
}
