package telemetry

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestSegmentSpoolLifecycle proves TEL-111 and TEL-112: segments are bounded,
// survive restart, and are returned oldest-first for deterministic replay.
func TestSegmentSpoolLifecycle(t *testing.T) {
	dir := t.TempDir()
	spool, err := NewSegmentSpool(dir, 10, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range [][]byte{[]byte("first\n"), []byte("second\n"), []byte("third\n")} {
		if err := spool.Append(payload); err != nil {
			t.Fatal(err)
		}
		if _, err := spool.Seal(); err != nil {
			t.Fatal(err)
		}
	}

	reopened, err := NewSegmentSpool(dir, 10, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := reopened.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 3 {
		t.Fatalf("pending segments=%d, want 3", len(segments))
	}
	for index, want := range [][]byte{[]byte("first\n"), []byte("second\n"), []byte("third\n")} {
		data, err := reopened.Read(segments[index].ID)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, want) {
			t.Fatalf("segment %d=%q, want %q", index, data, want)
		}
	}
}

func TestSegmentSpoolEvictsOldest(t *testing.T) {
	var evicted []string
	spool, err := NewSegmentSpool(t.TempDir(), 8, 8, func(segment Segment) {
		evicted = append(evicted, segment.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, payload := range [][]byte{[]byte("one\n"), []byte("two\n"), []byte("tri\n")} {
		if err := spool.Append(payload); err != nil {
			t.Fatal(err)
		}
		segment, err := spool.Seal()
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, segment.ID)
	}
	if len(evicted) != 1 || evicted[0] != ids[0] {
		t.Fatalf("evicted=%v, want oldest %q", evicted, ids[0])
	}
	segments, err := spool.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 || segments[0].ID != ids[1] || segments[1].ID != ids[2] {
		t.Fatalf("pending=%v, want last two segments", segments)
	}
}

func TestSegmentSpoolAcceptIsIdempotent(t *testing.T) {
	spool, err := NewSegmentSpool(t.TempDir(), 64, 256, nil)
	if err != nil {
		t.Fatal(err)
	}
	created, err := spool.Accept("batch-1", []byte("event\n"))
	if err != nil || !created {
		t.Fatalf("first accept created=%v err=%v", created, err)
	}
	created, err = spool.Accept("batch-1", []byte("event\n"))
	if err != nil || created {
		t.Fatalf("duplicate accept created=%v err=%v", created, err)
	}
	if _, err := spool.Accept("batch-1", []byte("different\n")); !errors.Is(err, ErrSegmentConflict) {
		t.Fatalf("conflicting accept error=%v, want ErrSegmentConflict", err)
	}
	if _, err := spool.Accept("../escape", []byte("bad\n")); !errors.Is(err, ErrInvalidSegmentID) {
		t.Fatalf("unsafe ID error=%v, want ErrInvalidSegmentID", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(spool.Dir()), "escape.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unsafe path was created: %v", err)
	}
}

func TestSegmentSpoolAutomaticallySealsBeforeLimit(t *testing.T) {
	spool, err := NewSegmentSpool(t.TempDir(), 8, 64, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.Append([]byte("12345\n")); err != nil {
		t.Fatal(err)
	}
	if err := spool.Append([]byte("6789\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := spool.Seal(); err != nil {
		t.Fatal(err)
	}
	segments, err := spool.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 2 {
		t.Fatalf("segments=%d, want 2", len(segments))
	}
}

func TestSegmentSpoolRejectsRecordLargerThanSegmentLimit(t *testing.T) {
	spool, err := NewSegmentSpool(t.TempDir(), 4, 64, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.Append([]byte("oversized\n")); !errors.Is(err, ErrSegmentTooLarge) {
		t.Fatalf("Append error=%v, want ErrSegmentTooLarge", err)
	}
	segments, err := spool.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 0 {
		t.Fatalf("oversized record created segments: %v", segments)
	}
	if _, err := spool.Accept("oversized", []byte("oversized\n")); !errors.Is(err, ErrSegmentTooLarge) {
		t.Fatalf("Accept error=%v, want ErrSegmentTooLarge", err)
	}
}

func TestSegmentSpoolCountsActiveSegmentTowardDiskBudget(t *testing.T) {
	var evicted []string
	spool, err := NewSegmentSpool(t.TempDir(), 8, 10, func(segment Segment) {
		evicted = append(evicted, segment.ID)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.Append([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	first, err := spool.Seal()
	if err != nil {
		t.Fatal(err)
	}
	if err := spool.Append([]byte("next\n")); err != nil {
		t.Fatal(err)
	}
	if len(evicted) != 1 || evicted[0] != first.ID {
		t.Fatalf("evicted=%v, want active bytes to evict %q", evicted, first.ID)
	}
}

func TestSegmentSpoolEnforcesSmallerDiskBudgetOnReopen(t *testing.T) {
	dir := t.TempDir()
	spool, err := NewSegmentSpool(dir, 8, 24, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range [][]byte{[]byte("first\n"), []byte("second\n"), []byte("third\n")} {
		if err := spool.Append(payload); err != nil {
			t.Fatal(err)
		}
		if _, err := spool.Seal(); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := NewSegmentSpool(dir, 8, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	segments, err := reopened.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 {
		t.Fatalf("pending segments=%d, want newest segment only", len(segments))
	}
	data, err := reopened.Read(segments[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, []byte("third\n")) {
		t.Fatalf("remaining segment=%q, want newest", data)
	}
}

func TestSegmentSpoolRejectsDiskBudgetSmallerThanSegment(t *testing.T) {
	if _, err := NewSegmentSpool(t.TempDir(), 11, 10, nil); err == nil {
		t.Fatal("disk budget smaller than segment limit was accepted")
	}
}
