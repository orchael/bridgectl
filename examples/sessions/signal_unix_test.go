//go:build !windows

package main

import (
	"os"
	"syscall"
	"testing"
)

func TestIsSigwinch(t *testing.T) {
	if !isSigwinch(syscall.SIGWINCH) {
		t.Error("isSigwinch(SIGWINCH) = false, want true")
	}
	if isSigwinch(os.Interrupt) {
		t.Error("isSigwinch(os.Interrupt) = true, want false")
	}
	if isSigwinch(syscall.SIGQUIT) {
		t.Error("isSigwinch(SIGQUIT) = true, want false")
	}
}
