package bridge

import (
	"path/filepath"
	"testing"
	"time"
)

func TestBoltSessionStore_Chunks(t *testing.T) {
	store, err := NewBoltSessionStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("NewBoltSessionStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now().UTC()
	chunks := []OutputChunk{
		{Seq: 1, Timestamp: now, Payload: []byte("hello ")},
		{Seq: 2, Timestamp: now.Add(time.Millisecond), Payload: []byte("world\n")},
		{Seq: 3, Timestamp: now.Add(2 * time.Millisecond), Payload: []byte("done\n")},
	}
	for _, c := range chunks {
		if err := store.SaveChunk("sess-1", c); err != nil {
			t.Fatalf("SaveChunk seq=%d: %v", c.Seq, err)
		}
	}

	got, err := store.LoadChunks("sess-1")
	if err != nil {
		t.Fatalf("LoadChunks: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("LoadChunks len=%d want 3", len(got))
	}
	for i, c := range got {
		if c.Seq != chunks[i].Seq {
			t.Errorf("chunk[%d] Seq=%d want %d", i, c.Seq, chunks[i].Seq)
		}
		if string(c.Payload) != string(chunks[i].Payload) {
			t.Errorf("chunk[%d] Payload=%q want %q", i, c.Payload, chunks[i].Payload)
		}
	}
}

func TestBoltSessionStore_ChunksPersistAcrossReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")

	store, err := NewBoltSessionStore(dbPath)
	if err != nil {
		t.Fatalf("NewBoltSessionStore: %v", err)
	}
	chunk := OutputChunk{Cols: 120, Rows: 40, Seq: 42, Timestamp: time.Now().UTC(), Payload: []byte("data")}
	if err := store.SaveChunk("sess-durable", chunk); err != nil {
		t.Fatalf("SaveChunk: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	store2, err := NewBoltSessionStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = store2.Close() }()

	got, err := store2.LoadChunks("sess-durable")
	if err != nil {
		t.Fatalf("LoadChunks after reopen: %v", err)
	}
	if len(got) != 1 || got[0].Seq != 42 || got[0].Cols != 120 || got[0].Rows != 40 {
		t.Fatalf("expected chunk seq=42, got %+v", got)
	}
}

func TestBoltSessionStore_ChunksIsolatedBySessions(t *testing.T) {
	store, err := NewBoltSessionStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("NewBoltSessionStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now().UTC()
	_ = store.SaveChunk("sess-a", OutputChunk{Seq: 1, Timestamp: now, Payload: []byte("a1")})
	_ = store.SaveChunk("sess-a", OutputChunk{Seq: 2, Timestamp: now, Payload: []byte("a2")})
	_ = store.SaveChunk("sess-b", OutputChunk{Seq: 1, Timestamp: now, Payload: []byte("b1")})

	a, _ := store.LoadChunks("sess-a")
	b, _ := store.LoadChunks("sess-b")
	if len(a) != 2 {
		t.Errorf("sess-a chunks=%d want 2", len(a))
	}
	if len(b) != 1 {
		t.Errorf("sess-b chunks=%d want 1", len(b))
	}
}

func TestBoltSessionStore_SaveAndLoad(t *testing.T) {
	store, err := NewBoltSessionStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("NewBoltSessionStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Now().UTC().Truncate(time.Millisecond)
	info := SessionInfo{
		SessionID: "test-session-1",
		ProjectID: "proj-a",
		Provider:  "fake",
		State:     SessionStateStopped,
		CreatedAt: now,
		StoppedAt: now.Add(5 * time.Second),
		ExitCode:  0,
	}

	if err := store.Save(info); err != nil {
		t.Fatalf("Save: %v", err)
	}

	infos, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("LoadAll len=%d want 1", len(infos))
	}
	got := infos[0]
	if got.SessionID != info.SessionID {
		t.Errorf("SessionID=%q want %q", got.SessionID, info.SessionID)
	}
	if got.State != SessionStateStopped {
		t.Errorf("State=%v want Stopped", got.State)
	}
	if !got.CreatedAt.Equal(info.CreatedAt) {
		t.Errorf("CreatedAt=%v want %v", got.CreatedAt, info.CreatedAt)
	}
}

func TestBoltSessionStore_OverwriteAndMultiple(t *testing.T) {
	store, err := NewBoltSessionStore(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("NewBoltSessionStore: %v", err)
	}
	defer func() { _ = store.Close() }()

	base := SessionInfo{SessionID: "s1", ProjectID: "p", Provider: "fake", State: SessionStateRunning, CreatedAt: time.Now().UTC()}
	if err := store.Save(base); err != nil {
		t.Fatalf("Save running: %v", err)
	}

	// Overwrite with terminal state
	base.State = SessionStateStopped
	base.StoppedAt = time.Now().UTC()
	if err := store.Save(base); err != nil {
		t.Fatalf("Save stopped: %v", err)
	}

	s2 := SessionInfo{SessionID: "s2", ProjectID: "p", Provider: "fake", State: SessionStateFailed, CreatedAt: time.Now().UTC()}
	if err := store.Save(s2); err != nil {
		t.Fatalf("Save s2: %v", err)
	}

	infos, err := store.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("LoadAll len=%d want 2", len(infos))
	}

	byID := make(map[string]SessionInfo)
	for _, i := range infos {
		byID[i.SessionID] = i
	}
	if byID["s1"].State != SessionStateStopped {
		t.Errorf("s1 State=%v want Stopped", byID["s1"].State)
	}
	if byID["s2"].State != SessionStateFailed {
		t.Errorf("s2 State=%v want Failed", byID["s2"].State)
	}
}

func TestBoltSessionStore_PersistsAcrossReopen(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")

	store, err := NewBoltSessionStore(dbPath)
	if err != nil {
		t.Fatalf("NewBoltSessionStore: %v", err)
	}
	info := SessionInfo{SessionID: "durable", ProjectID: "p", Provider: "fake", State: SessionStateStopped, CreatedAt: time.Now().UTC()}
	if err := store.Save(info); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open — data must survive
	store2, err := NewBoltSessionStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = store2.Close() }()

	infos, err := store2.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll after reopen: %v", err)
	}
	if len(infos) != 1 || infos[0].SessionID != "durable" {
		t.Fatalf("expected durable session, got %+v", infos)
	}
}
