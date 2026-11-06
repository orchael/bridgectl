package bridge

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
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
	// Leave state that a checkpoint must reset: margins, origin mode, a
	// graphics charset, a saved cursor and a hyperlink.
	_, _ = v.WriteString("\x1b[2;3r\x1b[?6h\x1b[4;4H\x1b7\x1b(0\x1b]8;;https://stale.test\x1b\\\x1b[1;44m")
	return v
}

// assertCheckpointContinues compares an uninterrupted terminal fed a+b with a
// dirty viewer fed checkpoint(a)+b, including pen, margins and wrap state.
func assertCheckpointContinues(t *testing.T, cols, rows int, a []string, b string) {
	t.Helper()
	s, ms := newScreenSession(1<<20, uint32(cols), uint32(rows))
	ref := vt.NewEmulator(cols, rows)
	for _, chunk := range a {
		feedScreen(s, ms, chunk)
		_, _ = ref.WriteString(chunk)
	}
	ms.mu.Lock()
	cp := ms.screen.checkpoint(ms.buf.LastSeq())
	ms.mu.Unlock()
	if len(cp) == 0 {
		t.Fatal("no checkpoint")
	}
	viewer := dirtyViewer(cols, rows)
	_, _ = viewer.Write(cp)
	_, _ = viewer.WriteString(b)
	_, _ = ref.WriteString(b)
	assertSameScreen(t, ref, viewer)
	want, ok1 := readVTState(ref)
	got, ok2 := readVTState(viewer)
	if !ok1 || !ok2 {
		t.Fatal("vt internals unreadable")
	}
	if !want.cursor.Pen.Equal(&got.cursor.Pen) || want.cursor.Link != got.cursor.Link {
		t.Fatalf("pen=%+v link=%+v want %+v %+v", got.cursor.Pen, got.cursor.Link, want.cursor.Pen, want.cursor.Link)
	}
	if want.scroll != got.scroll || want.phantom != got.phantom || want.gl != got.gl || want.gsingle != got.gsingle {
		t.Fatalf("state=%+v want %+v", got, want)
	}
	for i := range want.charsets {
		if reflect.ValueOf(want.charsets[i]).Pointer() != reflect.ValueOf(got.charsets[i]).Pointer() {
			t.Fatalf("G%d charset differs", i)
		}
	}
	if want.saved.Position != got.saved.Position || !want.saved.Pen.Equal(&got.saved.Pen) {
		t.Fatalf("saved cursor=%+v want %+v", got.saved, want.saved)
	}
}

func TestCheckpointContinuesLikeUninterruptedTerminal(t *testing.T) {
	full := strings.Repeat("a", 40)
	tests := []struct {
		name string
		a    []string
		b    string
	}{
		{"pending wrap at last column", []string{"\x1b[1;1H" + full}, "bc"},
		{"pending wrap in styled cell", []string{"\x1b[3;1H\x1b[35m" + full}, "\x1b[0mnext"},
		{"active colour then unstyled text", []string{"\x1b[2;2H\x1b[1;31;48;5;22mred"}, " still red"},
		{"active hyperlink", []string{"\x1b]8;;https://example.test\x1b\\li"}, "nk\x1b]8;;\x1b\\ plain"},
		{"scroll region then scrolling output", []string{"\x1b[1;1Htop\x1b[8;1Hbottom\x1b[2;5r\x1b[5;1Hregion"}, "\n1\n2\n3\n4\r\nX"},
		{"origin mode inside region", []string{"\x1b[3;6r\x1b[?6h\x1b[2;3H"}, "Z\x1b[1;1HQ\x1b[9;9HB"},
		{"saved cursor and pen", []string{"\x1b[5;5H\x1b[32m\x1b7\x1b[0m\x1b[1;1Hx"}, "\x1b8saved"},
		{"split CSI", []string{"hello\x1b["}, "2Jdone"},
		{"split CSI parameters", []string{"\x1b[4;", "1"}, "0Hmoved"},
		{"split UTF-8", []string{"caf\xc3"}, "\xa9 ok"},
		{"split 3-byte UTF-8", []string{"\x1b[2;1H\xe7", "\x95"}, "\x8c!"},
		{"split OSC title", []string{"before\x1b]0;tit"}, "le\x07after"},
		{"split ST", []string{"\x1b]8;;https://example.test\x1b"}, "\\link\x1b]8;;\x1b\\"},
		{"graphics charset in G0", []string{"\x1b(0lqqk"}, "x\x1b(Bplain"},
		{"graphics designated in G1, shifted out after", []string{"\x1b)0plain"}, "\x0eqqq\x0fq"},
		{"G1 shifted out before, shifted in after", []string{"\x1b)0\x0eqq"}, "q\x0fq"},
		{"UK set in G2 with locking shift", []string{"\x1b*A\x1bn#"}, "#\x1b(B\x0f#"},
		{"leave 1049 alternate screen", []string{"shell line 1\r\n$ codex\r\n\x1b[33m", "\x1b[?1049h\x1b[H\x1b[2J\x1b[1;1HCodex UI\x1b[5;3Hprompt"}, "\x1b[?1049l\x1b[0mback in shell\r\nmore"},
		{"leave 1047 with main margins and saved cursor", []string{"\x1b[2;6r\x1b[3;3H\x1b[32m\x1b7\x1b[0m\x1b[4;1Hmain text", "\x1b[?1047hALT"}, "\x1b[?1047l\x1b8after restore\n\n\n\nscroll"},
		{"alternate screen margins then leave", []string{"main\r\n", "\x1b[?1049h\x1b[3;5rinside"}, "\n\n\nX\x1b[?1049lY\n\n\n\n\n\n\n\n\nZ"},
		{"alt screen with modes", []string{"\x1b[?1049h\x1b[?2004h\x1b[?1h\x1b[?25l\x1b[H\x1b[2Jfull screen"}, "\x1b[2;1Hrow two"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertCheckpointContinues(t, 40, 10, tc.a, tc.b)
		})
	}
}

