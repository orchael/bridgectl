package telemetry

import (
	"encoding/json"
	"testing"
	"time"
)

// TestBridgeEnvelopePropagatesEventSchemaVersion guards against regressing to
// a hardcoded event schema_version. Bridge's collector rejects an envelope
// whose per-event schema_version does not match the event's real schema
// (HTTP 422 unsupported_event_schema), which silently drops every session
// from the dashboard since delivery runs on a detached background process
// with no visible logs.
func TestBridgeEnvelopePropagatesEventSchemaVersion(t *testing.T) {
	event := Event{
		SchemaVersion: 2,
		Timestamp:     time.Now().UTC(),
		SessionID:     "session-1",
		Provider:      "claude",
		Kind:          EventSessionStarted,
		Sequence:      1,
	}
	line, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := bridgeEnvelope("20260918T000000.000000000Z-test", line)
	if err != nil {
		t.Fatal(err)
	}

	var envelope struct {
		Segment struct {
			Events []struct {
				SchemaVersion int `json:"schema_version"`
			} `json:"events"`
		} `json:"segment"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Segment.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(envelope.Segment.Events))
	}
	if got := envelope.Segment.Events[0].SchemaVersion; got != event.SchemaVersion {
		t.Fatalf("event schema_version = %d, want %d (the event's own schema version)", got, event.SchemaVersion)
	}
}
