//go:build linux || aix || solaris

package main

import "golang.org/x/sys/unix"

func discardTerminalInput(fd int) error {
	return unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH)
}
