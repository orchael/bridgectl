package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type blockingSink struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingSink) Record(Event) error {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return nil
}

func TestAsyncSinkNeverBlocksAndFlushes(t *testing.T) {
	downstream := &blockingSink{started: make(chan struct{}), release: make(chan struct{})}
	sink := NewAsyncSink(downstream, 1, nil)

	if err := sink.Record(Event{Kind: EventQuestion}); err != nil {
		t.Fatalf("first Record: %v", err)
	}
	select {
	case <-downstream.started:
	case <-time.After(time.Second):
		t.Fatal("downstream worker did not start")
	}
	if err := sink.Record(Event{Kind: EventAnswer}); err != nil {
		t.Fatalf("queued Record: %v", err)
	}
	start := time.Now()
	if err := sink.Record(Event{Kind: EventAnswer}); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("full Record error=%v, want ErrQueueFull", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("full Record blocked for %v", elapsed)
	}
	if sink.Dropped() != 1 {
		t.Fatalf("Dropped=%d, want 1", sink.Dropped())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := sink.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error=%v, want deadline", err)
	}
	close(downstream.release)
	if err := sink.Close(context.Background()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := sink.Record(Event{}); !errors.Is(err, ErrSinkClosed) {
		t.Fatalf("Record after close error=%v, want ErrSinkClosed", err)
	}
}

func TestAsyncSinkCountsDownstreamFailures(t *testing.T) {
	recorded := make(chan struct{})
	sink := NewAsyncSink(sinkFunc(func(Event) error {
		close(recorded)
		return errors.New("disk full")
	}), 1, nil)
	if err := sink.Record(Event{}); err != nil {
		t.Fatal(err)
	}
	<-recorded
	if err := sink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sink.Failures() != 1 {
		t.Fatalf("Failures=%d, want 1", sink.Failures())
	}
}

func TestAsyncSinkClosesDownstreamAfterDraining(t *testing.T) {
	downstream := &closingSink{}
	sink := NewAsyncSink(downstream, 1, nil)
	if err := sink.Record(Event{Kind: EventQuestion}); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if downstream.records != 1 || downstream.closes != 1 {
		t.Fatalf("records=%d closes=%d, want 1/1", downstream.records, downstream.closes)
	}
	if err := sink.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if downstream.closes != 1 {
		t.Fatalf("downstream closes=%d, want exactly 1", downstream.closes)
	}
}

type closingSink struct {
	records int
	closes  int
}

func (s *closingSink) Record(Event) error {
	s.records++
	return nil
}

func (s *closingSink) Close(context.Context) error {
	s.closes++
	return nil
}

type sinkFunc func(Event) error

func (f sinkFunc) Record(event Event) error { return f(event) }
