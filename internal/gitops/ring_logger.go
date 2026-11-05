package gitops

import (
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

var (
	tokenRegex    = regexp.MustCompile(`(?i)(token|bearer|password|secret|key)[:= ]+([a-zA-Z0-9_\-\.!]+)`)
	authHeadRegex = regexp.MustCompile(`(?i)Authorization:\s*Basic\s+[a-zA-Z0-9+/=]+`)
	urlUserRegex  = regexp.MustCompile(`://[^/@\s]+@`)
)

// RingLogger is an in-memory, fixed-capacity circular buffer for execution and audit logs.
type RingLogger struct {
	mu       sync.RWMutex
	capacity int
	entries  []LogEntry
	cursor   int
	count    int
	seq      atomic.Uint64
}

// NewRingLogger creates a new RingLogger with the specified maximum capacity.
func NewRingLogger(capacity int) *RingLogger {
	if capacity <= 0 {
		capacity = 200
	}
	return &RingLogger{
		capacity: capacity,
		entries:  make([]LogEntry, capacity),
	}
}

// Log records a new log entry, redacting any sensitive token or password strings.
func (r *RingLogger) Log(entry LogEntry) {
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	if entry.ID == "" {
		id := r.seq.Add(1)
		entry.ID = fmt.Sprintf("log-%d", id)
	}

	// Redact sensitive patterns in details and messages
	entry.Details = redact(entry.Details)
	entry.Message = redact(entry.Message)
	if entry.Error != "" {
		entry.Error = redact(entry.Error)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	r.entries[r.cursor] = entry
	r.cursor = (r.cursor + 1) % r.capacity
	if r.count < r.capacity {
		r.count++
	}
}

// Tail returns the latest n entries, optionally filtered by binding name.
func (r *RingLogger) Tail(n int, bindingFilter string) []LogEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if n <= 0 || n > r.count {
		n = r.count
	}

	var all []LogEntry
	// Reconstruct chronological order
	start := (r.cursor - r.count + r.capacity) % r.capacity
	for i := range r.count {
		idx := (start + i) % r.capacity
		e := r.entries[idx]
		if bindingFilter == "" || e.BindingName == bindingFilter {
			all = append(all, e)
		}
	}

	if len(all) > n {
		return all[len(all)-n:]
	}
	return all
}

// All returns all recorded entries in chronological order.
func (r *RingLogger) All() []LogEntry {
	return r.Tail(r.capacity, "")
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	s = tokenRegex.ReplaceAllString(s, "${1}: [REDACTED]")
	s = authHeadRegex.ReplaceAllString(s, "Authorization: Basic [REDACTED]")
	s = urlUserRegex.ReplaceAllString(s, "://[REDACTED]@")
	return s
}
