package telemetry

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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

	downstreamCloseOnce sync.Once
	downstreamCloseErr  error
}

func NewAsyncSink(sink Sink, queueSize int, onError func(error)) *AsyncSink {
	if queueSize <= 0 {
		queueSize = 1024
	}
	s := &AsyncSink{
		sink: sink, queue: make(chan Event, queueSize), done: make(chan struct{}), onError: onError,
	}
	go s.run()
	return s
}

func (s *AsyncSink) run() {
	defer close(s.done)
	for event := range s.queue {
		if s.sink == nil {
			continue
		}
		if err := s.sink.Record(event); err != nil {
			s.failures.Add(1)
			if s.onError != nil {
				s.onError(err)
			}
		}
	}
}

func (s *AsyncSink) Record(event Event) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return ErrSinkClosed
	}
	select {
	case s.queue <- event:
		return nil
	default:
		s.dropped.Add(1)
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
		return s.downstreamCloseErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
