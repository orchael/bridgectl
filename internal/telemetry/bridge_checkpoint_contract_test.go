package telemetry

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// Optional output makes the real collector's wire bytes consumable by Bridge's
// validator tests, catching contract drift across the two repositories.
func TestBridgeCheckpointContract(t *testing.T) {
	sink := &memorySink{}
	collector := NewLiveCollector(sink, 128, true, nil, EventProviderOutput)
	session := Session{SessionID: "checkpoint-contract", Provider: "codex", Cols: 40, Rows: 4}
	collector.ObserveOutputChunk(session, []byte("\x1b[?2026h\x1b[31mFIRST 界\x1b[0m\r\nsecond\r\nthird\r\nfourth\r\nfifth safe words \x1b[?2026l"))
	collector.SessionEnded(session)
	if err := collector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var data []byte
	for _, event := range sink.snapshot() {
		if event.Kind != EventProviderOutput || event.Terminal == nil {
			continue
		}
		event.Timestamp = time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, raw...)
		data = append(data, '\n')
	}
	if len(data) == 0 {
		t.Fatal("collector produced no terminal checkpoint")
	}
	batches, err := bridgeBatches("20261008T000001.000000000Z-contract", data, "contract-test")
	if err != nil || len(batches) != 1 {
		t.Fatalf("batches=%d error=%v", len(batches), err)
	}
	if !strings.Contains(string(batches[0].Body), `"terminal"`) || !strings.Contains(string(batches[0].Body), "FIRST") {
		t.Fatal("transport dropped checkpoint or scrollback")
	}
	if path := os.Getenv("BRIDGE_CHECKPOINT_FIXTURE"); path != "" {
		if err := os.WriteFile(path, batches[0].Body, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
