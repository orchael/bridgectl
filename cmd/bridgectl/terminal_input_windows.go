package main

import "golang.org/x/sys/windows"

func discardTerminalInput(fd int) error {
	return windows.FlushConsoleInputBuffer(windows.Handle(fd))
}
