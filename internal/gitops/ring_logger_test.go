package gitops

import (
	"strings"
	"testing"
)

func TestRingLoggerCapacityAndFIFO(t *testing.T) {
	logger := NewRingLogger(3)

	logger.Log(LogEntry{BindingName: "b1", Details: "First"})
	logger.Log(LogEntry{BindingName: "b1", Details: "Second"})
	logger.Log(LogEntry{BindingName: "b2", Details: "Third"})
	logger.Log(LogEntry{BindingName: "b1", Details: "Fourth"}) // Should evict "First"

	entries := logger.All()
	if len(entries) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(entries))
	}

	if entries[0].Details != "Second" {
		t.Errorf("expected first entry to be 'Second', got %q", entries[0].Details)
	}
	if entries[2].Details != "Fourth" {
		t.Errorf("expected last entry to be 'Fourth', got %q", entries[2].Details)
	}

	// Filter by binding name
	filtered := logger.Tail(10, "b2")
	if len(filtered) != 1 || filtered[0].Details != "Third" {
		t.Errorf("expected 1 entry for b2 ('Third'), got %v", filtered)
	}
}

func TestRingLoggerRedaction(t *testing.T) {
	logger := NewRingLogger(10)
	logger.Log(LogEntry{
		Details: "Connecting with token: ghp_12345secret and password=supersecretpass",
	})

	entries := logger.All()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if strings.Contains(entries[0].Details, "ghp_12345secret") {
		t.Errorf("token was not redacted: %s", entries[0].Details)
	}
	if strings.Contains(entries[0].Details, "supersecretpass") {
		t.Errorf("password was not redacted: %s", entries[0].Details)
	}
}
