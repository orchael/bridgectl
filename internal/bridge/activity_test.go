package bridge

import (
	"encoding/json"
	"testing"
	"time"
)

func TestMAR95ActivityBoundedReplay(t *testing.T) {
	var b ActivityBuffer
	now := time.Now()
	for i := 0; i < 300; i++ {
		b.Append("command", "Command completed", now)
	}
	w := b.Window(0, 1000, 1<<20, now)
	if len(w.Events) != 128 || w.Events[0].Sequence != 173 || w.NextSequence != 300 || !w.Gap {
		t.Fatalf("bad retention: %+v", w)
	}
	w = b.Window(298, 20, 4096, now)
	if len(w.Events) != 2 || w.NextSequence != 300 || w.Gap {
		t.Fatal(w)
	}
	raw, _ := json.Marshal(b.Window(0, 128, 1024, now))
	if len(raw) > 1024 {
		t.Fatalf("byte bound: %d", len(raw))
	}
	w = b.Window(300, 20, 4096, now.Add(16*time.Minute))
	if len(w.Events) != 0 {
		t.Fatal("expired activity")
	}
	b.Append("status", "Working", now.Add(16*time.Minute))
	if got := b.Window(300, 20, 4096, now.Add(16*time.Minute)); len(got.Events) != 1 || got.Events[0].Sequence != 301 {
		t.Fatal(got)
	}
}
