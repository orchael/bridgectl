package telemetry

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBuildReportRollingWindow(t *testing.T) {
	now := time.Date(2026, 9, 13, 20, 0, 0, 0, time.UTC)
	events := []Event{
		{Timestamp: now.Add(-3 * time.Hour), SessionID: "old", Kind: EventQuestion, Fingerprint: "old"},
		{Timestamp: now.Add(-90 * time.Minute), SessionID: "s1", Provider: "codex", Kind: EventSessionStarted},
		{Timestamp: now.Add(-60 * time.Minute), SessionID: "s1", Kind: EventQuestion, Class: ClassPermission, Fingerprint: "same", Text: "Proceed?"},
		{Timestamp: now.Add(-59 * time.Minute), SessionID: "s1", Kind: EventAnswer, Fingerprint: "same", Decision: DecisionAccepted, LatencyMS: 1000},
		{Timestamp: now.Add(-30 * time.Minute), SessionID: "s1", Kind: EventQuestion, Class: ClassPermission, Fingerprint: "same", Text: "Proceed?"},
		{Timestamp: now.Add(-29 * time.Minute), SessionID: "s1", Kind: EventAnswer, Fingerprint: "same", Decision: DecisionChanged, LatencyMS: 3000},
		{Timestamp: now, SessionID: "s1", Kind: EventSessionEnded},
	}
	report := BuildReport(events, ReportOptions{Since: now.Add(-2 * time.Hour), Until: now, Top: 5})
	if report.Sessions != 1 || report.Questions != 2 {
		t.Fatalf("report=%+v", report)
	}
	if report.AgentHours != 1.5 || report.QuestionsPerSession != 2 || report.MedianLatencyMS != 2000 {
		t.Fatalf("rates/latency=%+v", report)
	}
	if report.AcceptanceRate != 0.5 || report.ChangeRate != 0.5 || len(report.TopQuestions) != 1 {
		t.Fatalf("outcomes/top=%+v", report)
	}

	feedback := BuildFeedback(events, now.Add(-2*time.Hour), now)
	if feedback.SchemaVersion != 1 || len(feedback.Questions) != 1 || feedback.Questions[0].Asked != 2 {
		t.Fatalf("feedback=%+v", feedback)
	}
}

func TestBuildReportCountsCompositeSessionIdentities(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	events := []Event{
		{Timestamp: now.Add(-time.Hour), SourceID: "bridge-a", SessionID: "shared", Kind: EventSessionStarted},
		{Timestamp: now.Add(-time.Hour), SourceID: "bridge-b", SessionID: "shared", Kind: EventSessionStarted},
		{Timestamp: now.Add(-time.Minute), SourceID: "bridge-a", SessionID: "shared", Kind: EventSessionEnded},
		{Timestamp: now.Add(-time.Minute), SourceID: "bridge-b", SessionID: "shared", Kind: EventSessionEnded},
	}
	report := BuildReport(events, ReportOptions{Since: now.Add(-2 * time.Hour), Until: now})
	if report.Sessions != 2 {
		t.Fatalf("Sessions=%d, want two composite identities", report.Sessions)
	}
}

func TestBuildReportFromPathStreamsCompositeSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	sink := NewJSONLSink(path)
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, event := range []Event{
		{Timestamp: now.Add(-time.Hour), SourceID: "bridge-a", SessionID: "shared", Kind: EventSessionStarted},
		{Timestamp: now.Add(-time.Hour), SourceID: "bridge-b", SessionID: "shared", Kind: EventSessionStarted},
		{Timestamp: now.Add(-time.Minute), SourceID: "bridge-a", SessionID: "shared", Kind: EventSessionEnded},
		{Timestamp: now.Add(-time.Minute), SourceID: "bridge-b", SessionID: "shared", Kind: EventSessionEnded},
	} {
		if err := sink.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	report, err := BuildReportFromPath(path, ReportOptions{Since: now.Add(-2 * time.Hour), Until: now})
	if err != nil {
		t.Fatal(err)
	}
	if report.Sessions != 2 {
		t.Fatalf("Sessions=%d, want two composite identities", report.Sessions)
	}
}

func TestReadEventsFromSegmentDirectory(t *testing.T) {
	dir := t.TempDir()
	first := eventJSONL(t, Event{Timestamp: time.Now(), SessionID: "s1", Kind: EventQuestion})
	second := eventJSONL(t, Event{Timestamp: time.Now(), SessionID: "s1", Kind: EventAnswer})
	if err := os.WriteFile(filepath.Join(dir, "20260914T000000.000000000Z-a.jsonl"), first, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "20260914T000001.000000000Z-b.jsonl"), second, 0o600); err != nil {
		t.Fatal(err)
	}
	events, err := ReadEvents(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Kind != EventQuestion || events[1].Kind != EventAnswer {
		t.Fatalf("events=%+v, want ordered question/answer", events)
	}
}

func TestBuildReportOmitsAnswerWhoseQuestionPredatesWindowFromTopQuestions(t *testing.T) {
	now := time.Date(2026, 9, 13, 20, 0, 0, 0, time.UTC)
	report := BuildReport([]Event{
		{Timestamp: now.Add(-time.Minute), SessionID: "s1", Kind: EventAnswer, Fingerprint: "old-question", Decision: DecisionAccepted},
	}, ReportOptions{Since: now.Add(-time.Hour), Until: now})
	if len(report.TopQuestions) != 0 {
		t.Fatalf("TopQuestions=%+v, want no zero-ask entries", report.TopQuestions)
	}
}
