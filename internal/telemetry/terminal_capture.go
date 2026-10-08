package telemetry

import (
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// terminalCapture interprets cursor-addressed PTY output before turning it into
// a transcript. It has no input/output connection to the real terminal. Only
// layout controls reach the emulator; device queries, OSC and DCS are discarded.
// LiveCollector.mu serializes access. The remote grid bounds retained memory.
type terminalCapture struct {
	screen   *vt.Emulator
	previous []string
	styled   string
	snapshot *TerminalSnapshot
	pending  *Event
	timer    *time.Timer
	analyzer *Analyzer
}

func (t *terminalCapture) flush() {
	if t.timer != nil {
		t.timer.Stop()
		t.timer = nil
	}
	if t.pending != nil {
		t.analyzer.record(*t.pending)
		t.pending = nil
	}
}
func (t *terminalCapture) close() { t.flush(); _ = t.screen.Close() }

// Coalesce redraw bursts, retaining the final styled screen and the redacted
// text for search. The callback shares the collector lock with output/close.
func (c *LiveCollector) recordTerminal(session Session, event Event) {
	capture := c.terminals[sessionKey(session)]
	if capture.pending != nil {
		if event.Terminal != nil && capture.pending.Terminal != nil {
			snapshot := *event.Terminal
			snapshot.Scrollback = capture.pending.Terminal.Scrollback + snapshot.Scrollback
			event.Terminal = &snapshot
		}
		event.Text = capture.pending.Text + event.Text
		event.ByteCount += capture.pending.ByteCount
		event.Redactions += capture.pending.Redactions
		if event.Terminal == nil {
			event.Terminal = capture.pending.Terminal
		}
	}
	capture.pending = &event
	if capture.timer == nil {
		capture.timer = time.AfterFunc(100*time.Millisecond, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.terminals[sessionKey(session)] == capture {
				capture.flush()
			}
		})
	}
	if len(event.Text) > maxInteractionBufferSize || (event.Terminal != nil && len(event.Terminal.Scrollback) > maxInteractionBufferSize) {
		capture.flush()
	}
}

func (c *LiveCollector) renderedOutput(session Session, data []byte) (string, int) {
	key := sessionKey(session)
	capture := c.terminals[key]
	cols, rows := min(int(session.Cols), 500), min(int(session.Rows), 300)
	if capture == nil {
		capture = &terminalCapture{screen: vt.NewEmulator(cols, rows), analyzer: c.analyzer}
		capture.screen.SetScrollbackSize(4096)
		c.terminals[key] = capture
	} else if capture.screen.Width() != cols || capture.screen.Height() != rows {
		capture.screen.Resize(cols, rows)
		capture.previous = nil
		capture.styled = "<resized>"
	}
	// Redact the contiguous source text first. Masking keeps cell widths and ANSI
	// positions intact, unlike inserting a variable-length redaction label.
	safe, redactions := maskTerminalSecrets(string(data), false)
	_, _ = capture.screen.WriteString(terminalLayoutOnly(safe))
	// Cursor edits can assemble a credential from separate records. Redact the
	// resulting screen too, including credentials wrapping across screen rows.
	screen, count := maskTerminalSecrets(capture.screen.String(), true)
	redactions += count
	lines := strings.Split(screen, "\n")
	// Keep newly discovered secrets masked in emulator state as well. Removing
	// the key on a later redraw must not make a previously masked value reappear.
	if count > 0 {
		for y, line := range lines {
			for offset, char := range line {
				if char == '*' {
					capture.screen.SetCell(ansi.StringWidth(line[:offset]), y, &uv.Cell{Content: "*", Width: 1})
				}
			}
		}
	}
	var changed strings.Builder
	for i, line := range lines {
		if strings.TrimSpace(line) != "" && (i >= len(capture.previous) || line != capture.previous[i]) {
			changed.WriteString(line)
			changed.WriteByte('\n')
		}
	}
	capture.previous = lines
	styled := capture.screen.Render()
	// Drain native scrollback into the revision before another redraw can evict
	// it. Redact across the scrollback/screen boundary, preserving SGR offsets.
	scrolled := capture.screen.Scrollback().Lines()
	history := make([]string, 0, len(scrolled)+1)
	for _, line := range scrolled {
		history = append(history, line.Render())
	}
	history = append(history, styled)
	safeStyled, extra := maskTerminalSecrets(strings.Join(history, "\n"), true)
	redactions += extra
	split := strings.SplitN(safeStyled, "\n", len(scrolled)+1)
	scrollback := ""
	if len(scrolled) > 0 {
		scrollback = strings.Join(split[:len(scrolled)], "\n") + "\n"
	}
	styled = split[len(split)-1]
	capture.screen.ClearScrollback()
	capture.snapshot = nil
	if styled != capture.styled || scrollback != "" {
		capture.styled = styled
		capture.snapshot = &TerminalSnapshot{Version: 1, Cols: cols, Rows: rows, ANSI: styled, Scrollback: scrollback}
	}
	return changed.String(), redactions
}

