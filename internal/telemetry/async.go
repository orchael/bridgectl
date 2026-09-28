package telemetry

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrQueueFull  = errors.New("telemetry queue full")
	ErrSinkClosed = errors.New("telemetry sink closed")
)

// AsyncSink isolates a synchronous Sink behind a bounded, non-blocking queue.
type AsyncSink struct {
	sink    Sink
	queue   chan Event
	done    chan struct{}
	onError func(error)

	mu       sync.RWMutex
	closed   bool
	dropped  atomic.Uint64
	failures atomic.Uint64

	sessions       map[sessionIdentity]*completenessState
	captureKinds   []EventKind
	includeText    bool
	checkpointWake chan struct{}
	checkpointErr  error

	downstreamCloseOnce sync.Once
	downstreamCloseErr  error
}

func NewAsyncSink(sink Sink, queueSize int, onError func(error)) *AsyncSink {
	if queueSize <= 0 {
		queueSize = 1024
	}
	s := &AsyncSink{
		sink: sink, queue: make(chan Event, queueSize), done: make(chan struct{}), onError: onError,
		sessions: make(map[sessionIdentity]*completenessState), checkpointWake: make(chan struct{}, 1),
	}
	go s.run()
	return s
}

func (s *AsyncSink) run() {
	defer close(s.done)
	ticker := time.NewTicker(DefaultFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case event, ok := <-s.queue:
			if !ok {
				s.checkpointErr = s.flushCheckpoints(false)
				return
			}
			var err error
			if s.sink != nil {
				err = s.sink.Record(event)
			}
			s.mu.Lock()
			if state := s.sessions[eventKey(event)]; state != nil {
				state.processed++
				state.revision++
				if err != nil {
					state.stats.SinkFailedEvents++
				}
			}
			s.mu.Unlock()
			if err != nil {
				s.failures.Add(1)
				if s.onError != nil {
					s.onError(err)
				}
			}
			_ = s.flushCheckpoints(true)
		case <-s.checkpointWake:
			_ = s.flushCheckpoints(true)
		case <-ticker.C:
			_ = s.flushCheckpoints(false)
		}
	}
}

func (s *AsyncSink) Record(event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSinkClosed
	}
	state := s.completenessLocked(event)
	if state != nil {
		state.stats.AttemptedEvents++
		state.stats.LastSequence = max(state.stats.LastSequence, event.Sequence)
		if event.OmittedReason != "" {
			state.stats.OmittedEvents++
		}
	}
	select {
	case s.queue <- event:
		return nil
	default:
		s.dropped.Add(1)
		if state != nil {
			state.stats.QueueDroppedEvents++
		}
		s.wakeCheckpoint()
		return ErrQueueFull
	}
}

func (s *AsyncSink) Dropped() uint64  { return s.dropped.Load() }
func (s *AsyncSink) Failures() uint64 { return s.failures.Load() }

func (s *AsyncSink) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.queue)
	}
	s.mu.Unlock()

	select {
	case <-s.done:
		s.downstreamCloseOnce.Do(func() {
			if closer, ok := s.sink.(interface{ Close(context.Context) error }); ok {
				s.downstreamCloseErr = closer.Close(ctx)
			}
		})
		return errors.Join(s.checkpointErr, s.downstreamCloseErr)
	case <-ctx.Done():
		return ctx.Err()
	}
}
