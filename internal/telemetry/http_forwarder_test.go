package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

func newTestSink(t *testing.T, endpoint string, onError func(error)) *HTTPForwardingSink {
	t.Helper()
	spool, err := NewSegmentSpool(t.TempDir(), 1<<20, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	sink := NewHTTPForwardingSink(spool, endpoint, "brc_test-credential", 50*time.Millisecond, 10*time.Millisecond, onError)
	t.Cleanup(func() { _ = sink.Close(context.Background()) })
	return sink
}

// TestNewHTTPForwardingSinkCapsInitialRetryInterval guards against a valid
// but large telemetry.retry_interval configuration waiting longer than the
// documented 30s maximum backoff on its very first failed retry.
func TestNewHTTPForwardingSinkCapsInitialRetryInterval(t *testing.T) {
	spool, err := NewSegmentSpool(t.TempDir(), 1<<20, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	sink := NewHTTPForwardingSink(spool, "https://example.invalid", "brc_test-credential", time.Second, time.Hour, func(error) {})
	defer func() { _ = sink.Close(context.Background()) }()
	if sink.retryInterval != httpForwarderMaxBackoff {
		t.Fatalf("expected initial retryInterval capped at %v, got %v", httpForwarderMaxBackoff, sink.retryInterval)
	}
}

func TestHTTPForwardingSinkDeliversWithAuthorizationHeader(t *testing.T) {
	var gotAuth atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	sink := newTestSink(t, server.URL, func(err error) { t.Errorf("unexpected delivery error: %v", err) })
	if err := sink.Record(Event{SchemaVersion: 2, Timestamp: time.Now().UTC(), SessionID: "s1", Provider: "claude", Kind: EventSessionStarted, Sequence: 1}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pending, err := sink.spool.Pending(); err == nil && len(pending) == 0 {
			if auth, _ := gotAuth.Load().(string); auth == "Bearer brc_test-credential" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("segment was not delivered with the expected Authorization header (got %v)", gotAuth.Load())
}

func TestHTTPForwardingSinkRetainsSegmentOnNon2xxAndRetries(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	var errCount atomic.Int32
	sink := newTestSink(t, server.URL, func(error) { errCount.Add(1) })
	if err := sink.Record(Event{SchemaVersion: 2, Timestamp: time.Now().UTC(), SessionID: "s1", Provider: "claude", Kind: EventSessionStarted, Sequence: 1}); err != nil {
		t.Fatal(err)
	}

	// Wait for the third (successful) attempt: attempts 1 and 2 return 500 and
	// must leave the segment retained in the spool (retried), not dropped.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && attempts.Load() < 3 {
		time.Sleep(10 * time.Millisecond)
	}
	if attempts.Load() < 3 {
		t.Fatalf("expected 3 attempts (2 failures then success), got %d", attempts.Load())
	}
	if errCount.Load() < 2 {
		t.Fatalf("expected at least 2 failed-delivery callbacks, got %d", errCount.Load())
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pending, err := sink.spool.Pending(); err == nil && len(pending) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("segment still pending after the successful attempt")
}

// TestHTTPForwardingSinkCloseCancelsInFlightUpload guards against Close
// blocking past its caller's deadline: a stalled collector must not keep the
// delivery worker alive once the shutdown context is done.
func TestHTTPForwardingSinkCloseCancelsInFlightUpload(t *testing.T) {
	unblock := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-unblock
		w.WriteHeader(http.StatusAccepted)
	}))
	defer func() {
		close(unblock)
		server.Close()
	}()

	spool, err := NewSegmentSpool(t.TempDir(), 1<<20, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	sink := NewHTTPForwardingSink(spool, server.URL, "brc_test-credential", 50*time.Millisecond, 10*time.Millisecond, func(error) {})
	if err := sink.Record(Event{SchemaVersion: 2, Timestamp: time.Now().UTC(), SessionID: "s1", Provider: "claude", Kind: EventSessionStarted, Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // let the worker start the blocked request

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = sink.Close(ctx)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Close took %v, want it to return promptly once its context expires", elapsed)
	}
	if err == nil {
		t.Fatal("expected Close to report the expired context")
	}
}
