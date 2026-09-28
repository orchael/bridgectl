package telemetry

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// MAR-106: local spool rotation must never determine the HTTP request limits.
func TestHTTPUploadBoundsAndRetries(t *testing.T) {
	spool, err := NewSegmentSpool(t.TempDir(), 10<<20, 20<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2100; i++ {
		text := strings.Repeat("<", 100)
		if i == 1001 {
			text = strings.Repeat("界", 400000)
		}
		if err := spool.Record(Event{SchemaVersion: 2, Timestamp: time.Now().UTC(), SessionID: "s1", Provider: "claude", Kind: EventProviderOutput, Sequence: uint64(i + 1), Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string][]byte{}
	fail := true
	count := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if len(body) > 1<<20 {
			t.Errorf("request is %d bytes, above Bridge limit", len(body))
			w.WriteHeader(413)
			return
		}
		var env struct {
			SegmentID string `json:"segment_id"`
			Segment   struct {
				Events []json.RawMessage `json:"events"`
			} `json:"segment"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			t.Error(err)
		}
		if len(env.Segment.Events) > 1000 {
			t.Errorf("too many events: %d", len(env.Segment.Events))
		}
		if prior, ok := seen[env.SegmentID]; ok && !bytes.Equal(prior, body) {
			t.Error("retry changed payload")
		}
		seen[env.SegmentID] = body
		count++
		if fail && count == 2 {
			w.WriteHeader(503)
			return
		}
		ackTelemetryID(w, env.SegmentID)
	}))
	defer server.Close()
	sink := &HTTPForwardingSink{spool: spool, endpoint: server.URL, version: "test", client: server.Client()}
	if err := sink.upload(context.Background()); err == nil {
		t.Fatal("expected partial failure")
	}
	pending, _ := spool.Pending()
	if len(pending) == 0 {
		t.Fatal("removed unacknowledged data")
	}
	fail = false
	if err := sink.upload(context.Background()); err != nil {
		t.Fatal(err)
	}
	pending, _ = spool.Pending()
	if len(pending) != 0 {
		t.Fatal("successful retry retained data")
	}
	if len(seen) < 3 {
		t.Fatalf("expected multiple bounded batches, got %d", len(seen))
	}
}

func TestHTTPRejectedSegmentDoesNotBlockLaterSegments(t *testing.T) {
	spool, err := NewSegmentSpool(t.TempDir(), 1<<20, 4<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range []string{"bad", "good"} {
		if err := spool.Record(Event{SchemaVersion: 2, Timestamp: time.Now().UTC(), SessionID: session, Kind: EventSessionStarted}); err != nil {
			t.Fatal(err)
		}
		if _, err := spool.Seal(); err != nil {
			t.Fatal(err)
		}
	}
	gotGood := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"session_id":"bad"`)) {
			w.WriteHeader(422)
			return
		}
		gotGood = true
		var env struct {
			SegmentID string `json:"segment_id"`
		}
		_ = json.Unmarshal(body, &env)
		ackTelemetryID(w, env.SegmentID)
	}))
	defer server.Close()
	sink := &HTTPForwardingSink{spool: spool, endpoint: server.URL, version: "test", client: server.Client()}
	if err := sink.upload(context.Background()); err == nil {
		t.Fatal("expected rejection")
	}
	if !gotGood {
		t.Fatal("rejected segment blocked later segment")
	}
	pending, _ := spool.Pending()
	if len(pending) != 1 {
		t.Fatalf("pending=%d, want rejected segment retained", len(pending))
	}
}

func TestHTTPUploadRequiresS3Receipt(t *testing.T) {
	for _, response := range []string{"", `{"status":"accepted","storage":"database"}`, `{"segment_id":"wrong","status":"accepted","storage":"s3","object_key":"key"}`} {
		t.Run(response, func(t *testing.T) {
			spool, err := NewSegmentSpool(t.TempDir(), 1<<20, 2<<20, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := spool.Record(Event{SchemaVersion: 2, Timestamp: time.Now().UTC(), SessionID: "s1", Kind: EventSessionStarted}); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(202); _, _ = w.Write([]byte(response)) }))
			defer server.Close()
			sink := &HTTPForwardingSink{spool: spool, endpoint: server.URL, version: "test", client: server.Client()}
			if err := sink.upload(context.Background()); err == nil {
				t.Fatal("unverified success removed telemetry")
			}
			pending, _ := spool.Pending()
			if len(pending) != 1 {
				t.Fatal("missing retained segment")
			}
		})
	}
}

