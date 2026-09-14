package telemetry

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestResolveSourceIDPersistsGeneratedIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry", "source-id")
	first, err := ResolveSourceID("", path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ResolveSourceID("", path)
	if err != nil {
		t.Fatal(err)
	}
	if first == "" || second != first || !ValidSourceID(first) {
		t.Fatalf("source IDs first=%q second=%q", first, second)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("source ID mode=%#o, want 0600", info.Mode().Perm())
	}
}

func TestResolveSourceIDUsesValidatedExplicitIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source-id")
	got, err := ResolveSourceID("bridge-east-1", path)
	if err != nil || got != "bridge-east-1" {
		t.Fatalf("ResolveSourceID()=%q, %v", got, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("explicit source ID unexpectedly persisted: %v", err)
	}
	if _, err := ResolveSourceID("bridge east", path); err == nil {
		t.Fatal("invalid explicit source ID accepted")
	}
}

func TestResolveSourceIDConcurrentFirstStartConverges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry", "source-id")
	const callers = 16
	results := make(chan string, callers)
	errors := make(chan error, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			sourceID, err := ResolveSourceID("", path)
			if err != nil {
				errors <- err
				return
			}
			results <- sourceID
		}()
	}
	group.Wait()
	close(results)
	close(errors)
	for err := range errors {
		t.Errorf("ResolveSourceID(): %v", err)
	}
	var want string
	for sourceID := range results {
		if want == "" {
			want = sourceID
		}
		if sourceID != want {
			t.Errorf("source ID=%q, want %q", sourceID, want)
		}
	}
	if want == "" {
		t.Fatal("no source ID returned")
	}
}

func TestResolveSourceIDRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("bridge-east-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "source-id")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveSourceID("", path); err == nil {
		t.Fatal("symlink source ID accepted")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("symlink target mode=%#o, want unchanged 0644", info.Mode().Perm())
	}
}
