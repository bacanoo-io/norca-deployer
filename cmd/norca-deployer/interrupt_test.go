package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func waitDone(t *testing.T, ctx context.Context, what string) {
	t.Helper()
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("%s was not cancelled", what)
	}
}

func stillRunning(t *testing.T, ctx context.Context, what string) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatalf("%s was cancelled too early", what)
	case <-time.After(50 * time.Millisecond):
	}
}

// The first interrupt ends the gentle context only; the second ends the abort
// context too.
func TestInterruptContexts(t *testing.T) {
	sig := make(chan os.Signal, 2)
	var stderr bytes.Buffer
	gentle, abort, stop := interruptContexts(t.Context(), sig, &stderr)
	defer stop()

	stillRunning(t, gentle, "gentle")
	sig <- os.Interrupt
	waitDone(t, gentle, "gentle")
	stillRunning(t, abort, "abort")

	sig <- os.Interrupt
	waitDone(t, abort, "abort")

	out := stderr.String()
	if !strings.Contains(out, "finishing the step in progress") || !strings.Contains(out, "Aborting.") {
		t.Fatalf("stderr = %q", out)
	}
}

// stop releases the goroutine and the contexts when the command is done
// without an interrupt.
func TestInterruptContextsStop(t *testing.T) {
	var stderr bytes.Buffer
	gentle, abort, stop := interruptContexts(t.Context(), make(chan os.Signal), &stderr)
	stop()
	waitDone(t, gentle, "gentle")
	waitDone(t, abort, "abort")
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want nothing without an interrupt", stderr.String())
	}
}

func TestExitCodeOfAnInterruptedCommand(t *testing.T) {
	interrupted := fmt.Errorf("deployment failed: %w", fmt.Errorf("interrupted before the restart: %w", context.Canceled))
	for name, tt := range map[string]struct {
		err  error
		want int
	}{
		"interrupted": {interrupted, exitInterrupted},
		"usage":       {usageError{errors.New("bad flag")}, exitUsage},
		"failure":     {errors.New("boom"), exitError},
	} {
		if got := exitCode(tt.err); got != tt.want {
			t.Errorf("%s: exit code = %d, want %d", name, got, tt.want)
		}
	}
}