func TestBridgeBatchesLosslesslyReassembleOversizedEvent(t *testing.T) {
	original := Event{SchemaVersion: 2, Timestamp: time.Now().UTC(), SessionID: "s1", Provider: "claude", Kind: EventProviderOutput, Sequence: 42, Text: strings.Repeat("<界", 300000)}
	jsonl, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	id := "20260928T180000.000000000Z-parent"
	unsplit, err := bridgeEnvelope(id, jsonl, "test")
	if err != nil {
		t.Fatal(err)
	}
	var originalEnvelope struct {
		Segment struct {
			Events []json.RawMessage `json:"events"`
		} `json:"segment"`
	}
	if err := json.Unmarshal(unsplit, &originalEnvelope); err != nil {
		t.Fatal(err)
	}
	batches, err := bridgeBatches(id, jsonl, "test")
	if err != nil {
		t.Fatal(err)
	}
	chunks := map[int][]byte{}
	count := 0
	for _, batch := range batches {
		if len(batch.Body) > 1<<20 {
			t.Fatal("oversized fragment batch")
		}
		var env struct {
			Checksum string          `json:"segment_checksum"`
			Segment  json.RawMessage `json:"segment"`
		}
		if err := json.Unmarshal(batch.Body, &env); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(env.Segment)
		if env.Checksum != "sha256:"+hex.EncodeToString(sum[:]) {
			t.Fatal("invalid batch checksum")
		}
		var seg struct {
			Events []struct {
				Payload struct {
					Fragment struct {
						Index int    `json:"index"`
						Count int    `json:"count"`
						Data  string `json:"data"`
						Hash  string `json:"sha256"`
					} `json:"fragment"`
				} `json:"payload"`
			} `json:"events"`
		}
		if err := json.Unmarshal(env.Segment, &seg); err != nil {
			t.Fatal(err)
		}
		for _, e := range seg.Events {
			f := e.Payload.Fragment
			count = f.Count
			decoded, err := base64.StdEncoding.DecodeString(f.Data)
			if err != nil {
				t.Fatal(err)
			}
			chunks[f.Index] = decoded
			sum := sha256.Sum256(originalEnvelope.Segment.Events[0])
			if f.Hash != hex.EncodeToString(sum[:]) {
				t.Fatal("wrong original hash")
			}
		}
	}
	var restored []byte
	for i := 0; i < count; i++ {
		restored = append(restored, chunks[i]...)
	}
	if !bytes.Equal(restored, originalEnvelope.Segment.Events[0]) {
		t.Fatal("fragmentation changed original event")
	}
	again, err := bridgeBatches(id, jsonl, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(batches, again) {
		t.Fatal("restarted batch generation is not deterministic")
	}
	upgraded, err := bridgeBatches(id, jsonl, "next-version")
	if err != nil {
		t.Fatal(err)
	}
	if upgraded[0].ID == batches[0].ID {
		t.Fatal("changed bytes reused immutable batch ID")
	}
}

func TestHTTPTransientFailureStopsPassForBackoff(t *testing.T) {
	for _, status := range []int{401, 403, 408, 429, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			spool, err := NewSegmentSpool(t.TempDir(), 1<<20, 3<<20, nil)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				if err := spool.Record(Event{SchemaVersion: 2, Timestamp: time.Now().UTC(), SessionID: "s1", Kind: EventSessionStarted}); err != nil {
					t.Fatal(err)
				}
				if _, err := spool.Seal(); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(status) }))
			defer server.Close()
			sink := &HTTPForwardingSink{spool: spool, endpoint: server.URL, version: "test", client: server.Client()}
			if err := sink.upload(context.Background()); err == nil {
				t.Fatal("expected failure")
			}
			if calls != 1 {
				t.Fatalf("made %d requests during outage, want backoff after first", calls)
			}
		})
	}
}
