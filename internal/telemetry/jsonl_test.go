package telemetry

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestRotatingJSONLSinkBoundsFilesAndReadEventsIncludesRotations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	sink := NewRotatingJSONLSink(path, 220, 3)
	for i := 0; i < 12; i++ {
		if err := sink.Record(Event{Timestamp: time.Now().UTC(), SessionID: fmt.Sprintf("session-%02d", i), Kind: EventQuestion, Text: "Proceed with this bounded event?"}); err != nil {
			t.Fatal(err)
		}
	}
	files, err := filepath.Glob(path + "*")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 3 {
		t.Fatalf("files=%v, want current plus two rotations", files)
	}
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > 220 {
			t.Fatalf("%s size=%d, want <=220", file, info.Size())
		}
	}
	events, err := ReadEvents(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[len(events)-1].SessionID != "session-11" {
		t.Fatalf("retained events=%+v", events)
	}
	for i := 1; i < len(events); i++ {
		if events[i-1].SessionID >= events[i].SessionID {
			t.Fatalf("events not read oldest-first: %+v", events)
		}
	}
}

func TestJSONLSinkRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "events.jsonl")
	sink := NewJSONLSink(path)

	const count = 24
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sink.Record(Event{SessionID: "session", Kind: EventQuestion}); err != nil {
				t.Errorf("Record: %v", err)
			}
		}()
	}
	wg.Wait()

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	lines := 0
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("invalid JSONL record: %v", err)
		}
		lines++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if lines != count {
		t.Fatalf("lines=%d, want %d", lines, count)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("mode=%#o, want 0600", got)
	}
	parentInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := parentInfo.Mode().Perm(); runtime.GOOS != "windows" && got != 0o700 {
		t.Fatalf("parent mode=%#o, want 0700", got)
	}
}

func TestJSONLSinkHardensExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewJSONLSink(path).Record(Event{Kind: EventQuestion}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("mode=%#o, want 0600", got)
	}
}

func TestJSONLSinkErrors(t *testing.T) {
	t.Run("marshal", func(t *testing.T) {
		err := NewJSONLSink(filepath.Join(t.TempDir(), "events.jsonl")).Record(Event{
			Timestamp: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		if err == nil {
			t.Fatal("Record accepted a timestamp JSON cannot represent")
		}
	})

	t.Run("parent is file", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "not-a-directory")
		if err := os.WriteFile(parent, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := NewJSONLSink(filepath.Join(parent, "events.jsonl")).Record(Event{}); err == nil {
			t.Fatal("Record succeeded with a file as its parent directory")
		}
	})

	t.Run("path is directory", func(t *testing.T) {
		path := t.TempDir()
		if err := NewJSONLSink(path).Record(Event{}); err == nil {
			t.Fatal("Record succeeded with a directory as its event file")
		}
	})
}
