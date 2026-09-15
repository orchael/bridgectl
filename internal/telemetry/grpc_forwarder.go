package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	bridgev1 "github.com/orchael/bridgectl/gen/bridge/v1"
)

// GRPCForwardingSink durably records events before forwarding sealed segments.
// Network work happens only in its worker and never in Record's caller beyond
// the local spool append.
type GRPCForwardingSink struct {
	spool           *SegmentSpool
	client          bridgev1.TelemetryCollectorServiceClient
	flushInterval   time.Duration
	retryInterval   time.Duration
	deliveryTimeout time.Duration
	onError         func(error)
	closer          io.Closer

	stateMu sync.Mutex
	closed  bool
	stop    chan struct{}
	done    chan struct{}
	sealed  chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc

	deliveryMu sync.Mutex
	stream     bridgev1.TelemetryCollectorService_StreamSegmentsClient
	streamStop context.CancelFunc
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
		deliveryTimeout: 10 * time.Second, onError: onError, stop: make(chan struct{}), done: make(chan struct{}), sealed: make(chan struct{}, 1), ctx: ctx, cancel: cancel,
	}
	spool.SetSealNotifier(func(Segment) {
		select {
		case sink.sealed <- struct{}{}:
		default:
		}
	})
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
		case <-s.sealed:
			if err := s.deliverPending(s.ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.report(fmt.Errorf("stream telemetry segment: %w", err))
				resetTimer(deliveryTimer, backoff)
				backoff = min(backoff*2, maxBackoff)
			} else {
				backoff = s.retryInterval
				resetTimer(deliveryTimer, s.flushInterval)
			}
		case <-flushTicker.C:
			segment, err := s.spool.Seal()
			if err != nil {
				s.report(fmt.Errorf("seal telemetry segment: %w", err))
			} else if segment.ID != "" {
				if err := s.deliverPending(s.ctx); err != nil && !errors.Is(err, context.Canceled) {
					s.report(fmt.Errorf("stream telemetry segment: %w", err))
					resetTimer(deliveryTimer, backoff)
					backoff = min(backoff*2, maxBackoff)
				} else {
					backoff = s.retryInterval
					resetTimer(deliveryTimer, s.flushInterval)
				}
			}
		case <-deliveryTimer.C:
			if err := s.deliverPending(s.ctx); err != nil && !errors.Is(err, context.Canceled) {
				s.report(fmt.Errorf("stream telemetry segment: %w", err))
				resetTimer(deliveryTimer, backoff)
				backoff = min(backoff*2, maxBackoff)
			} else {
				backoff = s.retryInterval
				resetTimer(deliveryTimer, s.flushInterval)
			}
		}
	}
}

func (s *GRPCForwardingSink) deliverPending(ctx context.Context) (returnErr error) {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	defer func() {
		if returnErr != nil {
			s.resetStreamLocked()
		}
	}()
	segments, err := s.spool.Pending()
	if err != nil {
		return err
	}
	if len(segments) == 0 {
		return nil
	}
	if err := s.ensureStreamLocked(); err != nil {
		return err
	}
	for _, segment := range segments {
		data, err := s.spool.Read(segment.ID)
		if err != nil {
			return err
		}
		if err := s.stream.Send(&bridgev1.TelemetrySegment{Id: segment.ID, Jsonl: data}); err != nil {
			return err
		}
		ack, err := s.receiveAckLocked(ctx)
		if err != nil {
			return err
		}
		if ack.GetId() != segment.ID {
			return fmt.Errorf("collector acknowledged segment %q, want %q", ack.GetId(), segment.ID)
		}
		if err := s.spool.Remove(segment.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *GRPCForwardingSink) ensureStreamLocked() error {
	if s.stream != nil {
		return nil
	}
	streamCtx, cancel := context.WithCancel(s.ctx)
	stream, err := s.client.StreamSegments(streamCtx)
	if err != nil {
		cancel()
		return err
	}
	s.stream = stream
	s.streamStop = cancel
	return nil
}

type telemetryAckResult struct {
	ack *bridgev1.TelemetryAck
	err error
}

func (s *GRPCForwardingSink) receiveAckLocked(ctx context.Context) (*bridgev1.TelemetryAck, error) {
	result := make(chan telemetryAckResult, 1)
	stream := s.stream
	go func() {
		ack, err := stream.Recv()
		result <- telemetryAckResult{ack: ack, err: err}
	}()
	timer := time.NewTimer(s.deliveryTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, context.DeadlineExceeded
	case received := <-result:
		return received.ack, received.err
	}
}

func (s *GRPCForwardingSink) resetStreamLocked() {
	if s.stream != nil {
		_ = s.stream.CloseSend()
		s.stream = nil
	}
	if s.streamStop != nil {
		s.streamStop()
		s.streamStop = nil
	}
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
}

func (s *GRPCForwardingSink) Close(ctx context.Context) (returnErr error) {
	defer func() {
		s.deliveryMu.Lock()
		s.resetStreamLocked()
		s.deliveryMu.Unlock()
		returnErr = errors.Join(returnErr, s.closeConnection())
	}()
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
		if err := s.deliverPending(ctx); err != nil {
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
