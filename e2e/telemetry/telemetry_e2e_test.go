package telemetry_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/orchael/bridgectl/internal/telemetry"
)

// TestQuestionTelemetryPipeline proves TEL-001 through TEL-005 across the
// public analyzer and sink boundary: observations become private JSONL events
// and deterministic, provider-neutral feedback without retaining credentials.
func TestQuestionTelemetryPipeline(t *testing.T) {
	eventsPath := filepath.Join(t.TempDir(), "private", "events.jsonl")
	if err := os.MkdirAll(filepath.Dir(eventsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(eventsPath, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	analyzer := telemetry.NewAnalyzer(telemetry.NewJSONLSink(eventsPath), nil)
	session := telemetry.Session{SourceID: "bridge-e2e", SessionID: "session-1", ProjectID: "project-1", Provider: "codex"}
	analyzer.ObserveOutput(session, []byte("\x1b[33mAuthorization: Bearer top-secret-1\x1b[0m\nDo you want me to run /tmp/build-42?"))
	analyzer.ObserveInput(session, []byte("yes\n"))
	analyzer.ObserveOutput(session, []byte("Authorization: Bearer top-secret-2\nDo you want me to run /var/build-99?"))
	analyzer.ObserveInput(session, []byte("no\n"))

	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range [][]byte{[]byte("top-secret-1"), []byte("top-secret-2")} {
		if bytes.Contains(data, secret) {
			t.Fatalf("persisted telemetry contains credential %q: %s", secret, data)
		}
	}

	var events []telemetry.Event
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var event telemetry.Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode JSONL event: %v", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("events=%d, want 4", len(events))
	}
	for _, event := range events {
		if event.SourceID != "bridge-e2e" {
			t.Fatalf("event source_id=%q, want bridge-e2e", event.SourceID)
		}
	}
	if events[0].Kind != telemetry.EventQuestion || events[0].Class != telemetry.ClassPermission {
		t.Fatalf("first event=%+v, want permission question", events[0])
	}
	if events[1].Decision != telemetry.DecisionAccepted || events[3].Decision != telemetry.DecisionRejected {
		t.Fatalf("answer decisions=%q/%q, want accepted/rejected", events[1].Decision, events[3].Decision)
	}

	info, err := os.Stat(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); runtime.GOOS != "windows" && got != 0o600 {
		t.Fatalf("event file mode=%#o, want 0600", got)
	}

	feedback := analyzer.Feedback()
	if feedback.SchemaVersion != 1 || len(feedback.Questions) != 1 {
		t.Fatalf("feedback=%+v, want one schema-v1 aggregate", feedback)
	}
	question := feedback.Questions[0]
	if question.Asked != 2 || question.Accepted != 1 || question.Rejected != 1 {
		t.Fatalf("question aggregate=%+v, want asked=2 accepted=1 rejected=1", question)
	}
	exported, err := feedback.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(exported, []byte("top-secret")) {
		t.Fatalf("feedback contains credential: %s", exported)
	}
}
