package telemetry

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestTerminalCaptureLayoutAndRedraw(t *testing.T) {
	for _, tc := range []struct{ name, input, want string }{
		{"cursor spacing", "\x1b[HHello\x1b[1Cworld\x1b[2;1HSecond line", "Hello world\nSecond line\n"},
		{"overwrite and erase", "\x1b[Hwrong text\x1b[Hright\x1b[K", "right\n"},
		{"wide characters", "\x1b[H界\x1b[1Cok", "界 ok\n"},
		{"queries and OSC ignored", "\x1b[6n\x1b]52;c;clipboard-secret\x07\x1b[Hvisible", "visible\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := NewLiveCollector(&memorySink{}, 32, true, nil, EventProviderOutput)
			t.Cleanup(func() {
				if err := c.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			session := Session{SessionID: tc.name, Cols: 40, Rows: 6}
			got, _ := c.renderedOutput(session, []byte(tc.input))
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
			got, _ = c.renderedOutput(session, []byte(tc.input))
			if got != "" {
				t.Fatalf("unchanged screen duplicated: %q", got)
			}
		})
	}
}

func TestTerminalCaptureRedactsBeforeLayoutAndAfterCursorEdits(t *testing.T) {
	for _, secret := range []string{"token=top-secret", "Bearer top-secret", `api_key="top secret"`, "sk-abcdefghijklmnopqrst"} {
		t.Run(secret, func(t *testing.T) {
			c := NewLiveCollector(&memorySink{}, 32, true, nil, EventProviderOutput)
			t.Cleanup(func() {
				if err := c.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			session := Session{SessionID: "redaction", Cols: 12, Rows: 8}
			got, count := c.renderedOutput(session, []byte("\x1b[H"+secret+" \x1b[4;1Hsafe"))
			if count == 0 || strings.Contains(got, "top") || strings.Contains(got, "secret") || strings.Contains(got, "abcdef") || !strings.Contains(got, "safe") {
				t.Fatalf("unsafe output %q", got)
			}
		})
	}
	c := NewLiveCollector(&memorySink{}, 32, true, nil, EventProviderOutput)
	t.Cleanup(func() {
		if err := c.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	session := Session{SessionID: "edits", Cols: 40, Rows: 4}
	_, _ = c.renderedOutput(session, []byte("\x1b[Htoken="))
	got, _ := c.renderedOutput(session, []byte("\x1b[1;7Htop-secret"))
	if strings.Contains(got, "top-secret") {
		t.Fatalf("cursor edits exposed credential: %q", got)
	}
	got, _ = c.renderedOutput(session, []byte("\x1b[H      "))
	if strings.Contains(got, "top-secret") {
		t.Fatalf("erasing key exposed masked value: %q", got)
	}
}

func TestTerminalCaptureCollectorSplitFramesAndIsolation(t *testing.T) {
	sink := &memorySink{}
	c := NewLiveCollector(sink, 128, true, nil, EventProviderOutput)
	session := Session{SessionID: "pty", Cols: 40, Rows: 6}
	frame := "\x1b[?2026h\x1b[HHello\x1b[1Cworld\x1b[2;1Hsecond line end safe words \x1b[?2026l"
	// Split every byte, including escapes and the synchronized-update delimiter.
	for _, value := range []byte(frame) {
		c.ObserveOutputChunk(session, []byte{value})
	}
	c.SessionEnded(session)
	if len(c.terminals) != 0 {
		t.Fatal("ended session retained terminal")
	}
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	for _, event := range capturedEvents(sink.snapshot()) {
		if event.Kind == EventProviderOutput {
			got.WriteString(event.Text)
		}
	}
	if !strings.Contains(got.String(), "Hello world\nsecond line") {
		t.Fatalf("lost terminal layout: %q", got.String())
	}
}

func TestTerminalCaptureResizeAndSessionIsolation(t *testing.T) {
	c := NewLiveCollector(&memorySink{}, 32, true, nil, EventProviderOutput)
	t.Cleanup(func() {
		if err := c.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	a := Session{SessionID: "a", Cols: 20, Rows: 4}
	b := Session{SessionID: "b", Cols: 20, Rows: 4}
	_, _ = c.renderedOutput(a, []byte("\x1b[HA-only"))
	got, _ := c.renderedOutput(b, []byte("\x1b[HB-only"))
	if strings.Contains(got, "A-only") {
		t.Fatal("sessions shared a screen")
	}
	a.Cols = 40
	got, _ = c.renderedOutput(a, []byte("\x1b[Hresized\x1b[K"))
	if !strings.Contains(got, "resized") || c.terminals[sessionKey(a)].screen.Width() != 40 {
		t.Fatal("resize not applied")
	}
	c.SessionStarted(a)
	if c.terminals[sessionKey(a)] != nil {
		t.Fatal("reused session retained old screen")
	}
}

func TestTerminalCaptureWrappedCursorSecret(t *testing.T) {
	c := NewLiveCollector(&memorySink{}, 32, true, nil, EventProviderOutput)
	t.Cleanup(func() {
		if err := c.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	session := Session{SessionID: "wrapped", Cols: 12, Rows: 4}
	_, _ = c.renderedOutput(session, []byte("\x1b[Htoken="))
	got, _ := c.renderedOutput(session, []byte("\x1b[1;7Habcdefghijkl"))
	if strings.ContainsAny(got, "abcdefghijkl") {
		t.Fatalf("wrapped credential exposed: %q", got)
	}
}

func TestTerminalCaptureRedactsScrollbackScreenBoundary(t *testing.T) {
	c := NewLiveCollector(&memorySink{}, 32, true, nil, EventProviderOutput)
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	session := Session{SessionID: "scrollback-secret", Cols: 24, Rows: 2}
	_, _ = c.renderedOutput(session, []byte("\x1b[Htoken="))
	text, _ := c.renderedOutput(session, []byte("\x1b[2;1Hprivate-value\r\n"))
	if strings.Contains(text, "private-value") {
		t.Fatalf("credential reached searchable text: %q", text)
	}
	capture := c.terminals[sessionKey(session)]
	if capture.snapshot == nil || strings.Contains(capture.snapshot.ANSI, "private-value") {
		t.Fatalf("credential reached styled checkpoint: %+v", capture.snapshot)
	}
	text, _ = c.renderedOutput(session, []byte("\x1b[2;1Hsafe"))
	if strings.Contains(text, "private-value") || strings.Contains(capture.snapshot.ANSI, "private-value") {
		t.Fatalf("credential reappeared after redraw: text=%q checkpoint=%+v", text, capture.snapshot)
	}
}

func TestTerminalCaptureSplitSecretFrames(t *testing.T) {
	secret := "token=top-secret"
	for split := 1; split < len(secret); split++ {
		sink := &memorySink{}
		c := NewLiveCollector(sink, 128, true, nil, EventProviderOutput)
		session := Session{SessionID: "split-pty", Cols: 80, Rows: 6}
		c.ObserveOutputChunk(session, []byte("\x1b[Hsafe prefix "+secret[:split]+"\x1b[?2026l"))
		c.ObserveOutputChunk(session, []byte("\x1b[?2026h"+secret[split:]+" trailing safe words \x1b[?2026l"))
		c.SessionEnded(session)
		if err := c.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		var text strings.Builder
		for _, e := range capturedEvents(sink.snapshot()) {
			text.WriteString(e.Text)
		}
		if strings.Contains(text.String(), "top-secret") || !strings.Contains(text.String(), "*") {
			t.Fatalf("split %d unsafe: %q", split, text.String())
		}
	}
}

func TestStyledCheckpointRedactionResizeAndFinalFlush(t *testing.T) {
	sink := &memorySink{}
	c := NewLiveCollector(sink, 128, true, nil, EventProviderOutput)
	session := Session{SessionID: "styled", Cols: 40, Rows: 8}
	c.ObserveOutputChunk(session, []byte("\x1b[?2026h\x1b[H\x1b[31;1mRED 界\x1b[0m\x1b[2;1Htoken=private-value \x1b[?2026l"))
	session.Cols = 60
	c.ObserveOutputChunk(session, []byte("\x1b[?2026h\x1b[3;1H\x1b[34mBLUE\x1b[0m settled words \x1b[?2026l"))
	c.SessionEnded(session)
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var last *TerminalSnapshot
	for _, event := range sink.snapshot() {
		if event.Terminal != nil {
			last = event.Terminal
		}
	}
	if last == nil {
		t.Fatal("missing checkpoint")
		return
	}
	if last.Version != 1 || last.Cols != 60 || last.Rows != 8 {
		t.Fatalf("geometry: %+v", last)
	}
	if !strings.Contains(last.ANSI, "\x1b[") || !strings.Contains(last.ANSI, "BLUE") || strings.Contains(last.ANSI, "private-value") {
		t.Fatalf("unsafe or unstyled checkpoint: %q", last.ANSI)
	}
}

func TestStyledCheckpointRetainsNativeScrollbackAndCapturePolicy(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		sink := &memorySink{}
		c := NewLiveCollector(sink, 256, enabled, nil, EventProviderOutput)
		session := Session{SessionID: "scroll", Cols: 40, Rows: 2}
		for i := 0; i < 30; i++ {
			c.ObserveOutputChunk(session, []byte(fmt.Sprintf("\x1b[31mLINE_%02d\x1b[0m\r\n", i)))
		}
		c.SessionEnded(session)
		if err := c.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		var output strings.Builder
		for _, e := range sink.snapshot() {
			if e.Terminal != nil {
				output.WriteString(e.Terminal.Scrollback)
				output.WriteString(e.Terminal.ANSI)
			}
		}
		if !enabled && output.Len() != 0 {
			t.Fatal("checkpoint bypassed text capture policy")
		}
		if enabled && (!strings.Contains(output.String(), "LINE_00") || !strings.Contains(output.String(), "LINE_29") || !strings.Contains(output.String(), "\x1b[")) {
			t.Fatalf("lost styled scrollback: %q", output.String())
		}
	}
}
