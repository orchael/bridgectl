package bridge

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// Opening a terminal must replay real bytes without acquiring the writer.
func TestTerminalWatchDoesNotClaimWriter(t *testing.T) {
	s, _ := newInteractionSupervisor(t, nil)
	startTestSession(t, s, "watch")
	w, err := s.TerminalWindow("watch", "viewer", 0)
	if err != nil || w.Writer {
		t.Fatalf("window: %+v %v", w, err)
	}
	info, _ := s.Get("watch")
	if info.ActiveWriterClientID != "" || info.ObserverCount != 0 {
		t.Fatal("watch changed ownership")
	}
	if _, err := s.TerminalWindow("missing", "viewer", 0); err == nil {
		t.Fatal("missing session accepted")
	}
}

func TestTerminalReplayBoundAndGap(t *testing.T) {
	ms := &managedSession{buf: NewByteBuffer(100000), info: SessionInfo{Cols: 80, Rows: 24}}
	s := &Supervisor{sessions: map[string]*managedSession{"s": ms}}
	for i := 0; i < 12; i++ {
		ms.buf.Append(bytes.Repeat([]byte{byte(i)}, 8192))
	}
	first, err := s.TerminalWindow("s", "viewer", 0)
	if err != nil || len(first.Data) != 65536 || first.NextSequence != 8 {
		t.Fatalf("%+v %v", first, err)
	}
	next, _ := s.TerminalWindow("s", "viewer", first.NextSequence)
	if len(next.Data) != 32768 || next.NextSequence != 12 {
		t.Fatal("replay cursor lost bytes")
	}
	ms.buf.Append(bytes.Repeat([]byte("x"), 65536))
	gap, _ := s.TerminalWindow("s", "viewer", 1)
	if !gap.Gap {
		t.Fatal("missing replay gap")
	}
	ms.liveClosed = true
	end, _ := s.TerminalWindow("s", "viewer", 13)
	if !end.Ended {
		t.Fatal("end missing")
	}
}

func TestTelemetrySessionUsesCurrentPTYGeometry(t *testing.T) {
	ms := &managedSession{info: SessionInfo{SessionID: "s", Cols: 120, Rows: 42}}
	session := telemetrySession(ms)
	if session.Cols != 120 || session.Rows != 42 {
		t.Fatalf("wrong PTY geometry: %+v", session)
	}
	ms.info.Cols = 90
	if telemetrySession(ms).Cols != 90 {
		t.Fatal("stale PTY geometry")
	}
	ms.streamJSON = true
	session = telemetrySession(ms)
	if session.Cols != 0 || session.Rows != 0 {
		t.Fatal("stream JSON marked as terminal")
	}
}

func TestTerminalReplayPreservesResizeBoundaries(t *testing.T) {
	ms := &managedSession{buf: NewByteBuffer(100000), info: SessionInfo{Cols: 120, Rows: 40}}
	s := &Supervisor{sessions: map[string]*managedSession{"s": ms}}
	ms.buf.AppendSized([]byte("old"), ChunkTypeOutput, 80, 24)
	ms.buf.AppendSized(nil, ChunkTypeOutput, 120, 40)
	ms.buf.AppendSized([]byte("new"), ChunkTypeOutput, 120, 40)
	first, err := s.TerminalWindow("s", "viewer", 0)
	if err != nil || string(first.Data) != "old" || first.Cols != 80 || !first.More {
		t.Fatalf("first: %+v %v", first, err)
	}
	next, err := s.TerminalWindow("s", "viewer", first.NextSequence)
	if err != nil || string(next.Data) != "new" || next.Cols != 120 || next.More {
		t.Fatalf("next: %+v %v", next, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.WaitTerminalWindow(ctx, "s", "viewer", next.NextSequence); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
	done := make(chan TerminalWindow, 1)
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { w, _ := s.WaitTerminalWindow(ctx, "s", "viewer", next.NextSequence); done <- w }()
	ms.buf.AppendSized([]byte("wake"), ChunkTypeOutput, 120, 40)
	select {
	case w := <-done:
		if string(w.Data) != "wake" {
			t.Fatal(w)
		}
	case <-ctx.Done():
		t.Fatal("missed notification")
	}
}
