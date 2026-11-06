package bridge

import (
	"bytes"
	"io"
	"strconv"
	"sync"

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
	// pending is input the emulator's parser holds across a chunk boundary.
	pending pendingInput
	// frozen holds the final checkpoint after the session output ends, so the
	// emulator can be released while late viewers still get the last screen.
	frozen []byte
	closed bool
	// final renders frozen once, off the session lock (see freeze).
	final sync.Once
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
		l.pending.feed(chunk.Payload)
	}
	l.seq = chunk.Seq
}

// freeze marks the screen final. Rendering it is deferred to a goroutine so
// that ending a session (including a forced shutdown with a short deadline)
// never waits for a full-screen render while holding the session lock. No
// output is applied after freeze, so the emulator is immutable from here on.
func (l *liveScreen) freeze() {
	if l == nil || l.closed {
		return
	}
	l.closed = true
	go l.finalize()
}

// finalize renders the final checkpoint once and releases the emulator.
// Callers that need the result before the goroutine finishes block on it.
func (l *liveScreen) finalize() {
	l.final.Do(func() {
		if !l.disabled && l.emu != nil {
			l.frozen = l.render()
		}
		l.emu = nil
	})
}

// checkpoint returns self-contained ANSI that redraws the screen as of seq, or
// nil when the screen is unknown, out of date, or exceeds the wire limit.
func (l *liveScreen) checkpoint(seq uint64) []byte {
	if l == nil || l.disabled || seq == 0 || l.seq != seq {
		return nil
	}
	var data []byte
	if l.closed {
		l.finalize()
		data = l.frozen
	} else if l.emu != nil {
		data = l.render()
	}
	if len(data) == 0 || len(data) > maxTerminalCheckpointBytes {
		return nil
	}
	return bytes.Clone(data)
}

// render draws the whole grid on a viewer in any prior state so that the
// next chunk continues exactly as on an uninterrupted terminal: soft reset,
// the main buffer and, when the app is in the alternate screen, the
// alternate buffer entered the same way (1049 or 1047), each with every row
// with SGR attributes and hyperlinks, its cursor, saved cursor and margins,
// the saved cursor, scrolling margins, input/render modes, the cursor with
// its pending-wrap flag, the active pen, and finally any escape sequence or
// UTF-8 character the last chunk left incomplete. It returns nil (no
// checkpoint) for state it cannot reproduce faithfully: unreadable emulator
// internals, custom tab stops, left/right margins, an unknown character set
// or pending single shift, or an incomplete sequence that is overlong or
// already executed a control.
func (l *liveScreen) render() []byte {
	e := l.emu
	width, height := e.Width(), e.Height()
	st, ok := readVTState(e)
	if !ok || !st.defaultTabs || l.modes[ansi.ModeLeftRightMargin] || st.scroll.Min.X != 0 || st.scroll.Max.X != width {
		return nil
	}
	charsets, ok := st.charsetSequence()
	if !ok {
		return nil
	}
	pending, ok := l.pending.prefix()
	if !ok {
		return nil
	}
	var b bytes.Buffer
	// DECSTR for real terminals, plus explicit resets of what it covers so
	// emulators without DECSTR start from the same state: SGR, hyperlink,
	// G0-G3 ASCII with G0 shifted in, origin mode off.
	b.WriteString("\x1b[!p\x1b[0m\x1b]8;;\x1b\\\x1b(B\x1b)B\x1b*B\x1b+B\x0f\x1b[?6l")
	// Start on the viewer's main buffer with full-screen margins and no
	// autowrap while drawing, so a full last column cannot scroll.
	b.WriteString("\x1b[?1049l\x1b[r\x1b[?7l\x1b[H\x1b[2J")
	if e.IsAltScreen() {
		// The main buffer is still there underneath and reappears when the
		// app leaves the alternate screen, so restore it first, then enter
		// the alternate screen the way the app did.
		var enter string
		switch {
		case l.modes[ansi.ModeAltScreenSaveCursor]:
			// 1049 saves the main cursor on entry (DECSC), replacing the
			// saved cursor; the live main buffer's saved cursor equals it.
			enter = ansi.SetMode(ansi.ModeAltScreenSaveCursor)
		case l.modes[ansi.ModeAltScreen]:
			enter = ansi.SetMode(ansi.ModeAltScreen)
		default:
			return nil
		}
		m := st.main
		if m.scroll.Min.X != 0 || m.scroll.Max.X != width {
			return nil
		}
		drawGrid(&b, m.screen.CellAt, width, height)
		b.WriteString("\x1b[0m")
		if !l.modes[ansi.ModeAltScreenSaveCursor] {
			writeSavedCursor(&b, m.saved)
		}
		writeMargins(&b, m.scroll, height)
		writeCUP(&b, m.cursor.Y, m.cursor.X)
		writePen(&b, m.cursor.Pen, m.cursor.Link)
		b.WriteString(enter)
		b.WriteString("\x1b[0m\x1b]8;;\x1b\\\x1b[r\x1b[H\x1b[2J")
	}
	drawGrid(&b, e.CellAt, width, height)
	b.WriteString("\x1b[0m")
	writeSavedCursor(&b, st.saved)
	writeMargins(&b, st.scroll, height)
	for _, mode := range checkpointModes {
		if l.modes[mode] {
			b.WriteString(ansi.SetMode(mode))
		} else {
			b.WriteString(ansi.ResetMode(mode))
		}
	}
	pos := st.cursor.Position
	cupY := pos.Y
	if l.modes[ansi.ModeOrigin] {
		b.WriteString(ansi.SetMode(ansi.ModeOrigin))
		cupY -= st.scroll.Min.Y
	}
	writeCUP(&b, cupY, pos.X)
	if st.phantom && l.modes[ansi.ModeAutoWrap] {
		// Pending wrap: redraw the last-column cell in place, so the viewer
		// sets its own wrap flag and the next character wraps.
		cell := e.CellAt(pos.X, pos.Y)
		if cell == nil || cell.Width != 1 {
			return nil
		}
		writePen(&b, cell.Style, cell.Link)
		if cell.Content == "" {
			b.WriteByte(' ')
		} else {
			b.WriteString(cell.Content)
		}
		b.WriteString("\x1b[0m\x1b]8;;\x1b\\")
	}
	// Character sets last: the grid and the wrap cell hold mapped glyphs and
	// are drawn in ASCII.
	b.WriteString(charsets)
	writePen(&b, st.cursor.Pen, st.cursor.Link)
	b.Write(pending)
	return b.Bytes()
}

