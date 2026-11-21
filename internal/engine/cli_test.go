package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca-deployer/internal/connector"
	"github.com/bacanoo-io/norca-deployer/internal/platform/storage"
)

// runFileArg passes the file as the last argument instead of stdin, on both
// platforms, and the file holds the value exactly (no newline).
func TestRunFileArg(t *testing.T) {
	for _, platform := range []string{"linux", "windows"} {
		t.Run(platform, func(t *testing.T) {
			r, first, _ := bootstrapRunner(t, bootstrapPlan)
			first.node.Platform = platform
			first.paths = storage.ResolveNodePaths(first.node, r.plan)
			conn := connector.NewFakeConnector()
			first.conn = conn
			if _, err := first.cli(r, "").runFileArg([]string{"acl", "bootstrap"}, []byte("secret")); err != nil {
				t.Fatal(err)
			}
			cmd := conn.Commands[len(conn.Commands)-1]
			bodyFile, quoted := "/var/lib/norca-deployer/cli.body", "'acl' 'bootstrap' '/var/lib/norca-deployer/cli.body'"
			if platform == "windows" {
				bodyFile = first.paths.DataDir + `\deployer\cli.body`
				quoted = "'acl' 'bootstrap' '" + bodyFile + "'"
			}
			if !strings.Contains(cmd, quoted) || strings.Contains(cmd, "Get-Content -Raw '"+bodyFile+"' |") || strings.Contains(cmd, "< ") {
				t.Fatalf("command does not pass the file as an argument:\n%s", cmd)
			}
			if got := string(conn.Files[bodyFile]); got != "secret" {
				t.Fatalf("file %q holds %q", bodyFile, got)
			}
		})
	}
}

// The message of a failed API call names the status and the body when the
// call was answered, and the error when it was not; it never formats a nil
// error.
func TestAPIError(t *testing.T) {
	failed := errors.New("ssh: connection lost")
	tests := []struct {
		name     string
		status   int
		out      string
		err      error
		expected string
	}{
		{
			name:     "answered with a status that is not accepted",
			status:   503,
			out:      "pack registry \"norca\" could not be fetched on this server\n",
			expected: `pack "nats": HTTP 503: pack registry "norca" could not be fetched on this server`,
		},
		{
			name:     "the call failed",
			err:      failed,
			expected: `pack "nats": ssh: connection lost`,
		},
		{
			name:     "the call failed with output",
			out:      "norca: command not found",
			err:      failed,
			expected: `pack "nats": ssh: connection lost: norca: command not found`,
		},
		{
			name:     "the error holds the output already",
			out:      "connection lost",
			err:      failed,
			expected: `pack "nats": ssh: connection lost`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := apiError(`pack "nats"`, tt.status, tt.out, tt.err)
			if got.Error() != tt.expected {
				t.Errorf("apiError() = %q, expected %q", got.Error(), tt.expected)
			}
			if tt.err != nil && !errors.Is(got, tt.err) {
				t.Errorf("apiError() does not wrap %v", tt.err)
			}
		})
	}
}
