//go:build windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func setupSignals(ch chan os.Signal) {
	// Windows has no SIGWINCH; only listen for termination.
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
}

func isSigwinch(_ os.Signal) bool {
	return false
}
