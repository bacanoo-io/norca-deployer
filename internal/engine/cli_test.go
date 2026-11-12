package engine

import (
	"errors"
	"testing"
)

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