func TestCheckpointDeclinesStateItCannotReproduce(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		resume string // completes the state; a checkpoint is available again
	}{
		{"overlong unterminated OSC", []string{"\x1b]52;c;" + strings.Repeat("A", maxPendingInput+10)}, "\x07"},
		{"pending single shift", []string{"\x1b*0\x8e"}, "q"},
		{"left and right margins", []string{"\x1b[?69h\x1b[2;10s"}, "\x1b[1;40s\x1b[?69l"},
		{"custom tab stops", []string{"\x1b[3g\x1b[1;5H\x1bH"}, "\x1bc"},
		{"control executed inside a split CSI", []string{"top\x1b[\n"}, "1m"},
		{"control executed after ESC", []string{"\x1b\r"}, "7"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, ms := newScreenSession(1<<20, 40, 10)
			for _, chunk := range tc.chunks {
				feedScreen(s, ms, chunk)
			}
			ms.mu.Lock()
			cp := ms.screen.checkpoint(ms.buf.LastSeq())
			ms.mu.Unlock()
			if cp != nil {
				t.Fatalf("checkpoint emitted for unsupported state: %q", cp)
			}
			feedScreen(s, ms, tc.resume)
			ms.mu.Lock()
			cp = ms.screen.checkpoint(ms.buf.LastSeq())
			ms.mu.Unlock()
			if cp == nil {
				t.Fatal("checkpoint not restored once the state ended")
			}
		})
	}
}

func TestReadVTStateMatchesPinnedLibrary(t *testing.T) {
	e := vt.NewEmulator(10, 4)
	_, _ = e.WriteString("\x1b[2;3r\x1b[31m")
	st, ok := readVTState(e)
	if !ok {
		t.Fatal("charmbracelet/x/vt internals changed; update screen_vtstate.go")
	}
	if st.scroll.Min.Y != 1 || st.scroll.Max.Y != 3 || st.cursor.Pen.IsZero() {
		t.Fatalf("unexpected state %+v", st)
	}
	if _, ok := readVTState(nil); ok {
		t.Fatal("nil emulator accepted")
	}
}

func TestPendingInputTracker(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"plain", ""},
		{"x\x1b", "\x1b"},
		{"x\x1b[1;2", "\x1b[1;2"},
		{"\x1b[1;2m", ""},
		{"\x1b(", "\x1b("},
		{"\x1b(B", ""},
		{"\x1b]0;t\x07", ""},
		{"\x1b]0;t\x1b", "\x1b]0;t\x1b"},
		{"\x1b]0;t\x1b\\", ""},
		{"\x1bPq#0\x1b", "\x1bPq#0\x1b"},
		{"\x1b]0;t\x1b[1", "\x1b[1"},
		{"\x1b[1\x18", ""},
		{"\xe7\x95", "\xe7\x95"},
		{"\xe7\x95\x8c", ""},
		{"\xf0\x9f\x98", "\xf0\x9f\x98"},
		{"\xc3x", ""},
		{"\xc3\x1b[", "\x1b["},
	}
	for _, tc := range tests {
		var p pendingInput
		for i := range len(tc.in) {
			p.feed([]byte{tc.in[i]}) // byte-at-a-time chunking
		}
		got, ok := p.prefix()
		if !ok || string(got) != tc.want {
			t.Errorf("feed(%q) prefix=%q ok=%v want %q", tc.in, got, ok, tc.want)
		}
	}
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
