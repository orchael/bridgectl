package main

import (
	"fmt"
	"io"
	"log/slog"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
)

// attachScreenReset puts the local terminal in a known state before session
// output is drawn: soft reset (modes, margins, SGR), then clear and home. The
// server's gap checkpoint switches to the session's screen buffer itself.
const attachScreenReset = "\x1b[!p\x1b[0m\x1b[H\x1b[2J"

const replayGapNotice = "[bridgectl] Earlier session output was no longer available, so the current screen was redrawn."

// attachDisplay owns local terminal resets for the attach and run paths. Text
// must not be printed inside the application's screen, so on a terminal the
// gap notice is deferred until the caller has restored the terminal.
type attachDisplay struct {
	out  io.Writer // session output (stdout)
	err  io.Writer // notices (stderr)
	tty  bool
	gaps int
}

func newAttachDisplay(out, errOut io.Writer, tty bool) *attachDisplay {
	return &attachDisplay{out: out, err: errOut, tty: tty}
}

// reset clears the local terminal so leftover shell text cannot mix with the
// redrawn application. It is a no-op when output is not a terminal.
func (d *attachDisplay) reset() error {
	if !d.tty {
		return nil
	}
	_, err := io.WriteString(d.out, attachScreenReset)
	return err
}

// replayGap handles REPLAY_GAP: the bytes that follow are either the server's
// screen checkpoint or live output, and neither can be drawn over stale cells.
func (d *attachDisplay) replayGap(ev *bridgev1.AttachSessionEvent) error {
	slog.Debug("attach replay gap", "session_id", ev.GetSessionId(), "oldest_seq", ev.GetOldestSeq(), "last_seq", ev.GetLastSeq())
	d.gaps++
	if !d.tty {
		_, err := fmt.Fprintln(d.err, replayGapNotice)
		return err
	}
	return d.reset()
}

// printDeferredNotices reports what happened while the session owned the
// screen. Call it after the terminal has been restored.
func (d *attachDisplay) printDeferredNotices() {
	if d.tty && d.gaps > 0 {
		_, _ = fmt.Fprintf(d.err, "\r\n%s\r\n", replayGapNotice)
	}
}
