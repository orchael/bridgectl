package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// TestGRPCCollectorDurableAckAndReplay proves TEL-111 and TEL-112: an ack is
// emitted after durable acceptance and replaying the same ID is idempotent.
func TestGRPCCollectorDurableAckAndReplay(t *testing.T) {
	spool, err := NewSegmentSpool(t.TempDir(), 1<<20, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, cleanup := startCollectorTestServer(t, NewGRPCCollectorServer(spool, 1<<20, EventQuestion, EventAnswer))
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.StreamSegments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	payload := eventJSONL(t, Event{Timestamp: time.Now(), SessionID: "session-1", Kind: EventQuestion})
	for index := 0; index < 2; index++ {
		if err := stream.Send(&bridgev1.TelemetrySegment{Id: "batch-1", Jsonl: payload}); err != nil {
			t.Fatal(err)
		}
		ack, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if ack.GetId() != "batch-1" || ack.GetStored() != (index == 0) {
			t.Fatalf("ack %d=%+v", index, ack)
		}
	}
	segments, err := spool.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 || segments[0].ID != "batch-1" {
		t.Fatalf("segments=%v, want one batch-1", segments)
	}
}

func TestGRPCCollectorRejectsInvalidOrFilteredSegments(t *testing.T) {
	tests := []struct {
		name    string
		segment *bridgev1.TelemetrySegment
		code    codes.Code
	}{
		{name: "unsafe ID", segment: &bridgev1.TelemetrySegment{Id: "../bad", Jsonl: eventJSONL(t, Event{Kind: EventQuestion})}, code: codes.InvalidArgument},
		{name: "invalid JSONL", segment: &bridgev1.TelemetrySegment{Id: "bad-json", Jsonl: []byte("not-json\n")}, code: codes.InvalidArgument},
		{name: "filtered kind", segment: &bridgev1.TelemetrySegment{Id: "wrong-kind", Jsonl: eventJSONL(t, Event{Kind: EventSessionStarted})}, code: codes.InvalidArgument},
		{name: "too large", segment: &bridgev1.TelemetrySegment{Id: "too-large", Jsonl: eventJSONL(t, Event{Kind: EventQuestion, Text: "long"})}, code: codes.ResourceExhausted},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spool, err := NewSegmentSpool(t.TempDir(), 1<<20, 10<<20, nil)
			if err != nil {
				t.Fatal(err)
			}
			limit := int64(1 << 20)
			if test.name == "too large" {
				limit = 8
			}
			client, cleanup := startCollectorTestServer(t, NewGRPCCollectorServer(spool, limit, EventQuestion, EventAnswer))
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := client.StreamSegments(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.Send(test.segment); err != nil {
				t.Fatal(err)
			}
			_, err = stream.Recv()
			if status.Code(err) != test.code {
				t.Fatalf("error=%v code=%s, want %s", err, status.Code(err), test.code)
			}
		})
	}
}

func TestGRPCCollectorRejectsMalformedFullInteractionEvents(t *testing.T) {
	tests := []struct {
		name  string
		event Event
	}{
		{name: "provider direction", event: Event{SchemaVersion: 1, Sequence: 1, Kind: EventProviderOutput, Direction: DirectionHuman, Stream: StreamOutput, ByteCount: 1}},
		{name: "provider stream", event: Event{SchemaVersion: 1, Sequence: 1, Kind: EventProviderOutput, Direction: DirectionAgent, Stream: StreamInput, ByteCount: 1}},
		{name: "user direction", event: Event{SchemaVersion: 1, Sequence: 1, Kind: EventUserInput, Direction: DirectionAgent, Stream: StreamInput, ByteCount: 1}},
		{name: "missing sequence", event: Event{SchemaVersion: 1, Kind: EventUserInput, Direction: DirectionHuman, Stream: StreamInput, ByteCount: 1}},
		{name: "opaque content leaked", event: Event{SchemaVersion: 1, Sequence: 1, Kind: EventProviderOutput, Direction: DirectionAgent, Stream: StreamOutput, ByteCount: 2, Text: "unsafe", OmittedReason: OmittedInvalidUTF8, ContentHash: "hash"}},
		{name: "unredacted content", event: Event{SchemaVersion: 1, Sequence: 1, Kind: EventProviderOutput, Direction: DirectionAgent, Stream: StreamOutput, ByteCount: 20, Text: "token=collector-secret"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spool, err := NewSegmentSpool(t.TempDir(), 1<<20, 10<<20, nil)
			if err != nil {
				t.Fatal(err)
			}
			client, cleanup := startCollectorTestServer(t, NewGRPCCollectorServer(spool, 1<<20))
			defer cleanup()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := client.StreamSegments(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.Send(&bridgev1.TelemetrySegment{Id: "bad-interaction", Jsonl: eventJSONL(t, test.event)}); err != nil {
				t.Fatal(err)
			}
			if _, err := stream.Recv(); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("error=%v code=%s, want %s", err, status.Code(err), codes.InvalidArgument)
			}
		})
	}
}

