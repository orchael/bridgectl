package bridge

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestTerminalReplaysDiskPrefixAndResizeAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.db")
	store, err := NewBoltSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ms := &managedSession{buf: NewByteBuffer(100), info: SessionInfo{SessionID: "s", Cols: 100, Rows: 30}, liveClosed: true}
	var expected strings.Builder
	for i := 0; i < 120; i++ {
		cols := uint32(80)
		if i >= 60 {
			cols = 100
		}
		text := fmt.Sprintf("row-%03d 界\r\n", i)
		expected.WriteString(text)
		chunk := ms.buf.AppendSized([]byte(text), ChunkTypeOutput, cols, 30)
		if err := store.SaveChunk("s", chunk); err != nil {
			t.Fatal(err)
		}
	}
	ms.info.LastSeq = 120
	s := &Supervisor{sessions: map[string]*managedSession{"s": ms}, store: store}
	check := func() {
		t.Helper()
		after := uint64(0)
		var got strings.Builder
		for n := 0; n < 200; n++ {
			w, err := s.WaitTerminalWindow(context.Background(), "s", "viewer", after)
			if err != nil || w.Gap || w.Writer {
				t.Fatalf("window=%+v err=%v", w, err)
			}
			got.Write(w.Data)
			if w.Ended {
				break
			}
			if w.NextSequence <= after {
				t.Fatal("replay stalled")
			}
			after = w.NextSequence
		}
		if got.String() != expected.String() {
			t.Fatalf("lost saved output: got %d bytes want %d", got.Len(), expected.Len())
		}
	}
	check()
	// Even a completely empty memory ring must retain its high-water cursor.
	ms.buf = NewByteBuffer(1)
	for i := 0; i < 120; i++ {
		ms.buf.Append([]byte("evicted"))
	}
	if ms.buf.LastSeq() != 120 {
		t.Fatal("empty ring forgot last sequence")
	}
	check()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewBoltSessionStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	s = &Supervisor{history: map[string]SessionInfo{"s": ms.info}, store: store}
	check()
	page, err := store.LoadChunkPage("s", 0, 100)
	if err != nil || len(page) == 0 || page[0].Seq != 1 || len(page) >= 120 {
		t.Fatalf("unbounded page: %d %v", len(page), err)
	}
}
