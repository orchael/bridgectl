//go:build windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func setupSignals(ch chan os.Signal) {
	// Windows has no SIGWINCH; SIGQUIT/SIGTERM are notional but still deliverable
	// via os.Interrupt/os.Kill semantics, so they're kept for symmetry with Unix.
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
}

func isSigwinch(_ os.Signal) bool {
	return false
}
