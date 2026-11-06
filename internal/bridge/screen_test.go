package bridge

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
)

func newScreenSession(bufSize int, cols, rows uint32) (*Supervisor, *managedSession) {
	ms := &managedSession{
		buf:    NewByteBuffer(bufSize),
		screen: newLiveScreen(),
		info:   SessionInfo{SessionID: "s", Cols: cols, Rows: rows},
	}
	return &Supervisor{sessions: map[string]*managedSession{"s": ms}}, ms
}

func feedScreen(s *Supervisor, ms *managedSession, data string) {
	ms.mu.Lock()
	s.appendChunkLocked(ms, []byte(data), ChunkTypeOutput)
	ms.mu.Unlock()
}

func resizeScreen(s *Supervisor, ms *managedSession, cols, rows uint32) {
	ms.mu.Lock()
	ms.info.Cols, ms.info.Rows = cols, rows
	s.appendChunkLocked(ms, nil, ChunkTypeOutput)
	ms.mu.Unlock()
}

// assertSameScreen compares the visible grid, cursor and active buffer.
func assertSameScreen(t *testing.T, want, got *vt.Emulator) {
	t.Helper()
	if want.Render() != got.Render() {
		t.Fatalf("screen mismatch\nwant:\n%q\ngot:\n%q", want.Render(), got.Render())
	}
	if want.CursorPosition() != got.CursorPosition() {
		t.Fatalf("cursor=%v want %v", got.CursorPosition(), want.CursorPosition())
	}
	if want.IsAltScreen() != got.IsAltScreen() {
		t.Fatalf("alt screen=%v want %v", got.IsAltScreen(), want.IsAltScreen())
	}
}

// A dirty viewer (shell text, colors, main screen) must end up showing the
// exact application screen after writing only the gap checkpoint.
func dirtyViewer(cols, rows int) *vt.Emulator {
	v := vt.NewEmulator(cols, rows)
	_, _ = v.WriteString("$ bridgectl session attach --take-over 4dbc0cb4\r\n\x1b[31mleftover shell text")
	return v
}

func TestTerminalGapCarriesScreenCheckpoint(t *testing.T) {
	s, ms := newScreenSession(512, 80, 24)
	ref := vt.NewEmulator(80, 24)
	write := func(data string) {
		feedScreen(s, ms, data)
		_, _ = ref.WriteString(data)
	}
	write("\x1b[?1049h\x1b[?2004h\x1b[H\x1b[2J")
	write("\x1b[1;1H\x1b[1;38;5;208m Codex \x1b[0m")
	write("\x1b[10;3HAllow command? \x1b[7m(y)es\x1b[0m / \x1b[4m(n)o\x1b[0m")
	write("\x1b]8;;https://example.test\x1b\\link\x1b]8;;\x1b\\")
	// Evict the start of the stream from the 512-byte ring.
	for i := range 40 {
		write(fmt.Sprintf("\x1b[%d;40Hstatus %02d", 2+i%5, i))
	}
	write("\x1b[?25l\x1b[12;5H")

	w, err := s.TerminalWindow("s", "viewer", 1)
	if err != nil {
		t.Fatal(err)
	}
	last := ms.buf.LastSeq()
	if !w.Gap || w.NextSequence != last || w.More || len(w.Data) == 0 {
		t.Fatalf("gap window: gap=%v next=%d last=%d more=%v data=%d", w.Gap, w.NextSequence, last, w.More, len(w.Data))
	}
	viewer := dirtyViewer(80, 24)
	_, _ = viewer.Write(w.Data)
	assertSameScreen(t, ref, viewer)
	for _, want := range []string{"\x1b[?2004h", "\x1b[?25l", "\x1b[?1049h"} {
		if !bytes.Contains(w.Data, []byte(want)) {
			t.Fatalf("checkpoint missing mode %q", want)
		}
	}

	// Output after the checkpoint continues in order from NextSequence.
	write("\x1b[12;5Hy")
	write("\x1b[?25h\x1b[13;1Hrunning…")
	next, err := s.TerminalWindow("s", "viewer", w.NextSequence)
	if err != nil || next.Gap || next.FirstSequence != last+1 || next.NextSequence != last+2 {
		t.Fatalf("continued window: %+v %v", next, err)
	}
	_, _ = viewer.Write(next.Data)
	assertSameScreen(t, ref, viewer)
}

