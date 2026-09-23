package bridgecontrol

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRevisionStore_MonotonicPerSession(t *testing.T) {
	rs := NewRevisionStore(filepath.Join(t.TempDir(), "revisions.json"))

	if got := rs.Current("s1"); got != 0 {
		t.Fatalf("Current(unseen) = %d, want 0", got)
	}
	if got := rs.Next("s1"); got != 1 {
		t.Fatalf("Next(s1) #1 = %d, want 1", got)
	}
	if got := rs.Next("s1"); got != 2 {
		t.Fatalf("Next(s1) #2 = %d, want 2", got)
	}
	if got := rs.Current("s1"); got != 2 {
		t.Fatalf("Current(s1) = %d, want 2", got)
	}
	// A different session_id has an independent counter.
	if got := rs.Next("s2"); got != 1 {
		t.Fatalf("Next(s2) #1 = %d, want 1", got)
	}
}

func TestRevisionStore_ForgetResetsCounter(t *testing.T) {
	rs := NewRevisionStore(filepath.Join(t.TempDir(), "revisions.json"))
	rs.Next("s1")
	rs.Next("s1")
	rs.Forget("s1")
	if got := rs.Current("s1"); got != 0 {
		t.Fatalf("Current after Forget = %d, want 0", got)
	}
	if got := rs.Next("s1"); got != 1 {
		t.Fatalf("Next after Forget = %d, want 1 (session_started requires revision 1)", got)
	}
}

func TestRevisionStore_SurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "revisions.json")
	rs := NewRevisionStore(path)
	rs.Next("s1")
	rs.Next("s1")
	rs.Next("s1")

	// Simulate a bridgectl daemon restart: a fresh RevisionStore loading the
	// same path must not restart a still-tracked (e.g. recovered) session's
	// revision at 1, or a subsequent snapshot would look stale to Bridge and
	// be silently ignored.
	reloaded := NewRevisionStore(path)
	if got := reloaded.Current("s1"); got != 3 {
		t.Fatalf("Current(s1) after reload = %d, want 3", got)
	}
	if got := reloaded.Next("s1"); got != 4 {
		t.Fatalf("Next(s1) after reload = %d, want 4", got)
	}
}

func TestRevisionStore_MissingFileIsEmptyNotError(t *testing.T) {
	rs := NewRevisionStore(filepath.Join(t.TempDir(), "does-not-exist.json"))
	if got := rs.Current("s1"); got != 0 {
		t.Fatalf("Current on fresh store = %d, want 0", got)
	}
}

func TestRevisionStore_EmptyPathDisablesPersistence(t *testing.T) {
	rs := NewRevisionStore("")
	if got := rs.Next("s1"); got != 1 {
		t.Fatalf("Next with empty path = %d, want 1 (in-memory still works)", got)
	}
}

// TestRevisionStore_JSONNullTreatedAsEmpty guards against a corrupted or
// manually edited revisions file containing the JSON literal null:
// json.Unmarshal accepts it and leaves the target map nil without erroring,
// which would otherwise make a later Next's map write panic (assignment to
// a nil map) instead of being treated the same as a missing/empty file.
func TestRevisionStore_JSONNullTreatedAsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "revisions.json")
	if err := os.WriteFile(path, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	rs := NewRevisionStore(path)
	if got := rs.Current("s1"); got != 0 {
		t.Fatalf("Current on a null-file store = %d, want 0", got)
	}
	if got := rs.Next("s1"); got != 1 { // must not panic
		t.Fatalf("Next on a null-file store = %d, want 1", got)
	}
}
