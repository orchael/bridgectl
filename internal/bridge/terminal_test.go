package bridge

import (
	"bytes"
	"testing"
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