func TestTerminalGapCheckpointMainScreen(t *testing.T) {
	s, ms := newScreenSession(64, 40, 6)
	ref := vt.NewEmulator(40, 6)
	for i := range 20 {
		line := fmt.Sprintf("line %02d\r\n", i)
		feedScreen(s, ms, line)
		_, _ = ref.WriteString(line)
	}
	w, err := s.TerminalWindow("s", "viewer", 0)
	if err != nil || !w.Gap || len(w.Data) == 0 || w.NextSequence != ms.buf.LastSeq() {
		t.Fatalf("%+v %v", w, err)
	}
	viewer := vt.NewEmulator(40, 6)
	_, _ = viewer.WriteString("\x1b[?1049hstale alternate screen")
	_, _ = viewer.Write(w.Data)
	assertSameScreen(t, ref, viewer)
}

func TestTerminalGapCheckpointFollowsResize(t *testing.T) {
	s, ms := newScreenSession(64, 80, 24)
	ref := vt.NewEmulator(80, 24)
	feedScreen(s, ms, "\x1b[?1049h\x1b[24;70Hbottom-right")
	_, _ = ref.WriteString("\x1b[?1049h\x1b[24;70Hbottom-right")
	resizeScreen(s, ms, 100, 30)
	ref.Resize(100, 30)
	redraw := "\x1b[H\x1b[2J\x1b[30;90Hresized" + string(bytes.Repeat([]byte("."), 80))
	feedScreen(s, ms, redraw)
	_, _ = ref.WriteString(redraw)

	w, err := s.TerminalWindow("s", "viewer", 0)
	if err != nil || !w.Gap || w.Cols != 100 || w.Rows != 30 || len(w.Data) == 0 {
		t.Fatalf("%+v %v", w, err)
	}
	viewer := dirtyViewer(100, 30)
	_, _ = viewer.Write(w.Data)
	assertSameScreen(t, ref, viewer)

	// Geometry outside the emulated range disables checkpoints for good.
	resizeScreen(s, ms, maxScreenCols+1, 30)
	resizeScreen(s, ms, 100, 30)
	feedScreen(s, ms, "more")
	w, _ = s.TerminalWindow("s", "viewer", 0)
	if !w.Gap || len(w.Data) != 0 {
		t.Fatalf("stale checkpoint after unsupported geometry: gap=%v data=%d", w.Gap, len(w.Data))
	}
}

func TestTerminalGapCheckpointSizeLimitFallsBackToNoData(t *testing.T) {
	s, ms := newScreenSession(1024, 300, 100)
	var b bytes.Buffer
	for i := range 300 * 100 {
		fmt.Fprintf(&b, "\x1b[38;5;%dm%c", i%256, 'a'+i%26)
	}
	feedScreen(s, ms, b.String())
	feedScreen(s, ms, "x")
	w, err := s.TerminalWindow("s", "viewer", 0)
	if err != nil || !w.Gap || len(w.Data) != 0 || w.NextSequence != ms.buf.LastSeq() {
		t.Fatalf("oversized checkpoint: gap=%v data=%d next=%d err=%v", w.Gap, len(w.Data), w.NextSequence, err)
	}
}

func TestTerminalGapCheckpointSurvivesSessionEnd(t *testing.T) {
	s, ms := newScreenSession(64, 40, 6)
	ref := vt.NewEmulator(40, 6)
	for i := range 10 {
		line := fmt.Sprintf("\x1b[%d;1Hdone %02d", i%6+1, i)
		feedScreen(s, ms, line)
		_, _ = ref.WriteString(line)
	}
	s.closeLive(ms)
	if ms.screen.emu != nil {
		t.Fatal("emulator retained after output ended")
	}
	feedScreen(s, ms, "ignored after close")
	w, err := s.TerminalWindow("s", "viewer", 0)
	if err != nil || !w.Gap || !w.Ended || len(w.Data) == 0 {
		t.Fatalf("%+v %v", w, err)
	}
	viewer := vt.NewEmulator(40, 6)
	_, _ = viewer.Write(w.Data)
	assertSameScreen(t, ref, viewer)
}

