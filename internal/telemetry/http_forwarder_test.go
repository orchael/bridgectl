package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

	raw, err := bridgeEnvelope("20260918T000000.000000000Z-test", line, "dev")
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

// TestBridgeEnvelopeForwardsFullEventData guards against silently dropping
// most of a normalized Event when forwarding it to Bridge: previously only
// schema_version, sequence, class, decision, and byte_count survived, losing
// project/actor identity, direction/stream, fingerprint, redacted text,
// redaction metadata, latency, and session context.
func TestBridgeEnvelopeForwardsFullEventData(t *testing.T) {
	event := Event{
		SchemaVersion: 2,
		Timestamp:     time.Now().UTC(),
		SessionID:     "session-1",
		ProjectID:     "local",
		ActorID:       "user-7",
		Provider:      "claude",
		Direction:     DirectionHuman,
		Kind:          EventQuestion,
		Stream:        StreamInput,
		Sequence:      3,
		Class:         "clarifying",
		Decision:      "asked",
		Fingerprint:   "fp-abc",
		Text:          "redacted question text",
		ByteCount:     42,
		Redactions:    2,
		ContentHash:   "sha256:deadbeef",
		OmittedReason: "",
		LatencyMS:     150,
		Context: &SessionContext{
			ActorID: "user-7", SourceLabel: "laptop", OS: "linux", Arch: "amd64",
			MachineID: "machine-hash", WorkingDirectoryID: "wd-hash", RepositoryID: "repo-hash",
			Branch: "main", CommitSHA: "abc123",
		},
	}
	line, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := bridgeEnvelope("20260918T000000.000000000Z-test", line, "dev")
	if err != nil {
		t.Fatal(err)
	}

	var envelope struct {
		Segment struct {
			Events []struct {
				ActorRef      string         `json:"actor_ref"`
				RepositoryRef string         `json:"repository_ref"`
				Payload       map[string]any `json:"payload"`
			} `json:"events"`
		} `json:"segment"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Segment.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(envelope.Segment.Events))
	}
	got := envelope.Segment.Events[0]

	// actor_ref/repository_ref are Bridge's sanctioned top-level slots.
	if got.ActorRef != "user-7" {
		t.Fatalf("actor_ref = %q, want user-7", got.ActorRef)
	}
	if got.RepositoryRef != "repo-hash" {
		t.Fatalf("repository_ref = %q, want repo-hash", got.RepositoryRef)
	}

	for key, want := range map[string]any{
		"project_id":     "local",
		"direction":      "human",
		"stream":         "input",
		"fingerprint":    "fp-abc",
		"text":           "redacted question text",
		"redactions":     float64(2),
		"content_sha256": "sha256:deadbeef",
		"latency_ms":     float64(150),
	} {
		if got.Payload[key] != want {
			t.Fatalf("payload[%q] = %v, want %v", key, got.Payload[key], want)
		}
	}

	context, ok := got.Payload["context"].(map[string]any)
	if !ok {
		t.Fatalf("expected payload.context to be an object, got %v", got.Payload["context"])
	}
	// Bridge's checkTelemetryJSON forbids actor_id and source_label
	// anywhere in the body (matched case/separator-insensitively): actor
	// identity must travel only via the top-level actor_ref field, and
	// source_label must never be sent to Bridge at all.
	if _, present := context["actor_id"]; present {
		t.Fatal("payload.context must not include actor_id (forbidden_field on Bridge)")
	}
	if _, present := context["source_label"]; present {
		t.Fatal("payload.context must not include source_label (forbidden_field on Bridge)")
	}
	for key, want := range map[string]any{
		"os": "linux", "arch": "amd64", "machine_id": "machine-hash",
		"working_directory_id": "wd-hash", "repository_id": "repo-hash",
		"branch": "main", "commit_sha": "abc123",
	} {
		if context[key] != want {
			t.Fatalf("payload.context[%q] = %v, want %v", key, context[key], want)
		}
	}
}

func TestHTTPForwardingSinkRecordFailsAfterClose(t *testing.T) {
	spool, err := NewSegmentSpool(t.TempDir(), 1<<20, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(202) }))
	defer server.Close()
	sink := NewHTTPForwardingSink(spool, server.URL, "brc_test-credential", "1.2.3", time.Hour, time.Hour, func(error) {})
	if err := sink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sink.Record(Event{SchemaVersion: 2, Timestamp: time.Now().UTC(), SessionID: "s1", Kind: EventSessionStarted}); err != ErrSinkClosed {
		t.Fatalf("expected ErrSinkClosed after Close, got %v", err)
	}
}

func newTestSink(t *testing.T, endpoint string, onError func(error)) *HTTPForwardingSink {
	t.Helper()
	spool, err := NewSegmentSpool(t.TempDir(), 1<<20, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	sink := NewHTTPForwardingSink(spool, endpoint, "brc_test-credential", "1.2.3", 50*time.Millisecond, 10*time.Millisecond, onError)
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
	sink := NewHTTPForwardingSink(spool, "https://example.invalid", "brc_test-credential", "", time.Second, time.Hour, func(error) {})
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
	sink := NewHTTPForwardingSink(spool, server.URL, "brc_test-credential", "1.2.3", 50*time.Millisecond, 10*time.Millisecond, func(error) {})
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

// TestBridgeEnvelopeUsesCollectorVersion guards against every uploaded
// segment reporting the hardcoded "dev" placeholder even from release
// binaries, which the Makefile/release build injects a real version into.
func TestBridgeEnvelopeUsesCollectorVersion(t *testing.T) {
	event := Event{SchemaVersion: 2, Timestamp: time.Now().UTC(), SessionID: "s1", Kind: EventSessionStarted}
	line, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := bridgeEnvelope("20260918T000000.000000000Z-test", line, "1.4.2")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Segment struct {
			Collector struct {
				Version string `json:"version"`
			} `json:"collector"`
		} `json:"segment"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Segment.Collector.Version != "1.4.2" {
		t.Fatalf("collector.version = %q, want 1.4.2", envelope.Segment.Collector.Version)
	}
}

// TestHTTPForwardingSinkErrorIncludesBridgeErrorCodeNotRawBody guards two
// things at once: a non-2xx response's structured Bridge error code (e.g.
// unsupported_event_schema) must reach the returned error so persistent
// failures are diagnosable, but the raw response body must never be
// included, since a malicious or misconfigured endpoint's response could
// otherwise get logged verbatim (and could in principle reflect the brc_
// Authorization header back).
func TestHTTPForwardingSinkErrorIncludesBridgeErrorCodeNotRawBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"error":"unsupported_event_schema","authorization_echo":"Bearer brc_should-not-be-logged"}`))
	}))
	defer server.Close()

	var gotErr error
	errCh := make(chan struct{}, 1)
	sink := NewHTTPForwardingSink(newTestSpool(t), server.URL, "brc_test-credential", "1.2.3", time.Hour, time.Hour, func(err error) {
		gotErr = err
		select {
		case errCh <- struct{}{}:
		default:
		}
	})
	defer func() { _ = sink.Close(context.Background()) }()
	if err := sink.Record(Event{SchemaVersion: 2, Timestamp: time.Now().UTC(), SessionID: "s1", Kind: EventSessionStarted}); err != nil {
		t.Fatal(err)
	}

	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a delivery error callback")
	}
	if gotErr == nil || !strings.Contains(gotErr.Error(), "unsupported_event_schema") {
		t.Fatalf("expected the error to include Bridge's error code, got %v", gotErr)
	}
	if strings.Contains(gotErr.Error(), "brc_should-not-be-logged") {
		t.Fatalf("error must not include the raw response body: %v", gotErr)
	}
}

func newTestSpool(t *testing.T) *SegmentSpool {
	t.Helper()
	spool, err := NewSegmentSpool(t.TempDir(), 1<<20, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	return spool
}
