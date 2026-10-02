package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// MAR-106: the final record can itself be dropped; absence of a later sequence
// must not hide queue loss. Checkpoints bypass the bounded event queue.
func TestCompletenessReportsDroppedTailAndSinkFailure(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var checkpoints []map[string]any
	sink := NewAsyncSink(sinkFunc(func(e Event) error {
		if e.Kind == "telemetry_checkpoint" {
			raw, _ := json.Marshal(e)
			var checkpoint map[string]any
			if err := json.Unmarshal(raw, &checkpoint); err != nil {
				t.Error(err)
			}
			checkpoints = append(checkpoints, checkpoint)
			return nil
		}
		once.Do(func() { close(started) })
		<-release
		if e.Sequence == 2 {
			return errors.New("disk full")
		}
		return nil
	}), 1, nil)
	event := Event{SchemaVersion: 2, SessionID: "s1", Kind: EventSessionStarted, Sequence: 1}
	if err := sink.Record(event); err != nil {
		t.Fatal(err)
	}
	<-started
	event.Kind = EventProviderOutput
	event.Sequence = 2
	if err := sink.Record(event); err != nil {
		t.Fatal(err)
	}
	event.Kind = EventSessionEnded
	event.Sequence = 3
	if err := sink.Record(event); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("want queue full, got %v", err)
	}
	close(release)
	if err := sink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(checkpoints) != 1 {
		t.Fatalf("got %d checkpoints, want final loss report", len(checkpoints))
	}
	stats, ok := checkpoints[0]["completeness"].(map[string]any)
	if !ok {
		t.Fatal("missing completeness data")
	}
	for key, want := range map[string]any{"attempted_events": float64(3), "last_sequence": float64(3), "queue_dropped_events": float64(1), "sink_failed_events": float64(1), "session_ended_observed": true} {
		if stats[key] != want {
			t.Errorf("%s=%v, want %v", key, stats[key], want)
		}
	}
}

func TestCompletenessCheckpointAcceptedByGRPCCollector(t *testing.T) {
	memory := &memorySink{}
	collector := NewLiveCollectorForSource(memory, 16, true, "source-1", nil, EventQuestion, EventAnswer)
	session := Session{SessionID: "session", Provider: "claude"}
	collector.SessionStarted(session)
	collector.ObserveOutputChunk(session, []byte("Proceed?"))
	collector.ObserveInputChunk(session, []byte("yes\n"))
	collector.SessionEnded(session)
	if err := collector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var found bool
	server := &GRPCCollectorServer{maxSegmentBytes: 1 << 20, kinds: map[EventKind]bool{EventQuestion: true, EventAnswer: true}}
	for _, e := range memory.snapshot() {
		if e.Kind != EventTelemetryCheckpoint {
			continue
		}
		found = true
		if e.Completeness == nil || e.Completeness.AttemptedEvents != 2 || !e.Completeness.SessionStartedObserved || !e.Completeness.SessionEndedObserved || !e.Completeness.IncludeRedactedText {
			t.Fatalf("invalid capture scope: %+v", e.Completeness)
		}
		raw, _ := json.Marshal(e)
		if err := server.validateJSONL(append(raw, '\n')); err != nil {
			t.Fatalf("checkpoint rejected: %v", err)
		}
	}
	if !found {
		t.Fatal("missing checkpoint")
	}
}

func TestCompletenessCheckpointPersistenceFailureIsReturned(t *testing.T) {
	diskErr := errors.New("disk full")
	sink := NewAsyncSink(sinkFunc(func(e Event) error {
		if e.Kind == EventTelemetryCheckpoint {
			return diskErr
		}
		return nil
	}), 2, nil)
	if err := sink.Record(Event{SchemaVersion: 2, SessionID: "s1", Kind: EventSessionStarted, Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(context.Background()); !errors.Is(err, diskErr) {
		t.Fatalf("Close=%v, want checkpoint failure", err)
	}
}

func TestCompletenessSurvivesSpoolEviction(t *testing.T) {
	evicted := 0
	spool, err := NewSegmentSpool(t.TempDir(), 2048, 4096, func(Segment) { evicted++ })
	if err != nil {
		t.Fatal(err)
	}
	sink := NewAsyncSink(spool, 64, nil)
	for i := 1; i <= 20; i++ {
		kind := EventProviderOutput
		if i == 1 {
			kind = EventSessionStarted
		}
		if i == 20 {
			kind = EventSessionEnded
		}
		if err := sink.Record(Event{SchemaVersion: 2, Timestamp: time.Now().UTC(), SessionID: "s1", Kind: kind, Sequence: uint64(i), Text: strings.Repeat("x", 1000)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if evicted == 0 {
		t.Fatal("test did not trigger bounded-spool eviction")
	}
	pending, err := spool.Pending()
	if err != nil {
		t.Fatal(err)
	}
	sequences := map[uint64]bool{}
	var checkpoint *Completeness
	for _, segment := range pending {
		raw, err := spool.Read(segment.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(bytes.TrimSpace(raw), []byte("\n")) {
			var event Event
			if err := json.Unmarshal(line, &event); err != nil {
				t.Fatal(err)
			}
			if event.Completeness != nil {
				checkpoint = event.Completeness
			} else {
				sequences[event.Sequence] = true
			}
		}
	}
	if sequences[1] {
		t.Fatal("oldest event should be evicted")
	}
	if checkpoint == nil || checkpoint.AttemptedEvents != 20 || checkpoint.LastSequence != 20 || !checkpoint.SessionEndedObserved {
		t.Fatalf("lost evidence needed to detect eviction: %+v", checkpoint)
	}
}

func TestCompletenessPeriodicOpenCheckpoint(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		downstream := &memorySink{}
		sink := NewAsyncSink(downstream, 4, nil)
		if err := sink.Record(Event{SchemaVersion: 2, SessionID: "open-session", Kind: EventSessionStarted, Sequence: 1}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(DefaultFlushInterval + time.Second)
		synctest.Wait()
		events := downstream.snapshot()
		if len(events) != 2 || events[1].Completeness == nil || events[1].Completeness.SessionEndedObserved {
			t.Fatalf("missing open checkpoint: %+v", events)
		}
		if err := sink.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}