// TestGRPCForwardingSinkRetriesUnacknowledgedSegment proves TEL-111: a stream
// failure leaves the bridge segment durable and the same ID is retried.
func TestGRPCForwardingSinkRetriesUnacknowledgedSegment(t *testing.T) {
	bridgeSpool, err := NewSegmentSpool(t.TempDir(), 1<<20, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	collectorSpool, err := NewSegmentSpool(t.TempDir(), 1<<20, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	collector := &failFirstCollector{delegate: NewGRPCCollectorServer(collectorSpool, 1<<20, EventQuestion)}
	client, cleanup := startCollectorTestServer(t, collector)
	defer cleanup()

	var observedErrors atomic.Int64
	forwarder := NewGRPCForwardingSink(bridgeSpool, client, nil, 10*time.Millisecond, 10*time.Millisecond, func(error) {
		observedErrors.Add(1)
	})
	if err := forwarder.Record(Event{Timestamp: time.Now(), SessionID: "session-1", Kind: EventQuestion}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		segments, err := collectorSpool.Pending()
		return err == nil && len(segments) == 1
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := forwarder.Close(ctx); err != nil {
		t.Fatal(err)
	}
	segments, err := bridgeSpool.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 0 {
		t.Fatalf("bridge retained acknowledged segments: %v", segments)
	}
	if collector.calls.Load() < 2 || observedErrors.Load() == 0 {
		t.Fatalf("stream calls=%d observed errors=%d, want a failed attempt and retry", collector.calls.Load(), observedErrors.Load())
	}
}

func TestGRPCForwardingSinkRetainsDataWhileCollectorUnavailable(t *testing.T) {
	spool, err := NewSegmentSpool(t.TempDir(), 1<<20, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := grpc.NewClient("passthrough:///unavailable",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return nil, errors.New("collector unavailable")
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	forwarder := NewGRPCForwardingSink(spool, bridgev1.NewTelemetryCollectorServiceClient(conn), conn, 5*time.Millisecond, 5*time.Millisecond, nil)
	if err := forwarder.Record(Event{Timestamp: time.Now(), SessionID: "offline", Kind: EventQuestion}); err != nil {
		t.Fatal(err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := forwarder.Close(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error=%v, want deadline while collector is unavailable", err)
	}
	segments, err := spool.Pending()
	if err != nil {
		t.Fatal(err)
	}
	if len(segments) != 1 {
		t.Fatalf("pending segments=%v, want durable unacknowledged segment", segments)
	}
}

type failFirstCollector struct {
	bridgev1.UnimplementedTelemetryCollectorServiceServer
	delegate *GRPCCollectorServer
	calls    atomic.Int64
}

func (s *failFirstCollector) StreamSegments(stream grpc.BidiStreamingServer[bridgev1.TelemetrySegment, bridgev1.TelemetryAck]) error {
	if s.calls.Add(1) == 1 {
		return status.Error(codes.Unavailable, "temporary outage")
	}
	return s.delegate.StreamSegments(stream)
}

func startCollectorTestServer(t *testing.T, collector bridgev1.TelemetryCollectorServiceServer) (bridgev1.TelemetryCollectorServiceClient, func()) {
	t.Helper()
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	bridgev1.RegisterTelemetryCollectorServiceServer(server, collector)
	go func() { _ = server.Serve(listener) }()
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
	)
	if err != nil {
		server.Stop()
		t.Fatal(err)
	}
	return bridgev1.NewTelemetryCollectorServiceClient(conn), func() {
		_ = conn.Close()
		server.Stop()
		_ = listener.Close()
	}
}

func eventJSONL(t *testing.T, event Event) []byte {
	t.Helper()
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !condition() {
		t.Fatal(errors.New("timed out waiting for condition"))
	}
}
