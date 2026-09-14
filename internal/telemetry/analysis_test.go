package telemetry

import (
	"testing"
	"time"
)

func TestInteractionAnalyzerReconstructsTurnsAndQuality(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	events := []Event{
		{SchemaVersion: 2, Timestamp: now, SourceID: "bridge-a", SessionID: "shared", Sequence: 1, Kind: EventSessionStarted},
		{SchemaVersion: 2, Timestamp: now.Add(time.Second), SourceID: "bridge-a", SessionID: "shared", Sequence: 2, Kind: EventProviderOutput, Direction: DirectionAgent, Stream: StreamOutput, Text: "hello ", ByteCount: 6},
		{SchemaVersion: 2, Timestamp: now.Add(2 * time.Second), SourceID: "bridge-a", SessionID: "shared", Sequence: 3, Kind: EventProviderOutput, Direction: DirectionAgent, Stream: StreamOutput, Text: "world", ByteCount: 5, Redactions: 1},
		{SchemaVersion: 2, Timestamp: now.Add(3 * time.Second), SourceID: "bridge-a", SessionID: "shared", Sequence: 4, Kind: EventUserInput, Direction: DirectionHuman, Stream: StreamInput, Text: "yes", ByteCount: 3},
		{SchemaVersion: 2, Timestamp: now.Add(4 * time.Second), SourceID: "bridge-a", SessionID: "shared", Sequence: 4, Kind: EventUserInput, Direction: DirectionHuman, Stream: StreamInput, Text: "duplicate", ByteCount: 9},
		{SchemaVersion: 2, Timestamp: now.Add(5 * time.Second), SourceID: "bridge-a", SessionID: "shared", Sequence: 6, Kind: EventProviderOutput, Direction: DirectionAgent, Stream: StreamThinking, Text: "plan", ByteCount: 4},
		{SchemaVersion: 2, Timestamp: now.Add(6 * time.Second), SourceID: "bridge-a", SessionID: "shared", Sequence: 7, Kind: EventSessionEnded},
		{SchemaVersion: 1, Timestamp: now, SessionID: "legacy", Sequence: 1, Kind: EventSessionStarted},
	}

	analyzer := NewInteractionAnalyzer()
	var turns []Turn
	for _, event := range events {
		turns = append(turns, analyzer.Observe(event)...)
	}
	turns = append(turns, analyzer.Finish()...)
	if len(turns) != 3 {
		t.Fatalf("turns=%+v, want agent, human, and thinking", turns)
	}
	if turns[0].Text != "hello world" || turns[0].SequenceStart != 2 || turns[0].SequenceEnd != 3 || turns[0].Redactions != 1 {
		t.Fatalf("agent turn=%+v", turns[0])
	}
	if turns[1].Direction != DirectionHuman || turns[2].Stream != StreamThinking {
		t.Fatalf("turn directions/streams=%+v", turns)
	}
	quality := analyzer.Quality()
	if quality.Events != len(events) || quality.Duplicates != 1 || quality.MissingSequences != 1 || quality.LegacyEvents != 1 || quality.Redactions != 1 {
		t.Fatalf("quality=%+v", quality)
	}
}

func TestInteractionAnalyzerSeparatesEqualSessionIDsBySource(t *testing.T) {
	analyzer := NewInteractionAnalyzer()
	for _, event := range []Event{
		{SchemaVersion: 2, Timestamp: time.Now(), SourceID: "a", SessionID: "same", Sequence: 1, Kind: EventProviderOutput, Direction: DirectionAgent, Stream: StreamOutput, Text: "A"},
		{SchemaVersion: 2, Timestamp: time.Now(), SourceID: "b", SessionID: "same", Sequence: 1, Kind: EventProviderOutput, Direction: DirectionAgent, Stream: StreamOutput, Text: "B"},
	} {
		analyzer.Observe(event)
	}
	turns := analyzer.Finish()
	if len(turns) != 2 || turns[0].SourceID == turns[1].SourceID {
		t.Fatalf("turns=%+v", turns)
	}
}
