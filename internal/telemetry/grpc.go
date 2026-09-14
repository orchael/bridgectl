package telemetry

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// GRPCCollectorServer accepts immutable telemetry segments and acknowledges
// only after SegmentSpool.Accept has durably persisted them.
type GRPCCollectorServer struct {
	bridgev1.UnimplementedTelemetryCollectorServiceServer
	spool           *SegmentSpool
	maxSegmentBytes int64
	kinds           map[EventKind]bool
}

func NewGRPCCollectorServer(spool *SegmentSpool, maxSegmentBytes int64, kinds ...EventKind) *GRPCCollectorServer {
	allowed := make(map[EventKind]bool, len(kinds))
	for _, kind := range kinds {
		allowed[kind] = true
	}
	return &GRPCCollectorServer{spool: spool, maxSegmentBytes: maxSegmentBytes, kinds: allowed}
}

func (s *GRPCCollectorServer) StreamSegments(stream grpc.BidiStreamingServer[bridgev1.TelemetrySegment, bridgev1.TelemetryAck]) error {
	for {
		segment, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if !validSegmentID(segment.GetId()) {
			return status.Error(codes.InvalidArgument, "invalid telemetry segment ID")
		}
		if int64(len(segment.GetJsonl())) > s.maxSegmentBytes {
			return status.Error(codes.ResourceExhausted, "telemetry segment exceeds size limit")
		}
		if err := s.validateJSONL(segment.GetJsonl()); err != nil {
			return status.Errorf(codes.InvalidArgument, "invalid telemetry segment: %v", err)
		}
		stored, err := s.spool.Accept(segment.GetId(), segment.GetJsonl())
		if errors.Is(err, ErrSegmentConflict) {
			return status.Error(codes.AlreadyExists, "telemetry segment ID already contains different data")
		}
		if err != nil {
			return status.Errorf(codes.Internal, "persist telemetry segment: %v", err)
		}
		if err := stream.Send(&bridgev1.TelemetryAck{Id: segment.GetId(), Stored: stored}); err != nil {
			return err
		}
	}
}

func (s *GRPCCollectorServer) validateJSONL(data []byte) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("empty JSONL")
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	maxToken := int(s.maxSegmentBytes)
	if maxToken < bufio.MaxScanTokenSize {
		maxToken = bufio.MaxScanTokenSize
	}
	scanner.Buffer(make([]byte, 4096), maxToken)
	count := 0
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return err
		}
		if !validEventKind(event.Kind) {
			return errors.New("unsupported event kind")
		}
		if err := validateCollectorEvent(event); err != nil {
			return err
		}
		if len(s.kinds) > 0 && !s.kinds[event.Kind] {
			return errors.New("event kind is not enabled")
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if count == 0 {
		return errors.New("empty JSONL")
	}
	return nil
}

func validateCollectorEvent(event Event) error {
	if event.Text != "" && DefaultRedactor(event.Text) != event.Text {
		return errors.New("event text contains unredacted sensitive content")
	}
	switch event.Kind {
	case EventProviderOutput:
		if event.SchemaVersion != 1 || event.Sequence == 0 || event.Direction != DirectionAgent || event.ByteCount < 1 {
			return errors.New("invalid provider output metadata")
		}
		if event.Stream != StreamOutput && event.Stream != StreamThinking {
			return errors.New("invalid provider output stream")
		}
		return validateOmittedContent(event)
	case EventUserInput:
		if event.SchemaVersion != 1 || event.Sequence == 0 || event.Direction != DirectionHuman || event.Stream != StreamInput || event.ByteCount < 1 {
			return errors.New("invalid user input metadata")
		}
		return validateOmittedContent(event)
	default:
		return nil
	}
}

func validateOmittedContent(event Event) error {
	if event.OmittedReason == "" {
		if event.ContentHash != "" {
			return errors.New("interaction content hash requires an omission reason")
		}
		return nil
	}
	if event.OmittedReason != OmittedInvalidUTF8 || event.Text != "" || len(event.ContentHash) != sha256HexLength {
		return errors.New("invalid omitted interaction content")
	}
	if _, err := hex.DecodeString(event.ContentHash); err != nil {
		return errors.New("invalid omitted interaction content hash")
	}
	return nil
}

const sha256HexLength = 64
