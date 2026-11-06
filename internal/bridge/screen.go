package bridge

import (
	"bytes"
	"io"
	"strconv"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

const (
	// maxTerminalCheckpointBytes matches the terminal window data limit that
	// the Bridge control service forwards unchanged.
	maxTerminalCheckpointBytes = 65536
	// Larger grids are not emulated. Bridge web clamps resizes to 500x300.
	maxScreenCols = 500
	maxScreenRows = 300
)

// checkpointModes are DEC private modes that change how the viewer renders
// later output or what its keyboard sends. They are restored after drawing.
var checkpointModes = []ansi.DECMode{
	ansi.ModeCursorKeys,
	ansi.ModeAutoWrap,
	ansi.ModeNumericKeypad,
	ansi.ModeMouseX10,
	ansi.ModeMouseNormal,
	ansi.ModeMouseButtonEvent,
	ansi.ModeMouseAnyEvent,
	ansi.ModeMouseExtSgr,
	ansi.ModeFocusEvent,
	ansi.ModeBracketedPaste,
	ansi.ModeTextCursorEnable,
}

// liveScreen is the current screen of a PTY session: a VT emulator fed with
// exactly the output chunks retained in the session buffer, at the session's
// size. Unlike telemetry's capture it is not redacted, because a viewer may be
// the writer and must see the real screen. managedSession.mu serializes all
// access, the same lock that assigns output sequences, so the emulator state
// always corresponds to seq. Memory is bounded by the grid (no scrollback).
type liveScreen struct {
	emu      *vt.Emulator
	modes    map[ansi.DECMode]bool
	seq      uint64
	disabled bool
	// frozen holds the final checkpoint after the session output ends, so the
	// emulator can be released while late viewers still get the last screen.
	frozen []byte
	closed bool
}

func newLiveScreen() *liveScreen {
	return &liveScreen{}
}

func (l *liveScreen) ensure(cols, rows uint32) bool {
	if l == nil || l.disabled || l.closed {
		return false
	}
	if cols == 0 || rows == 0 || cols > maxScreenCols || rows > maxScreenRows {
		// The state can no longer be reproduced; never serve a stale screen.
		l.disabled = true
		l.emu = nil
		return false
	}
	if l.emu == nil {
		emu := vt.NewEmulator(int(cols), int(rows))
		// Nothing reads replies to device queries (DA, DSR, DECRQM, OSC color
		// queries). Closing the reply pipe makes those writes fail fast
		// instead of blocking the PTY reader on an unread pipe.
		pw, ok := emu.InputPipe().(*io.PipeWriter)
		if !ok {
			l.disabled = true
			return false
		}
		_ = pw.Close()
		emu.SetScrollbackSize(1)
		l.modes = map[ansi.DECMode]bool{ansi.ModeAutoWrap: true, ansi.ModeTextCursorEnable: true}
		emu.SetCallbacks(vt.Callbacks{
			EnableMode: func(m ansi.Mode) {
				if dec, ok := m.(ansi.DECMode); ok {
					l.modes[dec] = true
				}
			},
			DisableMode: func(m ansi.Mode) {
				if dec, ok := m.(ansi.DECMode); ok {
					l.modes[dec] = false
				}
			},
		})
		l.emu = emu
	} else if l.emu.Width() != int(cols) || l.emu.Height() != int(rows) {
		l.emu.Resize(int(cols), int(rows))
	}
	return true
}

// apply feeds one output chunk. The caller holds managedSession.mu.
func (l *liveScreen) apply(chunk OutputChunk) {
	if !l.ensure(chunk.Cols, chunk.Rows) {
		return
	}
	if chunk.Seq != l.seq+1 {
		// A skipped sequence would desynchronize the screen.
		l.disabled = true
		l.emu = nil
		return
	}
	if len(chunk.Payload) > 0 {
		_, _ = l.emu.Write(chunk.Payload)
	}
	l.seq = chunk.Seq
}

// freeze renders the final screen and releases the emulator.
func (l *liveScreen) freeze() {
	if l == nil || l.closed {
		return
	}
	if !l.disabled && l.emu != nil {
		l.frozen = l.render()
	}
	l.emu = nil
	l.closed = true
}

// checkpoint returns self-contained ANSI that redraws the screen as of seq, or
// nil when the screen is unknown, out of date, or exceeds the wire limit.
func (l *liveScreen) checkpoint(seq uint64) []byte {
	if l == nil || l.disabled || seq == 0 || l.seq != seq {
		return nil
	}
	var data []byte
	if l.closed {
		data = l.frozen
	} else if l.emu != nil {
		data = l.render()
	}
	if len(data) == 0 || len(data) > maxTerminalCheckpointBytes {
		return nil
	}
	return bytes.Clone(data)
}

// render draws the whole grid on a viewer in any prior state: soft reset,
// the active screen buffer, every row with SGR attributes and hyperlinks,
// input/render modes, then the cursor position and visibility.
func (l *liveScreen) render() []byte {
	e := l.emu
	var b bytes.Buffer
	b.WriteString("\x1b[!p\x1b[0m")
	if e.IsAltScreen() {
		b.WriteString("\x1b[?1049h")
	} else {
		b.WriteString("\x1b[?1049l")
	}
	// Disable autowrap while drawing so a full last column cannot scroll.
	b.WriteString("\x1b[?7l\x1b[H\x1b[2J")
	width, height := e.Width(), e.Height()
	line := make(uv.Line, width)
	for y := range height {
		for x := range line {
			if c := e.CellAt(x, y); c != nil {
				line[x] = *c
			} else {
				line[x] = uv.Cell{}
			}
		}
		row := line.Render()
		if row == "" {
			continue
		}
		writeCUP(&b, y, 0)
		b.WriteString(row)
	}
	b.WriteString("\x1b[0m")
	for _, mode := range checkpointModes {
		if l.modes[mode] {
			b.WriteString(ansi.SetMode(mode))
		} else {
			b.WriteString(ansi.ResetMode(mode))
		}
	}
	pos := e.CursorPosition()
	writeCUP(&b, pos.Y, pos.X)
	return b.Bytes()
}

func writeCUP(b *bytes.Buffer, y, x int) {
	b.WriteString("\x1b[")
	b.WriteString(strconv.Itoa(y + 1))
	b.WriteByte(';')
	b.WriteString(strconv.Itoa(x + 1))
	b.WriteByte('H')
}
