package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
	"google.golang.org/grpc"
)

// GRPCForwardingSink durably records events before forwarding sealed segments.
// Network work happens only in its worker and never in Record's caller beyond
// the local spool append.
type GRPCForwardingSink struct {
	spool         *SegmentSpool
	client        bridgev1.TelemetryCollectorServiceClient
	flushInterval time.Duration
	retryInterval time.Duration
	onError       func(error)
	closer        io.Closer

	stateMu sync.Mutex
	closed  bool
	stop    chan struct{}
	done    chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc

	deliveryMu sync.Mutex
	stream     grpc.BidiStreamingClient[bridgev1.TelemetrySegment, bridgev1.TelemetryAck]
}

func NewGRPCForwardingSink(spool *SegmentSpool, client bridgev1.TelemetryCollectorServiceClient, closer io.Closer, flushInterval, retryInterval time.Duration, onError func(error)) *GRPCForwardingSink {
	if flushInterval <= 0 {
		flushInterval = 10 * time.Second
	}
	if retryInterval <= 0 {
		retryInterval = time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	sink := &GRPCForwardingSink{
		spool: spool, client: client, closer: closer, flushInterval: flushInterval, retryInterval: retryInterval,
		onError: onError, stop: make(chan struct{}), done: make(chan struct{}), ctx: ctx, cancel: cancel,
	}
	go sink.run()
	return sink
}

func (s *GRPCForwardingSink) Record(event Event) error {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.closed {
		return ErrSinkClosed
	}
	return s.spool.Record(event)
}

func (s *GRPCForwardingSink) run() {
	defer close(s.done)
	flushTicker := time.NewTicker(s.flushInterval)
	defer flushTicker.Stop()
	deliveryTimer := time.NewTimer(0)
	defer deliveryTimer.Stop()
	const maxBackoff = 30 * time.Second
	backoff := min(s.retryInterval, maxBackoff)
	for {
		select {
		case <-s.stop:
			return
		case <-flushTicker.C:
			if _, err := s.spool.Seal(); err != nil {
				s.report(fmt.Errorf("seal telemetry segment: %w", err))
			}
		case <-deliveryTimer.C:
			if err := s.deliverPending(s.ctx, true); err != nil && !errors.Is(err, context.Canceled) {
				s.report(fmt.Errorf("stream telemetry segment: %w", err))
				deliveryTimer.Reset(backoff)
				backoff = min(backoff*2, maxBackoff)
			} else {
				backoff = s.retryInterval
				deliveryTimer.Reset(s.flushInterval)
			}
		}
	}
}

func (s *GRPCForwardingSink) deliverPending(ctx context.Context, keepOpen bool) error {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	segments, err := s.spool.Pending()
	if err != nil {
		return err
	}
	for _, segment := range segments {
		if s.stream == nil {
			s.stream, err = s.client.StreamSegments(ctx)
			if err != nil {
				return err
			}
		}
		data, err := s.spool.Read(segment.ID)
		if err != nil {
			s.resetStreamLocked()
			return err
		}
		if err := s.stream.Send(&bridgev1.TelemetrySegment{Id: segment.ID, Jsonl: data}); err != nil {
			s.resetStreamLocked()
			return err
		}
		ack, err := s.stream.Recv()
		if err != nil {
			s.resetStreamLocked()
			return err
		}
		if ack.GetId() != segment.ID {
			s.resetStreamLocked()
			return fmt.Errorf("collector acknowledged segment %q, want %q", ack.GetId(), segment.ID)
		}
		if err := s.spool.Remove(segment.ID); err != nil {
			s.resetStreamLocked()
			return err
		}
	}
	if !keepOpen {
		s.resetStreamLocked()
	}
	return nil
}

func (s *GRPCForwardingSink) resetStreamLocked() {
	if s.stream != nil {
		_ = s.stream.CloseSend()
		s.stream = nil
	}
}

func (s *GRPCForwardingSink) Close(ctx context.Context) (returnErr error) {
	defer func() { returnErr = errors.Join(returnErr, s.closeConnection()) }()
	s.stateMu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stop)
	}
	s.stateMu.Unlock()

	select {
	case <-s.done:
	case <-ctx.Done():
		s.cancel()
		return ctx.Err()
	}

	s.deliveryMu.Lock()
	s.resetStreamLocked()
	s.deliveryMu.Unlock()
	if _, err := s.spool.Seal(); err != nil {
		s.cancel()
		return err
	}
	for {
		segments, err := s.spool.Pending()
		if err != nil {
			s.cancel()
			return err
		}
		if len(segments) == 0 {
			s.cancel()
			return nil
		}
		if err := s.deliverPending(ctx, false); err != nil {
			s.report(fmt.Errorf("flush telemetry segment: %w", err))
		} else {
			continue
		}
		timer := time.NewTimer(s.retryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			s.cancel()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *GRPCForwardingSink) closeConnection() error {
	if s.closer == nil {
		return nil
	}
	return s.closer.Close()
}

func (s *GRPCForwardingSink) report(err error) {
	if err != nil && s.onError != nil {
		s.onError(err)
	}
}

var _ Sink = (*GRPCForwardingSink)(nil)