// maskTerminalSecrets applies the same credential patterns as DefaultRedactor,
// mapping matches back onto the source so controls cannot split a secret. The
// optional newline-free pass protects credentials wrapped by the terminal.
func maskTerminalSecrets(raw string, joinLines bool) (string, int) {
	bytes := []byte(raw)
	masked := make([]bool, len(raw))
	matches := 0
	passes := []bool{false}
	if joinLines {
		passes = append(passes, true)
	}
	// Union both views before replacing anything: redacting the first row must
	// not hide the credential key from the pass that detects its wrapped suffix.
	for _, joined := range passes {
		var plain strings.Builder
		offsets := make([]int, 0, len(raw))
		for i := 0; i < len(raw); {
			if raw[i] == '\x1b' {
				end, complete := ansiSequenceEndFrom(bytes, i, i+2)
				if !complete {
					break
				}
				i = end
				continue
			}
			if joined && (raw[i] == '\r' || raw[i] == '\n') {
				i++
				continue
			}
			plain.WriteByte(raw[i])
			offsets = append(offsets, i)
			i++
		}
		for _, pattern := range []*regexp.Regexp{privateKeyRE, bearerRE, credentialValueRE, quotedSecretRE, secretRE} {
			for _, match := range pattern.FindAllStringIndex(plain.String(), -1) {
				matches++
				for _, offset := range offsets[match[0]:match[1]] {
					masked[offset] = true
				}
			}
		}
	}
	if matches == 0 {
		return raw, 0
	}
	var result strings.Builder
	for i, char := range raw {
		if masked[i] && char != '\r' && char != '\n' && char != '\t' {
			result.WriteString(strings.Repeat("*", max(0, ansi.StringWidth(string(char)))))
		} else {
			result.WriteRune(char)
		}
	}
	return result.String(), matches
}

func terminalLayoutOnly(raw string) string {
	var result strings.Builder
	bytes := []byte(raw)
	for i := 0; i < len(bytes); {
		if bytes[i] == '\x1b' {
			end, complete := ansiSequenceEndFrom(bytes, i, i+2)
			if !complete {
				break
			}
			seq := raw[i:end]
			if layoutControl(seq) {
				result.WriteString(seq)
			}
			i = end
			continue
		}
		char, size := utf8.DecodeRune(bytes[i:])
		if char >= 32 && (char < 127 || char > 159) || char == '\r' || char == '\n' || char == '\t' || char == '\b' {
			result.WriteRune(char)
		}
		i += size
	}
	return result.String()
}

func layoutControl(seq string) bool {
	if len(seq) == 2 {
		return strings.ContainsRune("78DEM", rune(seq[1]))
	}
	if len(seq) < 3 || seq[1] != '[' {
		return false
	}
	command := seq[len(seq)-1]
	params := seq[2 : len(seq)-1]
	if command == 'h' || command == 'l' {
		// Preserve wrapping and alternate-screen switches, never device reporting.
		switch params {
		case "?7", "?47", "?1047", "?1049":
			return true
		}
		return false
	}
	if !strings.ContainsRune("ABCDEFGHJKLMPSTX@`adefrsum", rune(command)) {
		return false
	}
	for _, char := range params {
		if char != ';' && (command != 'm' || char != ':') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}