// Device queries are answered into a pipe nobody reads; they must not block
// the PTY reader that holds the session lock.
func TestLiveScreenIgnoresDeviceQueries(t *testing.T) {
	s, ms := newScreenSession(4096, 80, 24)
	done := make(chan struct{})
	go func() {
		feedScreen(s, ms, "\x1b[c\x1b[>c\x1b[5n\x1b[6n\x1b[?6n\x1b[?2004$p\x1b]11;?\x07\x1b[?2048h")
		resizeScreen(s, ms, 90, 30)
		feedScreen(s, ms, "ok")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("device query blocked the emulator")
	}
	ms.mu.Lock()
	cp := ms.screen.checkpoint(ms.buf.LastSeq())
	ms.mu.Unlock()
	if !bytes.Contains(cp, []byte("ok")) {
		t.Fatalf("checkpoint=%q", cp)
	}
}

func TestLiveScreenCheckpointRequiresMatchingSequence(t *testing.T) {
	var nilScreen *liveScreen
	if nilScreen.checkpoint(1) != nil {
		t.Fatal("nil screen produced a checkpoint")
	}
	nilScreen.freeze()
	l := newLiveScreen()
	if l.checkpoint(0) != nil {
		t.Fatal("empty screen produced a checkpoint")
	}
	l.apply(OutputChunk{Seq: 1, Cols: 10, Rows: 2, Payload: []byte("a")})
	if l.checkpoint(2) != nil || l.checkpoint(1) == nil {
		t.Fatal("checkpoint must match the applied sequence")
	}
	l.apply(OutputChunk{Seq: 3, Cols: 10, Rows: 2, Payload: []byte("b")})
	if l.checkpoint(3) != nil || l.checkpoint(1) != nil {
		t.Fatal("skipped sequence must disable the screen")
	}
	l.freeze()
	if l.checkpoint(1) != nil {
		t.Fatal("disabled screen produced a checkpoint after freeze")
	}
}

func TestTerminalOversizedChunkServesCheckpoint(t *testing.T) {
	s, ms := newScreenSession(200000, 80, 24)
	feedScreen(s, ms, "\x1b[H"+string(bytes.Repeat([]byte("z"), 70000)))
	w, err := s.TerminalWindow("s", "viewer", 0)
	if err != nil || !w.Gap || len(w.Data) == 0 || w.NextSequence != 1 {
		t.Fatalf("oversized chunk: %+v %v", w, err)
	}
}

func screenBenchFrame() []byte {
	var frame bytes.Buffer
	for y := range 40 {
		fmt.Fprintf(&frame, "\x1b[%d;1H\x1b[38;5;%dm%s\x1b[0m", y+1, y, bytes.Repeat([]byte("| codex output "), 8))
	}
	return frame.Bytes()
}

func BenchmarkLiveScreenApply(b *testing.B) {
	payload := screenBenchFrame()
	l := newLiveScreen()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := range b.N {
		l.apply(OutputChunk{Seq: uint64(i + 1), Cols: 120, Rows: 40, Payload: payload})
	}
}

func BenchmarkLiveScreenCheckpoint(b *testing.B) {
	l := newLiveScreen()
	l.apply(OutputChunk{Seq: 1, Cols: 120, Rows: 40, Payload: screenBenchFrame()})
	b.ResetTimer()
	for range b.N {
		_ = l.checkpoint(1)
	}
}

func TestAttachCarriesCheckpointAtLastSeq(t *testing.T) {
	s, ms := newScreenSession(64, 40, 6)
	ref := vt.NewEmulator(40, 6)
	for i := range 12 {
		line := fmt.Sprintf("row %02d\r\n", i)
		feedScreen(s, ms, line)
		_, _ = ref.WriteString(line)
	}
	state, err := s.Attach("s", "observer", 0, AttachRoleObserver)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastSeq != 12 || len(state.Checkpoint) == 0 {
		t.Fatalf("last=%d checkpoint=%d", state.LastSeq, len(state.Checkpoint))
	}
	viewer := dirtyViewer(40, 6)
	_, _ = viewer.Write(state.Checkpoint)
	assertSameScreen(t, ref, viewer)
}
