package telemetry

import (
	"context"
	"sync"
	"time"
)

// LocalSpoolingSink owns timed sealing for local-only telemetry. Persistence
// callers run behind AsyncSink; sealed segments are safe for concurrent reports.
type LocalSpoolingSink struct {
	spool  *SegmentSpool
	stop   chan struct{}
	done   chan struct{}
	mu     sync.Mutex
	closed bool
}

func NewLocalSpoolingSink(spool *SegmentSpool, flushInterval time.Duration, onError func(error)) *LocalSpoolingSink {
	if flushInterval <= 0 {
		flushInterval = 10 * time.Second
	}
	sink := &LocalSpoolingSink{spool: spool, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(sink.done)
		ticker := time.NewTicker(flushInterval)
		defer ticker.Stop()
		for {
			select {
			case <-sink.stop:
				return
			case <-ticker.C:
				if _, err := spool.Seal(); err != nil && onError != nil {
					onError(err)
				}
			}
		}
	}()
	return sink
}

func (s *LocalSpoolingSink) Record(event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSinkClosed
	}
	return s.spool.Record(event)
}

func (s *LocalSpoolingSink) Close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stop)
	}
	s.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.spool.Close(ctx)
	}
}
