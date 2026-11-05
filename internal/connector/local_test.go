package connector

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

// The command of a local connector is killed when the context is done.
func TestLocalConnectorStopsACommandOnAbort(t *testing.T) {
	sleep := "sleep 30"
	if runtime.GOOS == "windows" {
		sleep = "Start-Sleep -Seconds 30"
	}
	ctx, abort := context.WithCancel(t.Context())
	conn := NewLocalConnector(ctx)
	go func() {
		time.Sleep(200 * time.Millisecond)
		abort()
	}()

	start := time.Now()
	_, err := conn.ExecuteCommand(sleep)
	if err == nil {
		t.Fatal("the command finished without an error")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatalf("the command ran for %v after the abort", time.Since(start))
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("ctx.Err() = %v", ctx.Err())
	}
}

func TestLocalConnectorRunsACommand(t *testing.T) {
	out, err := NewLocalConnector(context.Background()).ExecuteCommand("echo hello")
	if err != nil {
		t.Fatalf("%v (%s)", err, out)
	}
}
