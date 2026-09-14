package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orchael/bridgectl/internal/telemetry"
)

func TestTelemetryReportAndExportCommands(t *testing.T) {
	t.Setenv("BRIDGECTL_STATE_DIR", t.TempDir())
	path := filepath.Join(t.TempDir(), "events.jsonl")
	sink := telemetry.NewJSONLSink(path)
	now := time.Now().UTC()
	for _, event := range []telemetry.Event{
		{Timestamp: now.Add(-time.Hour), SessionID: "s1", Kind: telemetry.EventSessionStarted},
		{Timestamp: now.Add(-time.Minute), SessionID: "s1", Kind: telemetry.EventQuestion, Class: telemetry.ClassPermission, Fingerprint: "abc", Text: "Proceed?"},
		{Timestamp: now, SessionID: "s1", Kind: telemetry.EventAnswer, Fingerprint: "abc", Decision: telemetry.DecisionAccepted, LatencyMS: 500},
		{Timestamp: now, SessionID: "s1", Kind: telemetry.EventSessionEnded},
	} {
		if err := sink.Record(event); err != nil {
			t.Fatal(err)
		}
	}

	report := newTelemetryCmd()
	report.SetArgs([]string{"report", "--events", path, "--since", "2h"})
	var reportOut bytes.Buffer
	report.SetOut(&reportOut)
	if err := report.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := reportOut.String(); !strings.Contains(got, "Questions/session") || !strings.Contains(got, "Top fingerprints") {
		t.Fatalf("report output=%q", got)
	}

	export := newTelemetryCmd()
	export.SetArgs([]string{"export", "--events", path, "--since", "2h", "--format", "json"})
	var exportOut bytes.Buffer
	export.SetOut(&exportOut)
	if err := export.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := exportOut.String(); !strings.Contains(got, `"schema_version": 1`) || strings.Contains(got, "secret") {
		t.Fatalf("export output=%q", got)
	}
}

func TestTelemetryExportRejectsUnknownFormat(t *testing.T) {
	t.Setenv("BRIDGECTL_STATE_DIR", t.TempDir())
	cmd := newTelemetryCmd()
	cmd.SetArgs([]string{"export", "--format", "ballast"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "want json") {
		t.Fatalf("error=%v, want unsupported-format error", err)
	}
}

func TestParseTelemetryEventKinds(t *testing.T) {
	kinds, err := parseTelemetryEventKinds([]string{"question", "answer"})
	if err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 2 || kinds[0] != telemetry.EventQuestion || kinds[1] != telemetry.EventAnswer {
		t.Fatalf("kinds=%v", kinds)
	}
	if _, err := parseTelemetryEventKinds([]string{"transcript"}); err == nil {
		t.Fatal("unknown kind accepted")
	}
	all, err := parseTelemetryEventKinds([]string{"all"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("all kinds=%v, want empty collector allow-list", all)
	}
	if _, err := parseTelemetryEventKinds([]string{"all", "question"}); err == nil {
		t.Fatal("all combined with a concrete kind was accepted")
	}
}

func TestTelemetryCollectorRetentionDefaultsToOneGB(t *testing.T) {
	cmd := newTelemetryCollectCmd()
	maxSegmentBytes, err := cmd.Flags().GetInt64("max-segment-bytes")
	if err != nil {
		t.Fatal(err)
	}
	maxDiskSpace, err := cmd.Flags().GetString("max-disk-space")
	if err != nil {
		t.Fatal(err)
	}
	if maxSegmentBytes != 10<<20 || maxDiskSpace != "1GB" {
		t.Fatalf("collector retention defaults: segment=%d disk=%q, want segment=%d disk=1GB", maxSegmentBytes, maxDiskSpace, 10<<20)
	}
	if cmd.Flags().Lookup("max-segments") != nil {
		t.Fatal("removed --max-segments flag is still registered")
	}
}

func TestTelemetryCollectorRejectsInvalidDiskSpace(t *testing.T) {
	for _, value := range []string{"invalid", "0B", "1MiB"} {
		t.Run(value, func(t *testing.T) {
			cmd := newTelemetryCollectCmd()
			cmd.SetArgs([]string{"--max-segment-bytes", "10485760", "--max-disk-space", value})
			err := cmd.Execute()
			if err == nil || (!strings.Contains(err.Error(), "max-disk-space") && !strings.Contains(err.Error(), "max disk bytes")) {
				t.Fatalf("error=%v, want disk-space validation error", err)
			}
		})
	}
}

func TestTelemetryCommandMissingEvents(t *testing.T) {
	t.Setenv("BRIDGECTL_STATE_DIR", t.TempDir())
	cmd := newTelemetryCmd()
	cmd.SetArgs([]string{"report", "--events", filepath.Join(t.TempDir(), "missing.jsonl")})
	cmd.SetOut(&bytes.Buffer{})
	if err := cmd.Execute(); !os.IsNotExist(err) {
		t.Fatalf("error=%v, want not-exist", err)
	}
}

func TestTelemetryReportUsesConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	eventsPath := filepath.Join(dir, "configured-segments")
	sink, err := telemetry.NewSegmentSpool(eventsPath, 1<<20, 5<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, event := range []telemetry.Event{
		{Timestamp: now.Add(-3 * time.Hour), SessionID: "old", Kind: telemetry.EventQuestion, Fingerprint: "old"},
		{Timestamp: now.Add(-time.Hour), SessionID: "recent", Kind: telemetry.EventQuestion, Fingerprint: "recent"},
	} {
		if err := sink.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	configPath := filepath.Join(dir, "bridge.yaml")
	if _, err := sink.Seal(); err != nil {
		t.Fatal(err)
	}
	configYAML := "telemetry:\n  spool_dir: " + eventsPath + "\n  rolling_window: 2h\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := newTelemetryCmd()
	cmd.SetArgs([]string{"report", "--config", configPath})
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "Questions:              1") {
		t.Fatalf("configured report output=%q", got)
	}
}