// drawGrid writes every non-blank row of a buffer at absolute positions.
func drawGrid(b *bytes.Buffer, cellAt func(x, y int) *uv.Cell, width, height int) {
	line := make(uv.Line, width)
	for y := range height {
		for x := range line {
			if c := cellAt(x, y); c != nil {
				line[x] = *c
			} else {
				line[x] = uv.Cell{}
			}
		}
		row := line.Render()
		if row == "" {
			continue
		}
		writeCUP(b, y, 0)
		b.WriteString(row)
	}
}

// writeSavedCursor sets the DECSC slot: position and pen, restored by DECRC.
func writeSavedCursor(b *bytes.Buffer, saved vt.Cursor) {
	writeCUP(b, saved.Y, saved.X)
	writePen(b, saved.Pen, saved.Link)
	b.WriteString("\x1b7\x1b[0m\x1b]8;;\x1b\\")
}

// writeMargins sets DECSTBM when the region is not the full screen.
func writeMargins(b *bytes.Buffer, scroll uv.Rectangle, height int) {
	if scroll.Min.Y == 0 && scroll.Max.Y == height {
		return
	}
	b.WriteString("\x1b[")
	b.WriteString(strconv.Itoa(scroll.Min.Y + 1))
	b.WriteByte(';')
	b.WriteString(strconv.Itoa(scroll.Max.Y))
	b.WriteByte('r')
}

// writePen sets the SGR attributes and hyperlink of a pen from a reset state.
func writePen(b *bytes.Buffer, pen uv.Style, link uv.Link) {
	if !pen.IsZero() {
		b.WriteString(pen.String())
	}
	if link.URL != "" {
		b.WriteString(ansi.SetHyperlink(link.URL, link.Params))
	}
}

func writeCUP(b *bytes.Buffer, y, x int) {
	b.WriteString("\x1b[")
	b.WriteString(strconv.Itoa(y + 1))
	b.WriteByte(';')
	b.WriteString(strconv.Itoa(x + 1))
	b.WriteByte('H')
}
