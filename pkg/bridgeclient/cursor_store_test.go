package bridgeclient

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

func TestMemoryCursorStore(t *testing.T) {
	store := NewMemoryCursorStore()
	ctx := context.Background()
	const sessionID = "s1"
	const subscriberID = "sub1"

	got, err := store.LoadCursor(ctx, sessionID, subscriberID)
	if err != nil {
		t.Fatalf("LoadCursor empty: %v", err)
	}
	if got != 0 {
		t.Fatalf("LoadCursor empty got=%d want=0", got)
	}
	if err := store.SaveCursor(ctx, sessionID, subscriberID, 42); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}
	got, err = store.LoadCursor(ctx, sessionID, subscriberID)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if got != 42 {
		t.Fatalf("LoadCursor got=%d want=42", got)
	}
}

func TestFileCursorStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursors", "state.json")
	store := NewFileCursorStore(path)
	ctx := context.Background()
	const sessionID = "s2"
	const subscriberID = "sub2"

	if err := store.SaveCursor(ctx, sessionID, subscriberID, 9); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}
	got, err := store.LoadCursor(ctx, sessionID, subscriberID)
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if got != 9 {
		t.Fatalf("LoadCursor got=%d want=9", got)
	}
}

func TestFileCursorStore_LoadMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "cursors.json")
	store := NewFileCursorStore(path)

	// Loading from a non-existent file should return 0, nil.
	got, err := store.LoadCursor(context.Background(), "sess", "sub")
	if err != nil {
		t.Fatalf("LoadCursor missing file: %v", err)
	}
	if got != 0 {
		t.Errorf("LoadCursor missing file = %d, want 0", got)
	}
}

func TestFileCursorStore_CreatesDirOnSave(t *testing.T) {
	// Path has a nested directory that does not exist yet.
	path := filepath.Join(t.TempDir(), "a", "b", "c", "cursors.json")
	store := NewFileCursorStore(path)

	if err := store.SaveCursor(context.Background(), "sess", "sub", 7); err != nil {
		t.Fatalf("SaveCursor: %v", err)
	}
	got, err := store.LoadCursor(context.Background(), "sess", "sub")
	if err != nil {
		t.Fatalf("LoadCursor: %v", err)
	}
	if got != 7 {
		t.Errorf("LoadCursor = %d, want 7", got)
	}
}

func TestMemoryCursorStore_MultipleSubscribers(t *testing.T) {
	store := NewMemoryCursorStore()
	ctx := context.Background()

	_ = store.SaveCursor(ctx, "session", "sub-a", 1)
	_ = store.SaveCursor(ctx, "session", "sub-b", 2)
	_ = store.SaveCursor(ctx, "other", "sub-a", 99)

	a, _ := store.LoadCursor(ctx, "session", "sub-a")
	b, _ := store.LoadCursor(ctx, "session", "sub-b")
	c, _ := store.LoadCursor(ctx, "other", "sub-a")

	if a != 1 {
		t.Errorf("session/sub-a = %d, want 1", a)
	}
	if b != 2 {
		t.Errorf("session/sub-b = %d, want 2", b)
	}
	if c != 99 {
		t.Errorf("other/sub-a = %d, want 99", c)
	}
}

func TestMemoryCursorStore_ConcurrentAccess(t *testing.T) {
	store := NewMemoryCursorStore()
	ctx := context.Background()

	const goroutines = 20
	const iterations = 50

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sessionID := "session"
			subscriberID := "sub"
			for j := range iterations {
				seq := uint64(id*iterations + j)
				_ = store.SaveCursor(ctx, sessionID, subscriberID, seq)
				_, _ = store.LoadCursor(ctx, sessionID, subscriberID)
			}
		}(i)
	}
	wg.Wait()
}

func TestFileCursorStore_ConcurrentAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cursors.json")
	store := NewFileCursorStore(path)
	ctx := context.Background()

	const goroutines = 10
	const iterations = 20

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := range iterations {
				seq := uint64(id*iterations + j)
				_ = store.SaveCursor(ctx, "sess", "sub", seq)
				_, _ = store.LoadCursor(ctx, "sess", "sub")
			}
		}(i)
	}
	wg.Wait()
}
