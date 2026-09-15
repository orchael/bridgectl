package telemetry

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

// TEL-118: a distinct start is a boundary even when an identity is reused.
func TestInteractionAnalyzerSessionRestartAndStartReplay(t *testing.T) {
	a := NewInteractionAnalyzer()
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	start := Event{SourceID: "a", SessionID: "s", Sequence: 1, Kind: EventSessionStarted, Timestamp: now}
	a.Observe(start)
	a.Observe(Event{SourceID: "a", SessionID: "s", Sequence: 2, Kind: EventProviderOutput, Direction: DirectionAgent, Text: "old"})
	if got := a.Observe(start); len(got) != 0 {
		t.Fatalf("replayed start flushed %v", got)
	}
	start.Timestamp = now.Add(time.Second)
	if got := a.Observe(start); len(got) != 1 || got[0].Text != "old" {
		t.Fatalf("restart=%v", got)
	}
	a.Observe(Event{SourceID: "a", SessionID: "s", Sequence: 2, Kind: EventProviderOutput, Direction: DirectionAgent, Text: "new"})
	if got := a.Finish(); len(got) != 1 || got[0].Text != "new" {
		t.Fatalf("new session=%v", got)
	}
}

// TEL-118: missing evidence must not be merged into a continuous turn.
func TestInteractionAnalyzerGapSeparatesCompatibleTurns(t *testing.T) {
	a := NewInteractionAnalyzer()
	a.Observe(Event{SessionID: "s", Sequence: 1, Kind: EventProviderOutput, Direction: DirectionAgent, Text: "before"})
	got := a.Observe(Event{SessionID: "s", Sequence: 3, Kind: EventProviderOutput, Direction: DirectionAgent, Text: "after"})
	if len(got) != 1 || got[0].Text != "before" {
		t.Fatalf("gap completed=%v", got)
	}
	if got = a.Finish(); len(got) != 1 || got[0].Text != "after" || a.Quality().MissingSequences != 1 {
		t.Fatalf("after=%v quality=%v", got, a.Quality())
	}
}

// TEL-118: large same-direction streams remain bounded without losing evidence.
func TestInteractionAnalyzerBoundsTurnChunks(t *testing.T) {
	a := NewInteractionAnalyzer()
	text := strings.Repeat("界", 50000)
	var turns []Turn
	for i := uint64(1); i <= 3; i++ {
		turns = append(turns, a.Observe(Event{SessionID: "s", Sequence: i, Kind: EventProviderOutput, Direction: DirectionAgent, Text: text, ByteCount: len(text) + 7, Redactions: 2})...)
		for _, pending := range a.pending {
			if len(pending.Text) > 64*1024 {
				t.Fatalf("retained %d bytes", len(pending.Text))
			}
		}
	}
	turns = append(turns, a.Finish()...)
	var joined strings.Builder
	bytes, redactions := 0, 0
	for i, turn := range turns {
		if len(turn.Text) > 64*1024 || !utf8.ValidString(turn.Text) {
			t.Fatalf("invalid chunk size=%d", len(turn.Text))
		}
		if turn.ChunkIndex != i || turn.Continues != (i < len(turns)-1) || turn.SequenceStart < 1 || turn.SequenceEnd > 3 {
			t.Fatalf("chunk metadata=%+v", turn)
		}
		joined.WriteString(turn.Text)
		bytes += turn.ByteCount
		redactions += turn.Redactions
	}
	if joined.String() != strings.Repeat(text, 3) || bytes != 3*(len(text)+7) || redactions != 6 {
		t.Fatalf("lost evidence bytes=%d redactions=%d", bytes, redactions)
	}
}

func TestInteractionAnalyzerGapOnMetadataFlushesAndEmptyTextCounts(t *testing.T) {
	a := NewInteractionAnalyzer()
	a.Observe(Event{SessionID: "s", Sequence: 1, Kind: EventUserInput, Direction: DirectionHuman, ByteCount: 20, Redactions: 1})
	got := a.Observe(Event{SessionID: "s", Sequence: 3, Kind: EventSessionContext})
	if len(got) != 1 || got[0].ByteCount != 20 || got[0].Redactions != 1 || got[0].SequenceEnd != 1 {
		t.Fatalf("metadata gap=%+v", got)
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
