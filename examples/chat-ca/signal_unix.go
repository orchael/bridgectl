//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"
)

func setupSignals(ch chan os.Signal) {
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGWINCH)
}

func isSigwinch(sig os.Signal) bool {
	return sig == syscall.SIGWINCH
}
