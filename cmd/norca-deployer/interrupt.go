package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
)

// interruptContexts turns the interrupts of a long command into two contexts:
//
//   - the first interrupt (Ctrl-C, SIGTERM) ends gentle: the command finishes
//     the step in progress, a node being restarted, and does not start the
//     next one;
//   - the second ends abort: what is in progress is cut at once, the
//     connections are closed.
//
// stop releases the signal handling and must be called when the command is
// done. sig delivers the signals (signal.Notify in production, a channel in
// the tests).
func interruptContexts(parent context.Context, sig <-chan os.Signal, stderr io.Writer) (gentle, abort context.Context, stop func()) {
	gentle, endGentle := context.WithCancel(parent)
	abort, endAbort := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		select {
		case <-sig:
		case <-done:
			return
		}
		fmt.Fprintln(stderr, "Interrupted: finishing the step in progress, then stopping. Press Ctrl-C again to abort at once.")
		endGentle()
		select {
		case <-sig:
			fmt.Fprintln(stderr, "Aborting.")
			endAbort()
		case <-done:
		}
	}()
	return gentle, abort, func() {
		close(done)
		endGentle()
		endAbort()
	}
}

// notifyInterrupts returns a channel of the interrupts of the process and the
// function that stops the delivery.
func notifyInterrupts() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	return ch, func() { signal.Stop(ch) }
}
