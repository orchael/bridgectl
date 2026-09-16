package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/orchael/bridgectl/internal/telemetry"
)

func TestAnalysisExampleMetricsAPIAndBoundedLLMPacket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "segment.jsonl")
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	events := []telemetry.Event{
		{SchemaVersion: 2, Timestamp: now, SourceID: "bridge-a", SessionID: "s1", Sequence: 1, Kind: telemetry.EventSessionStarted},
		{SchemaVersion: 2, Timestamp: now, SourceID: "bridge-a", SessionID: "s1", Sequence: 2, Kind: telemetry.EventProviderOutput, Direction: telemetry.DirectionAgent, Stream: telemetry.StreamThinking, Text: "private chain of thought", ByteCount: 24},
		{SchemaVersion: 2, Timestamp: now, SourceID: "bridge-a", SessionID: "s1", Sequence: 4, Kind: telemetry.EventProviderOutput, Direction: telemetry.DirectionAgent, Stream: telemetry.StreamOutput, Text: "Proceed?", ByteCount: 8},
		{SchemaVersion: 2, Timestamp: now, SourceID: "bridge-a", SessionID: "s1", Sequence: 5, Kind: telemetry.EventUserInput, Direction: telemetry.DirectionHuman, Stream: telemetry.StreamInput, Text: "yes", ByteCount: 3},
		{SchemaVersion: 2, Timestamp: now, SourceID: "bridge-a", SessionID: "s1", Sequence: 6, Kind: telemetry.EventSessionEnded},
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	encoder := json.NewEncoder(f)
	for _, event := range events {
		if err := encoder.Encode(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	dataset, err := analyze(path)
	if err != nil {
		t.Fatal(err)
	}
	if dataset.Summary.Sessions != 1 || dataset.Summary.AgentTurns != 1 || dataset.Summary.HumanTurns != 1 || dataset.Summary.Quality.MissingSequences != 1 || len(dataset.Findings) != 1 {
		t.Fatalf("dataset=%+v", dataset)
	}
	packet := buildLLMPacket(dataset.Sessions[0])
	encoded, _ := json.Marshal(packet)
	if strings.Contains(string(encoded), "private chain of thought") || len(packet.Turns) != 2 {
		t.Fatalf("unsafe LLM packet=%s", encoded)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/analytics/summary", nil)
	response := httptest.NewRecorder()
	routes(dataset).ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"sessions":1`) {
		t.Fatalf("summary response=%d %s", response.Code, response.Body.String())
	}
}

func TestBuildLLMPacketTruncatesByUnicodeCharacters(t *testing.T) {
	packet := buildLLMPacket(SessionView{Turns: []telemetry.Turn{{Text: strings.Repeat("🙂", maxPacketChars+10)}}})
	if len(packet.Turns) != 1 || !packet.Truncated || !utf8.ValidString(packet.Turns[0].Text) || utf8.RuneCountInString(packet.Turns[0].Text) != maxPacketChars {
		t.Fatalf("packet did not preserve a %d-character UTF-8 boundary: %+v", maxPacketChars, packet)
	}
}

func TestAnalysisExampleCountsContinuationChunksAsOneTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "segment.jsonl")
	event := telemetry.Event{SchemaVersion: 2, Timestamp: time.Now(), SourceID: "bridge-a", SessionID: "large", Sequence: 1,
		Kind: telemetry.EventProviderOutput, Direction: telemetry.DirectionAgent, Stream: telemetry.StreamOutput,
		Text: strings.Repeat("x", telemetry.MaxTurnTextBytes+1), ByteCount: telemetry.MaxTurnTextBytes + 1}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	dataset, err := analyze(path)
	if err != nil {
		t.Fatal(err)
	}
	if dataset.Summary.AgentTurns != 1 || len(dataset.Sessions[0].Turns) != 2 {
		t.Fatalf("continuation chunks inflated logical turns: %+v", dataset.Summary)
	}
}
