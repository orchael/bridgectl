package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
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
	payload := eventJSONL(t, validCollectorEvent(EventQuestion))
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
		{name: "unsafe ID", segment: &bridgev1.TelemetrySegment{Id: "../bad", Jsonl: eventJSONL(t, validCollectorEvent(EventQuestion))}, code: codes.InvalidArgument},
		{name: "invalid JSONL", segment: &bridgev1.TelemetrySegment{Id: "bad-json", Jsonl: []byte("not-json\n")}, code: codes.InvalidArgument},
		{name: "filtered kind", segment: &bridgev1.TelemetrySegment{Id: "wrong-kind", Jsonl: eventJSONL(t, validCollectorEvent(EventSessionStarted))}, code: codes.InvalidArgument},
		{name: "too large", segment: &bridgev1.TelemetrySegment{Id: "too-large", Jsonl: eventJSONL(t, validCollectorEvent(EventQuestion))}, code: codes.ResourceExhausted},
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
	base := validCollectorEvent(EventProviderOutput)
	base.Direction, base.Stream, base.ByteCount = DirectionAgent, StreamOutput, 1
	tests := []struct {
		name  string
		event Event
	}{
		{name: "provider direction", event: withEvent(base, func(e *Event) { e.Direction = DirectionHuman })},
		{name: "provider stream", event: withEvent(base, func(e *Event) { e.Stream = StreamInput })},
		{name: "user direction", event: withEvent(base, func(e *Event) { e.Kind, e.Stream = EventUserInput, StreamInput })},
		{name: "missing sequence", event: withEvent(base, func(e *Event) {
			e.Kind, e.Direction, e.Stream, e.Sequence = EventUserInput, DirectionHuman, StreamInput, 0
		})},
		{name: "opaque content leaked", event: withEvent(base, func(e *Event) {
			e.ByteCount, e.Text, e.OmittedReason, e.ContentHash = 2, "unsafe", OmittedInvalidUTF8, "hash"
		})},
		{name: "unredacted content", event: withEvent(base, func(e *Event) { e.ByteCount, e.Text = 20, "token=collector-secret" })},
		{name: "invalid source ID", event: withEvent(base, func(e *Event) { e.SourceID = "bridge east" })},
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

func TestValidateCollectorEventRequiresCommonEnvelope(t *testing.T) {
	valid := validCollectorEvent(EventSessionStarted)
	v2 := valid
	v2.SchemaVersion, v2.SourceID = 2, "bridge-a"
	contextEvent := v2
	contextEvent.Kind, contextEvent.Context = EventSessionContext, &SessionContext{OS: "linux", Arch: "amd64"}
	for _, event := range []Event{valid, v2, contextEvent} {
		if err := validateCollectorEvent(event); err != nil {
			t.Fatalf("valid event rejected: %+v: %v", event, err)
		}
	}
	badLabelContext := contextEvent
	badLabel := *contextEvent.Context
	badLabel.SourceLabel = "private laptop"
	badLabelContext.Context = &badLabel
	badDirectoryContext := contextEvent
	badDirectory := *contextEvent.Context
	badDirectory.WorkingDirectoryID = "raw/path"
	badDirectoryContext.Context = &badDirectory
	invalid := []Event{
		withEvent(valid, func(e *Event) { e.SchemaVersion = 0 }),
		withEvent(valid, func(e *Event) { e.Timestamp = time.Time{} }),
		withEvent(valid, func(e *Event) { e.SessionID = "" }),
		withEvent(valid, func(e *Event) { e.Sequence = 0 }),
		withEvent(v2, func(e *Event) { e.SourceID = "" }),
		withEvent(v2, func(e *Event) { e.Kind = EventSessionContext }),
		withEvent(v2, func(e *Event) { e.ActorID = "user@example.com" }),
		badLabelContext,
		badDirectoryContext,
	}
	for _, event := range invalid {
		if err := validateCollectorEvent(event); err == nil {
			t.Fatalf("invalid event accepted: %+v", event)
		}
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
	if err := forwarder.Record(validCollectorEvent(EventQuestion)); err != nil {
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
	offlineEvent := validCollectorEvent(EventQuestion)
	offlineEvent.SessionID = "offline"
	if err := forwarder.Record(offlineEvent); err != nil {
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

func TestGRPCForwardingSinkDeliversImmediatelyAfterFlush(t *testing.T) {
	bridgeSpool, err := NewSegmentSpool(t.TempDir(), 1<<20, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	collectorSpool, err := NewSegmentSpool(t.TempDir(), 1<<20, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, cleanup := startCollectorTestServer(t, NewGRPCCollectorServer(collectorSpool, 1<<20))
	defer cleanup()
	forwarder := NewGRPCForwardingSink(bridgeSpool, client, nil, 100*time.Millisecond, 10*time.Millisecond, nil)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := forwarder.Close(ctx); err != nil {
			t.Error(err)
		}
	}()

	// Let the initial empty delivery run halfway to the first flush. Previously,
	// the delivery timer then raced ahead of the flush and delayed this record by
	// almost a second interval.
	time.Sleep(50 * time.Millisecond)
	flushEvent := validCollectorEvent(EventQuestion)
	flushEvent.SessionID = "flush"
	if err := forwarder.Record(flushEvent); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 80*time.Millisecond, func() bool {
		segments, err := collectorSpool.Pending()
		return err == nil && len(segments) == 1
	})
}

func TestGRPCForwardingSinkDeliversSizeRotatedSegmentImmediately(t *testing.T) {
	bridgeSpool, err := NewSegmentSpool(t.TempDir(), 1024, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	collectorSpool, err := NewSegmentSpool(t.TempDir(), 2048, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, cleanup := startCollectorTestServer(t, NewGRPCCollectorServer(collectorSpool, 2048))
	defer cleanup()
	forwarder := NewGRPCForwardingSink(bridgeSpool, client, nil, time.Hour, time.Second, nil)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := forwarder.Close(ctx); err != nil {
			t.Error(err)
		}
	}()
	for index := 0; index < 2; index++ {
		event := validCollectorEvent(EventQuestion)
		event.Text = strings.Repeat("x", 700)
		if err := forwarder.Record(event); err != nil {
			t.Fatal(err)
		}
	}
	waitFor(t, 500*time.Millisecond, func() bool {
		segments, err := collectorSpool.Pending()
		return err == nil && len(segments) == 1
	})
}

func TestGRPCForwardingSinkTimesOutMissingAcknowledgement(t *testing.T) {
	spool, err := NewSegmentSpool(t.TempDir(), 1<<20, 10<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, cleanup := startCollectorTestServer(t, &neverAckCollector{})
	defer cleanup()
	errorCh := make(chan error, 1)
	forwarder := NewGRPCForwardingSink(spool, client, nil, 5*time.Millisecond, 5*time.Millisecond, func(err error) {
		select {
		case errorCh <- err:
		default:
		}
	})
	forwarder.deliveryTimeout = 25 * time.Millisecond
	hungEvent := validCollectorEvent(EventQuestion)
	hungEvent.SessionID = "hung"
	if err := forwarder.Record(hungEvent); err != nil {
		t.Fatal(err)
	}
	select {
	case <-errorCh:
	case <-time.After(time.Second):
		t.Fatal("missing acknowledgement did not time out")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := forwarder.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error=%v, want deadline with no acknowledgement", err)
	}
}

type failFirstCollector struct {
	bridgev1.UnimplementedTelemetryCollectorServiceServer
	delegate *GRPCCollectorServer
	calls    atomic.Int64
}

type neverAckCollector struct {
	bridgev1.UnimplementedTelemetryCollectorServiceServer
}

func (*neverAckCollector) StreamSegments(stream grpc.BidiStreamingServer[bridgev1.TelemetrySegment, bridgev1.TelemetryAck]) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	<-stream.Context().Done()
	return stream.Context().Err()
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

func validCollectorEvent(kind EventKind) Event {
	return Event{SchemaVersion: 1, Timestamp: time.Now(), SessionID: "session-1", Sequence: 1, Kind: kind}
}

func withEvent(event Event, change func(*Event)) Event {
	change(&event)
	return event
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
